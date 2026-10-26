package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
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
