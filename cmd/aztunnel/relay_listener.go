package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/spf13/cobra"
)

func relayListenerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay-listener [hyco]",
		Short: "Listen on Azure Relay and forward connections to local targets",
		Long: `Start a relay-listener that accepts connections from the Azure Relay
hybrid connection and forwards them to the targets specified in the
connect envelopes. Optionally restrict targets with --allow.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runRelayListener,
	}

	addAuthFlags(cmd)
	cmd.Flags().StringSlice("allow", nil, "allowed targets (host:port, CIDR:port, CIDR:*)")
	cmd.Flags().Int("max-connections", 0, "max concurrent connections (0 = unlimited)")
	cmd.Flags().Duration("connect-timeout", 30*time.Second, "timeout for dialing targets")
	cmd.Flags().Duration("tcp-keepalive", 30*time.Second, "TCP keepalive interval")

	return cmd
}

func runRelayListener(cmd *cobra.Command, args []string) error {
	hyco, err := resolveHyco(cmd, args)
	if err != nil {
		return err
	}

	endpoint, tp, err := resolveAuth(cmd)
	if err != nil {
		return err
	}

	allow, _ := cmd.Flags().GetStringSlice("allow")
	maxConn, _ := cmd.Flags().GetInt("max-connections")
	connectTimeout, _ := cmd.Flags().GetDuration("connect-timeout")
	tcpKeepAlive, _ := cmd.Flags().GetDuration("tcp-keepalive")

	logLevel, _ := cmd.Flags().GetString("log-level")
	logger := newLogger(logLevel)

	cfg := listener.Config{
		Endpoint:       endpoint,
		EntityPath:     hyco,
		TokenProvider:  tp,
		AllowList:      allow,
		MaxConnections: maxConn,
		ConnectTimeout: connectTimeout,
		TCPKeepAlive:   tcpKeepAlive,
		Logger:         logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return listener.ListenAndServe(ctx, cfg)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
