// Command aztunnel-relay runs a mock Azure-Relay-compatible Hybrid
// Connections server for local development, CI, and offline end-to-end
// testing of aztunnel. It speaks the subset of the Azure Relay wire
// protocol used by aztunnel listeners and senders.
//
// It is not a drop-in replacement for Azure Relay and is not intended
// for production traffic: SAS validation uses a fixed dummy key
// (printed at startup so clients can match) and the server has no
// listener auth/authz or HA/clustering.
//
// See mockrelay/README.md for deployment notes and flag reference.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/alecthomas/kong"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

var version = "dev"

func init() {
	_, _ = memlimit.SetGoMemLimitWithOpts(memlimit.WithLogger(nil))
}

// CLI is the top-level command structure for aztunnel-relay.
var CLI struct {
	Bind                string        `name:"bind" help:"Address:port to bind on." default:"127.0.0.1:8080" env:"AZTUNNEL_RELAY_BIND"`
	TLS                 bool          `name:"tls" help:"Enable TLS (wss). If --tls-cert/--tls-key are unset, generate a self-signed cert." env:"AZTUNNEL_RELAY_TLS"`
	TLSCert             string        `name:"tls-cert" help:"Path to PEM-encoded TLS certificate." env:"AZTUNNEL_RELAY_TLS_CERT"`
	TLSKey              string        `name:"tls-key" help:"Path to PEM-encoded TLS private key." env:"AZTUNNEL_RELAY_TLS_KEY"`
	PublicURL           string        `name:"public-url" help:"Base URL for minted rendezvous addresses (e.g. https://relay.example.com). Required behind a reverse proxy or when binding to a non-loopback address." env:"AZTUNNEL_RELAY_PUBLIC_URL"`
	LogLevel            string        `name:"log-level" help:"Log level (debug, info, warn, error)." default:"info"`
	MaxConnections      int           `name:"max-connections" help:"Max concurrent rendezvous connections per entity (0 = unlimited)." default:"0"`
	ListenerIdleTimeout time.Duration `name:"listener-idle-timeout" help:"Close idle listener control channels after this duration." default:"2m"`
	RendezvousTimeout   time.Duration `name:"rendezvous-timeout" help:"Max time a sender waits for the listener to dial the rendezvous URL." default:"30s"`
	MetricsAddr         string        `name:"metrics-addr" help:"Address for Prometheus metrics server (e.g. :9090); disabled if empty." env:"AZTUNNEL_RELAY_METRICS_ADDR"`
	AuthKeyName         string        `name:"auth-key-name" help:"SAS key name the server will accept (clients set AZTUNNEL_KEY_NAME to match). Defaults to a fixed dev value." env:"AZTUNNEL_RELAY_AUTH_KEY_NAME"`
	AuthKey             string        `name:"auth-key" help:"SAS key value the server will accept (clients set AZTUNNEL_KEY to match). Defaults to a fixed dev value — NOT secret." env:"AZTUNNEL_RELAY_AUTH_KEY"` //nolint:gosec // intentionally a fixed dev value; mock relay is a test fixture.
	NoAuth              bool          `name:"no-auth" help:"Disable SAS validation entirely. Intended only for protocol-level tests; do not use unattended." env:"AZTUNNEL_RELAY_NO_AUTH"`
	Version             versionFlag   `name:"version" help:"Print version and exit."`
}

type versionFlag bool

func (v versionFlag) BeforeApply(app *kong.Kong) error {
	_, _ = fmt.Fprintln(app.Stdout, version)
	app.Exit(0)
	return nil
}

