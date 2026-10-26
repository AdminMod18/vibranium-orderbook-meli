package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

func sample() File {
	return File{JournalID: "jid", LastSeq: 42, JournalOffset: 1234, State: exchange.State{Seen: []string{"a", "b"}}}
}

func TestRoundTripAndAtomicOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.bin")
	if _, err := Read(path); !errors.Is(err, ErrNone) {
		t.Fatalf("sin archivo debe dar ErrNone, got %v", err)
	}
	if err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil || got.LastSeq != 42 || got.JournalID != "jid" || got.JournalOffset != 1234 || len(got.State.Seen) != 2 {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	f2 := sample()
	f2.LastSeq = 99
	if err := Write(path, f2); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(path); got.LastSeq != 99 {
		t.Fatal("el snapshot nuevo debe reemplazar al anterior")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("no debe quedar archivo temporal")
	}
}

func TestCorruptAndTruncatedAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)

	for name, mutate := range map[string]func([]byte) []byte{
		"byte alterado": func(b []byte) []byte { c := append([]byte(nil), b...); c[len(c)-3] ^= 0xFF; return c },
		"truncado":      func(b []byte) []byte { return b[:len(b)-5] },
		"solo cabecera": func(b []byte) []byte { return b[:4] },
		"vacío":         func(b []byte) []byte { return nil },
	} {
		if err := os.WriteFile(path, mutate(b), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: debe rechazarse como corrupto, got %v", name, err)
		}
	}
}

func richState(accounts, orders, seen int) exchange.State {
	st := exchange.State{Accounts: map[string]wallet.Account{}, DepositedBRL: 123456789, DepositedVIB: 987654}
	for i := 0; i < accounts; i++ {
		st.Accounts[fmt.Sprintf("user-%d", i)] = wallet.Account{BRLFree: int64(i), BRLHeld: int64(2 * i), VIBFree: int64(3 * i), VIBHeld: int64(4 * i)}
	}
	for i := 0; i < orders; i++ {
		st.Book.Orders = append(st.Book.Orders, domain.Order{ID: fmt.Sprintf("o-%d", i), UserID: fmt.Sprintf("user-%d", i%max(accounts, 1)),
			Side: domain.Buy, Price: domain.Price(100 + i%50), Qty: 7, Filled: int64ToQty(i % 3), Status: domain.StatusOpen, Seq: uint64(i + 1)})
	}
	st.Book.Seq, st.Book.TradeSeq = uint64(orders+1), 77
	for i := 0; i < seen; i++ {
		st.Seen = append(st.Seen, fmt.Sprintf("lg100276201-%d", i))
	}
	return st
}

func int64ToQty(i int) domain.Qty { return domain.Qty(i) }

// El snapshot se escribe en streaming (sin json.Marshal del todo): este test garantiza que el JSON resultante
// representa EXACTAMENTE lo mismo que json.Marshal(File) y que Read lo devuelve idéntico. Si alguien agrega un
// campo a File o a exchange.State y olvida el escritor en streaming, aquí falla.
func TestStreamedJSONEqualsMarshal(t *testing.T) {
	for name, st := range map[string]exchange.State{
		"vacío (todo nil)":        {},
		"colecciones vacías":      {Accounts: map[string]wallet.Account{}, Book: engine.BookState{Orders: []domain.Order{}}, Seen: []string{}},
		"una sola entrada":        richState(1, 1, 1),
		"muchas entradas (>4096)": richState(9000, 12000, 20000),
		"sin órdenes ni cuentas":  richState(0, 0, 5000),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snap.bin")
			f := File{JournalID: "jid-é\"\\<&>", LastSeq: 1 << 40, JournalOffset: 1 << 35, State: st}
			if err := Write(path, f); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			var got any
			if err := json.Unmarshal(raw[8:], &got); err != nil {
				t.Fatalf("el JSON en streaming no es válido: %v", err)
			}
			f.V, f.TS = formatVersion, 0
			want, _ := json.Marshal(f)
			var wantAny any
			_ = json.Unmarshal(want, &wantAny)
			gotMap, wantMap := got.(map[string]any), wantAny.(map[string]any)
			delete(gotMap, "ts") // la marca de tiempo la fija Write
			delete(wantMap, "ts")
			if !reflect.DeepEqual(gotMap, wantMap) {
				t.Fatalf("el JSON en streaming difiere de json.Marshal:\n got=%.300v\nwant=%.300v", gotMap, wantMap)
			}
			back, err := Read(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.State, st) {
				t.Fatalf("Read no devolvió el mismo estado que se escribió (cuentas %d/%d, órdenes %d/%d, ids %d/%d)",
					len(back.State.Accounts), len(st.Accounts), len(back.State.Book.Orders), len(st.Book.Orders), len(back.State.Seen), len(st.Seen))
			}
		})
	}
}

// Regresión del hallazgo del soak: Write reservaba ≈ 4,4 veces el tamaño del archivo (json.Marshal de todo el estado
// + una copia para la cabecera), y eso cada 200.000 registros. En streaming la memoria temporal NO crece con el estado.
func TestWriteAllocationDoesNotScaleWithTheState(t *testing.T) {
	if raceEnabled {
		t.Skip("el detector de carreras altera la contabilidad de memoria; se comprueba sin -race")
	}
	st := richState(2000, 100_000, 400_000)
	path := filepath.Join(t.TempDir(), "snap.bin")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if err := Write(path, File{JournalID: "j", LastSeq: 1, State: st}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	fi, _ := os.Stat(path)
	alloc := int64(m1.TotalAlloc - m0.TotalAlloc)
	t.Logf("archivo=%d KiB, memoria reservada=%d KiB (%.2f×)", fi.Size()>>10, alloc>>10, float64(alloc)/float64(fi.Size()))
	if alloc > fi.Size()/2 { // medido: ≈ 0,2-0,3×; la versión anterior reservaba 4,4×
		t.Fatalf("Write reservó %d KiB para un archivo de %d KiB: debería ser una fracción pequeña (antes: 4,4×)", alloc>>10, fi.Size()>>10)
	}
}
