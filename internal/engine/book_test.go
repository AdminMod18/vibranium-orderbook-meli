package engine

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
)

func ord(id, user string, side domain.Side, price domain.Price, qty domain.Qty) domain.Order {
	return domain.Order{ID: id, UserID: user, Side: side, Price: price, Qty: qty}
}

func mustSubmit(t *testing.T, b *Book, o domain.Order) Result {
	t.Helper()
	r, err := b.Submit(o)
	if err != nil {
		t.Fatalf("Submit(%s): %v", o.ID, err)
	}
	return r
}

func TestOrderWithoutCounterpartyRestsInBook(t *testing.T) {
	b := NewBook()
	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 10))

	if len(r.Trades) != 0 || r.Order.Status != domain.StatusOpen {
		t.Fatalf("esperaba orden OPEN sin trades, got %+v", r)
	}
	if d := b.Depth(domain.Buy, 5); len(d) != 1 || d[0] != (Level{100, 10}) {
		t.Fatalf("depth inesperado: %+v", d)
	}
}

func TestFullMatchUsesMakerPrice(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("s1", "bob", domain.Sell, 90, 10))        // maker vende a 90
	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 10)) // taker ofrece hasta 100

	if len(r.Trades) != 1 {
		t.Fatalf("esperaba 1 trade, got %d", len(r.Trades))
	}
	tr := r.Trades[0]
	if tr.Price != 90 || tr.Qty != 10 || tr.Total() != 900 {
		t.Fatalf("trade debe ejecutar a precio del maker (90): %+v", tr)
	}
	if tr.BuyUserID != "alice" || tr.SellUserID != "bob" || tr.MakerOrderID != "s1" {
		t.Fatalf("partes del trade incorrectas: %+v", tr)
	}
	if r.Order.Status != domain.StatusFilled {
		t.Fatalf("taker debe quedar FILLED, got %v", r.Order.Status)
	}
	if len(b.Depth(domain.Sell, 5)) != 0 {
		t.Fatal("el libro de ventas debe quedar vacío")
	}
}

func TestPartialFillLeavesRemainderResting(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("s1", "bob", domain.Sell, 100, 4))
	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 10))

	if r.Order.Status != domain.StatusPartiallyFilled || r.Order.Filled != 4 {
		t.Fatalf("taker debe quedar PARTIALLY_FILLED con 4 ejecutadas: %+v", r.Order)
	}
	if d := b.Depth(domain.Buy, 5); len(d) != 1 || d[0] != (Level{100, 6}) {
		t.Fatalf("debe quedar 6 unidades comprando a 100: %+v", d)
	}
}

func TestMakerPartiallyFilledKeepsQueuePosition(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("s1", "bob", domain.Sell, 100, 10))
	mustSubmit(t, b, ord("s2", "carol", domain.Sell, 100, 10))
	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 4))

	if r.Trades[0].MakerOrderID != "s1" {
		t.Fatal("debe ejecutar primero la venta más antigua (s1)")
	}
	r = mustSubmit(t, b, ord("b2", "dave", domain.Buy, 100, 8))
	// s1 tenía 6 restantes: primero esas 6, luego 2 de s2
	if len(r.Trades) != 2 || r.Trades[0].MakerOrderID != "s1" || r.Trades[0].Qty != 6 ||
		r.Trades[1].MakerOrderID != "s2" || r.Trades[1].Qty != 2 {
		t.Fatalf("prioridad de tiempo incumplida: %+v", r.Trades)
	}
}

func TestPricePriorityBestPriceFirst(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("s1", "bob", domain.Sell, 102, 5))
	mustSubmit(t, b, ord("s2", "carol", domain.Sell, 100, 5)) // más barata, llegó después
	mustSubmit(t, b, ord("s3", "dave", domain.Sell, 101, 5))

	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 105, 12))
	got := []domain.Price{r.Trades[0].Price, r.Trades[1].Price, r.Trades[2].Price}
	want := []domain.Price{100, 101, 102}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("debe barrer del más barato al más caro: got %v want %v", got, want)
	}
	if r.Trades[2].Qty != 2 {
		t.Fatalf("último trade debe ser parcial de 2: %+v", r.Trades[2])
	}
}

func TestNonCrossingOrdersDoNotMatch(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("s1", "bob", domain.Sell, 101, 5))
	r := mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 5))
	if len(r.Trades) != 0 {
		t.Fatal("compra a 100 no debe cruzar con venta a 101")
	}
}

