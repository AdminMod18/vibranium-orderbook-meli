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
	"fmt"
	"sync/atomic"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/snapshot"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

// maxBatch acota cuántos comandos se confirman con un mismo fsync (group commit).
const maxBatch = 1024

var (
	ErrOverloaded = errors.New("cola de entrada llena")
	ErrStopped    = errors.New("secuenciador detenido")
)

// Observer recibe mediciones internas del secuenciador (la implementación vive en el paquete
// de métricas; así este paquete no depende de ninguna librería de observabilidad).
type Observer interface {
	// Batch se invoca por cada lote: cuántos comandos, cuánto tardó ejecutarlos y cuánto el fsync.
	Batch(size int, exec, fsync time.Duration)
	// QueueWait es el tiempo que un comando esperó en la cola antes de ejecutarse.
	QueueWait(d time.Duration)
}

// command ejecuta la lógica sobre el exchange. Devuelve el registro a persistir (nil si no
// cambió el estado: lecturas y operaciones rechazadas) y la función que responde al cliente.
// La respuesta se entrega DESPUÉS del fsync del lote: nunca se confirma algo no durable.
type command func(*exchange.Exchange) (rec *journal.Record, reply func())

type Sequencer struct {
	ex        *exchange.Exchange
	jr        *journal.Journal // nil = sin persistencia (tests)
	obs       Observer         // nil = sin métricas
	snap      *snapshot.Writer // nil = sin snapshots
	snapEvery uint64           // cada cuántos registros del journal se toma un snapshot
	in        chan command
	quit      chan struct{}
	done      chan struct{}

	processed atomic.Uint64
	rejected  atomic.Uint64 // rechazadas por cola llena
	expired   atomic.Uint64 // descartadas sin ejecutar: su cliente ya no esperaba (plazo vencido o desconexión)
	batches   atomic.Uint64
}

// Option configura el secuenciador.
type Option func(*Sequencer)

// WithJournal activa la persistencia: cada comando que cambia estado se registra y se
// confirma con group commit.
func WithJournal(j *journal.Journal) Option { return func(s *Sequencer) { s.jr = j } }

// WithSnapshots activa los snapshots periódicos (cada "every" registros) y uno final al detenerse.
// Acotan el tiempo de arranque: se recupera snapshot + cola en vez de todo el journal.
func WithSnapshots(w *snapshot.Writer, every uint64) Option {
	return func(s *Sequencer) { s.snap, s.snapEvery = w, every }
}

// WithObserver activa la emisión de mediciones internas.
func WithObserver(o Observer) Option { return func(s *Sequencer) { s.obs = o } }

