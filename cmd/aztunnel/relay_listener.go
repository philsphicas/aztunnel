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
}

// Run executes the relay-listener command.
func (r *RelayListenerCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(r.Hyco)
	if err != nil {
		return err
	}

	endpoint, tp, err := resolveAuth(r.Relay, r.Namespace, r.RelaySuffix)
	if err != nil {
		return err
	}

	logger := newLogger(globals.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m, err := resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger)
	if err != nil {
		return err
	}

	cfg := listener.Config{
		Endpoint:       endpoint,
		EntityPath:     hyco,
		TokenProvider:  tp,
		AllowList:      r.Allow,
		MaxConnections: r.MaxConnections,
		ConnectTimeout: r.ConnectTimeout,
		TCPKeepAlive:   r.TCPKeepAlive,
		Logger:         logger,
		Metrics:        m,
	}

	return listener.ListenAndServe(ctx, cfg)
}