func main() {
	parser := kong.Must(&CLI,
		kong.Name("aztunnel-relay"),
		kong.Description("Mock Azure-Relay-compatible Hybrid Connections server for testing aztunnel."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
	)
	_, err := parser.Parse(os.Args[1:])
	parser.FatalIfErrorf(err)

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger(CLI.LogLevel)

	var tlsOpts *server.TLSOptions
	if CLI.TLS || CLI.TLSCert != "" || CLI.TLSKey != "" {
		opts, err := resolveTLS(logger)
		if err != nil {
			return fmt.Errorf("TLS: %w", err)
		}
		tlsOpts = opts
		defer func() { _ = tlsOpts.Cleanup() }()
	}

	srv, err := server.NewServer(server.Config{
		Logger:              logger,
		MaxConnections:      CLI.MaxConnections,
		ListenerIdleTimeout: CLI.ListenerIdleTimeout,
		RendezvousTimeout:   CLI.RendezvousTimeout,
		PublicURL:           CLI.PublicURL,
		SASKeyName:          CLI.AuthKeyName,
		SASKey:              CLI.AuthKey,
		SkipAuth:            CLI.NoAuth,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", CLI.Bind)
	if err != nil {
		return fmt.Errorf("bind %s: %w", CLI.Bind, err)
	}
	// Security guard: when --public-url is unset, the rendezvous URL
	// handed to the listener is derived from the inbound Host header,
	// which is sender-controlled. A non-loopback bind would let an
	// attacker make listeners dial arbitrary hosts (SSRF / data
	// exfiltration). Require --public-url for non-loopback binds.
	if CLI.PublicURL == "" {
		tcpAddr, ok := ln.Addr().(*net.TCPAddr)
		if !ok || !tcpAddr.IP.IsLoopback() {
			_ = ln.Close()
			return fmt.Errorf(
				"--public-url is required when binding to a non-loopback address (bound to %s); "+
					"set --public-url=https://relay.example.com (or bind to 127.0.0.1)",
				ln.Addr().String(),
			)
		}
	}
	logger.Info("aztunnel-relay starting",
		"version", version,
		"addr", ln.Addr().String(),
		"tls", tlsOpts != nil,
	)
	if tlsOpts != nil && tlsOpts.Fingerprint != "" {
		logger.Warn("using self-signed TLS certificate — clients must trust it manually",
			"sha256_fingerprint", tlsOpts.Fingerprint)
	}
	if CLI.PublicURL == "" {
		logger.Info("--public-url unset; rendezvous URLs will be derived from the inbound Host header (safe for loopback binds)")
	}
	logAuthBanner(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if CLI.MetricsAddr != "" {
		go serveMetrics(ctx, CLI.MetricsAddr, logger)
	}

	if err := srv.Serve(ctx, ln, tlsOpts); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("aztunnel-relay stopped")
	return nil
}

func resolveTLS(logger *slog.Logger) (*server.TLSOptions, error) {
	if CLI.TLSCert != "" && CLI.TLSKey != "" {
		return server.LoadTLSFromFiles(CLI.TLSCert, CLI.TLSKey)
	}
	if CLI.TLSCert != "" || CLI.TLSKey != "" {
		return nil, fmt.Errorf("both --tls-cert and --tls-key must be provided together")
	}
	logger.Info("generating self-signed TLS certificate")
	return server.SelfSignedTLS()
}

// logAuthBanner emits a prominent line telling the operator which
// AZTUNNEL_KEY_NAME / AZTUNNEL_KEY values aztunnel must use to
// authenticate against this relay. When --no-auth is set, prints a
// warning instead.
func logAuthBanner(logger *slog.Logger) {
	if CLI.NoAuth {
		logger.Warn("SAS validation disabled (--no-auth) — every client request is accepted; do not run unattended")
		return
	}
	keyName := CLI.AuthKeyName
	if keyName == "" {
		keyName = server.DefaultSASKeyName
	}
	key := CLI.AuthKey
	if key == "" {
		key = server.DefaultSASKey
	}
	logger.Info("SAS auth enabled — clients must set AZTUNNEL_KEY_NAME and AZTUNNEL_KEY to match",
		"AZTUNNEL_KEY_NAME", keyName,
		"AZTUNNEL_KEY", key,
	)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// serveMetrics runs a minimal /healthz and /metrics endpoint. The
// metrics handler is intentionally a placeholder for v1 — see todo
// relay-binary-metrics. We still expose /healthz for orchestrators.
func serveMetrics(ctx context.Context, addr string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# aztunnel_relay metrics placeholder\n"))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("metrics server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics server failed", "error", err)
	}
}
