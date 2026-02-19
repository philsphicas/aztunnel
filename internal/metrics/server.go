package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Serve starts an HTTP server on the provided listener that exposes
// Prometheus metrics at /metrics. It blocks until the context is cancelled,
// then shuts down gracefully.
func (m *Metrics) Serve(ctx context.Context, ln net.Listener, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(shutdownDone)
	}()

	logger.Info("metrics server listening", "addr", ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Wait for graceful shutdown only if it was triggered by ctx cancellation.
	if ctx.Err() != nil {
		<-shutdownDone
	}
	return nil
}
