// Package wallet mantiene los saldos de las personas usuarias en reales (BRL) y Vibranium (VIB).
//
// Idea central: RESERVAR antes de negociar. Al ingresar una orden se bloquea lo que
// podría gastarse (BRL en compras, VIB en ventas); al ejecutarse un trade se liquida
// desde lo reservado. Así nadie puede gastar dos veces el mismo saldo aunque envíe
// miles de órdenes seguidas.
//
// Ledger no es thread-safe: lo usa el mismo goroutine secuencial que el motor.
package wallet

import (
	"errors"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
)

var (
	ErrInsufficientFunds = errors.New("saldo insuficiente")
	ErrInvalidAmount     = errors.New("monto inválido")
	ErrInconsistent      = errors.New("liquidación inconsistente con las reservas")
)

// Account es la billetera de una persona. Todo en enteros (centavos / unidades).
//
//	Free = disponible para operar o retirar. Held = reservado por órdenes abiertas.
//	Saldo total = Free + Held.
type Account struct {
	BRLFree int64
	BRLHeld int64
	VIBFree int64
	VIBHeld int64
}

type Ledger struct {
	accounts map[string]*Account
}

func NewLedger() *Ledger { return &Ledger{accounts: make(map[string]*Account)} }

func (l *Ledger) account(user string) *Account {
	a, ok := l.accounts[user]
	if !ok {
		a = &Account{}
		l.accounts[user] = a
	}
	return a
}

// Get devuelve una copia de la billetera (cero si no existe).
func (l *Ledger) Get(user string) Account {
	if a, ok := l.accounts[user]; ok {
		return *a
	}
	return Account{}
}

// Users lista los ids con billetera (para auditorías y pruebas).
func (l *Ledger) Users() []string {
	out := make([]string, 0, len(l.accounts))
	for u := range l.accounts {
		out = append(out, u)
	}
	return out
}

// Deposit acredita saldo libre. Las billeteras se crean al primer depósito (sin registro de usuarios).
func (l *Ledger) Deposit(user string, brl, vib int64) error {
	const max = int64(1) << 60 // margen para que ninguna suma desborde int64
	if user == "" || brl < 0 || vib < 0 || (brl == 0 && vib == 0) {
		return ErrInvalidAmount
	}
	a := l.account(user)
	if a.BRLFree+a.BRLHeld+brl > max || a.VIBFree+a.VIBHeld+vib > max {
		return ErrInvalidAmount
	}
	a.BRLFree += brl
	a.VIBFree += vib
	return nil
}

// Hold reserva los fondos que una orden podría consumir:
// compra -> Price*Qty reales; venta -> Qty unidades de Vibranium.
func (l *Ledger) Hold(o domain.Order) error {
	a := l.account(o.UserID)
	if o.Side == domain.Buy {
		need := int64(o.Price) * int64(o.Qty)
		if a.BRLFree < need {
			return ErrInsufficientFunds
		}
		a.BRLFree -= need
		a.BRLHeld += need
		return nil
	}
	need := int64(o.Qty)
	if a.VIBFree < need {
		return ErrInsufficientFunds
	}
	a.VIBFree -= need
	a.VIBHeld += need
	return nil
}

// Release libera la parte de la reserva que corresponde a lo NO ejecutado de la orden
// (se usa al cancelar o al revertir una orden rechazada).
func (l *Ledger) Release(o domain.Order) error {
	rem := int64(o.Qty - o.Filled)
	a := l.account(o.UserID)
	if o.Side == domain.Buy {
		amt := int64(o.Price) * rem
		if a.BRLHeld < amt {
			return ErrInconsistent
		}
		a.BRLHeld -= amt
		a.BRLFree += amt
		return nil
	}
	if a.VIBHeld < rem {
		return ErrInconsistent
	}
	a.VIBHeld -= rem
	a.VIBFree += rem
	return nil
}

// Settle liquida un trade desde las reservas de ambas partes.
//
// buyLimit es el precio límite de la orden de compra. Si es mayor que el precio del trade
// (el comprador ofrecía más de lo que costó), la diferencia se devuelve a su saldo libre.
func (l *Ledger) Settle(t domain.Trade, buyLimit domain.Price) error {
	qty := int64(t.Qty)
	cost := int64(t.Price) * qty      // lo que realmente se paga
	reserved := int64(buyLimit) * qty // lo que el comprador había reservado para esas unidades
	buyer, seller := l.account(t.BuyUserID), l.account(t.SellUserID)

	// Se comprueban las precondiciones ANTES de mutar, para no dejar una liquidación a medias.
	if buyLimit < t.Price || buyer.BRLHeld < reserved || seller.VIBHeld < qty {
		return ErrInconsistent
	}

	buyer.BRLHeld -= reserved
	buyer.BRLFree += reserved - cost // mejora de precio
	buyer.VIBFree += qty

	seller.VIBHeld -= qty
	seller.BRLFree += cost
	return nil
}

// Totals suma todos los saldos del sistema. Mientras no haya depósitos ni retiros,
// estos totales son constantes: es la prueba de que el sistema ni crea ni destruye valor.
func (l *Ledger) Totals() (brl, vib int64) {
	for _, a := range l.accounts {
		brl += a.BRLFree + a.BRLHeld
		vib += a.VIBFree + a.VIBHeld
	}
	return brl, vib
}
