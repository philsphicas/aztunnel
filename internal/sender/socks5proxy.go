package sender

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender/socks5"
)

// SOCKS5Config holds configuration for socks5-proxy mode.
type SOCKS5Config struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	BindAddress   string // local address:port to listen on
	TCPKeepAlive  time.Duration
	Logger        *slog.Logger
}

// SOCKS5Proxy starts a local SOCKS5 proxy and forwards each connection
// through the relay. The target is determined per-connection from the
// SOCKS5 handshake. It blocks until ctx is cancelled.
func SOCKS5Proxy(ctx context.Context, cfg SOCKS5Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TCPKeepAlive == 0 {
		cfg.TCPKeepAlive = 30 * time.Second
	}

	ln, err := net.Listen("tcp", cfg.BindAddress)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.BindAddress, err)
	}
	defer ln.Close() //nolint:errcheck // best-effort cleanup
	cfg.Logger.Info("socks5-proxy listening", "bind", ln.Addr())

	go func() {
		<-ctx.Done()
		ln.Close() //nolint:errcheck // best-effort cleanup
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cfg.Logger.Warn("accept failed", "error", err)
			continue
		}

		go func() {
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			if err := handleSOCKS5(ctx, conn, cfg); err != nil {
				cfg.Logger.Warn("socks5 failed", "error", err)
			}
		}()
	}
}

func handleSOCKS5(ctx context.Context, conn net.Conn, cfg SOCKS5Config) error {
	// Set TCP keepalive.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Set a deadline for the SOCKS5 handshake.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Perform SOCKS5 handshake to get the target.
	target, err := socks5.Handshake(conn)
	if err != nil {
		_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
		return fmt.Errorf("socks5 handshake: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline

	cfg.Logger.Info("socks5 connect", "target", target)

	// Dial the relay.
	ws, err := relay.Dial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider)
	if err != nil {
		_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
		return err
	}
	defer ws.CloseNow()

	// Send envelope and check response.
	if err := sendEnvelopeAndCheck(ctx, ws, target); err != nil {
		_ = socks5.SendReply(conn, socks5.RepHostUnreachable, nil)
		return err
	}

	// Tell the SOCKS5 client we're connected.
	tcpAddr, _ := conn.LocalAddr().(*net.TCPAddr)
	_ = socks5.SendReply(conn, socks5.RepSuccess, tcpAddr)

	// Bridge data.
	return relay.Bridge(ctx, ws, conn)
}
