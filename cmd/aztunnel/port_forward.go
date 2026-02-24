package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/spf13/cobra"
)

func portForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port-forward <host:port>",
		Short: "Forward a local port through the relay to a specific target",
		Long: `Start a local TCP listener and forward each connection through the
Azure Relay to the specified target host:port.`,
		Args: cobra.ExactArgs(1),
		RunE: runPortForward,
	}

	addAuthFlags(cmd)
	cmd.Flags().StringP("bind", "b", "127.0.0.1:0", "local bind address:port")
	cmd.Flags().Bool("gateway", false, "bind to 0.0.0.0 instead of 127.0.0.1")
	cmd.Flags().Duration("tcp-keepalive", 30*time.Second, "TCP keepalive interval")
	cmd.Flags().Duration("dial-timeout", 30*time.Second, "total time budget for relay dial retries (0 = single attempt)")

	return cmd
}

func runPortForward(cmd *cobra.Command, args []string) error {
	hyco, err := resolveHyco(cmd, nil)
	if err != nil {
		return err
	}
	target := args[0]

	endpoint, tp, err := resolveAuth(cmd)
	if err != nil {
		return err
	}

	bind, _ := cmd.Flags().GetString("bind")
	gateway, _ := cmd.Flags().GetBool("gateway")
	if gateway {
		_, port, _ := net.SplitHostPort(bind)
		if port == "" {
			port = "0"
		}
		bind = "0.0.0.0:" + port
	}
	tcpKeepAlive, _ := cmd.Flags().GetDuration("tcp-keepalive")
	logLevel, _ := cmd.Flags().GetString("log-level")
	logger := newLogger(logLevel)
	dialTimeout, _ := cmd.Flags().GetDuration("dial-timeout")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.PortForwardConfig{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		Target:        target,
		BindAddress:   bind,
		TCPKeepAlive:  tcpKeepAlive,
		Logger:        logger,
		DialTimeout:   dialTimeout,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, cmd, logger); err != nil {
		return err
	}

	return sender.PortForward(ctx, cfg)
}
