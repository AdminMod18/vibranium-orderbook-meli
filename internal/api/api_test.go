package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/metrics"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
)

func newServer(t *testing.T, queue int) (*httptest.Server, *sequencer.Sequencer) {
	t.Helper()
	seq := sequencer.New(exchange.New(), queue)
	srv := httptest.NewServer(New(seq).Handler())
	t.Cleanup(func() { srv.Close(); seq.Stop() })
	return srv, seq
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestEndToEndTrade(t *testing.T) {
	srv, _ := newServer(t, 100)
	call(t, srv, "POST", "/deposits", `{"user_id":"alice","brl":100000}`)
	call(t, srv, "POST", "/deposits", `{"user_id":"bob","vib":50}`)

	code, _ := call(t, srv, "POST", "/orders", `{"id":"s1","user_id":"bob","side":"SELL","price":900,"qty":10}`)
	if code != http.StatusCreated {
		t.Fatalf("venta: %d", code)
	}
	code, body := call(t, srv, "POST", "/orders", `{"id":"b1","user_id":"alice","side":"BUY","price":1000,"qty":10}`)
	if code != http.StatusCreated {
		t.Fatalf("compra: %d %v", code, body)
	}
	trades := body["trades"].([]any)
	if len(trades) != 1 || trades[0].(map[string]any)["price"].(float64) != 900 {
		t.Fatalf("trade esperado a 900: %v", body)
	}

	_, bal := call(t, srv, "GET", "/balances/alice", "")
	if bal["vib_free"].(float64) != 10 || bal["brl_free"].(float64) != 100000-9000 {
		t.Fatalf("saldo alice: %v", bal)
	}
	_, audit := call(t, srv, "GET", "/audit", "")
	if audit["ok"] != true {
		t.Fatalf("auditoría: %v", audit)
	}
}

func TestErrorMapping(t *testing.T) {
	srv, _ := newServer(t, 100)
	call(t, srv, "POST", "/deposits", `{"user_id":"alice","brl":1000}`)

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"json inválido", "POST", "/orders", `{`, 400},
		{"campo desconocido", "POST", "/orders", `{"x":1}`, 400},
		{"lado inválido", "POST", "/orders", `{"user_id":"a","side":"HOLD","price":1,"qty":1}`, 400},
		{"cantidad inválida", "POST", "/orders", `{"user_id":"alice","side":"BUY","price":10,"qty":0}`, 400},
		{"sin saldo", "POST", "/orders", `{"user_id":"alice","side":"BUY","price":10,"qty":999}`, 422},
		{"cancelar inexistente", "DELETE", "/orders/nope", ``, 404},
		{"depth inválido", "GET", "/book?depth=0", ``, 400},
	}
	for _, c := range cases {
		if code, _ := call(t, srv, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s: got %d want %d", c.name, code, c.want)
		}
	}

	ok := `{"id":"dup","user_id":"alice","side":"BUY","price":10,"qty":1}`
	if code, _ := call(t, srv, "POST", "/orders", ok); code != 201 {
		t.Fatalf("primera: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/orders", ok); code != 409 {
		t.Fatalf("reintento debe ser 409, got %d", code)
	}
}

func TestCancelFlow(t *testing.T) {
	srv, _ := newServer(t, 100)
	call(t, srv, "POST", "/deposits", `{"user_id":"alice","brl":1000}`)
	call(t, srv, "POST", "/orders", `{"id":"b1","user_id":"alice","side":"BUY","price":10,"qty":5}`)
	if code, body := call(t, srv, "DELETE", "/orders/b1", ""); code != 200 || body["status"] != "CANCELLED" {
		t.Fatalf("cancel: %d %v", code, body)
	}
	_, bal := call(t, srv, "GET", "/balances/alice", "")
	if bal["brl_free"].(float64) != 1000 || bal["brl_held"].(float64) != 0 {
		t.Fatalf("debe liberar la reserva: %v", bal)
	}
}

func TestHistoryEndpointsReturn501WhenDisabled(t *testing.T) {
	srv, _ := newServer(t, 100)
	for _, path := range []string{"/orders/x", "/orders", "/trades", "/audit/history/alice"} {
		if code, _ := call(t, srv, "GET", path, ""); code != http.StatusNotImplemented {
			t.Errorf("%s: got %d want 501", path, code)
		}
	}
}

