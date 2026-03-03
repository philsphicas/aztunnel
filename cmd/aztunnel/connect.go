package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
)

// ConnectCmd connects stdin/stdout through the relay.
type ConnectCmd struct {
	AuthFlags
	Target string `arg:"" required:"" help:"Target host:port."`
}

// Run executes the connect command.
func (c *ConnectCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(c.Hyco)
	if err != nil {
		return err
	}

	endpoint, tp, err := resolveAuth(c.Relay, c.Namespace, c.RelaySuffix)
	if err != nil {
		return err
	}

	logger := newLogger(globals.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.ConnectConfig{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		Target:        c.Target,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Logger:        logger,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger); err != nil {
		return err
	}

	return sender.Connect(ctx, cfg)
}
