// Package history es el "read model": una copia consultable (Postgres) de lo que ocurrió,
// construida a partir del journal. NO está en el camino caliente del matching:
//   - si Postgres cae, las órdenes siguen procesándose (el journal es la fuente de verdad),
//   - al volver, el proyector se pone al día solo,
//   - y si se perdiera por completo, se reconstruye leyendo el journal desde el inicio.
package history

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("no encontrado")
	// ErrJournalMismatch: el historial en Postgres pertenece a OTRO journal. Proyectar mezclaría
	// historias distintas, así que se detiene hasta que alguien decida (ver docs).
	ErrJournalMismatch = errors.New("el historial pertenece a otro journal")
)

const schema = `
CREATE TABLE IF NOT EXISTS projector_state (
	id       int PRIMARY KEY CHECK (id = 1),
	last_seq bigint NOT NULL
);
ALTER TABLE projector_state ADD COLUMN IF NOT EXISTS journal_id text;
-- Byte del journal donde EMPIEZA el registro last_seq: permite reanudar sin re-leer todo el journal
-- (0 = desconocido, p. ej. historial creado antes de esta columna: se lee desde el inicio).
ALTER TABLE projector_state ADD COLUMN IF NOT EXISTS journal_offset bigint NOT NULL DEFAULT 0;
INSERT INTO projector_state (id, last_seq) VALUES (1, 0) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS orders (
	id          text PRIMARY KEY,
	user_id     text        NOT NULL,
	side        text        NOT NULL,
	price       bigint      NOT NULL,
	qty         bigint      NOT NULL,
	filled      bigint      NOT NULL,
	status      text        NOT NULL,
	created_seq bigint      NOT NULL,
	created_at  timestamptz NOT NULL,
	updated_at  timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_user_idx ON orders (user_id, created_seq DESC);

CREATE TABLE IF NOT EXISTS trades (
	id            bigint PRIMARY KEY,
	price         bigint      NOT NULL,
	qty           bigint      NOT NULL,
	total         bigint      NOT NULL,
	buy_order_id  text        NOT NULL,
	sell_order_id text        NOT NULL,
	buy_user_id   text        NOT NULL,
	sell_user_id  text        NOT NULL,
	maker_order_id text       NOT NULL,
	taker_order_id text       NOT NULL,
	seq           bigint      NOT NULL,
	executed_at   timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS trades_buy_user_idx  ON trades (buy_user_id, id DESC);
CREATE INDEX IF NOT EXISTS trades_sell_user_idx ON trades (sell_user_id, id DESC);
CREATE INDEX IF NOT EXISTS trades_buy_order_idx  ON trades (buy_order_id);
CREATE INDEX IF NOT EXISTS trades_sell_order_idx ON trades (sell_order_id);

CREATE TABLE IF NOT EXISTS deposits (
	seq        bigint PRIMARY KEY,
	user_id    text        NOT NULL,
	brl        bigint      NOT NULL,
	vib        bigint      NOT NULL,
	created_at timestamptz NOT NULL
);

-- Saldo total (libre + reservado) de cada usuario RECALCULADO solo a partir del historial:
-- depósitos + compras - ventas. Debe coincidir siempre con la billetera del motor.
CREATE OR REPLACE VIEW balances_from_history AS
SELECT user_id, SUM(brl)::bigint AS brl_total, SUM(vib)::bigint AS vib_total
FROM (
	SELECT user_id, brl, vib FROM deposits
	UNION ALL SELECT buy_user_id,  -total,  qty FROM trades
	UNION ALL SELECT sell_user_id,  total, -qty FROM trades
) m
GROUP BY user_id;
`

type Store struct{ pool *pgxpool.Pool }

