// Package api expone el exchange por HTTP/JSON. Es stateless: toda la lógica vive en el
// secuenciador, así que se pueden correr N réplicas de la API detrás de un balanceador.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

const (
	requestTimeout = 2 * time.Second
	maxBodyBytes   = 1 << 20
)

type API struct {
	seq *sequencer.Sequencer
}

func New(seq *sequencer.Sequencer) *API { return &API{seq: seq} }

// Handler registra las rutas (patrones de método+ruta de Go 1.22).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", a.placeOrder)
	mux.HandleFunc("DELETE /orders/{id}", a.cancelOrder)
	mux.HandleFunc("GET /balances/{user}", a.balance)
	mux.HandleFunc("POST /deposits", a.deposit)
	mux.HandleFunc("GET /book", a.book)
	mux.HandleFunc("GET /audit", a.audit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// --- DTOs (formato público; el dominio interno no se expone directamente) ---

type orderRequest struct {
	ID     string `json:"id"` // opcional: si falta se genera. Enviarlo habilita reintentos idempotentes.
	UserID string `json:"user_id"`
	Side   string `json:"side"`  // "BUY" | "SELL"
	Price  int64  `json:"price"` // centavos de real por unidad de Vibranium
	Qty    int64  `json:"qty"`   // unidades de Vibranium
}

type orderView struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Side      string `json:"side"`
	Price     int64  `json:"price"`
	Qty       int64  `json:"qty"`
	Filled    int64  `json:"filled"`
	Remaining int64  `json:"remaining"`
	Status    string `json:"status"`
}

type tradeView struct {
	ID          uint64 `json:"id"`
	Price       int64  `json:"price"`
	Qty         int64  `json:"qty"`
	Total       int64  `json:"total"`
	BuyOrderID  string `json:"buy_order_id"`
	SellOrderID string `json:"sell_order_id"`
	BuyUserID   string `json:"buy_user_id"`
	SellUserID  string `json:"sell_user_id"`
	MakerOrder  string `json:"maker_order_id"`
	TakerOrder  string `json:"taker_order_id"`
}

type orderResponse struct {
	Order  orderView   `json:"order"`
	Trades []tradeView `json:"trades"`
}

func toOrderView(o domain.Order) orderView {
	return orderView{ID: o.ID, UserID: o.UserID, Side: o.Side.String(), Price: int64(o.Price),
		Qty: int64(o.Qty), Filled: int64(o.Filled), Remaining: int64(o.Remaining()), Status: o.Status.String()}
}

func toTradeViews(ts []domain.Trade) []tradeView {
	out := make([]tradeView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tradeView{ID: t.ID, Price: int64(t.Price), Qty: int64(t.Qty), Total: t.Total(),
			BuyOrderID: t.BuyOrderID, SellOrderID: t.SellOrderID, BuyUserID: t.BuyUserID,
			SellUserID: t.SellUserID, MakerOrder: t.MakerOrderID, TakerOrder: t.TakerOrderID})
	}
	return out
}

// --- Handlers ---

func (a *API) placeOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if !decode(w, r, &req) {
		return
	}
	var side domain.Side
	switch req.Side {
	case "BUY":
		side = domain.Buy
	case "SELL":
		side = domain.Sell
	default:
		writeError(w, http.StatusBadRequest, "invalid_side", "side debe ser BUY o SELL")
		return
	}
	if req.ID == "" {
		req.ID = newID()
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	res, err := a.seq.PlaceOrder(ctx, domain.Order{
		ID: req.ID, UserID: req.UserID, Side: side,
		Price: domain.Price(req.Price), Qty: domain.Qty(req.Qty),
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, orderResponse{Order: toOrderView(res.Order), Trades: toTradeViews(res.Trades)})
}

func (a *API) cancelOrder(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	o, err := a.seq.Cancel(ctx, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toOrderView(o))
}

type balanceResponse struct {
	UserID  string `json:"user_id"`
	BRLFree int64  `json:"brl_free"`
	BRLHeld int64  `json:"brl_held"`
	VIBFree int64  `json:"vib_free"`
	VIBHeld int64  `json:"vib_held"`
}

func (a *API) balance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	user := r.PathValue("user")
	acc, err := a.seq.Balance(ctx, user)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{UserID: user, BRLFree: acc.BRLFree, BRLHeld: acc.BRLHeld,
		VIBFree: acc.VIBFree, VIBHeld: acc.VIBHeld})
}

type depositRequest struct {
	UserID string `json:"user_id"`
	BRL    int64  `json:"brl"` // centavos
	VIB    int64  `json:"vib"` // unidades
}

func (a *API) deposit(w http.ResponseWriter, r *http.Request) {
	var req depositRequest
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := a.seq.Deposit(ctx, req.UserID, req.BRL, req.VIB); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type levelView struct {
	Price int64 `json:"price"`
	Qty   int64 `json:"qty"`
}

func (a *API) book(w http.ResponseWriter, r *http.Request) {
	depth := 10
	if v := r.URL.Query().Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid_depth", "depth debe estar entre 1 y 100")
			return
		}
		depth = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	snap, err := a.seq.Depth(ctx, depth)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	conv := func(ls []engine.Level) []levelView {
		out := make([]levelView, 0, len(ls))
		for _, l := range ls {
			out = append(out, levelView{Price: int64(l.Price), Qty: int64(l.Qty)})
		}
		return out
	}
	writeJSON(w, http.StatusOK, map[string]any{"bids": conv(snap.Bids), "asks": conv(snap.Asks)})
}

func (a *API) audit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second) // recorre todo el sistema
	defer cancel()
	rep, err := a.seq.Audit(ctx)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusOK
	if !rep.OK {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"ok": rep.OK, "error": rep.Error,
		"total_brl": rep.TotalBRL, "total_vib": rep.TotalVIB})
}

// --- Utilidades ---

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrInvalidOrder), errors.Is(err, wallet.ErrInvalidAmount):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, wallet.ErrInsufficientFunds):
		writeError(w, http.StatusUnprocessableEntity, "insufficient_funds", err.Error())
	case errors.Is(err, exchange.ErrDuplicateOrder):
		writeError(w, http.StatusConflict, "duplicate_order", err.Error())
	case errors.Is(err, engine.ErrOrderNotFound):
		writeError(w, http.StatusNotFound, "order_not_found", err.Error())
	case errors.Is(err, sequencer.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "overloaded", err.Error())
	case errors.Is(err, sequencer.ErrStopped):
		writeError(w, http.StatusServiceUnavailable, "shutting_down", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "timeout", "tiempo de espera agotado; consulte el estado antes de reintentar")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "error interno")
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "cuerpo JSON inválido")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
