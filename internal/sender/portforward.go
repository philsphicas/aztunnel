// Package sender implements the relay-sender modes: port-forward,
// socks5-proxy, and connect (stdin/stdout).
package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// PortForwardConfig holds configuration for port-forward mode.
type PortForwardConfig struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	Target        string // host:port to forward to
	BindAddress   string // local address:port to listen on
	TCPKeepAlive  time.Duration
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // optional; nil disables metrics
	DialTimeout   time.Duration    // total retry budget for the relay dial (0 = single attempt)
}

// PortForward starts a local TCP listener and forwards each connection
// through the relay to the configured target. It blocks until ctx is cancelled.
func PortForward(ctx context.Context, cfg PortForwardConfig) error {
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
	cfg.Logger.Info("port-forward listening", "bind", ln.Addr(), "target", cfg.Target)

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
			if err := forwardConnection(ctx, conn, cfg.Target, cfg); err != nil {
				cfg.Logger.Warn("forward failed", "error", err)
			}
		}()
	}
}

func forwardConnection(ctx context.Context, conn net.Conn, target string, cfg PortForwardConfig) error {
	// Set TCP keepalive on the incoming connection.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	ws, err := cfg.Metrics.InstrumentedDial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, "sender", cfg.DialTimeout, cfg.Logger)
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	// Send envelope and read response.
	if err := sendEnvelopeAndCheck(ctx, ws, target); err != nil {
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		return err
	}

	// Bridge data.
	_, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "sender", target)
	return bridgeErr
}

// sendEnvelopeAndCheck sends a ConnectEnvelope and reads the ConnectResponse.
func sendEnvelopeAndCheck(ctx context.Context, ws *websocket.Conn, target string) error {
	env := protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}
	data, _ := json.Marshal(env) // simple struct, cannot fail
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("send envelope: %w", err)
	}

	_, respData, err := ws.Read(ctx)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var resp protocol.ConnectResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("connection rejected: %s", resp.Error)
	}
	return nil
}

// stdioConn adapts stdin/stdout to net.Conn for use with Bridge.
type stdioConn struct {
	in  io.ReadCloser
	out io.WriteCloser
}

func (c *stdioConn) Read(b []byte) (int, error)       { return c.in.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error)      { return c.out.Write(b) }
func (c *stdioConn) Close() error                     { return errors.Join(c.in.Close(), c.out.Close()) }
func (c *stdioConn) LocalAddr() net.Addr              { return stubAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return stubAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type stubAddr struct{}

func (stubAddr) Network() string { return "stdio" }
func (stubAddr) String() string  { return "stdio" }
