package exchange

import (
	"fmt"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/engine"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/wallet"
)

// State es una copia completa y consistente del exchange (billeteras, libro e ids vistos).
type State struct {
	Accounts map[string]wallet.Account
	Book     engine.BookState
	Seen     []string

	DepositedBRL, DepositedVIB int64 // para verificar la conservación al restaurar
}

// Capture copia el estado. Es una fotografía consistente solo si se invoca desde el goroutine
// secuenciador, entre dos comandos; la copia puede escribirse a disco luego, en otro goroutine.
func (x *Exchange) Capture() State {
	seen := make([]string, 0, len(x.seen))
	for id := range x.seen {
		seen = append(seen, id)
	}
	return State{Accounts: x.ledger.Capture(), Book: x.book.Capture(), Seen: seen,
		DepositedBRL: x.depBRL, DepositedVIB: x.depVIB}
}

// Restore reconstruye un Exchange idéntico al capturado y verifica sus invariantes
// (un snapshot corrupto o inconsistente se rechaza en vez de usarse).
func Restore(s State) (*Exchange, error) {
	book, err := engine.RestoreBook(s.Book)
	if err != nil {
		return nil, err
	}
	x := &Exchange{book: book, ledger: wallet.RestoreLedger(s.Accounts), seen: make(map[string]struct{}, len(s.Seen)),
		depBRL: s.DepositedBRL, depVIB: s.DepositedVIB}
	for _, id := range s.Seen {
		x.seen[id] = struct{}{}
	}
	for _, o := range book.RestingOrders() { // toda orden en el libro debe figurar como vista
		if _, ok := x.seen[o.ID]; !ok {
			return nil, fmt.Errorf("orden %q en el libro pero no en los ids vistos", o.ID)
		}
	}
	if err := x.CheckInvariants(); err != nil {
		return nil, fmt.Errorf("snapshot inconsistente: %w", err)
	}
	return x, nil
}
