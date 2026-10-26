// Package engine implementa el libro de ofertas (order book) y el matching.
//
// Diseño clave: el Book NO es seguro para uso concurrente a propósito.
// El orden de llegada define quién gana un cruce, así que el matching debe ser
// secuencial. Se escala haciéndolo rapidísimo (todo en memoria, sin I/O) y
// dejando que UN solo goroutine lo ejecute; la concurrencia vive fuera
// (API y cola de entrada). Ver docs/DEFENSA.md.
package engine

import (
	"container/heap"
	"container/list"
	"errors"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
)

// Límites que garantizan que Price*Qty nunca desborda int64 (1e9 * 1e9 = 1e18 < 9.2e18).
const (
	MaxPrice domain.Price = 1_000_000_000
	MaxQty   domain.Qty   = 1_000_000_000
)

var (
	ErrInvalidOrder   = errors.New("orden inválida")
	ErrDuplicateOrder = errors.New("la orden ya existe en el libro")
	ErrOrderNotFound  = errors.New("orden no encontrada en el libro")
)

// level agrupa las órdenes de un mismo precio, en cola FIFO (prioridad por tiempo).
type level struct {
	price  domain.Price
	orders *list.List // de *domain.Order; el frente es la más antigua
	index  int        // posición en el heap (para poder borrar el nivel en O(log n))
}

// priceHeap ordena los niveles de un lado: el mejor precio queda en la raíz.
// Bids: el precio más alto primero. Asks: el más bajo primero.
type priceHeap struct {
	levels []*level
	better func(a, b domain.Price) bool
}

func (h priceHeap) Len() int           { return len(h.levels) }
func (h priceHeap) Less(i, j int) bool { return h.better(h.levels[i].price, h.levels[j].price) }
func (h priceHeap) Swap(i, j int) {
	h.levels[i], h.levels[j] = h.levels[j], h.levels[i]
	h.levels[i].index = i
	h.levels[j].index = j
}
func (h *priceHeap) Push(x any) {
	l := x.(*level)
	l.index = len(h.levels)
	h.levels = append(h.levels, l)
}
func (h *priceHeap) Pop() any {
	n := len(h.levels)
	l := h.levels[n-1]
	h.levels[n-1] = nil
	h.levels = h.levels[:n-1]
	return l
}

// halfBook es un lado del libro: niveles por precio + heap para hallar el mejor.
type halfBook struct {
	byPrice map[domain.Price]*level
	heap    *priceHeap
}

func newHalfBook(better func(a, b domain.Price) bool) *halfBook {
	return &halfBook{
		byPrice: make(map[domain.Price]*level),
		heap:    &priceHeap{better: better},
	}
}

// best devuelve el mejor nivel o nil si el lado está vacío.
func (h *halfBook) best() *level {
	if len(h.heap.levels) == 0 {
		return nil
	}
	return h.heap.levels[0]
}

func (h *halfBook) add(o *domain.Order) *list.Element {
	l, ok := h.byPrice[o.Price]
	if !ok {
		l = &level{price: o.Price, orders: list.New()}
		h.byPrice[o.Price] = l
		heap.Push(h.heap, l)
	}
	return l.orders.PushBack(o)
}

func (h *halfBook) remove(o *domain.Order, e *list.Element) {
	l := h.byPrice[o.Price]
	l.orders.Remove(e)
	if l.orders.Len() == 0 {
		heap.Remove(h.heap, l.index)
		delete(h.byPrice, o.Price)
	}
}

// Book es el libro de ofertas de UN instrumento (en el MVP, Vibranium/BRL).
type Book struct {
	bids *halfBook // compras: gana el precio más alto
	asks *halfBook // ventas:  gana el precio más bajo

	// resting indexa las órdenes que están en el libro para cancelar en O(1).
	resting map[string]*restingRef

	seq      uint64 // contador de prioridad de órdenes
	tradeSeq uint64 // contador de trades
}

type restingRef struct {
	order *domain.Order
	elem  *list.Element
}

func NewBook() *Book {
	return &Book{
		bids:    newHalfBook(func(a, b domain.Price) bool { return a > b }),
		asks:    newHalfBook(func(a, b domain.Price) bool { return a < b }),
		resting: make(map[string]*restingRef),
	}
}

func (b *Book) half(s domain.Side) *halfBook {
	if s == domain.Buy {
		return b.bids
	}
	return b.asks
}

// Result es lo que produce procesar una orden: su estado final y los trades generados.
type Result struct {
	Order  domain.Order // copia del estado de la orden tras el matching
	Trades []domain.Trade
}

// Validate comprueba una orden sin tocar el libro (el exchange la usa antes de reservar fondos).
func Validate(o domain.Order) error {
	switch {
	case o.ID == "" || o.UserID == "":
		return ErrInvalidOrder
	case o.Side != domain.Buy && o.Side != domain.Sell:
		return ErrInvalidOrder
	case o.Price <= 0 || o.Price > MaxPrice:
		return ErrInvalidOrder
	case o.Qty <= 0 || o.Qty > MaxQty:
		return ErrInvalidOrder
	}
	return nil
}

