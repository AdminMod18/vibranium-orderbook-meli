package recovery_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/recovery"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/snapshot"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// normalized deja el estado en forma comparable (los mapas y la lista de ids no tienen orden).
func normalized(x *exchange.Exchange) exchange.State {
	s := x.Capture()
	sort.Strings(s.Seen)
	sort.Slice(s.Book.Orders, func(i, j int) bool { return s.Book.Orders[i].Seq < s.Book.Orders[j].Seq })
	return s
}

type env struct {
	dir, jpath, spath string
	ex                *exchange.Exchange
	jr                *journal.Journal
	seq               *sequencer.Sequencer
	w                 *snapshot.Writer
}

// start arranca un sistema con persistencia y snapshots cada "every" registros.
func start(t *testing.T, every uint64) *env { return startWindow(t, every, 0) }

func startWindow(t *testing.T, every uint64, window int) *env {
	t.Helper()
	e := &env{dir: t.TempDir()}
	e.jpath, e.spath = filepath.Join(e.dir, "journal.log"), filepath.Join(e.dir, "snapshot.bin")
	ex, jr, _, err := recovery.Recover(e.jpath, e.spath, journal.Options{}, window, quiet)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := journal.ReadID(e.jpath)
	e.ex, e.jr = ex, jr
	e.w = &snapshot.Writer{Path: e.spath, JournalID: id}
	e.seq = sequencer.New(ex, 100_000, sequencer.WithJournal(jr), sequencer.WithSnapshots(e.w, every))
	return e
}

func load(t *testing.T, s *sequencer.Sequencer, prefix string, goroutines, perG int) {
	t.Helper()
	ctx := context.Background()
	const users = 20
	for i := 0; i < users; i++ {
		_ = s.Deposit(ctx, fmt.Sprintf("u%d", i), 300_000, 600) // idempotente en efecto: suma saldo
	}
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + int64(len(prefix))*77))
			for i := 0; i < perG; i++ {
				side := domain.Buy
				if rng.Intn(2) == 0 {
					side = domain.Sell
				}
				id := fmt.Sprintf("%s-g%d-o%d", prefix, g, i)
				_, _ = s.PlaceOrder(ctx, domain.Order{ID: id, UserID: fmt.Sprintf("u%d", rng.Intn(users)),
					Side: side, Price: domain.Price(95 + rng.Intn(11)), Qty: domain.Qty(1 + rng.Intn(12))})
				if rng.Intn(9) == 0 {
					_, _ = s.Cancel(ctx, id)
				}
			}
		}(g)
	}
	wg.Wait()
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func recoverAt(t *testing.T, jpath, spath string) (*exchange.Exchange, recovery.Report) {
	t.Helper()
	ex, jr, rep, err := recovery.Recover(jpath, spath, journal.Options{}, 0, quiet)
	if err != nil {
		t.Fatalf("recuperación: %v", err)
	}
	jr.Close() // solo interesa el estado recuperado; soltar el candado para poder reabrir el journal
	return ex, rep
}

