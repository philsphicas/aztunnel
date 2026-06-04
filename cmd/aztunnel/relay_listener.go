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

	// MaxProtocolVersion is the highest aztunnel relay protocol
	// version this listener will accept from a sender. The default,
	// listener.DefaultListenerMaxProtocolVersion (2), accepts both
	// legacy (v1) and stream-multiplexed (v2) senders. An operator
	// who needs an emergency rollback to v1-only — for example to
	// mitigate a v2-specific bug while a fix ships — sets this to 1.
	//
	// The flag is intentionally NOT bound to an environment variable
	// (unlike its sender counterpart, AZTUNNEL_MAX_PROTOCOL_VERSION).
	// A workstation that exports the env to pin its outbound sender
	// to v1 should not accidentally disable v2 on a listener process
	// it happens to launch as well (systemd units, containers with a
	// shared env, etc.). Operators who genuinely want a fleet-wide
	// listener pin pass --max-protocol-version=1 explicitly.
	MaxProtocolVersion int `name:"max-protocol-version" help:"Highest aztunnel relay protocol version this listener will accept. 1 = legacy per-connection rendezvous only; senders requesting v2 (stream multiplexing) are rejected and fall back. 2 (default) = also accepts v2. Use 1 for an emergency rollback of a listener fleet." default:"${defaultListenerMaxProtocolVersion}"`
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
		Endpoint:           endpoint,
		EntityPath:         hyco,
		TokenProvider:      observeTokenFetch(tp, m, providerName),
		ClientOptions:      opts,
		AllowList:          r.Allow,
		MaxConnections:     r.MaxConnections,
		ConnectTimeout:     r.ConnectTimeout,
		TCPKeepAlive:       r.TCPKeepAlive,
		Logger:             logger,
		Metrics:            m,
		MaxProtocolVersion: r.MaxProtocolVersion,
	}

	return listener.ListenAndServe(ctx, cfg)
}
