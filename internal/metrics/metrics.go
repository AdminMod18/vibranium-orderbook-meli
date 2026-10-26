// Package metrics define las métricas Prometheus del sistema. Cada una responde una pregunta
// operativa concreta (ver docs/OBSERVABILIDAD.md). Todos los métodos toleran un receptor nil,
// así el resto del código no necesita comprobar si las métricas están habilitadas.
package metrics

import (
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "orderbook"

var (
	latencyBuckets = []float64{.0005, .001, .002, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}
	fastBuckets    = []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, 1}
	batchBuckets   = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}
)

type Metrics struct {
	reg *prometheus.Registry

	httpReqs     *prometheus.CounterVec
	httpDur      *prometheus.HistogramVec
	httpInFlight prometheus.Gauge

	orders       *prometheus.CounterVec
	cancels      *prometheus.CounterVec
	trades       prometheus.Counter
	tradedUnits  prometheus.Counter
	tradedCents  prometheus.Counter
	queueWait    prometheus.Histogram
	batchSize    prometheus.Histogram
	batchExec    prometheus.Histogram
	journalFsync prometheus.Histogram
}

func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{reg: reg}
	f := func(name, help string) prometheus.Opts { return prometheus.Opts{Namespace: ns, Name: name, Help: help} }

	m.httpReqs = prometheus.NewCounterVec(prometheus.CounterOpts(f("http_requests_total", "Requests HTTP por ruta, método y código.")), []string{"route", "method", "code"})
	m.httpDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "http_request_duration_seconds",
		Help: "Latencia HTTP vista por el servidor, por ruta.", Buckets: latencyBuckets}, []string{"route"})
	m.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts(f("http_in_flight_requests", "Requests HTTP en curso.")))

	m.orders = prometheus.NewCounterVec(prometheus.CounterOpts(f("orders_total", "Órdenes recibidas por lado y resultado.")), []string{"side", "result"})
	m.cancels = prometheus.NewCounterVec(prometheus.CounterOpts(f("cancels_total", "Cancelaciones por resultado.")), []string{"result"})
	m.trades = prometheus.NewCounter(prometheus.CounterOpts(f("trades_total", "Trades ejecutados.")))
	m.tradedUnits = prometheus.NewCounter(prometheus.CounterOpts(f("traded_units_total", "Unidades de Vibranium negociadas.")))
	m.tradedCents = prometheus.NewCounter(prometheus.CounterOpts(f("traded_value_cents_total", "Valor negociado en centavos de real.")))

	m.queueWait = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "queue_wait_seconds",
		Help: "Tiempo que un comando espera en la cola antes de ejecutarse.", Buckets: fastBuckets})
	m.batchSize = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "batch_size",
		Help: "Comandos por lote (group commit): cuántos comparten un fsync.", Buckets: batchBuckets})
	m.batchExec = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "batch_exec_seconds",
		Help: "Tiempo de matching+liquidación de un lote (sin fsync).", Buckets: fastBuckets})
	m.journalFsync = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "journal_fsync_seconds",
		Help: "Duración del fsync del journal por lote.", Buckets: fastBuckets})

	reg.MustRegister(m.httpReqs, m.httpDur, m.httpInFlight, m.orders, m.cancels, m.trades, m.tradedUnits,
		m.tradedCents, m.queueWait, m.batchSize, m.batchExec, m.journalFsync)

	info := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "build_info", Help: "Versión del servicio.",
		ConstLabels: prometheus.Labels{"version": version, "go": runtime.Version()}})
	info.Set(1)
	reg.MustRegister(info)
	return m
}

// Handler sirve /metrics en formato Prometheus.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// GaugeFunc registra una métrica que se lee de un valor vivo en cada scrape.
func (m *Metrics) GaugeFunc(name, help string, fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help}, fn))
}

// CounterFunc: como GaugeFunc pero para contadores monótonos que ya mantiene otro componente.
func (m *Metrics) CounterFunc(name, help string, fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, fn))
}

// --- implementación de sequencer.Observer ---

func (m *Metrics) Batch(size int, exec, fsync time.Duration) {
	if m == nil {
		return
	}
	m.batchSize.Observe(float64(size))
	m.batchExec.Observe(exec.Seconds())
	if fsync > 0 {
		m.journalFsync.Observe(fsync.Seconds())
	}
}

func (m *Metrics) QueueWait(d time.Duration) {
	if m != nil {
		m.queueWait.Observe(d.Seconds())
	}
}

// --- eventos de dominio (los emite la API) ---

// Order registra el resultado de una orden. result: accepted | insufficient_funds | duplicate |
// invalid | overloaded | timeout | error.
func (m *Metrics) Order(side, result string) {
	if m != nil {
		m.orders.WithLabelValues(side, result).Inc()
	}
}

func (m *Metrics) Cancel(result string) {
	if m != nil {
		m.cancels.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) Trade(units, cents int64) {
	if m != nil {
		m.trades.Inc()
		m.tradedUnits.Add(float64(units))
		m.tradedCents.Add(float64(cents))
	}
}

// --- HTTP ---

// InFlight suma/resta requests en curso.
func (m *Metrics) InFlight(delta float64) {
	if m != nil {
		m.httpInFlight.Add(delta)
	}
}

// HTTP registra una request terminada. route es el PATRÓN registrado (p. ej. "GET /orders/{id}")
// y no la URL real: así la cardinalidad de series es fija aunque haya millones de order_id distintos.
func (m *Metrics) HTTP(route, method string, code int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpDur.WithLabelValues(route).Observe(d.Seconds())
	m.httpReqs.WithLabelValues(route, method, codeClass(code)).Inc()
}

// codeClass deja el código exacto (200, 201, 409, 422, 503...): son pocos y distinguen causas.
func codeClass(c int) string {
	switch c {
	case 200, 201, 204, 400, 404, 409, 422, 501, 503, 504:
		return itoa(c)
	}
	if c >= 500 {
		return "5xx"
	}
	if c >= 400 {
		return "4xx"
	}
	return itoa(c)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}
