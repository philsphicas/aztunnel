package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/arc"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/spf13/cobra"
)

func arcPortForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port-forward",
		Short: "Forward a local port through an Arc relay",
		Long: `Start a local TCP listener and forward each connection through the
Azure Arc managed relay to the remote service.

Example:
  aztunnel arc port-forward --resource-id /subscriptions/.../machines/myVM -b 127.0.0.1:2222
  ssh -p 2222 user@127.0.0.1`,
		RunE: runArcPortForward,
	}

	cmd.Flags().StringP("bind", "b", "127.0.0.1:0", "local bind address:port")
	cmd.Flags().Bool("gateway", false, "bind to 0.0.0.0 instead of 127.0.0.1")
	cmd.Flags().Duration("tcp-keepalive", 30*time.Second, "TCP keepalive interval")

	return cmd
}

func runArcPortForward(cmd *cobra.Command, _ []string) error {
	resourceID, err := resolveResourceID(cmd)
	if err != nil {
		return err
	}
	port, _ := cmd.Flags().GetInt("port")
	service, _ := cmd.Flags().GetString("service")
	bind, _ := cmd.Flags().GetString("bind")
	gateway, _ := cmd.Flags().GetBool("gateway")
	if gateway {
		_, p, _ := net.SplitHostPort(bind)
		if p == "" {
			p = "0"
		}
		bind = "0.0.0.0:" + p
	}
	tcpKeepAlive, _ := cmd.Flags().GetDuration("tcp-keepalive")
	logLevel, _ := cmd.Flags().GetString("log-level")
	logger := newLogger(logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := arc.NewClient(logger, nil)
	if err != nil {
		return err
	}

	// Try to get relay credentials directly. If the endpoint doesn't exist
	// yet, create it. EnsureHybridConnectivity is only called on first
	// failure to avoid disrupting the Arc agent's relay listener.
	if _, err := client.GetRelayCredentials(ctx, resourceID, service); err != nil {
		logger.Debug("initial credential request failed, ensuring hybrid connectivity", "error", err)
		if ensureErr := client.EnsureHybridConnectivity(ctx, resourceID, service, port); ensureErr != nil {
			return ensureErr
		}
	}

	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("listen %s: %w", bind, err)
	}
	defer func() { _ = ln.Close() }()
	logger.Info("arc port-forward listening", "bind", ln.Addr(), "resource", resourceID, "port", port)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

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
			relay.SetTCPKeepAlive(conn, tcpKeepAlive)

			// Get fresh credentials for each connection to avoid SAS expiry.
			info, err := client.GetRelayCredentials(ctx, resourceID, service)
			if err != nil {
				logger.Warn("get relay credentials failed", "error", err)
				return
			}

			ws, err := arc.DialWithLogger(ctx, info, port, logger)
			if err != nil {
				logger.Warn("arc relay dial failed", "error", err)
				return
			}
			defer func() { _ = ws.CloseNow() }()

			if err := relay.Bridge(ctx, ws, conn); err != nil {
				logger.Debug("bridge ended", "error", err)
			}
		}()
	}
}
