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
	// 16 MiB: holgura sobre el peor caso (engine.MaxTradesPerOrder trades con ids de 64 caracteres ≈ 5 MB;
	// lo comprueba TestWorstCaseRecordFitsTheLimit). Un registro mayor haría fallar el Append (fail-stop).
	maxRecordSize = 16 << 20
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
	lock   *os.File // sostiene el candado de instancia única mientras el journal esté abierto

	// Contadores legibles desde otras goroutines (métricas, API).
	lastSeq atomic.Uint64
	bytes   atomic.Int64
	syncs   atomic.Uint64
	// durable es el offset hasta el cual el archivo ya está en disco (fsync hecho). Los consumidores que
	// leen el archivo (el proyector a Postgres) NO deben pasar de aquí: lo que está en la caché del
	// sistema operativo pero sin fsync puede perderse con un corte de energía.
	durable atomic.Int64
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
	return RecoverFrom(path, opts, nil, fn)
}

// Resume indica desde dónde reanudar la lectura (lo aporta un snapshot): el byte donde empieza el
// registro LastSeq+1. El primer registro leído debe tener exactamente esa secuencia; si no, es error.
type Resume struct {
	Offset  int64
	LastSeq uint64
}

// RecoverFrom es como Recover pero, con resume != nil, solo reproduce la cola posterior al snapshot.
func RecoverFrom(path string, opts Options, resume *Resume, fn func(Record) error) (*Journal, ReplayStats, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, ReplayStats{}, err
	}
	// Candado de instancia única ANTES de leer o escribir nada.
	lk, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, ReplayStats{}, err
	}
	j, st, err := recoverLocked(path, opts, resume, fn)
	if err != nil {
		lk.Close() // un arranque fallido no debe dejar el candado tomado
		return nil, st, err
	}
	j.lock = lk
	return j, st, nil
}

func recoverLocked(path string, opts Options, resume *Resume, fn func(Record) error) (*Journal, ReplayStats, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, ReplayStats{}, err
	}
	var start int64
	var prev uint64
	if resume != nil {
		start, prev = resume.Offset, resume.LastSeq
		if fi, err := f.Stat(); err != nil || start > fi.Size() {
			f.Close()
			return nil, ReplayStats{}, fmt.Errorf("%w: el snapshot apunta más allá del final del journal", ErrCorrupt)
		}
	}
	res, err := scan(f, start, 0, prev, 0, fn, nil)
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
	j.durable.Store(res.end) // lo que hay en disco al arrancar es, por definición, lo que sobrevivió
	if resume != nil && res.lastSeq < resume.LastSeq {
		f.Close()
		return nil, stats, fmt.Errorf("%w: el journal termina antes que el snapshot", ErrCorrupt)
	}
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
	j.durable.Store(j.bytes.Load()) // todo lo agregado hasta aquí ya está volcado y sincronizado
	j.syncs.Add(1)
	return nil
}

func (j *Journal) Close() error {
	if j.closed {
		return nil
	}
	j.closed = true
	err := j.Sync()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	if j.lock != nil {
		j.lock.Close() // libera el candado
	}
	return err
}

// Abandon suelta el archivo y el candado SIN vaciar el buffer ni hacer fsync: lo mismo que deja la muerte
// del proceso. Existe para simular caídas en las pruebas.
func (j *Journal) Abandon() {
	if j.closed {
		return
	}
	j.closed = true
	j.f.Close()
	if j.lock != nil {
		j.lock.Close()
	}
}

// Métricas simples para observabilidad.
func (j *Journal) LastSeq() uint64 { return j.lastSeq.Load() }
func (j *Journal) NextSeq() uint64 { return j.lastSeq.Load() + 1 }
func (j *Journal) Bytes() int64    { return j.bytes.Load() }
func (j *Journal) Syncs() uint64   { return j.syncs.Load() }

// DurableBytes es el offset hasta el que el archivo está sincronizado en disco.
func (j *Journal) DurableBytes() int64 { return j.durable.Load() }

// --- lectura ---

type scanResult struct {
	end       int64  // offset del final del último registro válido
	lastSeq   uint64 // seq del último registro válido
	lastStart int64  // offset donde empieza el último registro válido (-1 si no hubo)
	count     int
}

// scan lee registros desde start. prevSeq es el seq esperado previo (0 = no verificar el primero).
// limit > 0 corta tras ese número de registros (lectura por lotes). end > 0 acota la lectura a ese offset.
// Una cola incompleta (EOF a mitad de registro, o CRC malo en el ÚLTIMO registro) se tolera:
// es el rastro de una caída durante la escritura. Cualquier otra anomalía es corrupción real.
func scan(f *os.File, start, end int64, prevSeq uint64, limit int, fn func(Record) error, buf *scanBuf) (scanResult, error) {
	if end <= 0 { // 0 = hasta el final actual del archivo
		fi, err := f.Stat()
		if err != nil {
			return scanResult{}, err
		}
		end = fi.Size()
	}
	res := scanResult{end: start, lastSeq: prevSeq, lastStart: -1}
	if end <= start {
		return res, nil
	}
	if buf == nil {
		buf = &scanBuf{}
	}
	r := buf.reader(io.NewSectionReader(f, start, end-start))
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
		payload := buf.payload(int(n))
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
		res.lastStart = res.end
		res.end += int64(headerSize) + int64(n)
		res.lastSeq = rec.Seq
		res.count++
	}
	return res, nil
}

