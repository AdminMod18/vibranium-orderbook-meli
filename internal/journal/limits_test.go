package journal

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
)

// Garantía, no anécdota: el registro MÁS GRANDE que el sistema puede producir (una orden que ejecuta el
// tope de trades, con ids del largo máximo y valores máximos) cabe en maxRecordSize. Si alguien sube
// MaxTradesPerOrder o MaxIDLen sin subir el límite del journal, este test falla en vez de dejar un
// panic latente en producción (la falla que encontró la auditoría: un registro de 1,2-2 MB).
func TestWorstCaseRecordFitsTheLimit(t *testing.T) {
	id := strings.Repeat("x", domain.MaxIDLen)
	rec := Record{V: formatVersion, Seq: ^uint64(0), TS: 1 << 62, Kind: KindOrder,
		Order: &OrderRec{ID: id, UserID: id, Side: "SELL", Price: int64(engine.MaxPrice), Qty: int64(engine.MaxQty),
			Filled: int64(engine.MaxQty), Status: "PARTIALLY_FILLED"}}
	for i := 0; i < engine.MaxTradesPerOrder; i++ {
		rec.Trades = append(rec.Trades, TradeRec{ID: ^uint64(0), Price: int64(engine.MaxPrice), Qty: int64(engine.MaxQty),
			BuyOrderID: id, SellOrderID: id, BuyUserID: id, SellUserID: id, MakerOrderID: id, TakerOrderID: id})
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("peor caso: %d trades -> registro de %.1f MiB (límite %d MiB)", engine.MaxTradesPerOrder, float64(len(b))/(1<<20), maxRecordSize>>20)
	if len(b) >= maxRecordSize {
		t.Fatalf("el peor registro (%d bytes) no cabe en maxRecordSize (%d)", len(b), maxRecordSize)
	}
}
