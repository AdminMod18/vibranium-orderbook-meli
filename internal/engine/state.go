package engine

import (
	"fmt"
	"sort"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
)

// BookState es una copia serializable del libro: las órdenes en negociación y los contadores.
// Con ella se reconstruye el libro idéntico (incluida la prioridad de tiempo) sin reproducir el log.
type BookState struct {
	Orders   []domain.Order
	Seq      uint64
	TradeSeq uint64
}

// Capture copia el estado del libro. Debe llamarse desde el mismo goroutine que lo usa.
func (b *Book) Capture() BookState {
	return BookState{Orders: b.RestingOrders(), Seq: b.seq, TradeSeq: b.tradeSeq}
}

// RestoreBook reconstruye un libro. Las órdenes se reinsertan por Seq ascendente, que es lo que
// restablece la cola FIFO de cada nivel de precio (prioridad de tiempo).
func RestoreBook(s BookState) (*Book, error) {
	b := NewBook()
	b.seq, b.tradeSeq = s.Seq, s.TradeSeq
	orders := append([]domain.Order(nil), s.Orders...)
	sort.Slice(orders, func(i, j int) bool { return orders[i].Seq < orders[j].Seq })
	for _, o := range orders {
		if err := Validate(o); err != nil || o.Remaining() <= 0 || o.Seq == 0 || o.Seq > s.Seq {
			return nil, fmt.Errorf("orden %q inválida en el snapshot", o.ID)
		}
		if _, dup := b.resting[o.ID]; dup {
			return nil, fmt.Errorf("orden %q duplicada en el snapshot", o.ID)
		}
		stored := o
		b.resting[o.ID] = &restingRef{order: &stored, elem: b.half(o.Side).add(&stored)}
	}
	return b, nil
}
