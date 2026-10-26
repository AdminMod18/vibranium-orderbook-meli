// Package recovery arranca el sistema: reconstruye el exchange y reabre el journal.
//
// Estrategia: snapshot + cola. Se carga el último snapshot válido y solo se reproduce lo que el
// journal tiene DESPUÉS de él. Si el snapshot no sirve por cualquier motivo, se reproduce todo el
// journal desde el inicio: el snapshot acelera, pero nunca es imprescindible ni fuente de verdad.
package recovery

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/snapshot"
)

// Report resume cómo se recuperó el sistema (para logs y métricas).
type Report struct {
	FromSnapshot   bool
	SnapshotSeq    uint64
	SnapshotReason string // por qué NO se usó el snapshot, si existía
	Replayed       int    // registros reproducidos del journal
	LastSeq        uint64
	TruncatedBytes int64
	Took           time.Duration
}

// Recover devuelve el exchange reconstruido y el journal listo para escribir.
// snapPath vacío desactiva los snapshots. window es el tamaño de la ventana de idempotencia (0 = sin límite).
func Recover(journalPath, snapPath string, opts journal.Options, window int, log *slog.Logger) (*exchange.Exchange, *journal.Journal, Report, error) {
	t0 := time.Now()
	var rep Report

	if snapPath != "" {
		ex, jr, r, reason := tryWithSnapshot(journalPath, snapPath, opts, window)
		if reason == "" {
			r.Took = time.Since(t0)
			return ex, jr, r, nil
		}
		rep.SnapshotReason = reason
		if reason != snapshot.ErrNone.Error() { // que no exista snapshot no es una anomalía
			log.Warn("snapshot no utilizable: se reproduce el journal completo", "motivo", reason)
		}
	}

	ex := exchange.NewWithWindow(window)
	jr, stats, err := journal.Recover(journalPath, opts, func(r journal.Record) error { return journal.Apply(ex, r) })
	if err != nil {
		return nil, nil, rep, err
	}
	if err := ex.CheckInvariants(); err != nil {
		jr.Close()
		return nil, nil, rep, fmt.Errorf("invariantes rotos tras recuperar: %w", err)
	}
	rep.Replayed, rep.LastSeq, rep.TruncatedBytes, rep.Took = stats.Records, stats.LastSeq, stats.TruncatedBytes, time.Since(t0)
	return ex, jr, rep, nil
}

// tryWithSnapshot intenta la ruta rápida. Si devuelve reason != "", la ruta rápida NO se usó
// (y no deja nada abierto ni modificado).
func tryWithSnapshot(journalPath, snapPath string, opts journal.Options, window int) (*exchange.Exchange, *journal.Journal, Report, string) {
	snap, err := snapshot.Read(snapPath)
	if err != nil {
		return nil, nil, Report{}, err.Error()
	}
	id, err := journal.ReadID(journalPath)
	if err != nil {
		return nil, nil, Report{}, "no se pudo leer la identidad del journal: " + err.Error()
	}
	if id == "" || id != snap.JournalID {
		return nil, nil, Report{}, "el snapshot pertenece a otro journal"
	}
	ex, err := exchange.Restore(snap.State, window)
	if err != nil {
		return nil, nil, Report{}, "estado del snapshot inválido: " + err.Error()
	}
	jr, stats, err := journal.RecoverFrom(journalPath, opts, &journal.Resume{Offset: snap.JournalOffset, LastSeq: snap.LastSeq},
		func(r journal.Record) error { return journal.Apply(ex, r) })
	if err != nil {
		return nil, nil, Report{}, "la cola del journal no encaja con el snapshot: " + err.Error()
	}
	if err := ex.CheckInvariants(); err != nil {
		jr.Close()
		return nil, nil, Report{}, "invariantes rotos con el snapshot: " + err.Error()
	}
	return ex, jr, Report{FromSnapshot: true, SnapshotSeq: snap.LastSeq, Replayed: stats.Records,
		LastSeq: stats.LastSeq, TruncatedBytes: stats.TruncatedBytes}, ""
}
