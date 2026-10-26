// Package journal es el log de eventos append-only: la FUENTE DE VERDAD del sistema.
//
// Cada comando que cambia el estado (depósito, orden, cancelación) se escribe aquí y se
// confirma al cliente sólo DESPUÉS de un fsync. Como el motor es determinista, reproducir
// el log reconstruye exactamente el mismo libro y las mismas billeteras tras una caída.
//
// Formato de cada registro:  [len uint32 LE][crc32c uint32 LE][payload JSON]
//   - el CRC detecta escrituras truncadas (corte de energía a mitad de un registro),
//   - el payload es JSON a propósito: legible y auditable (trazabilidad) con cualquier editor.
package journal

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	formatVersion = 1
	headerSize    = 8
	maxRecordSize = 1 << 20
)

var (
	ErrCorrupt = errors.New("journal corrupto")
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
)

type Kind string

const (
	KindDeposit Kind = "deposit"
	KindOrder   Kind = "order"
	KindCancel  Kind = "cancel"
	// KindGenesis es el primer registro de todo journal nuevo: fija su identidad (JournalID).
	// Ata el journal a su historial en Postgres: si alguno se reemplaza por otro, se detecta.
	KindGenesis Kind = "genesis"
)

// OrderRec es la orden tal como se recibió (campos de entrada) más su estado final
// tras el matching (Filled/Status), útil para trazabilidad.
type OrderRec struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Side   string `json:"side"` // BUY | SELL
	Price  int64  `json:"price"`
	Qty    int64  `json:"qty"`
	Filled int64  `json:"filled"`
	Status string `json:"status"`
}

type TradeRec struct {
	ID           uint64 `json:"id"`
	Price        int64  `json:"price"`
	Qty          int64  `json:"qty"`
	BuyOrderID   string `json:"buy_order_id"`
	SellOrderID  string `json:"sell_order_id"`
	BuyUserID    string `json:"buy_user_id"`
	SellUserID   string `json:"sell_user_id"`
	MakerOrderID string `json:"maker_order_id"`
	TakerOrderID string `json:"taker_order_id"`
}

// Record es un evento del log. La marca de tiempo es informativa: NUNCA se usa en la lógica
// (si se usara, reproducir el log no sería determinista).
type Record struct {
	V    int    `json:"v"`
	Seq  uint64 `json:"seq"`
	TS   int64  `json:"ts"`
	Kind Kind   `json:"kind"`

	User string `json:"user,omitempty"` // deposit
	BRL  int64  `json:"brl,omitempty"`
	VIB  int64  `json:"vib,omitempty"`

	Order   *OrderRec  `json:"order,omitempty"`    // order, cancel (estado final)
	Trades  []TradeRec `json:"trades,omitempty"`   // order
	OrderID string     `json:"order_id,omitempty"` // cancel

	JournalID string `json:"journal_id,omitempty"` // genesis
}

// Options configura el journal.
type Options struct {
	// NoSync desactiva fsync. SOLO para tests y benchmarks: sin fsync un corte de energía
	// puede perder órdenes ya confirmadas.
	NoSync bool
}

type Journal struct {
	f      *os.File
	w      *bufio.Writer
	next   uint64 // solo lo toca el goroutine que escribe
	opts   Options
	dirty  bool
	closed bool

	// Contadores legibles desde otras goroutines (métricas, API).
	lastSeq atomic.Uint64
	bytes   atomic.Int64
	syncs   atomic.Uint64
}

// ReplayStats describe lo ocurrido al recuperar.
type ReplayStats struct {
	Records        int
	LastSeq        uint64
	ValidBytes     int64
	TruncatedBytes int64 // cola descartada por escritura incompleta
}

// Recover reproduce el log existente llamando a fn por cada registro, descarta una cola
// truncada (escritura interrumpida por una caída) y devuelve un Journal listo para seguir
// escribiendo a continuación del último registro válido.
func Recover(path string, opts Options, fn func(Record) error) (*Journal, ReplayStats, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, ReplayStats{}, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, ReplayStats{}, err
	}
	res, err := scan(f, 0, 0, 0, fn)
	if err != nil {
		f.Close()
		return nil, ReplayStats{}, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, ReplayStats{}, err
	}
	stats := ReplayStats{Records: res.count, LastSeq: res.lastSeq, ValidBytes: res.end, TruncatedBytes: size - res.end}
	if stats.TruncatedBytes > 0 {
		if err := f.Truncate(res.end); err != nil {
			f.Close()
			return nil, stats, err
		}
		if !opts.NoSync {
			if err := f.Sync(); err != nil {
				f.Close()
				return nil, stats, err
			}
		}
	}
	if _, err := f.Seek(res.end, io.SeekStart); err != nil {
		f.Close()
		return nil, stats, err
	}
	j := &Journal{f: f, w: bufio.NewWriterSize(f, 1<<20), next: res.lastSeq + 1, opts: opts}
	j.lastSeq.Store(res.lastSeq)
	j.bytes.Store(res.end)
	if res.count == 0 && res.lastSeq == 0 { // journal nuevo: se le da identidad
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		if err := j.Append(&Record{Kind: KindGenesis, JournalID: hex.EncodeToString(id)}); err != nil {
			f.Close()
			return nil, stats, err
		}
		if err := j.Sync(); err != nil {
			f.Close()
			return nil, stats, err
		}
	}
	return j, stats, nil
}

