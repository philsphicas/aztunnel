package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
)

// Socks5ProxyCmd runs a local SOCKS5 proxy through the relay.
type Socks5ProxyCmd struct {
	AuthFlags
	BindFlags
}

// Run executes the socks5-proxy command.
func (s *Socks5ProxyCmd) Run(globals *Globals) error {
	hyco, err := resolveHyco(s.Hyco)
	if err != nil {
		return err
	}

	endpoint, tp, err := resolveAuth(s.Relay, s.Namespace, s.RelaySuffix)
	if err != nil {
		return err
	}

	bind := s.Bind
	if s.Gateway {
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

	cfg := sender.SOCKS5Config{
		Endpoint:     endpoint,
		EntityPath:   hyco,
		BindAddress:  bind,
		TCPKeepAlive: s.TCPKeepAlive,
		Logger:       logger,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger); err != nil {
		return err
	}
	cfg.TokenProvider = tp

	return sender.SOCKS5Proxy(ctx, cfg)
}
