package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/arc"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// ArcPortForwardCmd forwards a local port through an Arc relay.
type ArcPortForwardCmd struct {
	BindFlags
}

// Run executes the arc port-forward command.
func (p *ArcPortForwardCmd) Run(globals *Globals, arcCmd *ArcCmd) error {
	resourceID, err := resolveResourceID(arcCmd.ResourceID)
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

	m, err := resolveMetrics(ctx, globals.MetricsAddr, globals.MetricsMaxTargets, logger)
	if err != nil {
		return err
	}

	client, err := arc.NewClient(logger, nil)
	if err != nil {
		return err
	}

	// Try to get relay credentials directly. If the endpoint doesn't exist
	// yet, create it. EnsureHybridConnectivity is only called on first
	// failure to avoid disrupting the Arc agent's relay listener.
	if _, err := client.GetRelayCredentials(ctx, resourceID, arcCmd.Service); err != nil {
		logger.Debug("initial credential request failed, ensuring hybrid connectivity", "error", err)
		if ensureErr := client.EnsureHybridConnectivity(ctx, resourceID, arcCmd.Service, arcCmd.Port); ensureErr != nil {
			return ensureErr
		}
	}

	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("listen %s: %w", bind, err)
	}
	defer func() { _ = ln.Close() }()
	logger.Info("arc port-forward listening", "bind", ln.Addr(), "resource", resourceID, "port", arcCmd.Port)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	target := fmt.Sprintf("%s:%d", resourceID, arcCmd.Port)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("accept failed", "error", err)
			continue
		}

		go func() {
			defer func() { _ = conn.Close() }()
			relay.SetTCPKeepAlive(conn, p.TCPKeepAlive)

			// Get fresh credentials for each connection to avoid SAS expiry.
			info, err := client.GetRelayCredentials(ctx, resourceID, arcCmd.Service)
			if err != nil {
				logger.Warn("get relay credentials failed", "error", err)
				m.ConnectionError("sender", metrics.ReasonAuthFailed)
				return
			}

			dialStart := time.Now()
			ws, err := arc.DialWithLogger(ctx, info, arcCmd.Port, logger)
			m.ObserveDialDuration("sender", time.Since(dialStart).Seconds())
			if err != nil {
				logger.Warn("arc relay dial failed", "error", err)
				m.ConnectionError("sender", metrics.DialReason(err, metrics.ReasonRelayFailed))
				return
			}
			defer func() { _ = ws.CloseNow() }()

			_, bridgeErr := m.TrackedBridge(ctx, ws, conn, "sender", target)
			if bridgeErr != nil {
				logger.Debug("bridge ended", "error", bridgeErr)
			}
		}()
	}
}
