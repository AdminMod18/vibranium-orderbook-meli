package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/api"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/history"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/metrics"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/recovery"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/snapshot"
)

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// version se fija en el build (-ldflags "-X main.version=..."); aparece en orderbook_build_info.
var version = "dev"

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	met := metrics.New(version)
	addr := ":" + strconv.Itoa(envInt("PORT", 8080))
	// Cola ≈ ritmo × plazo de la request (2 s): una cola mayor solo hace esperar a los clientes lo que luego
	// será un 504, en vez de decirles 503 al instante. 10.000 ≈ 2 s a 5.000 órdenes/s.
	queueSize := envInt("QUEUE_SIZE", 10_000)

	// Recuperación: snapshot + cola del journal (o el journal completo si no hay snapshot utilizable).
	journalPath := envStr("JOURNAL_PATH", "data/journal.log")
	snapEvery := 200_000 // registros entre snapshots; SNAPSHOT_EVERY=0 los desactiva
	if v, err := strconv.Atoi(os.Getenv("SNAPSHOT_EVERY")); err == nil && v >= 0 {
		snapEvery = v
	}
	snapPath := ""
	if snapEvery > 0 {
		snapPath = envStr("SNAPSHOT_PATH", filepath.Join(filepath.Dir(journalPath), "snapshot.bin"))
	}
	// Ventana de idempotencia: cuántos ids recientes se recuerdan para rechazar reintentos (409).
	// Acota la memoria, el tamaño del snapshot y el arranque. IDEMPOTENCY_WINDOW=0 = sin límite (no recomendado).
	idemWindow := 5_000_000
	if v, err := strconv.Atoi(os.Getenv("IDEMPOTENCY_WINDOW")); err == nil && v >= 0 {
		idemWindow = v
	}
	ex, jr, rep, err := recovery.Recover(journalPath, snapPath, journal.Options{NoSync: os.Getenv("JOURNAL_NOSYNC") == "1"}, idemWindow, log)
	if err != nil {
		if errors.Is(err, journal.ErrLocked) {
			log.Error("OTRA instancia ya está usando este journal; se aborta para no corromperlo. Detenga la otra instancia o use otro JOURNAL_PATH", "err", err)
		} else {
			log.Error("no se pudo recuperar; se aborta para no operar con estado inválido", "err", err)
		}
		os.Exit(1)
	}
	log.Info("recuperación completa", "path", journalPath, "desde_snapshot", rep.FromSnapshot, "snapshot_seq", rep.SnapshotSeq,
		"registros_reproducidos", rep.Replayed, "last_seq", rep.LastSeq, "truncated_bytes", rep.TruncatedBytes, "took", rep.Took.String())
	met.GaugeFunc("recovery_seconds", "Duración de la última recuperación al arrancar.", func() float64 { return rep.Took.Seconds() })
	met.GaugeFunc("recovery_replayed_records", "Registros del journal reproducidos al arrancar.", func() float64 { return float64(rep.Replayed) })

	met.GaugeFunc("idempotency_window_size", "Ids de órdenes recordados para detectar reintentos.", func() float64 { return float64(ex.SeenCount()) })
	met.GaugeFunc("idempotency_window_limit", "Tamaño máximo de la ventana de idempotencia (0 = sin límite).", func() float64 { return float64(ex.SeenLimit()) })
	met.CounterFunc("idempotency_evicted_total", "Ids olvidados por salir de la ventana de idempotencia.", func() float64 { return float64(ex.SeenEvicted()) })

	opts := []sequencer.Option{sequencer.WithJournal(jr), sequencer.WithObserver(met)}
	if snapEvery > 0 {
		id, err := journal.ReadID(journalPath)
		if err != nil {
			log.Error("no se pudo leer la identidad del journal", "err", err)
			os.Exit(1)
		}
		w := &snapshot.Writer{Path: snapPath, JournalID: id}
		w.SetBaseline(rep.SnapshotSeq)
		opts = append(opts, sequencer.WithSnapshots(w, uint64(snapEvery)))
		met.GaugeFunc("snapshot_last_seq", "Último registro cubierto por el snapshot.", func() float64 { return float64(w.LastSeq()) })
		met.GaugeFunc("snapshot_age_records", "Registros del journal posteriores al último snapshot (lo que habría que reproducir).", func() float64 { return float64(int64(jr.LastSeq()) - int64(w.LastSeq())) })
		met.GaugeFunc("snapshot_write_seconds", "Duración de la última escritura de snapshot.", w.WriteSeconds)
		met.GaugeFunc("snapshot_pause_seconds", "Pausa del secuenciador al copiar el estado en el último snapshot.", w.PauseSeconds)
		met.CounterFunc("snapshot_failures_total", "Snapshots que fallaron al escribirse.", func() float64 { return float64(w.Failures()) })
	}
	seq := sequencer.New(ex, queueSize, opts...)

	// Medidores "vivos": se leen en cada scrape de Prometheus.
	met.GaugeFunc("queue_depth", "Comandos esperando en la cola del secuenciador.", func() float64 { return float64(seq.QueueLen()) })
	met.GaugeFunc("queue_capacity", "Capacidad de la cola del secuenciador.", func() float64 { return float64(seq.QueueCap()) })
	met.CounterFunc("queue_rejected_total", "Comandos rechazados por cola llena (HTTP 503).", func() float64 { return float64(seq.Overloaded()) })
	// Espacio libre donde vive el journal: con el disco lleno el servicio hace fail-stop (no puede persistir).
	// NaN = la plataforma no lo informa (ninguna alerta se dispara con NaN).
	journalDir := filepath.Dir(journalPath)
	met.GaugeFunc("journal_disk_free_bytes", "Bytes libres en el sistema de archivos del journal.", func() float64 {
		if v, err := journal.DiskFree(journalDir); err == nil {
			return float64(v)
		}
		return math.NaN()
	})
	met.CounterFunc("queue_expired_total", "Comandos descartados sin ejecutar porque su cliente ya no esperaba (timeout/desconexión).", func() float64 { return float64(seq.Expired()) })
	met.CounterFunc("commands_processed_total", "Comandos ejecutados por el secuenciador.", func() float64 { return float64(seq.Processed()) })
	met.GaugeFunc("journal_last_seq", "Último registro confirmado en el journal.", func() float64 { return float64(jr.LastSeq()) })
	met.GaugeFunc("journal_bytes", "Tamaño del journal en bytes.", func() float64 { return float64(jr.Bytes()) })
	met.CounterFunc("journal_syncs_total", "fsyncs realizados sobre el journal.", func() float64 { return float64(jr.Syncs()) })

	// Historial en Postgres (opcional). El servidor arranca y opera aunque Postgres esté caído:
	// el proyector reintenta en segundo plano y se pone al día solo.
	handler := api.New(seq).WithMetrics(met).WithLogger(log)
	projCtx, stopProj := context.WithCancel(context.Background())
	projDone := make(chan struct{})
	var proj *history.Projector
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		st, err := history.Open(projCtx, dsn)
		if err != nil {
			log.Error("DATABASE_URL inválida", "err", err)
			os.Exit(1)
		}
		defer st.Close()
		handler = handler.WithHistory(st)
		proj = history.NewProjector(st, journalPath, log)
		proj.SetDurable(jr.DurableBytes) // el historial solo refleja lo que ya está sincronizado en disco
		go func() { proj.Run(projCtx); close(projDone) }()
		met.GaugeFunc("projector_seq", "Último registro del journal reflejado en Postgres.", func() float64 { return float64(proj.ProjectedSeq()) })
		met.GaugeFunc("projector_lag", "Registros del journal aún no reflejados en Postgres.", func() float64 {
			return float64(int64(jr.LastSeq()) - int64(proj.ProjectedSeq()))
		})
		met.GaugeFunc("projector_up", "1 si el proyector está sano, 0 si no.", func() float64 {
			if proj.Healthy() {
				return 1
			}
			return 0
		})
		met.CounterFunc("projector_failures_total", "Errores del proyector (Postgres caído, etc.).", func() float64 { return float64(proj.Failures()) })
		log.Info("historial en Postgres habilitado")
	} else {
		close(projDone)
		log.Warn("DATABASE_URL no definida: sin historial consultable (el journal igual registra todo)")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Servidor de administración en OTRO puerto: métricas y perfiles no deben quedar expuestos
	// junto con la API pública (en producción este puerto solo es alcanzable desde la red interna).
	adminMux := http.NewServeMux()
	adminMux.Handle("GET /metrics", met.Handler())
	adminMux.HandleFunc("/debug/pprof/", pprof.Index)
	adminMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	adminMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	adminMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	adminMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	adminSrv := &http.Server{Addr: ":" + strconv.Itoa(envInt("ADMIN_PORT", 9102)), Handler: adminMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("fallo del servidor de administración", "err", err)
		}
	}()

	go func() {
		log.Info("servidor escuchando", "addr", addr, "queue_size", queueSize, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("fallo del servidor", "err", err)
			os.Exit(1)
		}
	}()

	// Apagado ordenado: 1) dejar de aceptar requests, 2) esperar las que están en vuelo,
	// 3) vaciar la cola del secuenciador. Ninguna orden aceptada se pierde.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("apagando...")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown", "err", err)
	}
	_ = adminSrv.Shutdown(ctx)
	seq.Stop()
	if err := jr.Close(); err != nil {
		log.Error("cerrando journal", "err", err)
	}
	// Se da un margen al proyector para reflejar los últimos registros antes de salir.
	if proj != nil {
		for end := time.Now().Add(3 * time.Second); proj.ProjectedSeq() < jr.LastSeq() && time.Now().Before(end); {
			time.Sleep(20 * time.Millisecond)
		}
	}
	stopProj()
	<-projDone
	log.Info("apagado completo")
}