func TestMetricsReflectTrafficWithBoundedCardinality(t *testing.T) {
	met := metrics.New("test")
	seq := sequencer.New(exchange.New(), 100, sequencer.WithObserver(met))
	srv := httptest.NewServer(New(seq).WithMetrics(met).Handler())
	defer func() { srv.Close(); seq.Stop() }()

	call(t, srv, "POST", "/deposits", `{"user_id":"alice","brl":100000}`)
	call(t, srv, "POST", "/deposits", `{"user_id":"bob","vib":50}`)
	call(t, srv, "POST", "/orders", `{"id":"s1","user_id":"bob","side":"SELL","price":900,"qty":4}`)
	call(t, srv, "POST", "/orders", `{"id":"b1","user_id":"alice","side":"BUY","price":1000,"qty":10}`)
	call(t, srv, "POST", "/orders", `{"id":"b1","user_id":"alice","side":"BUY","price":1,"qty":1}`)       // duplicada
	call(t, srv, "POST", "/orders", `{"id":"x","user_id":"alice","side":"BUY","price":1000000,"qty":99}`) // sin saldo
	for i := 0; i < 50; i++ {                                                                             // 50 ids distintos
		call(t, srv, "GET", fmt.Sprintf("/balances/user-%d", i), "")
	}

	rec := httptest.NewRecorder()
	met.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		`orderbook_orders_total{result="accepted",side="BUY"} 1`,
		`orderbook_orders_total{result="accepted",side="SELL"} 1`,
		`orderbook_orders_total{result="duplicate",side="BUY"} 1`,
		`orderbook_orders_total{result="insufficient_funds",side="BUY"} 1`,
		`orderbook_trades_total 1`,
		`orderbook_traded_units_total 4`,
		`orderbook_traded_value_cents_total 3600`,
		`orderbook_http_requests_total{code="409",method="POST",route="POST /orders"} 1`,
		`orderbook_http_requests_total{code="422",method="POST",route="POST /orders"} 1`,
		`orderbook_batch_size_count`,
		`orderbook_queue_wait_seconds_count`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("falta la métrica %q", want)
		}
	}
	// 50 usuarios distintos deben producir UNA sola serie de ruta, no 50.
	if n := strings.Count(out, `route="GET /balances/{user}"`); n > 20 { // histograma + contador: pocas líneas fijas
		t.Errorf("cardinalidad no acotada: %d líneas para /balances/{user}", n)
	}
	if strings.Contains(out, "user-7") {
		t.Error("el id de usuario no debe aparecer en las etiquetas")
	}
}

// Regresión del defecto bloqueante de la auditoría: una sola petición con un id de 400 KB tumbaba TODO el
// proceso (panic al escribir el journal). Ahora se rechaza con 400 y el servicio sigue funcionando.
func TestHugeIdentifiersAreRejectedAndServiceKeepsWorking(t *testing.T) {
	srv, _ := newServer(t, 100)
	call(t, srv, "POST", "/deposits", `{"user_id":"alice","brl":1000000}`)
	call(t, srv, "POST", "/deposits", `{"user_id":"bob","vib":100}`)
	call(t, srv, "POST", "/orders", `{"id":"s1","user_id":"bob","side":"SELL","price":10,"qty":5}`)

	huge := strings.Repeat("X", 400_000)
	for name, body := range map[string]string{
		"orden que cruza con id de 400 KB": fmt.Sprintf(`{"id":%q,"user_id":"alice","side":"BUY","price":10,"qty":5}`, huge),
		"user_id de 400 KB":                fmt.Sprintf(`{"id":"x1","user_id":%q,"side":"BUY","price":10,"qty":1}`, huge),
		"id con caracteres no permitidos":  `{"id":"a b/c","user_id":"alice","side":"BUY","price":10,"qty":1}`,
	} {
		code, resp := call(t, srv, "POST", "/orders", body)
		if code != http.StatusBadRequest || resp["code"] != "invalid_id" {
			t.Errorf("%s: esperaba 400 invalid_id, got %d %v", name, code, resp)
		}
	}
	if code, _ := call(t, srv, "POST", "/deposits", fmt.Sprintf(`{"user_id":%q,"brl":1}`, huge)); code != http.StatusBadRequest {
		t.Errorf("depósito con user_id enorme: got %d", code)
	}
	// el servicio sigue vivo y operando con normalidad
	if code, body := call(t, srv, "POST", "/orders", `{"id":"b1","user_id":"alice","side":"BUY","price":10,"qty":5}`); code != 201 || len(body["trades"].([]any)) != 1 {
		t.Fatalf("tras los rechazos el servicio debe seguir operando: %d %v", code, body)
	}
	if _, audit := call(t, srv, "GET", "/audit", ""); audit["ok"] != true {
		t.Fatalf("auditoría: %v", audit)
	}
}

func TestOversizedRequestIDHeaderIsReplaced(t *testing.T) {
	srv, _ := newServer(t, 10)
	req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-ID", strings.Repeat("Z", 5000))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); len(got) == 0 || len(got) > 64 || strings.Contains(got, "Z") {
		t.Fatalf("el X-Request-ID enorme debe sustituirse por uno propio: %q", got)
	}
	req.Header.Set("X-Request-ID", "mi-traza-123")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("X-Request-ID") != "mi-traza-123" {
		t.Fatal("un X-Request-ID válido debe conservarse")
	}
}
