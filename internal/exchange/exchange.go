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
	seen   map[string]struct{} // ids ya procesados: un reintento no puede duplicar una orden
}

func New() *Exchange {
	return &Exchange{
		book:   engine.NewBook(),
		ledger: wallet.NewLedger(),
		seen:   make(map[string]struct{}),
	}
}

func (x *Exchange) Deposit(user string, brl, vib int64) error {
	return x.ledger.Deposit(user, brl, vib)
}

func (x *Exchange) Balance(user string) wallet.Account { return x.ledger.Get(user) }

func (x *Exchange) Book() *engine.Book { return x.book }

func (x *Exchange) Ledger() *wallet.Ledger { return x.ledger }

// PlaceOrder procesa una orden límite de compra o venta.
func (x *Exchange) PlaceOrder(o domain.Order) (engine.Result, error) {
	if err := engine.Validate(o); err != nil {
		return engine.Result{}, err
	}
	if _, dup := x.seen[o.ID]; dup {
		return engine.Result{}, ErrDuplicateOrder
	}
	if err := x.ledger.Hold(o); err != nil { // 1) sin saldo, la orden ni entra al libro
		return engine.Result{}, err
	}

	res, err := x.book.Submit(o) // 2) matching
	if err != nil {
		// No debería ocurrir tras Validate, pero si pasa se devuelve lo reservado.
		_ = x.ledger.Release(o)
		return engine.Result{}, err
	}
	x.seen[o.ID] = struct{}{}

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
//  2. lo reservado de cada usuario == lo que comprometen sus órdenes abiertas.
//
// (La conservación de totales se comprueba comparando ledger.Totals() antes y después.)
func (x *Exchange) CheckInvariants() error {
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
