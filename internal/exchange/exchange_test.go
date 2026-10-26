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

// Un exchange restaurado desde una captura debe comportarse EXACTAMENTE igual que el original
// ante las mismas operaciones futuras (mismos trades, mismo orden de prioridad, mismos saldos).
func TestCaptureRestoreBehavesIdentically(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	a := New()
	const users = 20
	for i := 0; i < users; i++ {
		_ = a.Deposit(fmt.Sprintf("u%d", i), 200_000, 400)
	}
	step := func(x *Exchange, i int, r *rand.Rand) string {
		side := domain.Buy
		if r.Intn(2) == 0 {
			side = domain.Sell
		}
		o := ord(fmt.Sprintf("o%d", i), fmt.Sprintf("u%d", r.Intn(users)), side, domain.Price(95+r.Intn(11)), domain.Qty(1+r.Intn(15)))
		if r.Intn(9) == 0 && i > 0 {
			_, _ = x.Cancel(fmt.Sprintf("o%d", r.Intn(i)))
		}
		res, err := x.PlaceOrder(o)
		return fmt.Sprintf("%v|%+v", err, res)
	}
	for i := 0; i < 3000; i++ {
		step(a, i, rng)
	}

	b, err := Restore(a.Capture(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// mismas operaciones futuras sobre ambos, con generadores idénticos
	ra, rb := rand.New(rand.NewSource(99)), rand.New(rand.NewSource(99))
	for i := 3000; i < 6000; i++ {
		if ga, gb := step(a, i, ra), step(b, i, rb); ga != gb {
			t.Fatalf("op %d: el restaurado se comporta distinto:\n original: %s\nrestaurado: %s", i, ga, gb)
		}
	}
	for i := 0; i < users; i++ {
		u := fmt.Sprintf("u%d", i)
		if a.Balance(u) != b.Balance(u) {
			t.Fatalf("saldos distintos para %s", u)
		}
	}
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsInconsistentState(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 0}})
	place(t, x, ord("b1", "alice", domain.Buy, 100, 5))

	s := x.Capture()
	s.Accounts["alice"] = wallet.Account{BRLFree: 1000} // reserva borrada: ya no cuadra con la orden abierta
	if _, err := Restore(s, 0); err == nil {
		t.Fatal("un snapshot con reservas inconsistentes debe rechazarse")
	}
	s = x.Capture()
	s.DepositedBRL += 5 // el total depositado no coincide con los saldos
	if _, err := Restore(s, 0); err == nil {
		t.Fatal("un snapshot que viola la conservación del valor debe rechazarse")
	}
}

func TestInvariantDetectsValueCreatedOrDestroyed(t *testing.T) {
	x := newEx(t, map[string][2]int64{"alice": {1000, 5}, "bob": {500, 0}})
	place(t, x, ord("b1", "alice", domain.Buy, 10, 3))
	if err := x.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	// Si por un bug aparece dinero de la nada, la auditoría debe saltar:
	if err := x.Ledger().Deposit("mallory", 1, 0); err != nil { // acredita SIN pasar por Exchange.Deposit
		t.Fatal(err)
	}
	if err := x.CheckInvariants(); err == nil {
		t.Fatal("debe detectarse valor creado fuera de un depósito registrado")
	}
}

// --- Ventana de idempotencia ---

