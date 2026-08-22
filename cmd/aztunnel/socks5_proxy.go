package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
)

// Socks5ProxyCmd runs a local SOCKS5 proxy through the relay.
type Socks5ProxyCmd struct {
	AuthFlags
	LocalListenerFlags
	Bind string `short:"b" help:"Local bind address:port." default:"127.0.0.1:1080"`
}

// Run executes the socks5-proxy command.
func (s *Socks5ProxyCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(s.Hyco)
	if err != nil {
		return err
	}

	endpoint, opts, tp, providerName, err := resolveAuth(s.AuthFlags)
	if err != nil {
		return err
	}

	bind, err := resolveBindAddress(s.Bind, s.Gateway)
	if err != nil {
		return err
	}
	logger := newLogger(globals.LogLevel)
	warnInsecureTLS(opts, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.SOCKS5Config{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		ClientOptions: opts,
		BindAddress:   bind,
		TCPKeepAlive:  s.TCPKeepAlive,
		Logger:        logger,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger); err != nil {
		return err
	}
	cfg.TokenProvider = observeTokenFetch(tp, cfg.Metrics, providerName)

	return sender.SOCKS5Proxy(ctx, cfg)
}
