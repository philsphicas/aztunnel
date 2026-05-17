package main

import (
	"context"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
)

// RelayListenerCmd listens on Azure Relay and forwards to local targets.
type RelayListenerCmd struct {
	AuthFlags
	Allow          []string      `help:"Allowed targets (host:port, CIDR:port, CIDR:*)."`
	MaxConnections int           `name:"max-connections" help:"Max concurrent connections (0 = unlimited)." default:"0"`
	ConnectTimeout time.Duration `name:"connect-timeout" help:"Timeout for dialing targets." default:"30s"`
	TCPKeepAlive   time.Duration `name:"tcp-keepalive" help:"TCP keepalive interval." default:"30s"`

	// RejectMux is an e2e test seam that makes the listener reject the
	// v2 mux handshake with a v1-style "unsupported protocol version"
	// response, so the sender's mux-unsupported fallback path can be
	// driven against a real subprocess. Hidden from --help and
	// deliberately not bound to an environment variable so a stray
	// production env can't silently disable mux at the listener side.
	RejectMux bool `name:"reject-mux" help:"Reject v2 mux handshakes as if this were a v1-only listener (test seam)." hidden:""`
}

// Run executes the relay-listener command.
func (r *RelayListenerCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(r.Hyco)
	if err != nil {
		return err
	}

	endpoint, opts, tp, providerName, err := resolveAuth(r.AuthFlags)
	if err != nil {
		return err
	}

	logger := newLogger(globals.LogLevel)
	warnInsecureTLS(opts, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m, err := resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger)
	if err != nil {
		return err
	}

	cfg := listener.Config{
		Endpoint:       endpoint,
		EntityPath:     hyco,
		TokenProvider:  observeTokenFetch(tp, m, providerName),
		ClientOptions:  opts,
		AllowList:      r.Allow,
		MaxConnections: r.MaxConnections,
		ConnectTimeout: r.ConnectTimeout,
		TCPKeepAlive:   r.TCPKeepAlive,
		Logger:         logger,
		Metrics:        m,
		RejectMux:      r.RejectMux,
	}

	return listener.ListenAndServe(ctx, cfg)
}
