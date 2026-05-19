package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/philsphicas/aztunnel/internal/idgen"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender/socks5"
)

// SOCKS5Config holds configuration for socks5-proxy mode.
type SOCKS5Config struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	ClientOptions relay.ClientOptions
	BindAddress   string // local address:port to listen on
	TCPKeepAlive  time.Duration
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // optional; nil disables metrics
	// Ready, if non-nil, is invoked once after the local bind succeeds
	// and before the accept loop starts. Tests use this to learn the
	// chosen bind address (when BindAddress is :0) without having to
	// open a probe TCP connection. Production callers leave this nil.
	Ready func(net.Addr)
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
	if cfg.Ready != nil {
		cfg.Ready(ln.Addr())
	}

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
			// handleSOCKS5 logs its own per-bridge errors with the
			// bridge_id-bound logger (or with the unbound logger for
			// failures that happen before the SOCKS5 handshake reveals
			// a target). The returned error is surfaced for tests and
			// metrics, not for top-level logging.
			_ = handleSOCKS5(ctx, conn, cfg)
		}()
	}
}

func handleSOCKS5(ctx context.Context, conn net.Conn, cfg SOCKS5Config) error {
	// Set TCP keepalive.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Set a deadline for the SOCKS5 handshake.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Perform SOCKS5 handshake to get the target. No bridge_id is
	// available yet — the per-bridge ID is minted after the target
	// is known.
	target, err := socks5.Handshake(conn)
	if err != nil {
		_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
		err = fmt.Errorf("socks5 handshake: %w", err)
		cfg.Logger.Warn("socks5 failed", "error", err)
		return err
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline

	// Mint the bridge_id once the target is known so the per-bridge
	// logger carries both attributes from this point on.
	bridgeID := idgen.NewBridgeID()
	logger := cfg.Logger.With("bridge_id", bridgeID)
	logger.Info("connection requested", "target", target)

	// Dial the relay.
	ws, err := cfg.Metrics.InstrumentedDial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, cfg.ClientOptions, "sender", logger)
	if err != nil {
		_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
		logger.Warn("socks5 failed", "error", err)
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	// Send envelope and check response.
	if err := sendEnvelopeAndCheck(ctx, ws, target, bridgeID); err != nil {
		_ = socks5.SendReply(conn, socks5RepForError(err), nil)
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		logger.Warn("socks5 failed", "error", err)
		return err
	}

	// Tell the SOCKS5 client we're connected.
	tcpAddr, _ := conn.LocalAddr().(*net.TCPAddr)
	_ = socks5.SendReply(conn, socks5.RepSuccess, tcpAddr)

	// Bridge data.
	_, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "sender", target)
	if bridgeErr != nil {
		logger.Warn("socks5 failed", "error", bridgeErr)
	}
	return bridgeErr
}

// socks5RepForError maps a listener-side connect failure to the
// matching SOCKS5 REP byte. Unclassified failures fall through to
// RepHostUnreachable so the client still sees a non-success REP — the
// historical default before per-code propagation existed.
func socks5RepForError(err error) byte {
	var ce *connectRejected
	if errors.As(err, &ce) {
		switch ce.Code {
		case protocol.CodeConnectionRefused:
			return socks5.RepConnectionRefused
		case protocol.CodeNetworkUnreachable:
			return socks5.RepNetworkUnreachable
		case protocol.CodeHostUnreachable, protocol.CodeTimeout:
			return socks5.RepHostUnreachable
		}
	}
	return socks5.RepHostUnreachable
}