func mustEqual(t *testing.T, want exchange.State, got *exchange.Exchange, msg string) {
	t.Helper()
	if !reflect.DeepEqual(want, normalized(got)) {
		t.Fatalf("%s: el estado recuperado difiere del original", msg)
	}
	if err := got.CheckInvariants(); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// Apagado ordenado: queda un snapshot final y el siguiente arranque no reproduce NADA.
func TestGracefulStopLeavesFinalSnapshot(t *testing.T) {
	e := start(t, 400)
	load(t, e.seq, "a", 20, 150)
	e.seq.Stop()
	want := normalized(e.ex)
	e.jr.Close()

	ex, rep := recoverAt(t, e.jpath, e.spath)
	if !rep.FromSnapshot || rep.Replayed != 0 {
		t.Fatalf("debía arrancar del snapshot final sin reproducir nada: %+v", rep)
	}
	mustEqual(t, want, ex, "apagado ordenado")
}

// Caída SIN apagado ordenado: se queda el último snapshot periódico y se reproduce solo la cola.
func TestCrashUsesPeriodicSnapshotPlusTail(t *testing.T) {
	e := start(t, 500)
	load(t, e.seq, "a", 20, 200)
	e.w.Wait()
	if e.w.Taken() == 0 {
		t.Fatal("debió tomarse al menos un snapshot periódico")
	}
	want := normalized(e.ex) // estado "en el momento de la caída" (el secuenciador está ocioso y todo sincronizado)

	// Foto del disco tal como estaría tras un kill -9: journal + último snapshot periódico (sin el final).
	crash := t.TempDir()
	cj, cs := filepath.Join(crash, "journal.log"), filepath.Join(crash, "snapshot.bin")
	copyFile(t, e.jpath, cj)
	copyFile(t, e.spath, cs)
	e.seq.Stop()
	e.jr.Close()

	ex, rep := recoverAt(t, cj, cs)
	total := int(e.jr.LastSeq())
	if !rep.FromSnapshot || rep.Replayed == 0 || rep.Replayed >= total {
		t.Fatalf("debía usar snapshot + cola parcial (cola=%d de %d): %+v", rep.Replayed, total, rep)
	}
	t.Logf("recuperación: snapshot en seq %d + cola de %d registros (de %d en total)", rep.SnapshotSeq, rep.Replayed, total)
	mustEqual(t, want, ex, "caída")
}

// Un snapshot inservible NUNCA debe impedir arrancar: se reproduce el journal completo y el estado es el mismo.
func TestUnusableSnapshotFallsBackToFullReplay(t *testing.T) {
	e := start(t, 300)
	load(t, e.seq, "a", 10, 200)
	e.seq.Stop()
	want := normalized(e.ex)
	e.jr.Close()
	good, _ := snapshot.Read(e.spath)

	cases := map[string]func(){
		"snapshot corrupto": func() {
			b, _ := os.ReadFile(e.spath)
			b[len(b)/2] ^= 0xFF
			_ = os.WriteFile(e.spath, b, 0o644)
		},
		"snapshot truncado": func() {
			b, _ := os.ReadFile(e.spath)
			_ = os.WriteFile(e.spath, b[:len(b)/3], 0o644)
		},
		"snapshot de OTRO journal": func() {
			g := good
			g.JournalID = "otro-journal"
			_ = snapshot.Write(e.spath, g)
		},
		"offset inválido (cae en medio de un registro)": func() {
			g := good
			g.JournalOffset += 3
			_ = snapshot.Write(e.spath, g)
		},
		"offset más allá del final": func() {
			g := good
			g.JournalOffset += 1_000_000
			_ = snapshot.Write(e.spath, g)
		},
		"estado inconsistente": func() {
			g := good
			for u, a := range g.State.Accounts {
				a.BRLFree += 777 // descuadra: ya no se conservan los invariantes
				g.State.Accounts[u] = a
				break
			}
			_ = snapshot.Write(e.spath, g)
		},
	}
	for name, corrupt := range cases {
		_ = snapshot.Write(e.spath, good) // restablece
		corrupt()
		ex, rep := recoverAt(t, e.jpath, e.spath)
		if rep.FromSnapshot || rep.SnapshotReason == "" {
			t.Errorf("%s: no debió usarse el snapshot: %+v", name, rep)
			continue
		}
		mustEqual(t, want, ex, name)
		t.Logf("%-48s -> reproducción completa de %d registros (motivo: %.60s)", name, rep.Replayed, rep.SnapshotReason)
	}
}

func TestNoSnapshotMeansFullReplay(t *testing.T) {
	e := start(t, 0) // snapshots desactivados
	load(t, e.seq, "a", 10, 100)
	e.seq.Stop()
	want := normalized(e.ex)
	e.jr.Close()
	if _, err := os.Stat(e.spath); !os.IsNotExist(err) {
		t.Fatal("con snapshots desactivados no debe existir el archivo")
	}
	ex, rep := recoverAt(t, e.jpath, e.spath)
	if rep.FromSnapshot || rep.Replayed == 0 {
		t.Fatalf("%+v", rep)
	}
	mustEqual(t, want, ex, "sin snapshot")
}

// Tras recuperar con snapshot, el sistema sigue operando y vuelve a recuperarse igual (encadenado).
func TestChainedRestartsStayConsistent(t *testing.T) {
	e := start(t, 300)
	load(t, e.seq, "a", 10, 120)
	e.seq.Stop()
	e.jr.Close()

	for round := 0; round < 3; round++ {
		ex, jr, _, err := recovery.Recover(e.jpath, e.spath, journal.Options{}, 0, quiet)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := journal.ReadID(e.jpath)
		w := &snapshot.Writer{Path: e.spath, JournalID: id}
		s := sequencer.New(ex, 100_000, sequencer.WithJournal(jr), sequencer.WithSnapshots(w, 300))
		load(t, s, fmt.Sprintf("r%d", round), 10, 100)
		s.Stop()
		want := normalized(ex)
		jr.Close()

		got, rep := recoverAt(t, e.jpath, e.spath)
		if !rep.FromSnapshot || rep.Replayed != 0 {
			t.Fatalf("ronda %d: %+v", round, rep)
		}
		mustEqual(t, want, got, fmt.Sprintf("ronda %d", round))
	}
}

// Cambiar el tamaño de la ventana entre ejecuciones NO puede impedir arrancar ni cambiar el estado
// del libro y las billeteras: el journal solo contiene lo que se aceptó, aunque la ventana fuera otra.
func TestChangingTheWindowNeverBlocksStartup(t *testing.T) {
	for _, withSnapshot := range []bool{true, false} {
		e := startWindow(t, 100, 3) // ventana diminuta en la ejecución original
		ctx := context.Background()
		_ = e.seq.Deposit(ctx, "alice", 1_000_000, 0)
		place := func(id string) {
			if _, err := e.seq.PlaceOrder(ctx, domain.Order{ID: id, UserID: "alice", Side: domain.Buy, Price: 10, Qty: 1}); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if _, err := e.seq.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		place("x")
		for _, id := range []string{"y1", "y2", "y3", "y4"} { // sacan a "x" de la ventana de 3
			place(id)
		}
		place("x") // el MISMO id otra vez: con la ventana original se aceptó, así que quedó en el journal dos veces
		e.seq.Stop()
		want := normalized(e.ex)
		want.Seen = nil
		e.jr.Close()
		if !withSnapshot {
			_ = os.Remove(e.spath)
		}

		for _, window := range []int{0, 1, 3, 1000} {
			ex, rep := recoverWindow(t, e.jpath, e.spath, window)
			got := normalized(ex)
			if withSnapshot != rep.FromSnapshot {
				t.Fatalf("snapshot=%v pero FromSnapshot=%v", withSnapshot, rep.FromSnapshot)
			}
			if window > 0 && len(got.Seen) > window {
				t.Fatalf("ventana %d: quedaron %d ids", window, len(got.Seen))
			}
			got.Seen = nil
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("ventana %d (snapshot=%v): el estado difiere", window, withSnapshot)
			}
		}
	}
}

func recoverWindow(t *testing.T, jpath, spath string, window int) (*exchange.Exchange, recovery.Report) {
	t.Helper()
	ex, jr, rep, err := recovery.Recover(jpath, spath, journal.Options{}, window, quiet)
	if err != nil {
		t.Fatalf("la recuperación con ventana %d falló: %v", window, err)
	}
	jr.Close() // solo interesa el estado recuperado; soltar el candado para poder reabrir el journal
	return ex, rep
}

// Una orden que ejecuta el tope de trades (y cuyo remanente se cancela) debe reproducirse IDÉNTICA desde el
// journal: es el registro más grande que existe y el que antes hacía caer el servicio.
func TestOrderTruncatedByTradeCapReplaysIdentically(t *testing.T) {
	e := start(t, 0)
	ctx := context.Background()
	const makers = 12_000 // > engine.MaxTradesPerOrder
	_ = e.seq.Deposit(ctx, "maker", 0, makers)
	_ = e.seq.Deposit(ctx, "taker", 10*makers, 0)
	for i := 0; i < makers; i++ {
		if _, err := e.seq.PlaceOrder(ctx, domain.Order{ID: fmt.Sprintf("m%d", i), UserID: "maker", Side: domain.Sell, Price: 10, Qty: 1}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.seq.PlaceOrder(ctx, domain.Order{ID: "big", UserID: "taker", Side: domain.Buy, Price: 10, Qty: makers})
	if err != nil || res.Order.Status != domain.StatusCancelled || len(res.Trades) != 10_000 {
		t.Fatalf("orden truncada: %v %+v trades=%d", err, res.Order, len(res.Trades))
	}
	e.seq.Stop()
	want := normalized(e.ex)
	e.jr.Close()

	ex, rep := recoverAt(t, e.jpath, e.spath)
	if rep.FromSnapshot {
		t.Fatal("este caso debe ejercitar el replay completo")
	}
	mustEqual(t, want, ex, "orden truncada")
}