// Open prepara el pool SIN conectar: el servidor puede arrancar aunque Postgres no esté listo.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.HealthCheckPeriod = 15 * time.Second
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate crea el esquema (idempotente).
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// LastSeq devuelve hasta qué registro del journal está reflejado en Postgres.
func (s *Store) LastSeq(ctx context.Context) (uint64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT last_seq FROM projector_state WHERE id = 1`).Scan(&n)
	return uint64(n), err
}

// Position devuelve hasta qué registro está reflejado el historial y en qué byte del journal empieza ese
// último registro (0 si no se conoce).
func (s *Store) Position(ctx context.Context) (seq uint64, offset int64, err error) {
	var n int64
	err = s.pool.QueryRow(ctx, `SELECT last_seq, journal_offset FROM projector_state WHERE id = 1`).Scan(&n, &offset)
	return uint64(n), offset, err
}

// --- modelos de lectura (con tags JSON: se devuelven tal cual por la API) ---

type Order struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Side      string    `json:"side"`
	Price     int64     `json:"price"`
	Qty       int64     `json:"qty"`
	Filled    int64     `json:"filled"`
	Remaining int64     `json:"remaining"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Trade struct {
	ID          int64     `json:"id"`
	Price       int64     `json:"price"`
	Qty         int64     `json:"qty"`
	Total       int64     `json:"total"`
	BuyOrderID  string    `json:"buy_order_id"`
	SellOrderID string    `json:"sell_order_id"`
	BuyUserID   string    `json:"buy_user_id"`
	SellUserID  string    `json:"sell_user_id"`
	MakerOrder  string    `json:"maker_order_id"`
	TakerOrder  string    `json:"taker_order_id"`
	JournalSeq  int64     `json:"journal_seq"`
	ExecutedAt  time.Time `json:"executed_at"`
}

const orderCols = `id, user_id, side, price, qty, filled, status, created_at, updated_at`
const tradeCols = `id, price, qty, total, buy_order_id, sell_order_id, buy_user_id, sell_user_id,
	maker_order_id, taker_order_id, seq, executed_at`

func scanOrder(r pgx.Row) (Order, error) {
	var o Order
	err := r.Scan(&o.ID, &o.UserID, &o.Side, &o.Price, &o.Qty, &o.Filled, &o.Status, &o.CreatedAt, &o.UpdatedAt)
	o.Remaining = o.Qty - o.Filled
	return o, err
}

func scanTrade(r pgx.Row) (Trade, error) {
	var t Trade
	err := r.Scan(&t.ID, &t.Price, &t.Qty, &t.Total, &t.BuyOrderID, &t.SellOrderID, &t.BuyUserID,
		&t.SellUserID, &t.MakerOrder, &t.TakerOrder, &t.JournalSeq, &t.ExecutedAt)
	return t, err
}

// GetOrder devuelve la orden, sus trades y hasta qué registro del journal está reflejado.
// Es eventualmente consistente: una orden recién aceptada puede tardar unos ms en aparecer.
func (s *Store) GetOrder(ctx context.Context, id string) (Order, []Trade, uint64, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1`, id))
	asOf, _ := s.LastSeq(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, nil, asOf, ErrNotFound
	}
	if err != nil {
		return Order{}, nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+tradeCols+` FROM trades WHERE buy_order_id = $1 OR sell_order_id = $1 ORDER BY id`, id)
	if err != nil {
		return Order{}, nil, 0, err
	}
	defer rows.Close()
	trades := []Trade{}
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return Order{}, nil, 0, err
		}
		trades = append(trades, t)
	}
	return o, trades, asOf, rows.Err()
}

func (s *Store) ListOrders(ctx context.Context, user, status string, limit int) ([]Order, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+orderCols+` FROM orders
		WHERE ($1 = '' OR user_id = $1) AND ($2 = '' OR status = $2)
		ORDER BY created_seq DESC LIMIT $3`, user, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) ListTrades(ctx context.Context, user string, limit int) ([]Trade, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tradeCols+` FROM trades
		WHERE $1 = '' OR buy_user_id = $1 OR sell_user_id = $1
		ORDER BY id DESC LIMIT $2`, user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trade{}
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// BalanceFromHistory devuelve el saldo total recalculado desde depósitos y trades.
func (s *Store) BalanceFromHistory(ctx context.Context, user string) (brl, vib int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT brl_total, vib_total FROM balances_from_history WHERE user_id = $1`, user).Scan(&brl, &vib)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return
}

func (s *Store) String() string { return fmt.Sprintf("history.Store(%p)", s) }

// Exec ejecuta SQL directo (uso en pruebas y mantenimiento).
func (s *Store) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := s.pool.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

// BindJournal ata el historial a la identidad de un journal. La primera vez la registra;
// después comprueba que siga siendo el mismo.
func (s *Store) BindJournal(ctx context.Context, journalID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE projector_state SET journal_id = $1
		WHERE id = 1 AND (journal_id IS NULL OR journal_id = $1)`, journalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w (journal actual %s)", ErrJournalMismatch, journalID)
	}
	return nil
}
