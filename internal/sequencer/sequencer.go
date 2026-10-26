// Package sequencer es la frontera entre el mundo concurrente (miles de requests HTTP)
// y el motor secuencial.
//
// Todas las operaciones sobre el Exchange se convierten en "comandos" que entran a
// UNA cola y los ejecuta UN solo goroutine, en orden de llegada. Así:
//   - el orden de la cola es el orden total de las operaciones (define la prioridad de tiempo),
//   - el Exchange no necesita locks,
//   - la cola es acotada: si se llena se rechaza (ErrOverloaded) en lugar de acumular latencia.
package sequencer

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

var (
	ErrOverloaded = errors.New("cola de entrada llena")
	ErrStopped    = errors.New("secuenciador detenido")
)

type Sequencer struct {
	ex   *exchange.Exchange
	in   chan func(*exchange.Exchange)
	quit chan struct{}
	done chan struct{}

	processed atomic.Uint64
	rejected  atomic.Uint64 // rechazadas por cola llena
}

// New crea el secuenciador y arranca su goroutine. queueSize es la capacidad de la cola.
func New(ex *exchange.Exchange, queueSize int) *Sequencer {
	s := &Sequencer{
		ex:   ex,
		in:   make(chan func(*exchange.Exchange), queueSize),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *Sequencer) run() {
	defer close(s.done)
	for {
		select {
		case cmd := <-s.in:
			cmd(s.ex)
			s.processed.Add(1)
		case <-s.quit:
			for { // procesa lo que ya estaba encolado antes de salir
				select {
				case cmd := <-s.in:
					cmd(s.ex)
					s.processed.Add(1)
				default:
					return
				}
			}
		}
	}
}

// Stop detiene el secuenciador tras vaciar la cola. Debe llamarse DESPUÉS de dejar de
// aceptar requests (shutdown ordenado del servidor HTTP).
func (s *Sequencer) Stop() {
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	<-s.done
}

// QueueLen y QueueCap alimentan las métricas de saturación.
func (s *Sequencer) QueueLen() int      { return len(s.in) }
func (s *Sequencer) QueueCap() int      { return cap(s.in) }
func (s *Sequencer) Processed() uint64  { return s.processed.Load() }
func (s *Sequencer) Overloaded() uint64 { return s.rejected.Load() }

// Do encola fn y espera su resultado. El encolado NO bloquea: si la cola está llena
// devuelve ErrOverloaded de inmediato (backpressure). Si ctx expira mientras se espera,
// el comando igualmente se ejecutará (ya está en la cola): ver limitación L2.
func Do[T any](ctx context.Context, s *Sequencer, fn func(*exchange.Exchange) (T, error)) (T, error) {
	var zero T
	type reply struct {
		v   T
		err error
	}
	ch := make(chan reply, 1) // con buffer: el secuenciador nunca se bloquea al responder
	cmd := func(x *exchange.Exchange) {
		v, err := fn(x)
		ch <- reply{v, err}
	}

	select {
	case <-s.quit:
		return zero, ErrStopped
	default:
	}
	select {
	case s.in <- cmd:
	default:
		s.rejected.Add(1)
		return zero, ErrOverloaded
	}

	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// --- Operaciones tipadas ---

func (s *Sequencer) PlaceOrder(ctx context.Context, o domain.Order) (engine.Result, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (engine.Result, error) { return x.PlaceOrder(o) })
}

func (s *Sequencer) Cancel(ctx context.Context, id string) (domain.Order, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (domain.Order, error) { return x.Cancel(id) })
}

func (s *Sequencer) Deposit(ctx context.Context, user string, brl, vib int64) error {
	_, err := Do(ctx, s, func(x *exchange.Exchange) (struct{}, error) {
		return struct{}{}, x.Deposit(user, brl, vib)
	})
	return err
}

func (s *Sequencer) Balance(ctx context.Context, user string) (wallet.Account, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (wallet.Account, error) { return x.Balance(user), nil })
}

// BookSnapshot es una vista del libro para consulta.
type BookSnapshot struct {
	Bids []engine.Level
	Asks []engine.Level
}

func (s *Sequencer) Depth(ctx context.Context, n int) (BookSnapshot, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (BookSnapshot, error) {
		return BookSnapshot{
			Bids: x.Book().Depth(domain.Buy, n),
			Asks: x.Book().Depth(domain.Sell, n),
		}, nil
	})
}

// AuditReport es el resultado de la auditoría de saldos.
type AuditReport struct {
	OK       bool
	Error    string
	TotalBRL int64
	TotalVIB int64
}

// Audit comprueba los invariantes dentro del secuenciador (vista consistente, sin carreras).
func (s *Sequencer) Audit(ctx context.Context) (AuditReport, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (AuditReport, error) {
		brl, vib := x.Ledger().Totals()
		rep := AuditReport{OK: true, TotalBRL: brl, TotalVIB: vib}
		if err := x.CheckInvariants(); err != nil {
			rep.OK, rep.Error = false, err.Error()
		}
		return rep, nil
	})
}
