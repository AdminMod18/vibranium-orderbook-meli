package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"testing"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
)

// TestScaleReport mide el costo de capturar y escribir un snapshot con un estado del tamaño del soak
// (ventana de idempotencia llena y muchas órdenes en el libro). Es una MEDICIÓN, no una aserción de
// tiempos: solo corre con SNAPSHOT_SCALE=1 (p. ej. SNAPSHOT_SCALE=1 go test -run ScaleReport -v ./internal/snapshot).
//
//	SNAPSHOT_SCALE_IDS   ids en la ventana de idempotencia (default 5.000.000)
//	SNAPSHOT_SCALE_REST  órdenes que quedan en el libro    (default 1.000.000)
func TestScaleReport(t *testing.T) {
	if os.Getenv("SNAPSHOT_SCALE") == "" {
		t.Skip("medición de escala: defina SNAPSHOT_SCALE=1")
	}
	ids := envInt("SNAPSHOT_SCALE_IDS", 5_000_000)
	rest := envInt("SNAPSHOT_SCALE_REST", 1_000_000)

	x := exchange.NewWithWindow(ids)
	const users = 1000
	for u := 0; u < users; u++ {
		if err := x.Deposit(fmt.Sprintf("u%d", u), 1<<50, 1<<40); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	place := func(side domain.Side, price domain.Price, u int) {
		n++
		if _, err := x.PlaceOrder(domain.Order{ID: fmt.Sprintf("lg100276201-%d", n), UserID: fmt.Sprintf("u%d", u%users),
			Side: side, Price: price, Qty: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for n < ids-rest { // pares que se cruzan del todo: llenan la ventana sin dejar órdenes en el libro
		place(domain.Buy, 10000, n)
		place(domain.Sell, 10000, n+1)
	}
	for i := 0; i < rest; i++ { // órdenes que se quedan esperando (lados que no se cruzan)
		if i%2 == 0 {
			place(domain.Buy, domain.Price(9000-i%500), i)
		} else {
			place(domain.Sell, domain.Price(11000+i%500), i)
		}
	}
	t.Logf("estado: %d ids en la ventana, %d órdenes en el libro", x.SeenCount(), len(x.Book().RestingOrders()))

	runtime.GC()
	var m0, m1 runtime.MemStats
	for i := 0; i < 3; i++ {
		runtime.ReadMemStats(&m0)
		t0 := time.Now()
		st := x.Capture()
		d := time.Since(t0)
		tb := time.Now()
		_ = x.Book().Capture()
		dBook := time.Since(tb)
		tl := time.Now()
		_ = x.Ledger().Capture()
		dLedger := time.Since(tl)
		t.Logf("   desglose: libro=%s billeteras=%s ids(resto)=%s", dBook.Round(time.Millisecond), dLedger.Round(time.Millisecond), (d - dBook - dLedger).Round(time.Millisecond))
		runtime.ReadMemStats(&m1)
		t.Logf("Capture #%d: pausa=%s asignó=%d MiB (ids=%d órdenes=%d)", i+1, d.Round(time.Millisecond),
			(m1.TotalAlloc-m0.TotalAlloc)>>20, len(st.Seen), len(st.Book.Orders))

		path := filepath.Join(t.TempDir(), "snap.bin")
		runtime.ReadMemStats(&m0)
		t0 = time.Now()
		if err := Write(path, File{JournalID: "j", LastSeq: 1, State: st}); err != nil {
			t.Fatal(err)
		}
		d = time.Since(t0)
		runtime.ReadMemStats(&m1)
		fi, _ := os.Stat(path)
		t.Logf("Write   #%d: %s asignó=%d MiB para un archivo de %d MiB (x%.1f)", i+1, d.Round(time.Millisecond),
			(m1.TotalAlloc-m0.TotalAlloc)>>20, fi.Size()>>20, float64(m1.TotalAlloc-m0.TotalAlloc)/float64(fi.Size()))
		st = exchange.State{}
		runtime.GC()
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// TestScaleStopTheWorld comprueba si capturar el estado mientras el GC está marcando retrasa los
// stop-the-world del propio GC (copias grandes de memoria con punteros no son interrumpibles).
// Mide con runtime/metrics el MÁXIMO tiempo que tardó el runtime en detener todos los hilos.
func TestScaleStopTheWorld(t *testing.T) {
	if os.Getenv("SNAPSHOT_SCALE") == "" {
		t.Skip("medición de escala: defina SNAPSHOT_SCALE=1")
	}
	x := exchange.NewWithWindow(5_000_000)
	_ = x.Deposit("u", 1<<50, 1<<40)
	for i := 0; i < 5_000_000; i++ {
		if _, err := x.PlaceOrder(domain.Order{ID: fmt.Sprintf("lg100276201-%d", i), UserID: "u", Side: domain.Buy,
			Price: domain.Price(1 + i%1000), Qty: 1}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()

	stop := make(chan struct{})
	go func() { // mantiene al GC marcando casi todo el tiempo, como la carga real
		var sink [][]byte
		for {
			select {
			case <-stop:
				return
			default:
				sink = append(sink, make([]byte, 1<<20))
				if len(sink) > 200 {
					sink = sink[:0]
				}
			}
		}
	}()
	for i := 0; i < 3; i++ {
		before := stopHist()
		t0 := time.Now()
		_ = x.Capture()
		d := time.Since(t0)
		t.Logf("Capture con GC activo #%d: %s; máx. tiempo en detener el mundo durante la captura: %s", i+1,
			d.Round(time.Millisecond), maxNew(before, stopHist()))
	}
	close(stop)
}

func stopHist() *metrics.Float64Histogram {
	s := []metrics.Sample{{Name: "/sched/pauses/stopping/gc:seconds"}}
	metrics.Read(s)
	return s[0].Value.Float64Histogram()
}

// maxNew devuelve el límite superior del bucket más alto que ganó muestras entre dos lecturas.
func maxNew(a, b *metrics.Float64Histogram) time.Duration {
	var mx float64
	for i := range b.Counts {
		if b.Counts[i] > a.Counts[i] && b.Buckets[i+1] < 1e9 {
			mx = b.Buckets[i+1]
		}
	}
	return time.Duration(mx * float64(time.Second)).Round(time.Microsecond)
}
