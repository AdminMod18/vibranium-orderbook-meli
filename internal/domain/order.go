// Package domain define los tipos del negocio. No tiene dependencias externas
// ni I/O: es el lenguaje común que comparten el motor, la API y la persistencia.
package domain

import "errors"

// Dinero y cantidades son SIEMPRE enteros, nunca float:
//   - Price: centavos de real por 1 unidad de Vibranium (R$ 12,34 -> 1234).
//   - Qty:   unidades enteras de Vibranium.
//
// Así Price*Qty es exacto (centavos) y los saldos cuadran sin errores de redondeo.
type (
	Price int64
	Qty   int64
)

// MaxIDLen es la longitud máxima de un identificador (id de orden o de usuario).
const MaxIDLen = 64

// ErrInvalidID indica un identificador fuera de las reglas: 1-64 caracteres entre [A-Za-z0-9._:@+-].
// Se acota porque los ids se guardan en memoria (ventana de idempotencia), en el journal y en
// Postgres: sin límite, una sola petición con un id enorme podría romper el journal o agotar memoria.
var ErrInvalidID = errors.New("identificador inválido: debe tener 1-64 caracteres entre [A-Za-z0-9._:@+-]")

// ValidID comprueba un identificador.
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > MaxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '@', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

type Side uint8

const (
	Buy Side = iota + 1
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	}
	return "UNKNOWN"
}

// Opposite devuelve el lado contrario (con quién se cruza una orden).
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

type Status uint8

const (
	StatusOpen            Status = iota + 1 // en el libro, sin ejecuciones
	StatusPartiallyFilled                   // en el libro, ejecutada en parte
	StatusFilled                            // totalmente ejecutada
	StatusCancelled                         // cancelada (puede haber ejecutado algo antes)
)

func (s Status) String() string {
	switch s {
	case StatusOpen:
		return "OPEN"
	case StatusPartiallyFilled:
		return "PARTIALLY_FILLED"
	case StatusFilled:
		return "FILLED"
	case StatusCancelled:
		return "CANCELLED"
	}
	return "UNKNOWN"
}

// Order es una orden límite: "comprar/vender hasta Qty unidades a Price o mejor".
type Order struct {
	ID     string
	UserID string
	Side   Side
	Price  Price // límite: máximo que pago (Buy) o mínimo que acepto (Sell)
	Qty    Qty   // cantidad original
	Filled Qty   // cantidad ya ejecutada
	Status Status

	// Seq lo asigna el motor al recibir la orden. Define la prioridad en el
	// tiempo (menor Seq = llegó antes) sin depender del reloj de pared.
	Seq uint64
}

// Remaining es la cantidad aún no ejecutada.
func (o *Order) Remaining() Qty { return o.Qty - o.Filled }

// Trade es una ejecución: una compra y una venta que se cruzaron.
// Es inmutable y es la base de la trazabilidad y de la liquidación de billeteras.
type Trade struct {
	ID    uint64
	Seq   uint64 // posición global en la secuencia de eventos del libro
	Price Price  // precio del trade = precio de la orden que ya estaba en el libro
	Qty   Qty

	BuyOrderID  string
	SellOrderID string
	BuyUserID   string
	SellUserID  string

	// MakerOrderID es la orden que ya estaba en el libro; la otra es la taker.
	MakerOrderID string
	TakerOrderID string
}

// Total es el importe en centavos de reales que cambia de manos.
func (t Trade) Total() int64 { return int64(t.Price) * int64(t.Qty) }
