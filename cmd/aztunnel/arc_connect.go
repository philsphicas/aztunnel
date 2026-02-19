package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/arc"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/spf13/cobra"
)

func arcConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect",
		Short: "One-shot stdin/stdout connection through an Arc relay",
		Long: `Connect to an Azure Arc-enrolled machine through the automatically
provisioned Azure Relay. Bridges stdin/stdout with the tunnel, then exits
when the connection closes. Designed for use as an SSH ProxyCommand.

Example:
  ssh -o ProxyCommand="aztunnel arc connect --resource-id /subscriptions/.../machines/myVM" user@host`,
		RunE: runArcConnect,
	}
}

func runArcConnect(cmd *cobra.Command, _ []string) error {
	resourceID, err := resolveResourceID(cmd)
	if err != nil {
		return err
	}
	port, _ := cmd.Flags().GetInt("port")
	service, _ := cmd.Flags().GetString("service")
	logLevel, _ := cmd.Flags().GetString("log-level")
	logger := newLogger(logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m, err := resolveMetrics(ctx, cmd, logger)
	if err != nil {
		return err
	}

	client, err := arc.NewClient(logger, nil)
	if err != nil {
		return err
	}

	// Try to get relay credentials directly. If the endpoint doesn't exist
	// yet, create it and retry.
	info, err := client.GetRelayCredentials(ctx, resourceID, service)
	if err != nil {
		logger.Debug("initial credential request failed, ensuring hybrid connectivity", "error", err)
		if ensureErr := client.EnsureHybridConnectivity(ctx, resourceID, service, port); ensureErr != nil {
			return ensureErr
		}
		info, err = client.GetRelayCredentials(ctx, resourceID, service)
		if err != nil {
			return err
		}
	}

	target := fmt.Sprintf("%s:%d", resourceID, port)

	dialStart := time.Now()
	ws, err := arc.DialWithLogger(ctx, info, port, logger)
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
