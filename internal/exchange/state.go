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
	Seen     []string // ventana de idempotencia, del id más antiguo al más nuevo

	DepositedBRL, DepositedVIB int64 // para verificar la conservación al restaurar
}

// Capture copia el estado. Es una fotografía consistente solo si se invoca desde el goroutine
// secuenciador, entre dos comandos; la copia puede escribirse a disco luego, en otro goroutine.
func (x *Exchange) Capture() State {
	return State{Accounts: x.ledger.Capture(), Book: x.book.Capture(), Seen: x.seen.items(),
		DepositedBRL: x.depBRL, DepositedVIB: x.depVIB}
}

// Restore reconstruye un Exchange idéntico al capturado y verifica sus invariantes
// (un snapshot corrupto o inconsistente se rechaza en vez de usarse).
// window es el tamaño de la ventana de idempotencia con que seguirá operando (puede diferir del que
// tenía al capturarse: si es menor se descartan los ids más antiguos).
func Restore(s State, window int) (*Exchange, error) {
	book, err := engine.RestoreBook(s.Book)
	if err != nil {
		return nil, err
	}
	x := &Exchange{book: book, ledger: wallet.RestoreLedger(s.Accounts), seen: newIDWindow(window),
		depBRL: s.DepositedBRL, depVIB: s.DepositedVIB}
	for _, id := range s.Seen {
		x.seen.add(id)
	}
	if err := x.CheckInvariants(); err != nil {
		return nil, fmt.Errorf("snapshot inconsistente: %w", err)
	}
	return x, nil
}