// scanBuf son los buffers de lectura que un lector de larga vida reutiliza entre llamadas. Sin ellos cada
// ReadNew (el proyector llama varias veces por segundo) reservaba un bufio de 1 MiB nuevo: medido, el 49 % de
// TODA la basura que generaba el servidor a 5.000 órdenes/s, y de ahí venían ~20 ciclos de GC por minuto.
type scanBuf struct {
	br  *bufio.Reader
	buf []byte
}

func (b *scanBuf) reader(src io.Reader) *bufio.Reader {
	if b.br == nil {
		b.br = bufio.NewReaderSize(src, 1<<20)
	} else {
		b.br.Reset(src)
	}
	return b.br
}

// payload devuelve un buffer de n bytes. Es seguro reutilizarlo: json.Unmarshal copia las cadenas y
// Record no guarda referencias al buffer.
func (b *scanBuf) payload(n int) []byte {
	if cap(b.buf) < n {
		b.buf = make([]byte, n, max(n, 4096))
	}
	return b.buf[:n]
}

// Reader lee el log de forma incremental sin escribir: lo usará el proyector del historial.
type Reader struct {
	f         *os.File
	offset    int64
	lastSeq   uint64
	lastStart int64        // donde empieza el último registro leído (-1 si aún no se leyó ninguno)
	durable   func() int64 // si no es nil, no se lee más allá de este offset
	buf       scanBuf
}

// SetDurable limita la lectura al offset que devuelva fn (p. ej. Journal.DurableBytes): el lector solo
// ve lo que ya está sincronizado en disco, nunca bytes que podrían perderse con un corte de energía.
func (r *Reader) SetDurable(fn func() int64) { r.durable = fn }

func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Reader{f: f, lastStart: -1}, nil
}

// ErrBehind indica que el journal es más corto que el offset desde el que se pidió leer: el archivo
// retrocedió o es otro. Quien reanuda desde un offset guardado debe volver a leer desde el inicio.
var ErrBehind = errors.New("el journal es más corto que el offset de reanudación")

// OpenReaderAt abre el lector reanudando en offset, que debe ser el byte donde empieza el registro
// afterSeq+1. No se confía en el offset: la primera lectura comprueba que el registro que hay ahí
// sea exactamente afterSeq+1 (si no, falla con ErrCorrupt) y que el archivo llegue hasta ese punto
// (si no, ErrBehind). Quien lo use debe exigir además que esa primera lectura devuelva al menos un
// registro: un offset en el final del archivo no contradice nada pero tampoco prueba nada.
func OpenReaderAt(path string, offset int64, afterSeq uint64) (*Reader, error) {
	r, err := OpenReader(path)
	if err != nil {
		return nil, err
	}
	r.offset, r.lastSeq = offset, afterSeq
	return r, nil
}

// ReadNew entrega a fn hasta limit registros (0 = todos) agregados desde la última llamada.
func (r *Reader) ReadNew(limit int, fn func(Record) error) (int, error) {
	var end int64
	if r.durable != nil {
		if end = r.durable(); end < r.offset {
			return 0, ErrBehind
		} else if end == r.offset {
			return 0, nil // aún no hay nada nuevo que esté en disco
		}
	} else if fi, err := r.f.Stat(); err == nil && fi.Size() < r.offset {
		return 0, ErrBehind
	}
	res, err := scan(r.f, r.offset, end, r.lastSeq, limit, fn, &r.buf)
	if err != nil {
		return res.count, err
	}
	r.offset, r.lastSeq = res.end, res.lastSeq
	if res.lastStart >= 0 {
		r.lastStart = res.lastStart
	}
	return res.count, nil
}

// Offset es el byte donde empezará la próxima lectura (justo después del último registro leído).
func (r *Reader) Offset() int64 { return r.offset }

// LastRecordStart es el byte donde EMPIEZA el último registro leído (-1 si no se leyó ninguno). Guardarlo
// permite reanudar volviendo a leer ese registro: así la reanudación siempre comprueba que el journal
// contiene de verdad lo que el historial dice haber proyectado.
func (r *Reader) LastRecordStart() int64 { return r.lastStart }

func (r *Reader) Close() error { return r.f.Close() }

// ReadID devuelve la identidad del journal (su registro genesis); "" si no tiene.
func ReadID(path string) (string, error) {
	rd, err := OpenReader(path)
	if err != nil {
		return "", err
	}
	defer rd.Close()
	id := ""
	_, err = rd.ReadNew(1, func(r Record) error {
		if r.Kind == KindGenesis {
			id = r.JournalID
		}
		return nil
	})
	return id, err
}
