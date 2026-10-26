package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string, n int) {
	t.Helper()
	j, _, err := Recover(path, Options{}, func(Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := j.Append(NewDepositRecord("u", int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

func recoverAll(t *testing.T, path string) (*Journal, ReplayStats, []Record, error) {
	t.Helper()
	var got []Record
	j, st, err := Recover(path, Options{}, func(r Record) error { got = append(got, r); return nil })
	return j, st, got, err
}

func TestRoundTripAndContinue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	write(t, path, 100)

	j, st, got, err := recoverAll(t, path)
	if err != nil || st.Records != 101 || st.LastSeq != 101 || st.TruncatedBytes != 0 { // 100 + genesis
		t.Fatalf("recover: %+v %v", st, err)
	}
	if got[0].Kind != KindGenesis || got[0].JournalID == "" {
		t.Fatalf("el primer registro debe ser genesis: %+v", got[0])
	}
	for i, r := range got[1:] {
		if r.Seq != uint64(i+2) || r.BRL != int64(i+1) {
			t.Fatalf("registro %d fuera de orden: %+v", i, r)
		}
	}
	if err := j.Append(NewDepositRecord("u", 1, 0)); err != nil || j.NextSeq() != 103 {
		t.Fatalf("la secuencia debe continuar en 102: next=%d err=%v", j.NextSeq(), err)
	}
	j.Close()
}

func TestTornTailIsDiscardedAndWritingResumes(t *testing.T) {
	for _, cut := range []int64{1, 5, 9, 20} { // cortes dentro de la cabecera o del payload del último registro
		path := filepath.Join(t.TempDir(), "j.log")
		write(t, path, 10)
		fi, _ := os.Stat(path)
		if err := os.Truncate(path, fi.Size()-cut); err != nil {
			t.Fatal(err)
		}

		j, st, got, err := recoverAll(t, path)
		if err != nil || len(got) != 10 || st.TruncatedBytes == 0 { // genesis + 9
			t.Fatalf("corte %d: esperaba 10 registros y cola descartada: %+v %v", cut, st, err)
		}
		if err := j.Append(NewDepositRecord("u", 99, 0)); err != nil {
			t.Fatal(err)
		}
		j.Close()

		_, st2, got2, err := recoverAll(t, path)
		if err != nil || len(got2) != 11 || got2[10].BRL != 99 || st2.TruncatedBytes != 0 {
			t.Fatalf("corte %d: tras reanudar debe haber 11 registros limpios: %+v %v", cut, st2, err)
		}
	}
}

func TestBadCRCOnLastRecordIsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	write(t, path, 5)
	b, _ := os.ReadFile(path)
	b[len(b)-1] ^= 0xFF
	os.WriteFile(path, b, 0o644)

	_, _, got, err := recoverAll(t, path)
	if err != nil || len(got) != 5 { // genesis + 4
		t.Fatalf("got %d registros, err %v", len(got), err)
	}
}

func TestCorruptionInTheMiddleIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	write(t, path, 5)
	b, _ := os.ReadFile(path)
	b[20] ^= 0xFF // dentro del primer registro, con más datos después
	os.WriteFile(path, b, 0o644)

	if _, _, _, err := recoverAll(t, path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("debe detectarse corrupción real y NO descartar datos en silencio, got %v", err)
	}
}

func TestReaderTailsIncrementally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	j, _, _, _ := recoverAll(t, path)
	rd, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	count := func() int {
		n, err := rd.ReadNew(0, func(Record) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("un journal nuevo contiene solo el registro genesis")
	}
	for i := 0; i < 3; i++ {
		j.Append(NewDepositRecord("u", 1, 0))
	}
	if count() != 0 {
		t.Fatal("lo no sincronizado no debe ser visible")
	}
	j.Sync()
	if count() != 3 {
		t.Fatal("debe leer 3 tras sync")
	}
	j.Append(NewDepositRecord("u", 1, 0))
	j.Sync()
	if count() != 1 {
		t.Fatal("solo el registro nuevo")
	}
	j.Close()
}