// New crea el secuenciador y arranca su goroutine. queueSize es la capacidad de la cola.
func New(ex *exchange.Exchange, queueSize int, opts ...Option) *Sequencer {
	s := &Sequencer{
		ex:   ex,
		in:   make(chan command, queueSize),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	go s.run()
	return s
}

func (s *Sequencer) run() {
	defer close(s.done)
	batch := make([]command, 0, maxBatch)
	replies := make([]func(), 0, maxBatch)
	for {
		select {
		case cmd := <-s.in:
			batch = append(batch[:0], cmd)
		case <-s.quit:
			// Antes de salir se procesa lo que ya estaba encolado.
			for {
				select {
				case cmd := <-s.in:
					s.processBatch(append(batch[:0], cmd), &replies)
				default:
					s.finalSnapshot()
					return
				}
			}
		}
		// Group commit: se toma lo que haya esperando (sin bloquear) hasta maxBatch.
	fill:
		for len(batch) < maxBatch {
			select {
			case cmd := <-s.in:
				batch = append(batch, cmd)
			default:
				break fill
			}
		}
		s.processBatch(batch, &replies)
	}
}

// maybeSnapshot toma un snapshot si pasaron "snapEvery" registros desde el último. Solo COPIA el
// estado aquí (breve pausa); la escritura a disco ocurre en otro goroutine.
func (s *Sequencer) maybeSnapshot() {
	if s.snap == nil || s.jr == nil || s.snapEvery == 0 {
		return
	}
	if seq := s.jr.LastSeq(); seq-s.snap.LastSeq() >= s.snapEvery {
		s.snap.Async(s.ex, seq, s.jr.Bytes())
	}
}

// finalSnapshot deja un snapshot al día en el apagado ordenado: el siguiente arranque es casi instantáneo.
func (s *Sequencer) finalSnapshot() {
	if s.snap == nil || s.jr == nil || s.snapEvery == 0 {
		return
	}
	if seq := s.jr.LastSeq(); seq > s.snap.LastSeq() {
		s.snap.Sync(s.ex, seq, s.jr.Bytes())
	} else {
		s.snap.Wait()
	}
}

// processBatch ejecuta los comandos EN ORDEN, persiste sus registros con UN fsync y recién
// entonces responde a los clientes.
func (s *Sequencer) processBatch(batch []command, replies *[]func()) {
	*replies = (*replies)[:0]
	t0 := time.Now()
	for _, cmd := range batch {
		rec, reply := cmd(s.ex)
		if rec != nil && s.jr != nil {
			if err := s.jr.Append(rec); err != nil {
				// Fail-stop: sin poder persistir no se puede garantizar nada. Al reiniciar,
				// el estado se reconstruye desde lo que SÍ quedó en el log.
				panic(fmt.Sprintf("no se pudo escribir el journal: %v", err))
			}
		}
		*replies = append(*replies, reply)
	}
	tExec := time.Since(t0)
	var tSync time.Duration
	if s.jr != nil {
		t1 := time.Now()
		if err := s.jr.Sync(); err != nil {
			panic(fmt.Sprintf("fsync del journal falló: %v", err))
		}
		tSync = time.Since(t1)
	}
	if s.obs != nil {
		s.obs.Batch(len(batch), tExec, tSync)
	}
	for _, reply := range *replies {
		reply()
	}
	s.processed.Add(uint64(len(batch)))
	s.batches.Add(1)
	s.maybeSnapshot()
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

// Stopped indica si el secuenciador dejó de aceptar comandos (apagado en curso).
func (s *Sequencer) Stopped() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

// QueueLen y QueueCap alimentan las métricas de saturación.
func (s *Sequencer) QueueLen() int      { return len(s.in) }
func (s *Sequencer) QueueCap() int      { return cap(s.in) }
func (s *Sequencer) Processed() uint64  { return s.processed.Load() }
func (s *Sequencer) Overloaded() uint64 { return s.rejected.Load() }
func (s *Sequencer) Batches() uint64    { return s.batches.Load() }

// Expired cuenta los comandos descartados sin ejecutar porque su cliente ya no esperaba.
func (s *Sequencer) Expired() uint64 { return s.expired.Load() }

// JournalSeq es el último registro confirmado en el journal (0 si no hay persistencia).
func (s *Sequencer) JournalSeq() uint64 {
	if s.jr == nil {
		return 0
	}
	return s.jr.LastSeq()
}

// Do encola fn y espera su resultado. fn devuelve además el registro de journal a persistir
// (nil si no cambia el estado). El encolado NO bloquea: con la cola llena devuelve
// ErrOverloaded de inmediato (backpressure). Si ctx expira mientras se espera, el comando
// NO se ejecutará si llega a la cabeza de la cola después de ese momento (se descarta y se cuenta en
// Expired); solo puede ejecutarse si ya estaba en marcha cuando venció (L2).
func Do[T any](ctx context.Context, s *Sequencer, fn func(*exchange.Exchange) (T, *journal.Record, error)) (T, error) {
	var zero T
	type reply struct {
		v   T
		err error
	}
	ch := make(chan reply, 1) // con buffer: el secuenciador nunca se bloquea al responder
	enqueued := time.Now()
	cmd := func(x *exchange.Exchange) (*journal.Record, func()) {
		if s.obs != nil {
			s.obs.QueueWait(time.Since(enqueued))
		}
		// Si el cliente ya no espera (venció su plazo o se desconectó) NO se ejecuta: su respuesta fue un
		// error/504, así que ejecutar la orden ahora crearía una "ejecución fantasma" que él no conoce.
		// Medido antes de esta guarda: 50 de 50 órdenes cuyo cliente recibió timeout se ejecutaron igual.
		if err := ctx.Err(); err != nil {
			s.expired.Add(1)
			return nil, func() { ch <- reply{zero, err} }
		}
		v, rec, err := fn(x)
		if err != nil {
			rec = nil // lo rechazado no cambia el estado: no se registra
		}
		return rec, func() { ch <- reply{v, err} }
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
	return Do(ctx, s, func(x *exchange.Exchange) (engine.Result, *journal.Record, error) {
		res, err := x.PlaceOrder(o)
		if err != nil {
			return res, nil, err
		}
		return res, journal.NewOrderRecord(o, res), nil
	})
}

func (s *Sequencer) Cancel(ctx context.Context, id string) (domain.Order, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (domain.Order, *journal.Record, error) {
		o, err := x.Cancel(id)
		if err != nil {
			return o, nil, err
		}
		return o, journal.NewCancelRecord(o), nil
	})
}

func (s *Sequencer) Deposit(ctx context.Context, user string, brl, vib int64) error {
	_, err := Do(ctx, s, func(x *exchange.Exchange) (struct{}, *journal.Record, error) {
		if err := x.Deposit(user, brl, vib); err != nil {
			return struct{}{}, nil, err
		}
		return struct{}{}, journal.NewDepositRecord(user, brl, vib), nil
	})
	return err
}

func (s *Sequencer) Balance(ctx context.Context, user string) (wallet.Account, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (wallet.Account, *journal.Record, error) {
		return x.Balance(user), nil, nil
	})
}

// BookSnapshot es una vista del libro para consulta.
type BookSnapshot struct {
	Bids []engine.Level
	Asks []engine.Level
}

func (s *Sequencer) Depth(ctx context.Context, n int) (BookSnapshot, error) {
	return Do(ctx, s, func(x *exchange.Exchange) (BookSnapshot, *journal.Record, error) {
		return BookSnapshot{
			Bids: x.Book().Depth(domain.Buy, n),
			Asks: x.Book().Depth(domain.Sell, n),
		}, nil, nil
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
	return Do(ctx, s, func(x *exchange.Exchange) (AuditReport, *journal.Record, error) {
		brl, vib := x.Ledger().Totals()
		rep := AuditReport{OK: true, TotalBRL: brl, TotalVIB: vib}
		if err := x.CheckInvariants(); err != nil {
			rep.OK, rep.Error = false, err.Error()
		}
		return rep, nil, nil
	})
}
