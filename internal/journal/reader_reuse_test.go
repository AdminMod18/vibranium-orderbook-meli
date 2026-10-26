package journal

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
)

func domainOrder(id string) domain.Order {
	return domain.Order{ID: id, UserID: "u", Side: domain.Sell, Price: 11, Qty: 5, Filled: 2, Status: domain.StatusCancelled}
}

// El lector reutiliza su bufio y su buffer de payload entre llamadas. Con registros de tamaños muy distintos
// (chicos, grandes y otra vez chicos) y lecturas de a 1 o a varios, cada Record debe llegar íntegro: si algún
// campo apuntara al buffer compartido, el siguiente registro lo pisaría.
func TestReaderBufferReuseDoesNotCorruptRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	j, _, _, err := recoverAll(t, path)
	if err != nil {
		t.Fatal(err)
	}
	var want []Record
	for i := 0; i < 60; i++ {
		var rec *Record
		switch {
		case i%7 == 3: // registro grande: una orden con muchos trades
			trades := make([]TradeRec, 300)
			for k := range trades {
				trades[k] = TradeRec{ID: uint64(k), Price: 10, Qty: 1, BuyOrderID: fmt.Sprintf("b%d-%d", i, k), SellOrderID: strings.Repeat("s", 60),
					BuyUserID: "ub", SellUserID: "us", MakerOrderID: "m", TakerOrderID: "t"}
			}
			rec = &Record{Kind: KindOrder, Order: &OrderRec{ID: fmt.Sprintf("big-%d", i), UserID: "u", Side: "BUY", Price: 10, Qty: 300, Status: "FILLED"}, Trades: trades}
		case i%7 == 5:
			rec = NewCancelRecord(domainOrder(fmt.Sprintf("c-%d", i)))
		default:
			rec = NewDepositRecord(fmt.Sprintf("user-%d", i), int64(i+1), int64(i))
		}
		if err := j.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	j.Sync()
	j.Close()

	// referencia: lectura con buffers nuevos (recuperación)
	if _, _, err := Recover(path, Options{}, func(r Record) error { want = append(want, r); return nil }); err != nil {
		t.Fatal(err)
	}

	rd, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var got []Record
	for _, limit := range []int{1, 3, 1, 50, 0} {
		if _, err := rd.ReadNew(limit, func(r Record) error { got = append(got, r); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("leyó %d registros, esperaba %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("el registro %d cambió al reutilizar buffers:\n got=%+v\nwant=%+v", i, got[i], want[i])
		}
	}
}

// Regresión del hallazgo del perfil de memoria: cada ReadNew reservaba un bufio de 1 MiB nuevo, incluso para no
// leer nada nuevo. El proyector llama a ReadNew decenas de veces por segundo: era el 49 % de toda la basura del
// servidor y ~20 ciclos de GC por minuto. Ahora el costo por llamada es una fracción de KiB.
func TestReadNewDoesNotAllocateAMebibytePerCall(t *testing.T) {
	if raceEnabled {
		t.Skip("el detector de carreras altera la contabilidad de memoria")
	}
	path := filepath.Join(t.TempDir(), "j.log")
	j, _, _, _ := recoverAll(t, path)
	for i := 0; i < 10; i++ {
		j.Append(NewDepositRecord("u", 1, 0))
	}
	j.Sync()
	rd, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	read := func() {
		if _, err := rd.ReadNew(5000, func(Record) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	read() // la primera lectura reserva los buffers
	const calls = 200
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for i := 0; i < calls; i++ {
		j.Append(NewDepositRecord("u", 1, 0))
		j.Sync()
		read()
	}
	runtime.ReadMemStats(&m1)
	perCall := (m1.TotalAlloc - m0.TotalAlloc) / calls
	t.Logf("memoria reservada por ReadNew (con 1 registro nuevo): %d B", perCall)
	if perCall > 32<<10 {
		t.Fatalf("ReadNew reservó %d B por llamada: no debe volver a reservar el buffer de 1 MiB", perCall)
	}
	j.Close()
}
