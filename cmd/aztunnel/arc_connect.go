package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/arc"
	"github.com/philsphicas/aztunnel/internal/metrics"
)

// ArcConnectCmd connects stdin/stdout through an Arc relay.
type ArcConnectCmd struct{}

// Run executes the arc connect command.
func (c *ArcConnectCmd) Run(globals *Globals, arcCmd *ArcCmd) error {
	resourceID, err := resolveResourceID(arcCmd.ResourceID)
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

	client, err := arc.NewClient(logger, nil)
	if err != nil {
		return err
	}

	// Try to get relay credentials directly. If the endpoint doesn't exist
	// yet, create it and retry.
	info, err := client.GetRelayCredentials(ctx, resourceID, arcCmd.Service)
	if err != nil {
		logger.Debug("initial credential request failed, ensuring hybrid connectivity", "error", err)
		if ensureErr := client.EnsureHybridConnectivity(ctx, resourceID, arcCmd.Service, arcCmd.Port); ensureErr != nil {
			return ensureErr
		}
		info, err = client.GetRelayCredentials(ctx, resourceID, arcCmd.Service)
		if err != nil {
			return err
		}
	}

	target := fmt.Sprintf("%s:%d", resourceID, arcCmd.Port)

	dialStart := time.Now()
	ws, err := arc.DialWithLogger(ctx, info, arcCmd.Port, logger)
	m.ObserveDialDuration("sender", time.Since(dialStart).Seconds())
	if err != nil {
		m.ConnectionError("sender", metrics.DialReason(err, metrics.ReasonRelayFailed))
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	logger.Debug("connected to arc relay", "resource", resourceID)

	stdio := &arcStdioConn{in: os.Stdin, out: os.Stdout}
	_, bridgeErr := m.TrackedBridge(ctx, ws, stdio, "sender", target)
	return bridgeErr
}
