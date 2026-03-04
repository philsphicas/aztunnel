package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
)

// PortForwardCmd forwards a local port through the relay.
type PortForwardCmd struct {
	AuthFlags
	BindFlags
	Target string `arg:"" required:"" help:"Target host:port."`
}

// Run executes the port-forward command.
func (p *PortForwardCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(p.Hyco)
	if err != nil {
		return err
	}

	endpoint, tp, err := resolveAuth(p.Relay, p.Namespace, p.RelaySuffix)
	if err != nil {
		return err
	}

	bind := p.Bind
	if p.Gateway {
		_, port, err := net.SplitHostPort(bind)
		if err != nil {
			return fmt.Errorf("invalid --bind address %q: %w", bind, err)
		}
		if port == "" {
			port = "0"
		}
		bind = "0.0.0.0:" + port
	}
	logger := newLogger(globals.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.PortForwardConfig{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		Target:        p.Target,
		BindAddress:   bind,
		TCPKeepAlive:  p.TCPKeepAlive,
		Logger:        logger,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger); err != nil {
		return err
	}

	return sender.PortForward(ctx, cfg)
}
