package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AdminMod18/vibranium-orderbook-meli/internal/api"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/exchange"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/journal"
	"github.com/AdminMod18/vibranium-orderbook-meli/internal/sequencer"
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

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := ":" + strconv.Itoa(envInt("PORT", 8080))
	queueSize := envInt("QUEUE_SIZE", 50_000)

	// Recuperación: reproducir el journal reconstruye libro y billeteras exactamente como estaban.
	journalPath := envStr("JOURNAL_PATH", "data/journal.log")
	ex := exchange.New()
	t0 := time.Now()
	jr, stats, err := journal.Recover(journalPath, journal.Options{NoSync: os.Getenv("JOURNAL_NOSYNC") == "1"},
		func(r journal.Record) error { return journal.Apply(ex, r) })
	if err != nil {
		log.Error("no se pudo recuperar el journal; se aborta para no operar con estado inválido", "err", err)
		os.Exit(1)
	}
	if err := ex.CheckInvariants(); err != nil {
		log.Error("invariantes rotos tras recuperar", "err", err)
		os.Exit(1)
	}
	log.Info("journal recuperado", "path", journalPath, "records", stats.Records, "last_seq", stats.LastSeq,
		"truncated_bytes", stats.TruncatedBytes, "took", time.Since(t0).String())

	seq := sequencer.New(ex, queueSize, sequencer.WithJournal(jr))
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(seq).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("servidor escuchando", "addr", addr, "queue_size", queueSize)
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
	seq.Stop()
	if err := jr.Close(); err != nil {
		log.Error("cerrando journal", "err", err)
	}
	log.Info("apagado completo")
}
