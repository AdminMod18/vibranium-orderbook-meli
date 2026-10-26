package sequencer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
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
		_, _ = Do(context.Background(), s, func(*exchange.Exchange) (int, error) {
			close(started)
			<-release
			return 0, nil
		})
	}()
	<-started

	for i := 0; i < 2; i++ { // llena la cola (capacidad 2)
		go func() { _, _ = Do(context.Background(), s, func(*exchange.Exchange) (int, error) { return 0, nil }) }()
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
