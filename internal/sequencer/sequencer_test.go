package sequencer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

// Cientos de goroutines mandan órdenes a la vez: el resultado debe seguir siendo consistente.
func TestConcurrentOrdersKeepInvariants(t *testing.T) {
	s := New(exchange.New(), 100_000)
	defer s.Stop()
	ctx := context.Background()

	const users = 40
	for i := 0; i < users; i++ {
		if err := s.Deposit(ctx, fmt.Sprintf("u%d", i), 1_000_000, 2_000); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.Audit(ctx)

	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 200; i++ {
				side := domain.Buy
				if rng.Intn(2) == 0 {
					side = domain.Sell
				}
				_, _ = s.PlaceOrder(ctx, domain.Order{
					ID: fmt.Sprintf("g%d-o%d", g, i), UserID: fmt.Sprintf("u%d", rng.Intn(users)),
					Side: side, Price: domain.Price(95 + rng.Intn(11)), Qty: domain.Qty(1 + rng.Intn(20)),
				})
			}
		}(g)
	}
	wg.Wait()

	after, err := s.Audit(ctx)
	if err != nil || !after.OK {
		t.Fatalf("auditoría falló: %+v %v", after, err)
	}
	if after.TotalBRL != before.TotalBRL || after.TotalVIB != before.TotalVIB {
		t.Fatalf("no se conservan los totales: %+v -> %+v", before, after)
	}
}

// Con la cola llena se rechaza de inmediato (503 en HTTP), sin bloquearse.
func TestOverloadReturnsErrOverloaded(t *testing.T) {
	s := New(exchange.New(), 2)
	defer s.Stop()

	release := make(chan struct{})
	started := make(chan struct{})
	go func() { // ocupa el secuenciador
		_, _ = Do(context.Background(), s, func(*exchange.Exchange) (int, *journal.Record, error) {
			close(started)
			<-release
			return 0, nil, nil
		})
	}()
	<-started

	for i := 0; i < 2; i++ { // llena la cola (capacidad 2)
		go func() {
			_, _ = Do(context.Background(), s, func(*exchange.Exchange) (int, *journal.Record, error) { return 0, nil, nil })
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.QueueLen() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	_, err := s.Balance(context.Background(), "x")
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("esperaba ErrOverloaded, got %v", err)
	}
	if s.Overloaded() == 0 {
		t.Fatal("el contador de rechazos debe incrementarse")
	}
	close(release)
}

func TestStopDrainsQueueAndRejectsNew(t *testing.T) {
	s := New(exchange.New(), 10)
	if err := s.Deposit(context.Background(), "a", 10, 0); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if _, err := s.Balance(context.Background(), "a"); !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v", err)
	}
}

type stateView struct {
	accounts map[string]wallet.Account
	bids     []engine.Level
	asks     []engine.Level
}

func takeStateView(x *exchange.Exchange) stateView {
	s := stateView{accounts: map[string]wallet.Account{}}
	for _, u := range x.Ledger().Users() {
		if a := x.Ledger().Get(u); a != (wallet.Account{}) {
			s.accounts[u] = a
		}
	}
	s.bids, s.asks = x.Book().Depth(domain.Buy, 1000), x.Book().Depth(domain.Sell, 1000)
	return s
}

// Simula una caída: se opera con persistencia, se "mata" el proceso (sin cerrar el journal)
// y se reconstruye todo reproduciendo el log. El estado debe ser idéntico.
func TestCrashRecoveryRebuildsIdenticalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	ctx := context.Background()

	j, _, err := journal.Recover(path, journal.Options{}, func(journal.Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ex := exchange.New()
	s := New(ex, 100_000, WithJournal(j))

	const users = 25
	for i := 0; i < users; i++ {
		if err := s.Deposit(ctx, fmt.Sprintf("u%d", i), 500_000, 1_000); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 150; i++ {
				side := domain.Buy
				if rng.Intn(2) == 0 {
					side = domain.Sell
				}
				id := fmt.Sprintf("g%d-o%d", g, i)
				_, _ = s.PlaceOrder(ctx, domain.Order{ID: id, UserID: fmt.Sprintf("u%d", rng.Intn(users)),
					Side: side, Price: domain.Price(95 + rng.Intn(11)), Qty: domain.Qty(1 + rng.Intn(20))})
				if rng.Intn(8) == 0 {
					_, _ = s.Cancel(ctx, id)
				}
			}
		}(g)
	}
	wg.Wait()
	s.Stop()
	j.Abandon() // suelta archivo y candado SIN sincronizar ni cerrar con orden: equivale a un kill -9
	if j.Syncs() == 0 || s.Batches() == 0 {
		t.Fatal("debe haber habido fsyncs")
	}
	t.Logf("comandos=%d lotes(fsync)=%d -> group commit confirma ~%d comandos por fsync",
		s.Processed(), j.Syncs(), s.Processed()/j.Syncs())
	want := takeStateView(ex)

	// --- reinicio ---
	ex2 := exchange.New()
	_, st, err := journal.Recover(path, journal.Options{}, func(r journal.Record) error { return journal.Apply(ex2, r) })
	if err != nil {
		t.Fatalf("recuperación: %v", err)
	}
	if st.Records == 0 || st.TruncatedBytes != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if err := ex2.CheckInvariants(); err != nil {
		t.Fatalf("invariantes tras recuperar: %v", err)
	}
	if got := takeStateView(ex2); !reflect.DeepEqual(want, got) {
		t.Fatal("el estado recuperado difiere del estado previo a la caída")
	}
}