// Submit procesa una orden límite: la cruza contra el lado opuesto mientras los
// precios sean compatibles y deja el remanente "en negociación" en el libro.
func (b *Book) Submit(in domain.Order) (Result, error) {
	if err := Validate(in); err != nil {
		return Result{}, err
	}
	if _, dup := b.resting[in.ID]; dup {
		return Result{}, ErrDuplicateOrder
	}

	b.seq++
	o := in
	o.Seq = b.seq
	o.Filled = 0
	o.Status = domain.StatusOpen

	trades := b.match(&o)

	switch {
	case o.Remaining() == 0:
		o.Status = domain.StatusFilled
	default:
		if o.Filled > 0 {
			o.Status = domain.StatusPartiallyFilled
		}
		// El remanente queda esperando contraparte en el libro.
		stored := o
		b.resting[o.ID] = &restingRef{order: &stored, elem: b.half(o.Side).add(&stored)}
	}

	return Result{Order: o, Trades: trades}, nil
}

// crosses indica si el precio límite del taker acepta el precio del maker.
func crosses(taker *domain.Order, makerPrice domain.Price) bool {
	if taker.Side == domain.Buy {
		return makerPrice <= taker.Price // compro si me venden a mi límite o más barato
	}
	return makerPrice >= taker.Price // vendo si me pagan mi límite o más
}

// match ejecuta el cruce de la orden entrante (taker) contra el libro (makers).
func (b *Book) match(taker *domain.Order) []domain.Trade {
	var trades []domain.Trade
	opposite := b.half(taker.Side.Opposite())

	for taker.Remaining() > 0 {
		best := opposite.best()
		if best == nil || !crosses(taker, best.price) {
			break
		}

		front := best.orders.Front()
		maker := front.Value.(*domain.Order)

		qty := min(taker.Remaining(), maker.Remaining())
		taker.Filled += qty
		maker.Filled += qty

		b.tradeSeq++
		t := domain.Trade{
			ID:           b.tradeSeq,
			Seq:          b.tradeSeq,
			Price:        maker.Price, // el precio lo fija quien ya estaba en el libro
			Qty:          qty,
			MakerOrderID: maker.ID,
			TakerOrderID: taker.ID,
		}
		if taker.Side == domain.Buy {
			t.BuyOrderID, t.BuyUserID = taker.ID, taker.UserID
			t.SellOrderID, t.SellUserID = maker.ID, maker.UserID
		} else {
			t.BuyOrderID, t.BuyUserID = maker.ID, maker.UserID
			t.SellOrderID, t.SellUserID = taker.ID, taker.UserID
		}
		trades = append(trades, t)

		if maker.Remaining() == 0 {
			maker.Status = domain.StatusFilled
			opposite.remove(maker, front)
			delete(b.resting, maker.ID)
		} else {
			maker.Status = domain.StatusPartiallyFilled
		}
	}
	return trades
}

// Cancel retira una orden del libro. Devuelve su estado final.
func (b *Book) Cancel(id string) (domain.Order, error) {
	ref, ok := b.resting[id]
	if !ok {
		return domain.Order{}, ErrOrderNotFound
	}
	b.half(ref.order.Side).remove(ref.order, ref.elem)
	delete(b.resting, id)
	ref.order.Status = domain.StatusCancelled
	return *ref.order, nil
}

// Level es una fila agregada del libro: cuánto hay disponible a cada precio.
type Level struct {
	Price domain.Price
	Qty   domain.Qty
}

// Depth devuelve hasta n niveles de un lado, del mejor al peor precio.
// Pensado para consulta/observabilidad; recorre una copia ordenada del heap.
func (b *Book) Depth(side domain.Side, n int) []Level {
	h := b.half(side)
	levels := make([]*level, len(h.heap.levels))
	copy(levels, h.heap.levels)
	// ordenamiento simple por mejor precio (n de niveles es chico en consulta)
	for i := 1; i < len(levels); i++ {
		for j := i; j > 0 && h.heap.better(levels[j].price, levels[j-1].price); j-- {
			levels[j], levels[j-1] = levels[j-1], levels[j]
		}
	}
	if n > len(levels) {
		n = len(levels)
	}
	out := make([]Level, 0, n)
	for _, l := range levels[:n] {
		var q domain.Qty
		for e := l.orders.Front(); e != nil; e = e.Next() {
			q += e.Value.(*domain.Order).Remaining()
		}
		out = append(out, Level{Price: l.price, Qty: q})
	}
	return out
}

// RestingOrders devuelve copias de todas las órdenes que están en negociación.
// Sirve para auditar invariantes (p. ej. fondos reservados == órdenes abiertas).
func (b *Book) RestingOrders() []domain.Order {
	out := make([]domain.Order, 0, len(b.resting))
	for _, ref := range b.resting {
		out = append(out, *ref.order)
	}
	return out
}
