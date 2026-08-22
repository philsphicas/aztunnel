package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
)

// PortForwardCmd forwards a local port through the relay.
type PortForwardCmd struct {
	AuthFlags
	LocalListenerFlags
	Bind   string `short:"b" help:"Local bind address:port." default:"127.0.0.1:0"`
	Target string `arg:"" required:"" help:"Target host:port."`
}

// Run executes the port-forward command.
func (p *PortForwardCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(p.Hyco)
	if err != nil {
		return err
	}

	endpoint, opts, tp, providerName, err := resolveAuth(p.AuthFlags)
	if err != nil {
		return err
	}

	bind, err := resolveBindAddress(p.Bind, p.Gateway)
	if err != nil {
		return err
	}
	logger := newLogger(globals.LogLevel)
	warnInsecureTLS(opts, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.PortForwardConfig{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		ClientOptions: opts,
		Target:        p.Target,
		BindAddress:   bind,
		TCPKeepAlive:  p.TCPKeepAlive,
		Logger:        logger,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger); err != nil {
		return err
	}
	cfg.TokenProvider = observeTokenFetch(tp, cfg.Metrics, providerName)

	return sender.PortForward(ctx, cfg)
}
