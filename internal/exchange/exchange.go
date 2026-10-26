// Package exchange une el libro de ofertas con las billeteras.
//
// Cada orden es una unidad atómica con tres pasos:  1) reservar fondos,
// 2) cruzar en el libro, 3) liquidar los trades. Como todo corre en un único
// goroutine (el "secuenciador", Fase 3) no hacen falta locks ni estados intermedios visibles.
package exchange

import (
	"errors"
	"fmt"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

var ErrDuplicateOrder = errors.New("la orden ya fue procesada")

type Exchange struct {
	book   *engine.Book
	ledger *wallet.Ledger
	seen   *idWindow // ventana de ids ya procesados: un reintento reciente no puede duplicar una orden

	// Total depositado en el sistema. Como los trades solo MUEVEN valor entre billeteras, la suma de
	// todos los saldos debe ser SIEMPRE igual a estos totales (invariante de conservación).
	depBRL, depVIB int64
}

// New crea un exchange cuya idempotencia recuerda TODOS los ids (sin ventana). Útil en pruebas;
// en producción se usa NewWithWindow para acotar la memoria.
func New() *Exchange { return NewWithWindow(0) }

// NewWithWindow crea un exchange que recuerda los últimos `window` ids aceptados (0 = sin límite).
// Un reintento de una orden DENTRO de la ventana se rechaza (409); pasada la ventana se trataría
// como una orden nueva (salvo que la original siga en el libro, que la protege por sí mismo).
func NewWithWindow(window int) *Exchange {
	return &Exchange{
		book:   engine.NewBook(),
		ledger: wallet.NewLedger(),
		seen:   newIDWindow(window),
	}
}

// SeenCount y SeenEvicted alimentan las métricas (seguros de leer desde otros goroutines).
func (x *Exchange) SeenCount() int64    { return x.seen.size.Load() }
func (x *Exchange) SeenEvicted() uint64 { return x.seen.evicted.Load() }
func (x *Exchange) SeenLimit() int      { return x.seen.limit }

func (x *Exchange) Deposit(user string, brl, vib int64) error {
	if err := x.ledger.Deposit(user, brl, vib); err != nil {
		return err
	}
	x.depBRL += brl
	x.depVIB += vib
	return nil
}

func (x *Exchange) Balance(user string) wallet.Account { return x.ledger.Get(user) }

func (x *Exchange) Book() *engine.Book { return x.book }

func (x *Exchange) Ledger() *wallet.Ledger { return x.ledger }

// PlaceOrder procesa una orden límite de compra o venta (con control de idempotencia).
func (x *Exchange) PlaceOrder(o domain.Order) (engine.Result, error) { return x.placeOrder(o, true) }

// ReplayOrder reaplica una orden YA ACEPTADA que se está leyendo del journal. No comprueba la ventana
// de idempotencia: el journal solo contiene lo que el sistema aceptó, y la ventana pudo ser distinta
// (más chica o más grande) cuando se escribió. Así cambiar IDEMPOTENCY_WINDOW nunca impide arrancar.
func (x *Exchange) ReplayOrder(o domain.Order) (engine.Result, error) { return x.placeOrder(o, false) }

func (x *Exchange) placeOrder(o domain.Order, checkDup bool) (engine.Result, error) {
	if err := engine.Validate(o); err != nil {
		return engine.Result{}, err
	}
	if checkDup && x.seen.has(o.ID) {
		return engine.Result{}, ErrDuplicateOrder
	}
	if err := x.ledger.Hold(o); err != nil { // 1) sin saldo, la orden ni entra al libro
		return engine.Result{}, err
	}

	res, err := x.book.Submit(o) // 2) matching
	if err != nil {
		// Se devuelve lo reservado. Un id olvidado por la ventana pero cuya orden sigue en el libro
		// llega hasta aquí: es el mismo caso de "orden ya procesada", con el mismo error.
		_ = x.ledger.Release(o)
		if checkDup && errors.Is(err, engine.ErrDuplicateOrder) {
			return engine.Result{}, ErrDuplicateOrder
		}
		return engine.Result{}, err
	}
	x.seen.add(o.ID)

	for _, t := range res.Trades { // 3) liquidación
		// Un maker compra a su propio límite (== precio del trade). Un taker comprador
		// pudo reservar más de lo que pagó: esa diferencia se le devuelve.
		buyLimit := t.Price
		if o.Side == domain.Buy {
			buyLimit = o.Price
		}
		if err := x.ledger.Settle(t, buyLimit); err != nil {
			// Violación de invariante: es un bug, no un error de usuario. Se detiene todo.
			panic(fmt.Sprintf("liquidación inconsistente en trade %d: %v", t.ID, err))
		}
	}
	return res, nil
}

// Cancel retira una orden en negociación y libera la parte reservada que no se ejecutó.
func (x *Exchange) Cancel(orderID string) (domain.Order, error) {
	o, err := x.book.Cancel(orderID)
	if err != nil {
		return domain.Order{}, err
	}
	if err := x.ledger.Release(o); err != nil {
		panic(fmt.Sprintf("liberación inconsistente en orden %s: %v", o.ID, err))
	}
	return o, nil
}

// CheckInvariants audita el sistema completo. Es la "prueba de que los saldos cuadran":
//  1. nadie tiene saldos negativos,
//  2. lo reservado de cada usuario == lo que comprometen sus órdenes abiertas,
//  3. CONSERVACIÓN: la suma de todos los saldos == total depositado (no se crea ni se destruye valor).
func (x *Exchange) CheckInvariants() error {
	if brl, vib := x.ledger.Totals(); brl != x.depBRL || vib != x.depVIB {
		return fmt.Errorf("no se conserva el valor: saldos BRL=%d VIB=%d vs depositado BRL=%d VIB=%d", brl, vib, x.depBRL, x.depVIB)
	}
	wantBRL := map[string]int64{}
	wantVIB := map[string]int64{}
	for _, o := range x.book.RestingOrders() {
		if o.Side == domain.Buy {
			wantBRL[o.UserID] += int64(o.Price) * int64(o.Remaining())
		} else {
			wantVIB[o.UserID] += int64(o.Remaining())
		}
	}
	for _, u := range x.ledger.Users() {
		a := x.ledger.Get(u)
		if a.BRLFree < 0 || a.BRLHeld < 0 || a.VIBFree < 0 || a.VIBHeld < 0 {
			return fmt.Errorf("saldo negativo en %s: %+v", u, a)
		}
		if a.BRLHeld != wantBRL[u] || a.VIBHeld != wantVIB[u] {
			return fmt.Errorf("reservas de %s no cuadran con sus órdenes abiertas: %+v (esperado BRL=%d VIB=%d)",
				u, a, wantBRL[u], wantVIB[u])
		}
	}
	return nil
}