func TestSellTakerMatchesBestBid(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("b1", "alice", domain.Buy, 98, 5))
	mustSubmit(t, b, ord("b2", "carol", domain.Buy, 100, 5))
	r := mustSubmit(t, b, ord("s1", "bob", domain.Sell, 97, 7))

	if r.Trades[0].Price != 100 || r.Trades[0].BuyUserID != "carol" {
		t.Fatalf("debe ejecutar contra la mejor compra (100): %+v", r.Trades[0])
	}
	if r.Trades[1].Price != 98 || r.Trades[1].Qty != 2 {
		t.Fatalf("segundo trade a 98 por 2: %+v", r.Trades[1])
	}
}

func TestCancel(t *testing.T) {
	b := NewBook()
	mustSubmit(t, b, ord("b1", "alice", domain.Buy, 100, 5))

	o, err := b.Cancel("b1")
	if err != nil || o.Status != domain.StatusCancelled {
		t.Fatalf("cancel falló: %v %+v", err, o)
	}
	if len(b.Depth(domain.Buy, 5)) != 0 {
		t.Fatal("libro debe quedar vacío tras cancelar")
	}
	if _, err := b.Cancel("b1"); err != ErrOrderNotFound {
		t.Fatalf("cancelar dos veces debe dar ErrOrderNotFound, got %v", err)
	}
	// una orden cancelada no puede ejecutarse
	r := mustSubmit(t, b, ord("s1", "bob", domain.Sell, 100, 5))
	if len(r.Trades) != 0 {
		t.Fatal("no debe cruzar contra orden cancelada")
	}
}

func TestValidationAndDuplicates(t *testing.T) {
	b := NewBook()
	bad := []domain.Order{
		ord("", "u", domain.Buy, 1, 1),
		ord("x", "", domain.Buy, 1, 1),
		ord("x", "u", 0, 1, 1),
		ord("x", "u", domain.Buy, 0, 1),
		ord("x", "u", domain.Buy, 1, 0),
		ord("x", "u", domain.Buy, -5, 1),
		ord("x", "u", domain.Buy, MaxPrice+1, 1),
		ord("x", "u", domain.Buy, 1, MaxQty+1),
	}
	for i, o := range bad {
		if _, err := b.Submit(o); err != ErrInvalidOrder {
			t.Errorf("caso %d: esperaba ErrInvalidOrder, got %v", i, err)
		}
	}
	mustSubmit(t, b, ord("dup", "u", domain.Buy, 10, 1))
	if _, err := b.Submit(ord("dup", "u", domain.Buy, 10, 1)); err != ErrDuplicateOrder {
		t.Fatalf("esperaba ErrDuplicateOrder, got %v", err)
	}
}

// TestRandomizedInvariants lanza miles de órdenes aleatorias y verifica, tras cada una:
//  1. el libro nunca queda "cruzado" (mejor compra < mejor venta),
//  2. conservación: unidades y reales que salen de compradores == los que entran a vendedores,
//  3. cada trade respeta el límite de precio de ambas órdenes,
//  4. toda orden cumple Filled <= Qty.
func TestRandomizedInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	b := NewBook()
	orders := map[string]domain.Order{}
	var buyUnits, sellUnits, buyCash, sellCash int64

	for i := 0; i < 20000; i++ {
		side := domain.Buy
		if rng.Intn(2) == 0 {
			side = domain.Sell
		}
		o := ord(fmt.Sprintf("o%d", i), fmt.Sprintf("u%d", rng.Intn(50)),
			side, domain.Price(95+rng.Intn(11)), domain.Qty(1+rng.Intn(20)))
		orders[o.ID] = o

		if rng.Intn(10) == 0 && i > 0 { // 10% de las veces, cancelar una orden al azar
			_, _ = b.Cancel(fmt.Sprintf("o%d", rng.Intn(i)))
		}

		r := mustSubmit(t, b, o)
		for _, tr := range r.Trades {
			if tr.Qty <= 0 {
				t.Fatalf("trade con qty no positiva: %+v", tr)
			}
			if tr.Price > orders[tr.BuyOrderID].Price || tr.Price < orders[tr.SellOrderID].Price {
				t.Fatalf("trade viola límites de precio: %+v", tr)
			}
			buyUnits += int64(tr.Qty)
			sellUnits += int64(tr.Qty)
			buyCash += tr.Total()
			sellCash += tr.Total()
		}
		if r.Order.Filled > r.Order.Qty {
			t.Fatalf("sobreejecución: %+v", r.Order)
		}

		bids, asks := b.Depth(domain.Buy, 1), b.Depth(domain.Sell, 1)
		if len(bids) == 1 && len(asks) == 1 && bids[0].Price >= asks[0].Price {
			t.Fatalf("libro cruzado tras %s: bid=%d ask=%d", o.ID, bids[0].Price, asks[0].Price)
		}
	}
	if buyUnits != sellUnits || buyCash != sellCash {
		t.Fatal("no se conserva el valor")
	}
}

