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

func socks5ProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "socks5-proxy [hyco]",
		Short: "Run a local SOCKS5 proxy that forwards through the relay",
		Long: `Start a local SOCKS5 proxy server. The target for each connection
is determined from the SOCKS5 handshake and forwarded through the relay.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runSOCKS5Proxy,
	}

	addAuthFlags(cmd)
	cmd.Flags().StringP("bind", "b", "127.0.0.1:1080", "local bind address:port")
	cmd.Flags().Bool("gateway", false, "bind to 0.0.0.0 instead of 127.0.0.1")
	cmd.Flags().Duration("tcp-keepalive", 30*time.Second, "TCP keepalive interval")

	return cmd
}

func runSOCKS5Proxy(cmd *cobra.Command, args []string) error {
	hyco, err := resolveHyco(cmd, args)
	if err != nil {
		return err
	}

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

	cfg := sender.SOCKS5Config{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		BindAddress:   bind,
		TCPKeepAlive:  tcpKeepAlive,
		Logger:        newLogger(logLevel),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return sender.SOCKS5Proxy(ctx, cfg)
}
