package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
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
