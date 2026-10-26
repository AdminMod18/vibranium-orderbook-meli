package history

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
)

// ErrHistoryAhead: Postgres refleja más registros de los que tiene el journal. Es imposible en una operación
// correcta (el proyector solo lee lo ya sincronizado), así que indica que el journal retrocedió o que se
// restauró una copia antigua. Proyectar así saltaría en silencio las órdenes nuevas.
var ErrHistoryAhead = errors.New("el historial de Postgres está por delante del journal")

const (
	readBatch    = 5000
	pollInterval = 50 * time.Millisecond
	maxBackoff   = 5 * time.Second
)

// Projector lee el journal (solo lectura) y refleja los eventos en Postgres.
//
// Garantías:
//   - exactly-once efectivo: cada lote se aplica en UNA transacción junto con la actualización
//     de projector_state.last_seq; tras un fallo se reintenta el mismo lote sin duplicar,
//   - tolera Postgres caído: reintenta con backoff exponencial y NO afecta al matching,
//   - al reiniciar retoma desde last_seq (los registros ya reflejados se saltan).
//
// Como solo depende del archivo del journal, puede ejecutarse como proceso aparte (o sustituirse
// por un consumidor de Kafka) sin tocar el motor.
type Projector struct {
	store   *Store
	path    string
	log     *slog.Logger
	durable func() int64 // si no es nil: solo se lee hasta el offset ya sincronizado en disco

	projected atomic.Uint64 // último seq reflejado en Postgres
	healthy   atomic.Bool
	failures  atomic.Uint64
}

func NewProjector(store *Store, journalPath string, log *slog.Logger) *Projector {
	return &Projector{store: store, path: journalPath, log: log}
}

// SetDurable limita la lectura del journal al offset ya sincronizado en disco (Journal.DurableBytes).
// Sin esto el proyector podría reflejar en Postgres órdenes que un corte de energía borraría del journal
// (y a las que NUNCA se les respondió al cliente). Debe llamarse antes de Run.
func (p *Projector) SetDurable(fn func() int64) { p.durable = fn }

func (p *Projector) ProjectedSeq() uint64 { return p.projected.Load() }
func (p *Projector) Healthy() bool        { return p.healthy.Load() }
func (p *Projector) Failures() uint64     { return p.failures.Load() }

// Run bloquea hasta que ctx se cancele.
func (p *Projector) Run(ctx context.Context) {
	backoff := 200 * time.Millisecond
	fail := func(msg string, err error) bool {
		p.healthy.Store(false)
		p.failures.Add(1)
		p.log.Warn(msg, "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
		return true
	}

	for ctx.Err() == nil {
		// 1) esquema y punto de partida
		if err := p.store.Migrate(ctx); err != nil {
			if !fail("proyector: no se pudo migrar el esquema (¿Postgres caído?)", err) {
				return
			}
			continue
		}
		last, err := p.store.LastSeq(ctx)
		if err != nil {
			if !fail("proyector: no se pudo leer last_seq", err) {
				return
			}
			continue
		}

		// identidad del journal: debe ser el mismo al que pertenece este historial
		jid, err := journalID(p.path)
		if err == nil && jid != "" {
			err = p.store.BindJournal(ctx, jid)
		}
		if err != nil {
			if errors.Is(err, ErrJournalMismatch) {
				p.log.Error("proyector DETENIDO: el historial de Postgres no corresponde a este journal. "+
					"Si el journal es el correcto, vacíe las tablas del historial para reconstruirlo desde cero", "err", err)
			}
			if !fail("proyector: no se pudo verificar el journal", err) {
				return
			}
			continue
		}

		p.projected.Store(last) // recién ahora el progreso de la BD es "nuestro"

		rd, err := journal.OpenReader(p.path)
		if err != nil {
			if !fail("proyector: no se pudo abrir el journal", err) {
				return
			}
			continue
		}
		if p.durable != nil {
			rd.SetDurable(p.durable)
		}

		// 2) bucle principal: leer lote -> escribir transacción -> avanzar
		err = p.loop(ctx, rd, last, &backoff)
		rd.Close()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrHistoryAhead) {
			p.log.Error("proyector DETENIDO: el historial de Postgres está POR DELANTE del journal (el journal retrocedió o se "+
				"restauró una copia antigua). No se proyecta nada para no mezclar historias. Si el journal es el correcto, "+
				"vacíe las tablas del historial para reconstruirlo desde cero", "err", err)
		}
		if !fail("proyector: reiniciando tras error", err) {
			return
		}
	}
}