func buyAndCancel(t *testing.T, x *Exchange, id string) error {
	t.Helper()
	if _, err := x.PlaceOrder(ord(id, "alice", domain.Buy, 10, 1)); err != nil {
		return err
	}
	if _, err := x.Cancel(id); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestIdempotencyWindowRejectsRecentAndForgetsOld(t *testing.T) {
	x := NewWithWindow(3)
	_ = x.Deposit("alice", 1_000_000, 0)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		if err := buyAndCancel(t, x, id); err != nil {
			t.Fatal(err)
		}
	}
	// ventana = {a3, a4, a5}
	if x.SeenCount() != 3 || x.SeenEvicted() != 2 {
		t.Fatalf("esperaba 3 ids y 2 expulsados, got %d y %d", x.SeenCount(), x.SeenEvicted())
	}
	if _, err := x.PlaceOrder(ord("a5", "alice", domain.Buy, 10, 1)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("un reintento reciente debe rechazarse: %v", err)
	}
	if _, err := x.PlaceOrder(ord("a3", "alice", domain.Buy, 10, 1)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("a3 sigue dentro de la ventana: %v", err)
	}
	// a2 salió de la ventana y su orden ya no está en el libro: se trata como una orden NUEVA
	if err := buyAndCancel(t, x, "a2"); err != nil {
		t.Fatalf("un id fuera de la ventana debe aceptarse como nuevo: %v", err)
	}
	if err := x.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// Si el id salió de la ventana pero su orden SIGUE en el libro, el libro la protege: mismo error,
// y no se queda ninguna reserva colgada.
func TestForgottenIDWithRestingOrderIsStillRejected(t *testing.T) {
	x := NewWithWindow(2)
	_ = x.Deposit("bob", 0, 100)
	_ = x.Deposit("alice", 1_000_000, 0)
	place(t, x, ord("s1", "bob", domain.Sell, 1000, 5)) // queda en el libro
	for _, id := range []string{"o2", "o3"} {           // empujan a s1 fuera de la ventana
		if err := buyAndCancel(t, x, id); err != nil {
			t.Fatal(err)
		}
	}
	if x.seen.has("s1") {
		t.Fatal("s1 debía haber salido de la ventana")
	}
	before := x.Balance("bob")
	if _, err := x.PlaceOrder(ord("s1", "bob", domain.Sell, 1000, 5)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("debe rechazarse con el MISMO error (409), got %v", err)
	}
	if x.Balance("bob") != before {
		t.Fatalf("no debe quedar reserva colgada: %+v -> %+v", before, x.Balance("bob"))
	}
	if err := x.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowStaysBoundedUnderLongRun(t *testing.T) {
	const limit = 1000
	x := NewWithWindow(limit)
	_ = x.Deposit("alice", 1<<40, 0)
	for i := 0; i < 60_000; i++ {
		if err := buyAndCancel(t, x, fmt.Sprintf("o%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if x.SeenCount() != limit || len(x.Capture().Seen) != limit {
		t.Fatalf("la ventana debe quedar en %d ids: contador=%d snapshot=%d", limit, x.SeenCount(), len(x.Capture().Seen))
	}
	if len(x.seen.set) != limit || len(x.seen.queue) > 40_000 {
		t.Fatalf("estructuras sin acotar: set=%d cola=%d (se compacta)", len(x.seen.set), len(x.seen.queue))
	}
	if x.SeenEvicted() != 60_000-limit {
		t.Fatalf("expulsados: %d", x.SeenEvicted())
	}
}

func TestWindowIsDeterministic(t *testing.T) {
	run := func() []string {
		x := NewWithWindow(50)
		_ = x.Deposit("alice", 1<<40, 0)
		rng := rand.New(rand.NewSource(5))
		for i := 0; i < 2000; i++ {
			_ = buyAndCancel(t, x, fmt.Sprintf("o%d", rng.Intn(300))) // ids que se repiten
		}
		return x.Capture().Seen
	}
	a, b := run(), run()
	if len(a) != 50 || fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("la ventana debe ser idéntica entre ejecuciones (len=%d)", len(a))
	}
}

func TestRestoreAppliesTheCurrentWindow(t *testing.T) {
	x := NewWithWindow(0)
	_ = x.Deposit("alice", 1<<40, 0)
	for i := 0; i < 10; i++ {
		_ = buyAndCancel(t, x, fmt.Sprintf("o%d", i))
	}
	st := x.Capture()
	small, err := Restore(st, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := small.Capture().Seen; fmt.Sprint(got) != "[o6 o7 o8 o9]" {
		t.Fatalf("debe conservar los 4 más recientes en orden: %v", got)
	}
	big, _ := Restore(st, 1000)
	if big.SeenCount() != 10 {
		t.Fatalf("got %d", big.SeenCount())
	}
}
