package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/metricspush"
	"github.com/frappe/boat/internal/run"
)

// serveMetrics runs an HTTP server that answers GET /metrics with the host and
// per-VM metrics in Prometheus exposition format, gathered live on each scrape.
// It is registered only when --metrics-listen is set, and shuts down when ctx is
// cancelled. Unauthenticated by design: a Prometheus metrics port carries host
// telemetry and is meant to be bound to a private address the scraper reaches.
func (parts *daemonParts) serveMetrics(ctx context.Context, address string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		host := metrics.Gather(request.Context(), run.NewRunner(io.Discard))
		virtualMachines, err := parts.store.ListVirtualMachines()
		if err != nil {
			slog.Warn("metrics endpoint: could not list virtual machines", "error", err)
		}
		body := metricspush.RenderPrometheus(parts.serverName, host, virtualMachines, metricspush.DefaultRoots())
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(writer, body)
	})

	server := &http.Server{Addr: address, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("serving prometheus metrics", "address", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
