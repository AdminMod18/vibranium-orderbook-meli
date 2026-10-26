package journal

import (
	"errors"
	"fmt"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/domain"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
)

// ErrDiverged indica que reproducir un registro dio un resultado distinto al registrado:
// el motor dejó de ser determinista (bug grave). Es mejor detenerse que operar así.
var ErrDiverged = errors.New("la reproducción divergió del log")

func SideString(s domain.Side) string { return s.String() }

func parseSide(s string) (domain.Side, error) {
	switch s {
	case "BUY":
		return domain.Buy, nil
	case "SELL":
		return domain.Sell, nil
	}
	return 0, fmt.Errorf("side desconocido %q", s)
}

// NewOrderRecord arma el registro de una orden procesada.
func NewOrderRecord(in domain.Order, res engine.Result) *Record {
	rec := &Record{Kind: KindOrder, Order: &OrderRec{
		ID: in.ID, UserID: in.UserID, Side: in.Side.String(), Price: int64(in.Price), Qty: int64(in.Qty),
		Filled: int64(res.Order.Filled), Status: res.Order.Status.String(),
	}}
	for _, t := range res.Trades {
		rec.Trades = append(rec.Trades, TradeRec{
			ID: t.ID, Price: int64(t.Price), Qty: int64(t.Qty),
			BuyOrderID: t.BuyOrderID, SellOrderID: t.SellOrderID, BuyUserID: t.BuyUserID, SellUserID: t.SellUserID,
			MakerOrderID: t.MakerOrderID, TakerOrderID: t.TakerOrderID,
		})
	}
	return rec
}

func NewCancelRecord(o domain.Order) *Record {
	return &Record{Kind: KindCancel, OrderID: o.ID, Order: &OrderRec{
		ID: o.ID, UserID: o.UserID, Side: o.Side.String(), Price: int64(o.Price), Qty: int64(o.Qty),
		Filled: int64(o.Filled), Status: o.Status.String(),
	}}
}

func NewDepositRecord(user string, brl, vib int64) *Record {
	return &Record{Kind: KindDeposit, User: user, BRL: brl, VIB: vib}
}

// Apply re-ejecuta un registro sobre el exchange y verifica que el resultado coincida.
func Apply(x *exchange.Exchange, rec Record) error {
	switch rec.Kind {
	case KindGenesis:
		return nil // solo identifica al journal; no cambia el estado

	case KindDeposit:
		return x.Deposit(rec.User, rec.BRL, rec.VIB)

	case KindOrder:
		side, err := parseSide(rec.Order.Side)
		if err != nil {
			return err
		}
		res, err := x.PlaceOrder(domain.Order{
			ID: rec.Order.ID, UserID: rec.Order.UserID, Side: side,
			Price: domain.Price(rec.Order.Price), Qty: domain.Qty(rec.Order.Qty),
		})
		if err != nil {
			return fmt.Errorf("%w: seq %d orden %s: %v", ErrDiverged, rec.Seq, rec.Order.ID, err)
		}
		if len(res.Trades) != len(rec.Trades) || int64(res.Order.Filled) != rec.Order.Filled {
			return fmt.Errorf("%w: seq %d orden %s: trades/filled distintos", ErrDiverged, rec.Seq, rec.Order.ID)
		}
		for i, t := range res.Trades {
			w := rec.Trades[i]
			if t.ID != w.ID || int64(t.Price) != w.Price || int64(t.Qty) != w.Qty ||
				t.MakerOrderID != w.MakerOrderID || t.TakerOrderID != w.TakerOrderID {
				return fmt.Errorf("%w: seq %d trade %d distinto", ErrDiverged, rec.Seq, t.ID)
			}
		}
		return nil

	case KindCancel:
		if _, err := x.Cancel(rec.OrderID); err != nil {
			return fmt.Errorf("%w: seq %d cancelar %s: %v", ErrDiverged, rec.Seq, rec.OrderID, err)
		}
		return nil
	}
	return fmt.Errorf("%w: tipo de registro desconocido %q", ErrCorrupt, rec.Kind)
}