// Append agrega un registro al buffer (asigna Seq y TS). No es durable hasta llamar a Sync.
func (j *Journal) Append(rec *Record) error {
	rec.V = formatVersion
	rec.Seq = j.next
	rec.TS = time.Now().UnixNano()
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(payload) > maxRecordSize {
		return fmt.Errorf("registro de %d bytes excede el máximo", len(payload))
	}
	var hdr [headerSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, castagnoli))
	if _, err := j.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := j.w.Write(payload); err != nil {
		return err
	}
	j.next++
	j.lastSeq.Store(rec.Seq)
	j.bytes.Add(int64(headerSize + len(payload)))
	j.dirty = true
	return nil
}

// Sync vuelca el buffer y hace fsync: al volver, todo lo agregado es durable.
// Una sola llamada confirma TODO un lote de registros (group commit).
func (j *Journal) Sync() error {
	if !j.dirty {
		return nil
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	if !j.opts.NoSync {
		if err := j.f.Sync(); err != nil {
			return err
		}
	}
	j.dirty = false
	j.syncs.Add(1)
	return nil
}

func (j *Journal) Close() error {
	if j.closed {
		return nil
	}
	j.closed = true
	if err := j.Sync(); err != nil {
		j.f.Close()
		return err
	}
	return j.f.Close()
}

// Métricas simples para observabilidad.
func (j *Journal) LastSeq() uint64 { return j.lastSeq.Load() }
func (j *Journal) NextSeq() uint64 { return j.lastSeq.Load() + 1 }
func (j *Journal) Bytes() int64    { return j.bytes.Load() }
func (j *Journal) Syncs() uint64   { return j.syncs.Load() }

// --- lectura ---

type scanResult struct {
	end     int64  // offset del final del último registro válido
	lastSeq uint64 // seq del último registro válido
	count   int
}

// scan lee registros desde start. prevSeq es el seq esperado previo (0 = no verificar el primero).
// limit > 0 corta tras ese número de registros (lectura por lotes).
// Una cola incompleta (EOF a mitad de registro, o CRC malo en el ÚLTIMO registro) se tolera:
// es el rastro de una caída durante la escritura. Cualquier otra anomalía es corrupción real.
func scan(f *os.File, start int64, prevSeq uint64, limit int, fn func(Record) error) (scanResult, error) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return scanResult{}, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	res := scanResult{end: start, lastSeq: prevSeq}
	for limit <= 0 || res.count < limit {
		var hdr [headerSize]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return res, nil // fin limpio o cabecera cortada
			}
			return res, err
		}
		n := binary.LittleEndian.Uint32(hdr[0:4])
		sum := binary.LittleEndian.Uint32(hdr[4:8])
		if n == 0 || n > maxRecordSize {
			return res, fmt.Errorf("%w: longitud inválida %d en offset %d", ErrCorrupt, n, res.end)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return res, nil // payload cortado: cola truncada
			}
			return res, err
		}
		if crc32.Checksum(payload, castagnoli) != sum {
			if _, err := r.Peek(1); err == io.EOF {
				return res, nil // último registro dañado: cola truncada
			}
			return res, fmt.Errorf("%w: CRC inválido en offset %d con datos posteriores", ErrCorrupt, res.end)
		}
		var rec Record
		if err := json.Unmarshal(payload, &rec); err != nil {
			return res, fmt.Errorf("%w: JSON inválido en offset %d: %v", ErrCorrupt, res.end, err)
		}
		if res.lastSeq != 0 && rec.Seq != res.lastSeq+1 {
			return res, fmt.Errorf("%w: secuencia %d tras %d en offset %d", ErrCorrupt, rec.Seq, res.lastSeq, res.end)
		}
		if err := fn(rec); err != nil {
			return res, err
		}
		res.end += int64(headerSize) + int64(n)
		res.lastSeq = rec.Seq
		res.count++
	}
	return res, nil
}

// Reader lee el log de forma incremental sin escribir: lo usará el proyector del historial.
type Reader struct {
	f       *os.File
	offset  int64
	lastSeq uint64
}

func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Reader{f: f}, nil
}

// ReadNew entrega a fn hasta limit registros (0 = todos) agregados desde la última llamada.
func (r *Reader) ReadNew(limit int, fn func(Record) error) (int, error) {
	res, err := scan(r.f, r.offset, r.lastSeq, limit, fn)
	if err != nil {
		return res.count, err
	}
	r.offset, r.lastSeq = res.end, res.lastSeq
	return res.count, nil
}

func (r *Reader) Close() error { return r.f.Close() }