func BenchmarkSubmit(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	book := NewBook()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		side := domain.Buy
		if i%2 == 0 {
			side = domain.Sell
		}
		_, _ = book.Submit(ord(fmt.Sprintf("o%d", i), "u", side,
			domain.Price(95+rng.Intn(11)), domain.Qty(1+rng.Intn(20))))
	}
}

// --- Acotación del trabajo por orden (MaxTradesPerOrder) y rendimiento de Depth ---

func TestSweepIsCappedAndRemainderIsCancelledNotRested(t *testing.T) {
	const makers = 25_000
	b := NewBook()
	for i := 0; i < makers; i++ {
		mustSubmit(t, b, ord(fmt.Sprintf("m%d", i), "maker", domain.Sell, 10, 1))
	}
	r := mustSubmit(t, b, ord("big", "taker", domain.Buy, 10, makers))
	if len(r.Trades) != MaxTradesPerOrder || r.Order.Filled != MaxTradesPerOrder {
		t.Fatalf("debía ejecutar exactamente el tope (%d): trades=%d filled=%d", MaxTradesPerOrder, len(r.Trades), r.Order.Filled)
	}
	if r.Order.Status != domain.StatusCancelled {
		t.Fatalf("el remanente debe cancelarse: %v", r.Order.Status)
	}
	if len(b.Depth(domain.Buy, 5)) != 0 {
		t.Fatal("el remanente NO puede quedar en el libro (lo dejaría cruzado)")
	}
	if d := b.Depth(domain.Sell, 5); len(d) != 1 || d[0].Qty != makers-MaxTradesPerOrder {
		t.Fatalf("deben quedar %d ventas: %+v", makers-MaxTradesPerOrder, d)
	}
	// prioridad de tiempo preservada: se ejecutaron las más antiguas
	if r.Trades[0].MakerOrderID != "m0" || r.Trades[MaxTradesPerOrder-1].MakerOrderID != fmt.Sprintf("m%d", MaxTradesPerOrder-1) {
		t.Fatal("debe ejecutar primero las órdenes más antiguas")
	}
	// una orden cuyo cruce cabe exacto en el tope NO se trunca
	b2 := NewBook()
	for i := 0; i < MaxTradesPerOrder; i++ {
		mustSubmit(t, b2, ord(fmt.Sprintf("m%d", i), "maker", domain.Sell, 10, 1))
	}
	if r := mustSubmit(t, b2, ord("exact", "taker", domain.Buy, 10, MaxTradesPerOrder)); r.Order.Status != domain.StatusFilled {
		t.Fatalf("un cruce que cabe en el tope debe quedar FILLED: %v", r.Order.Status)
	}
}

func TestDepthWithManyLevelsIsOrderedAndFast(t *testing.T) {
	const levels = 50_000 // con el ordenamiento O(L²) anterior, esto bloqueaba el motor ~0,8 s
	asks, bids := NewBook(), NewBook()
	for i, p := range rand.New(rand.NewSource(3)).Perm(levels) {
		mustSubmit(t, asks, ord(fmt.Sprintf("a%d", i), "u", domain.Sell, domain.Price(p+1), 1))
		mustSubmit(t, bids, ord(fmt.Sprintf("b%d", i), "u", domain.Buy, domain.Price(p+1), 1))
	}
	t0 := time.Now()
	a, bd := asks.Depth(domain.Sell, 10), bids.Depth(domain.Buy, 10)
	if el := time.Since(t0); el > 250*time.Millisecond {
		t.Fatalf("Depth con %d niveles tardó %v (debe ser O(L log L))", levels, el)
	}
	for i := 0; i < 10; i++ {
		if a[i].Price != domain.Price(i+1) || bd[i].Price != domain.Price(levels-i) {
			t.Fatalf("orden incorrecto en %d: ask=%d bid=%d", i, a[i].Price, bd[i].Price)
		}
	}
}
