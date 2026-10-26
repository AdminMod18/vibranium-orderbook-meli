package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) (*Journal, error) {
	t.Helper()
	j, _, err := Recover(path, Options{}, func(Record) error { return nil })
	return j, err
}

func TestSecondInstanceCannotOpenTheSameJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	a, err := open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, path); !errors.Is(err, ErrLocked) {
		t.Fatalf("la segunda instancia debe recibir ErrLocked, got %v", err)
	}
	// la primera no se ve afectada y puede seguir escribiendo
	if err := a.Append(NewDepositRecord("u", 1, 0)); err != nil || a.Sync() != nil {
		t.Fatalf("la instancia dueña debe seguir operando: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := open(t, path) // tras cerrar, el candado queda libre
	if err != nil {
		t.Fatalf("tras Close debe poder abrirse: %v", err)
	}
	b.Close()
}

// Un proceso que muere (kill -9) no puede dejar un candado huérfano: el sistema operativo lo libera.
func TestAbandonReleasesTheLockLikeAProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	a, _ := open(t, path)
	_ = a.Append(NewDepositRecord("u", 1, 0))
	_ = a.Sync()
	a.Abandon()
	b, err := open(t, path)
	if err != nil {
		t.Fatalf("tras la 'muerte' del dueño el journal debe poder abrirse: %v", err)
	}
	defer b.Close()
}

// Un arranque que FALLA (journal corrupto) no debe quedarse con el candado tomado.
func TestFailedRecoveryDoesNotKeepTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	a, _ := open(t, path)
	for i := 0; i < 4; i++ {
		_ = a.Append(NewDepositRecord("u", int64(i+1), 0))
	}
	_ = a.Close()
	b, _ := os.ReadFile(path)
	b[20] ^= 0xFF // corrupción real en medio del log
	_ = os.WriteFile(path, b, 0o644)

	for i := 0; i < 2; i++ { // si el candado quedara tomado, el segundo intento daría ErrLocked
		if _, err := open(t, path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("intento %d: esperaba ErrCorrupt (y no ErrLocked), got %v", i, err)
		}
	}
}

// R2: el lector solo ve lo que está SINCRONIZADO. Entre el Flush y el fsync el archivo ya contiene los
// bytes (en la caché del sistema), pero un corte de energía podría borrarlos.
func TestReaderNeverSeesBeyondTheDurableOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	j, _ := open(t, path)
	offsets := []int64{j.DurableBytes()} // tras el genesis
	for i := 0; i < 3; i++ {
		if err := j.Append(NewDepositRecord("u", int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
		if j.DurableBytes() >= j.Bytes() {
			t.Fatal("antes del Sync, lo agregado NO es durable")
		}
		if err := j.Sync(); err != nil {
			t.Fatal(err)
		}
		if j.DurableBytes() != j.Bytes() {
			t.Fatalf("tras el Sync todo es durable: %d vs %d", j.DurableBytes(), j.Bytes())
		}
		offsets = append(offsets, j.DurableBytes())
	}
	defer j.Close()

	// El archivo ya tiene los 4 registros (genesis + 3), pero el "offset durable" solo cubre los primeros 3.
	limit := offsets[2]
	rd, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	rd.SetDurable(func() int64 { return limit })
	count := func() int {
		n, err := rd.ReadNew(0, func(Record) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 { // genesis + 2 depósitos
		t.Fatalf("con el límite en el 3er registro debe ver 3, vio %d", n)
	}
	if n := count(); n != 0 {
		t.Fatalf("no debe ver nada más allá del límite, vio %d", n)
	}
	limit = offsets[3] // el fsync del último registro "termina"
	if n := count(); n != 1 {
		t.Fatalf("al avanzar el límite debe ver el registro restante, vio %d", n)
	}
}