func (p *Projector) loop(ctx context.Context, rd *journal.Reader, last uint64, backoff *time.Duration) error {
	var pending []journal.Record
	var journalLast uint64 // mayor secuencia leída del journal (incluidas las ya reflejadas que se saltan)
	for ctx.Err() == nil {
		if len(pending) == 0 {
			n, err := rd.ReadNew(readBatch, func(r journal.Record) error {
				if r.Seq > journalLast {
					journalLast = r.Seq
				}
				if r.Seq > last { // los ya reflejados se saltan
					pending = append(pending, r)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("leyendo journal: %w", err)
			}
			if n > 0 && len(pending) == 0 {
				continue // solo se saltaron registros ya reflejados: seguir leyendo sin dormir
			}
			if len(pending) == 0 {
				// Al día con lo durable. Postgres jamás puede haber reflejado más de lo que el journal tiene.
				if last > journalLast {
					return fmt.Errorf("%w: Postgres refleja hasta el registro %d y el journal solo llega al %d", ErrHistoryAhead, last, journalLast)
				}
				p.healthy.Store(true)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(pollInterval):
				}
				continue
			}
		}
		if err := p.store.applyBatch(ctx, pending); err != nil {
			// El lote se conserva y se reintenta íntegro (la transacción es todo o nada).
			return fmt.Errorf("aplicando lote de %d registros: %w", len(pending), err)
		}
		last = pending[len(pending)-1].Seq
		p.projected.Store(last)
		p.healthy.Store(true)
		*backoff = 200 * time.Millisecond
		pending = pending[:0]
	}
	return nil
}

// applyBatch refleja los registros en una sola transacción (con pipeline de sentencias).
func (s *Store) applyBatch(ctx context.Context, recs []journal.Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op si ya hubo commit

	b := &pgx.Batch{}
	for _, r := range recs {
		ts := time.Unix(0, r.TS).UTC()
		switch r.Kind {
		case journal.KindDeposit:
			b.Queue(`INSERT INTO deposits (seq, user_id, brl, vib, created_at) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (seq) DO NOTHING`, r.Seq, r.User, r.BRL, r.VIB, ts)

		case journal.KindOrder:
			o := r.Order
			b.Queue(`INSERT INTO orders (id, user_id, side, price, qty, filled, status, created_seq, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9) ON CONFLICT (id) DO NOTHING`,
				o.ID, o.UserID, o.Side, o.Price, o.Qty, o.Filled, o.Status, r.Seq, ts)
			for _, t := range r.Trades {
				b.Queue(`INSERT INTO trades (id, price, qty, total, buy_order_id, sell_order_id, buy_user_id, sell_user_id,
					maker_order_id, taker_order_id, seq, executed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
					ON CONFLICT (id) DO NOTHING`,
					t.ID, t.Price, t.Qty, t.Price*t.Qty, t.BuyOrderID, t.SellOrderID, t.BuyUserID, t.SellUserID,
					t.MakerOrderID, t.TakerOrderID, r.Seq, ts)
				// El maker ya estaba en el libro: avanza su ejecución.
				b.Queue(`UPDATE orders SET filled = filled + $2,
					status = CASE WHEN filled + $2 >= qty THEN 'FILLED' ELSE 'PARTIALLY_FILLED' END,
					updated_at = $3 WHERE id = $1`, t.MakerOrderID, t.Qty, ts)
			}

		case journal.KindCancel:
			b.Queue(`UPDATE orders SET status = 'CANCELLED', filled = $2, updated_at = $3 WHERE id = $1`,
				r.OrderID, r.Order.Filled, ts)
		}
	}
	b.Queue(`UPDATE projector_state SET last_seq = $1 WHERE id = 1`, int64(recs[len(recs)-1].Seq))

	br := tx.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return err
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// journalID lee la identidad (registro genesis) de un journal; "" si no tiene.
func journalID(path string) (string, error) {
	rd, err := journal.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer rd.Close()
	id := ""
	_, err = rd.ReadNew(1, func(r journal.Record) error {
		if r.Kind == journal.KindGenesis {
			id = r.JournalID
		}
		return nil
	})
	return id, err
}
