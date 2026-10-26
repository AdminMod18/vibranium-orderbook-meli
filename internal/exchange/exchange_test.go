package exchange

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

func ord(id, user string, side domain.Side, price domain.Price, qty domain.Qty) domain.Order {
	return domain.Order{ID: id, UserID: user, Side: side, Price: price, Qty: qty}
}

func newEx(t *testing.T, deposits map[string][2]int64) *Exchange {
	t.Helper()
	x := New()
	for u, d := range deposits {
		if err := x.Deposit(u, d[0], d[1]); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

func place(t *testing.T, x *Exchange, o domain.Order) {
	t.Helper()
	if _, err := x.PlaceOrder(o); err != nil {
		t.Fatalf("PlaceOrder(%s): %v", o.ID, err)
	}
	if err := x.CheckInvariants(); err != nil {
		t.Fatalf("invariantes tras %s: %v", o.ID, err)
	}
}

func TestBuyHoldsFundsWhileResting(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 0}})
	place(t, x, ord("b1", "alice", domain.Buy, 100, 4))

	if got := x.Balance("alice"); got != (wallet.Account{BRLFree: 600, BRLHeld: 400}) {
		t.Fatalf("reserva incorrecta: %+v", got)
	}
}

func TestCannotSpendSameFundsTwice(t *testing.T) {
	x := newEx(t, map[string][2]int64{"bot": {100, 0}})
	place(t, x, ord("b1", "bot", domain.Buy, 50, 2)) // reserva los 100

	_, err := x.PlaceOrder(ord("b2", "bot", domain.Buy, 50, 1))
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("la segunda orden debe rechazarse por saldo, got %v", err)
	}
	if len(x.Book().Depth(domain.Buy, 5)) != 1 {
		t.Fatal("la orden rechazada no debe entrar al libro")
	}
}

func TestCannotSellWhatYouDontHave(t *testing.T) {
	x := newEx(t, map[string][2]int64{"bob": {0, 5}})
	if _, err := x.PlaceOrder(ord("s1", "bob", domain.Sell, 10, 6)); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("got %v", err)
	}
}

func TestTradeSettlesBothWalletsWithPriceImprovement(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 0}, "bob": {0, 10}})
	place(t, x, ord("s1", "bob", domain.Sell, 90, 10))   // maker: vende a 90
	place(t, x, ord("b1", "alice", domain.Buy, 100, 10)) // taker: ofrece hasta 100

	// Alice paga 900 (no 1000): la diferencia de 100 vuelve a su saldo libre.
	if got := x.Balance("alice"); got != (wallet.Account{BRLFree: 100, VIBFree: 10}) {
		t.Fatalf("alice: %+v", got)
	}
	if got := x.Balance("bob"); got != (wallet.Account{BRLFree: 900}) {
		t.Fatalf("bob: %+v", got)
	}
}

func TestPartialFillThenCancelReleasesRemainder(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 0}, "bob": {0, 4}})
	place(t, x, ord("b1", "alice", domain.Buy, 100, 10)) // reserva 1000
	place(t, x, ord("s1", "bob", domain.Sell, 100, 4))   // ejecuta 4 -> alice gasta 400

	if got := x.Balance("alice"); got != (wallet.Account{BRLHeld: 600, VIBFree: 4}) {
		t.Fatalf("alice tras parcial: %+v", got)
	}
	if _, err := x.Cancel("b1"); err != nil {
		t.Fatal(err)
	}
	if err := x.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if got := x.Balance("alice"); got != (wallet.Account{BRLFree: 600, VIBFree: 4}) {
		t.Fatalf("alice tras cancelar: %+v", got)
	}
}

func TestDuplicateOrderIDIsRejectedEvenIfAlreadyFilled(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 0}, "bob": {0, 10}})
	place(t, x, ord("s1", "bob", domain.Sell, 100, 1))
	place(t, x, ord("b1", "alice", domain.Buy, 100, 1)) // b1 queda FILLED (ya no está en el libro)

	if _, err := x.PlaceOrder(ord("b1", "alice", domain.Buy, 100, 1)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("un reintento de b1 no debe ejecutarse otra vez: %v", err)
	}
}

func TestSelfTradeKeepsBalancesConsistent(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 10}})
	place(t, x, ord("s1", "alice", domain.Sell, 100, 5))
	place(t, x, ord("b1", "alice", domain.Buy, 100, 5))
	if got := x.Balance("alice"); got != (wallet.Account{BRLFree: 1000, VIBFree: 10}) {
		t.Fatalf("un self-trade no debe cambiar el patrimonio: %+v", got)
	}
}

// TestRandomizedConservation simula muchos usuarios operando de forma caótica y comprueba,
// tras CADA operación, que los totales no cambian y las reservas cuadran con el libro.
func TestRandomizedConservation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	x := New()
	const users = 30
	for i := 0; i < users; i++ {
		if err := x.Deposit(fmt.Sprintf("u%d", i), 100_000, 500); err != nil {
			t.Fatal(err)
		}
	}
	brl0, vib0 := x.Ledger().Totals()

	rejected := 0
	for i := 0; i < 8000; i++ {
		if rng.Intn(10) == 0 && i > 0 {
			_, _ = x.Cancel(fmt.Sprintf("o%d", rng.Intn(i)))
		}
		side := domain.Buy
		if rng.Intn(2) == 0 {
			side = domain.Sell
		}
		o := ord(fmt.Sprintf("o%d", i), fmt.Sprintf("u%d", rng.Intn(users)),
			side, domain.Price(90+rng.Intn(21)), domain.Qty(1+rng.Intn(30)))
		if _, err := x.PlaceOrder(o); err != nil {
			if !errors.Is(err, wallet.ErrInsufficientFunds) {
				t.Fatalf("error inesperado: %v", err)
			}
			rejected++
		}
		if err := x.CheckInvariants(); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
		if brl, vib := x.Ledger().Totals(); brl != brl0 || vib != vib0 {
			t.Fatalf("op %d: no se conservan totales: BRL %d->%d VIB %d->%d", i, brl0, brl, vib0, vib)
		}
	}
	t.Logf("órdenes rechazadas por saldo: %d de 8000", rejected)
}
