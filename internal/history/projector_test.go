package history_test

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/history"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
)

// Pruebas de integración contra un Postgres REAL.
//
//	docker compose exec -T postgres psql -U postgres -c "CREATE DATABASE orderbook_test"   (una vez)
//	TEST_DATABASE_URL=postgres://postgres:orderbook@localhost:5432/orderbook_test go test ./internal/history
func openStore(t *testing.T) *history.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL no definida: se omiten las pruebas con Postgres")
	}
	// Estos tests hacen DROP TABLE: por seguridad solo corren contra bases cuyo nombre contenga "test"
	// (p. ej. .../orderbook_test), NUNCA contra la base viva del compose.
	if u, perr := neturl.Parse(url); perr != nil || !strings.Contains(strings.ToLower(strings.TrimPrefix(u.Path, "/")), "test") {
		t.Skipf("TEST_DATABASE_URL no apunta a una base de pruebas (el nombre debe contener 'test'): se omite para no borrar datos")
	}
	ctx := context.Background()
	st, err := history.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	for _, q := range []string{`DROP VIEW IF EXISTS balances_from_history`, `DROP TABLE IF EXISTS trades, orders, deposits, projector_state`} {
		if _, err := st.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// truth lleva la cuenta independiente de lo que debería haber en Postgres,
// usando solo las respuestas que recibieron los clientes.
type truth struct {
	mu        sync.Mutex
	qty       map[string]int64
	filled    map[string]int64
	cancelled map[string]bool
	trades    int
}

func newTruth() *truth {
	return &truth{qty: map[string]int64{}, filled: map[string]int64{}, cancelled: map[string]bool{}}
}

func (tr *truth) status(id string) string {
	switch {
	case tr.cancelled[id]:
		return "CANCELLED"
	case tr.filled[id] == tr.qty[id]:
		return "FILLED"
	case tr.filled[id] > 0:
		return "PARTIALLY_FILLED"
	}
	return "OPEN"
}

func runLoad(t *testing.T, s *sequencer.Sequencer, tr *truth, prefix string, goroutines, perG, users int) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + int64(len(prefix))*1000))
			for i := 0; i < perG; i++ {
				side := domain.Buy
				if rng.Intn(2) == 0 {
					side = domain.Sell
				}
				id := fmt.Sprintf("%s-g%d-o%d", prefix, g, i)
				res, err := s.PlaceOrder(ctx, domain.Order{ID: id, UserID: fmt.Sprintf("u%d", rng.Intn(users)),
					Side: side, Price: domain.Price(95 + rng.Intn(11)), Qty: domain.Qty(1 + rng.Intn(15))})
				if err != nil {
					continue // sin saldo: no entra, no se registra
				}
				tr.mu.Lock()
				tr.qty[id] = int64(res.Order.Qty)
				for _, x := range res.Trades {
					tr.filled[x.BuyOrderID] += int64(x.Qty)
					tr.filled[x.SellOrderID] += int64(x.Qty)
					tr.trades++
				}
				tr.mu.Unlock()
				if rng.Intn(8) == 0 {
					if o, err := s.Cancel(ctx, id); err == nil {
						tr.mu.Lock()
						tr.cancelled[o.ID] = true
						tr.mu.Unlock()
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func waitProjected(t *testing.T, p *history.Projector, j *journal.Journal) {
	t.Helper()
	want := j.NextSeq() - 1
	deadline := time.Now().Add(20 * time.Second)
	for p.ProjectedSeq() < want {
		if time.Now().After(deadline) {
			t.Fatalf("el proyector no alcanzó el journal: %d de %d", p.ProjectedSeq(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func verify(t *testing.T, st *history.Store, ex *exchange.Exchange, tr *truth, users int) {
	t.Helper()
	ctx := context.Background()

	// 1) cada orden en Postgres == lo que implican las respuestas recibidas
	for id, q := range tr.qty {
		o, _, _, err := st.GetOrder(ctx, id)
		if err != nil {
			t.Fatalf("orden %s: %v", id, err)
		}
		if o.Qty != q || o.Filled != tr.filled[id] || o.Status != tr.status(id) {
			t.Fatalf("orden %s: PG=%+v esperado filled=%d status=%s", id, o, tr.filled[id], tr.status(id))
		}
	}
	// 2) cantidad de trades
	all, err := st.ListTrades(ctx, "", 1_000_000)
	if err != nil || len(all) != tr.trades {
		t.Fatalf("trades en PG=%d esperado=%d err=%v", len(all), tr.trades, err)
	}
	// 3) TRAZABILIDAD: el saldo de cada usuario recalculado SOLO desde el historial
	//    (depósitos + trades) coincide con la billetera real del motor.
	for i := 0; i < users; i++ {
		u := fmt.Sprintf("u%d", i)
		acc := ex.Balance(u)
		brl, vib, err := st.BalanceFromHistory(ctx, u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if brl != acc.BRLFree+acc.BRLHeld || vib != acc.VIBFree+acc.VIBHeld {
			t.Fatalf("%s: historial BRL=%d VIB=%d vs motor %+v", u, brl, vib, acc)
		}
	}
}

func TestProjectorMirrorsEngineAndSurvivesRestart(t *testing.T) {
	st := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	path := filepath.Join(t.TempDir(), "journal.log")
	j, _, err := journal.Recover(path, journal.Options{}, func(journal.Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ex := exchange.New()
	s := sequencer.New(ex, 100_000, sequencer.WithJournal(j))

	// el proyector arranca ANTES de la carga: prueba que sigue el log "en vivo"
	p1ctx, stop1 := context.WithCancel(ctx)
	done1 := make(chan struct{})
	p1 := history.NewProjector(st, path, log)
	p1.SetDurable(j.DurableBytes) // como en producción: solo se proyecta lo ya sincronizado
	go func() { p1.Run(p1ctx); close(done1) }()

	const users = 20
	for i := 0; i < users; i++ {
		if err := s.Deposit(ctx, fmt.Sprintf("u%d", i), 400_000, 800); err != nil {
			t.Fatal(err)
		}
	}
	tr := newTruth()
	runLoad(t, s, tr, "a", 40, 120, users)
	waitProjected(t, p1, j)
	verify(t, st, ex, tr, users)

	// --- reinicio del proyector: detenerlo, seguir operando, y arrancar uno nuevo ---
	stop1()
	<-done1
	runLoad(t, s, tr, "b", 40, 120, users) // mientras el proyector está caído
	if got, _ := st.LastSeq(ctx); got >= j.NextSeq()-1 {
		t.Fatal("el proyector detenido no debería estar al día")
	}
	p2 := history.NewProjector(st, path, log)
	p2.SetDurable(j.DurableBytes)
	go p2.Run(ctx)
	waitProjected(t, p2, j)
	verify(t, st, ex, tr, users) // sin duplicados ni faltantes tras retomar desde last_seq

	s.Stop()
	t.Logf("registros en journal=%d trades=%d", j.NextSeq()-1, tr.trades)
}

func TestProjectorToleratesPostgresDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Puerto donde no hay nadie escuchando
	st, err := history.Open(ctx, "postgres://postgres:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	path := filepath.Join(t.TempDir(), "journal.log")
	j, _, _ := journal.Recover(path, journal.Options{}, func(journal.Record) error { return nil })
	s := sequencer.New(exchange.New(), 1000, sequencer.WithJournal(j))
	defer s.Stop()

	p := history.NewProjector(st, path, log)
	go p.Run(ctx)

	// El motor sigue aceptando y confirmando operaciones aunque el historial esté caído.
	for i := 0; i < 50; i++ {
		if err := s.Deposit(ctx, fmt.Sprintf("x%d", i), 100, 1); err != nil {
			t.Fatalf("el matching no debe depender de Postgres: %v", err)
		}
	}
	time.Sleep(600 * time.Millisecond)
	if p.Healthy() || p.Failures() == 0 {
		t.Fatalf("el proyector debe reportarse no saludable y reintentando (healthy=%v failures=%d)", p.Healthy(), p.Failures())
	}
}

// Un historial ya atado a un journal NO debe mezclarse con otro distinto.
func TestProjectorRefusesForeignJournal(t *testing.T) {
	st := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	mk := func() (string, *sequencer.Sequencer, *journal.Journal) {
		path := filepath.Join(t.TempDir(), "journal.log")
		j, _, err := journal.Recover(path, journal.Options{}, func(journal.Record) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		s := sequencer.New(exchange.New(), 100, sequencer.WithJournal(j))
		t.Cleanup(s.Stop)
		_ = s.Deposit(ctx, "u0", 100, 1)
		return path, s, j
	}
	pathA, _, jA := mk()
	pA := history.NewProjector(st, pathA, log)
	go pA.Run(ctx)
	waitProjected(t, pA, jA)

	pathB, _, _ := mk() // otro journal, mismo Postgres
	pB := history.NewProjector(st, pathB, log)
	go pB.Run(ctx)
	time.Sleep(700 * time.Millisecond)
	if pB.Healthy() || pB.ProjectedSeq() != 0 {
		t.Fatalf("el proyector de un journal ajeno no debe proyectar (healthy=%v seq=%d)", pB.Healthy(), pB.ProjectedSeq())
	}
}

// R2: si el journal RETROCEDE (se restauró una copia antigua o se perdió su cola) mientras Postgres ya había
// reflejado más, el proyector debe DETENERSE y avisar; antes saltaba en silencio las órdenes nuevas porque
// sus secuencias eran menores que el last_seq de Postgres.
func TestProjectorStopsWhenPostgresIsAheadOfTheJournal(t *testing.T) {
	st := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	path := filepath.Join(t.TempDir(), "journal.log")
	j, _, err := journal.Recover(path, journal.Options{}, func(journal.Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s := sequencer.New(exchange.New(), 1000, sequencer.WithJournal(j))
	for i := 0; i < 30; i++ {
		if err := s.Deposit(ctx, fmt.Sprintf("u%d", i), 100, 1); err != nil {
			t.Fatal(err)
		}
	}
	p1ctx, stop1 := context.WithCancel(ctx)
	p1 := history.NewProjector(st, path, log)
	p1.SetDurable(j.DurableBytes)
	done := make(chan struct{})
	go func() { p1.Run(p1ctx); close(done) }()
	waitProjected(t, p1, j)
	stop1()
	<-done
	s.Stop()
	j.Close()
	projected, _ := st.LastSeq(ctx)

	// El journal "retrocede": se pierde su cola (como tras un corte de energía sin fsync, o una restauración).
	fi, _ := os.Stat(path)
	if err := os.Truncate(path, fi.Size()/2); err != nil {
		t.Fatal(err)
	}

	p2 := history.NewProjector(st, path, log)
	go p2.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for p2.Failures() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p2.Failures() == 0 || p2.Healthy() {
		t.Fatalf("el proyector debe detenerse y reportarse no saludable (fallos=%d, sano=%v)", p2.Failures(), p2.Healthy())
	}
	if after, _ := st.LastSeq(ctx); after != projected {
		t.Fatalf("no debe tocar el historial: last_seq %d -> %d", projected, after)
	}
}
