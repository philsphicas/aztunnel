package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/arc"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
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
	var setupRan bool
	if err != nil {
		if isHybridConnectivitySetupErr(err) {
			logger.Info("creating Arc HybridConnectivity configuration; the Arc agent may need a moment to register a relay listener")
			setupRan = true
		} else {
			logger.Debug("initial credential request failed, ensuring hybrid connectivity", "error", err)
		}
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
	ws, err := arc.DialWithOptions(ctx, info, arcCmd.Port, logger, arc.DialOptions{ExplainSetup: setupRan})
	m.ObserveDialDuration("sender", time.Since(dialStart).Seconds())
	if err != nil {
		m.ConnectionError("sender", metrics.DialReason(err, metrics.ReasonRelayFailed))
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	logger.Debug("connected to arc relay", "resource", resourceID)

	stdio := &arcStdioConn{in: os.Stdin, out: os.Stdout}
	result, bridgeErr := m.TrackedBridge(ctx, ws, stdio, "sender", target)
	attrs := []any{
		"target", target,
		"cause", result.EndCause,
		"tcp_to_ws", result.Stats.TCPToWS,
		"ws_to_tcp", result.Stats.WSToTCP,
	}
	if bridgeErr != nil {
		attrs = append(attrs, "error", bridgeErr)
	}
	if result.TCPToWS != nil {
		attrs = append(attrs, "tcp_to_ws_err", result.TCPToWS)
	}
	if result.WSToTCP != nil {
		attrs = append(attrs, "ws_to_tcp_err", result.WSToTCP)
	}
	if code, ok := relay.WSCloseCode(bridgeErr); ok {
		attrs = append(attrs, "close_code", code)
	}
	logger.Debug("bridge ended", attrs...)
	return bridgeErr
}

// isHybridConnectivitySetupErr reports whether err is an ARM error that
// indicates the HybridConnectivity endpoint or service configuration needs
// to be created. We treat 404 (ResourceNotFound) and 412 (PreconditionFailed
// — used when the endpoint exists but service config is missing) as setup
// conditions.
func isHybridConnectivitySetupErr(err error) bool {
	var armErr *arc.ARMError
	if !errors.As(err, &armErr) {
		return false
	}
	return armErr.StatusCode == http.StatusNotFound || armErr.StatusCode == http.StatusPreconditionFailed
}
