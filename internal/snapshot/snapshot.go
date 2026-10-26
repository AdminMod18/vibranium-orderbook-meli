// Package snapshot guarda y carga fotografías del estado del exchange.
//
// Un snapshot es solo una OPTIMIZACIÓN del arranque: la fuente de verdad sigue siendo el journal.
// Por eso, si el snapshot falta, está dañado o pertenece a otro journal, se ignora y se reproduce
// el log completo: nunca puede dejar el sistema en un estado peor que sin él.
package snapshot

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
)

const formatVersion = 1

var (
	ErrNone    = errors.New("no hay snapshot")
	ErrCorrupt = errors.New("snapshot corrupto")
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
)

// File es el contenido del snapshot.
type File struct {
	V             int            `json:"v"`
	JournalID     string         `json:"journal_id"`     // a qué journal pertenece
	LastSeq       uint64         `json:"last_seq"`       // último registro del journal reflejado
	JournalOffset int64          `json:"journal_offset"` // byte donde empieza el registro LastSeq+1
	TS            int64          `json:"ts"`
	State         exchange.State `json:"state"`
}

// Write guarda de forma ATÓMICA: se escribe a un archivo temporal, se hace fsync, se renombra
// y se hace fsync del directorio. Ante una caída queda el snapshot anterior o el nuevo, nunca uno a medias.
func Write(path string, f File) error {
	f.V, f.TS = formatVersion, time.Now().UnixNano()
	payload, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, castagnoli))

	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(hdr[:], payload...)); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil { // en Windows abrir un directorio puede fallar: no es crítico
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Read carga el snapshot. ErrNone si no existe; ErrCorrupt si está dañado o truncado.
func Read(path string) (File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, ErrNone
	}
	if err != nil {
		return File{}, err
	}
	if len(b) < 8 {
		return File{}, fmt.Errorf("%w: archivo truncado", ErrCorrupt)
	}
	n := binary.LittleEndian.Uint32(b[0:4])
	if int(n) != len(b)-8 {
		return File{}, fmt.Errorf("%w: longitud %d no coincide con %d", ErrCorrupt, n, len(b)-8)
	}
	if crc32.Checksum(b[8:], castagnoli) != binary.LittleEndian.Uint32(b[4:8]) {
		return File{}, fmt.Errorf("%w: CRC inválido", ErrCorrupt)
	}
	var f File
	if err := json.Unmarshal(b[8:], &f); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if f.V != formatVersion {
		return File{}, fmt.Errorf("%w: versión %d no soportada", ErrCorrupt, f.V)
	}
	return f, nil
}

// Writer escribe snapshots en segundo plano (uno a la vez) para no frenar al secuenciador:
// el secuenciador solo COPIA el estado (rápido) y la escritura a disco ocurre en otro goroutine.
type Writer struct {
	Path      string
	JournalID string

	busy     atomic.Bool
	wg       sync.WaitGroup
	lastSeq  atomic.Uint64
	lastNs   atomic.Int64 // duración de la última escritura
	lastCopy atomic.Int64 // duración de la última captura (pausa del secuenciador)
	failures atomic.Uint64
	taken    atomic.Uint64
}

func (w *Writer) LastSeq() uint64       { return w.lastSeq.Load() }
func (w *Writer) Failures() uint64      { return w.failures.Load() }
func (w *Writer) Taken() uint64         { return w.taken.Load() }
func (w *Writer) WriteSeconds() float64 { return time.Duration(w.lastNs.Load()).Seconds() }
func (w *Writer) PauseSeconds() float64 { return time.Duration(w.lastCopy.Load()).Seconds() }

// SetBaseline informa del snapshot con el que se arrancó (para el cálculo de "cada N registros").
func (w *Writer) SetBaseline(seq uint64) { w.lastSeq.Store(seq) }

// Async captura el estado (síncrono, en el goroutine que llama) y lo escribe en segundo plano.
// Devuelve false si ya hay una escritura en curso (se omite este intento).
func (w *Writer) Async(x *exchange.Exchange, seq uint64, offset int64) bool {
	if !w.busy.CompareAndSwap(false, true) {
		return false
	}
	t0 := time.Now()
	st := x.Capture()
	w.lastCopy.Store(int64(time.Since(t0)))
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer w.busy.Store(false)
		w.write(st, seq, offset)
	}()
	return true
}

// Sync captura y escribe en el acto (apagado ordenado). Espera antes a cualquier escritura en curso.
func (w *Writer) Sync(x *exchange.Exchange, seq uint64, offset int64) {
	w.wg.Wait()
	t0 := time.Now()
	st := x.Capture()
	w.lastCopy.Store(int64(time.Since(t0)))
	w.write(st, seq, offset)
}

func (w *Writer) write(st exchange.State, seq uint64, offset int64) {
	t0 := time.Now()
	err := Write(w.Path, File{JournalID: w.JournalID, LastSeq: seq, JournalOffset: offset, State: st})
	w.lastNs.Store(int64(time.Since(t0)))
	if err != nil {
		w.failures.Add(1)
		return
	}
	w.lastSeq.Store(seq)
	w.taken.Add(1)
}

// Wait espera a que termine la escritura en curso.
func (w *Writer) Wait() { w.wg.Wait() }
