package wallet

// Capture devuelve una copia de todas las billeteras.
func (l *Ledger) Capture() map[string]Account {
	out := make(map[string]Account, len(l.accounts))
	for u, a := range l.accounts {
		out[u] = *a
	}
	return out
}

// RestoreLedger reconstruye el Ledger desde una copia.
func RestoreLedger(accounts map[string]Account) *Ledger {
	l := NewLedger()
	for u, a := range accounts {
		acc := a
		l.accounts[u] = &acc
	}
	return l
}
