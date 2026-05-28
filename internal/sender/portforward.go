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
	"github.com/philsphicas/aztunnel/internal/idgen"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// PortForwardConfig holds configuration for port-forward mode.
type PortForwardConfig struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	ClientOptions relay.ClientOptions
	Target        string // host:port to forward to
	BindAddress   string // local address:port to listen on
	TCPKeepAlive  time.Duration
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // optional; nil disables metrics
	// Ready, if non-nil, is invoked once after the local bind succeeds
	// and before the accept loop starts. Tests use this to learn the
	// chosen bind address (when BindAddress is :0) without having to
	// probe with a real TCP dial that would consume a listener slot
	// under MaxConnections. Production callers leave this nil.
	Ready func(net.Addr)
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
			// forwardConnection logs its own per-bridge errors with
			// the bridge_id-bound logger; the returned error is
			// surfaced for tests and metrics, not for top-level
			// logging.
			_ = forwardConnection(ctx, conn, cfg.Target, cfg)
		}()
	}
}

func forwardConnection(ctx context.Context, conn net.Conn, target string, cfg PortForwardConfig) error {
	// Set TCP keepalive on the incoming connection.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)
	ctx, conn, stopWatch := connBoundContext(ctx, conn)
	defer stopWatch()

	bridgeID := idgen.NewBridgeID()
	logger := cfg.Logger.With("bridge_id", bridgeID)
	logger.Info("connection requested", "target", target)

	ws, err := cfg.Metrics.InstrumentedDial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, cfg.ClientOptions, "sender", logger)
	if err != nil {
		logger.Warn("forward failed", "error", err)
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	// Send envelope and read response.
	listenerID, err := sendEnvelopeAndCheck(ctx, ws, target, bridgeID)
	if err != nil {
		// logRejection already emits a contextual WARN with target,
		// code, and listener_id; do not log "forward failed" on top
		// of it (the doubled WARN obscures rather than clarifies).
		logRejection(logger, target, listenerID, err)
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		return err
	}
	logAccept(logger, target, listenerID)
	stopWatch()

	// Bridge data.
	result, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "sender", target)
	attrs := []any{
		"cause", result.EndCause,
		"tcp_to_ws", result.Stats.TCPToWS,
		"ws_to_tcp", result.Stats.WSToTCP,
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
	if bridgeErr != nil {
		errAttrs := append([]any{"error", bridgeErr}, attrs...)
		logger.Warn("forward failed", errAttrs...)
	} else {
		logger.Debug("bridge ended", attrs...)
	}
	return bridgeErr
}

// sendEnvelopeAndCheck sends a ConnectEnvelope and reads the ConnectResponse.
// On a listener-side rejection (ConnectResponse.OK == false), the returned
// error wraps a *connectRejected carrying both the human-readable message
// and the machine-readable Code. Callers that need to surface the code to
// the client (SOCKS5 sender → REP byte) inspect the wrapped value via
// errors.As; callers that only care about the failure (port-forward) can
// treat it as an opaque error.
//
// bridgeID is the sender-minted correlation ID for this bridge; it is
// propagated to the listener via ConnectEnvelope.BridgeID so logs on
// both ends carry the same value.
//
// The returned listenerID is the value from ConnectResponse.ListenerID:
// non-empty for current-version listeners (success or rejection), empty
// for pre-listener_id listeners or for failures that occur before any
// response was read (write/read/parse errors).
func sendEnvelopeAndCheck(ctx context.Context, ws *websocket.Conn, target, bridgeID string) (string, error) {
	env := protocol.ConnectEnvelope{
		Version:  protocol.CurrentVersion,
		Target:   target,
		BridgeID: bridgeID,
	}
	data, _ := json.Marshal(env) // simple struct, cannot fail
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		return "", fmt.Errorf("send envelope: %w", err)
	}

	_, respData, err := ws.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	var resp protocol.ConnectResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if !resp.OK {
		return resp.ListenerID, &connectRejected{Message: resp.Error, Code: resp.Code, ListenerID: resp.ListenerID}
	}
	return resp.ListenerID, nil
}

// connectRejected is returned from sendEnvelopeAndCheck when the
// listener answers with OK=false. It carries the wire-level Code so
// the SOCKS5 sender can map dial classifications back to REP bytes,
// and the listener_id so operators correlating the rejection back to
// a specific listener instance see the same identifier the listener
// emitted.
type connectRejected struct {
	Message    string
	Code       string
	ListenerID string
}

func (e *connectRejected) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("connection rejected (%s): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("connection rejected: %s", e.Message)
}

// logAccept emits a structured Info on a successful listener accept.
// Centralised alongside logRejection so all three sender entry points
// share the same log shape. listener_id is omitted when empty so
// pre-listener_id listeners (mixed-version traffic) don't show as
// `listener_id=""`.
func logAccept(logger *slog.Logger, target, listenerID string) {
	attrs := []any{"target", target}
	if listenerID != "" {
		attrs = append(attrs, "listener_id", listenerID)
	}
	logger.Info("listener accepted connection", attrs...)
}

// logRejection emits a structured Warn for a sendEnvelopeAndCheck
// failure. Centralised so all three sender entry points share the
// same log shape.
//
// The message and attributes branch on whether the error is a
// listener-side rejection (the listener answered with OK=false) or a
// pre-response transport/parse failure. Mixing the two under a single
// "listener refused connection" message would be operationally
// misleading — a write/read error happens before the listener has a
// chance to accept or refuse anything. Empty listener_id and empty
// code attributes are omitted so older listeners (no listener_id) and
// rejections without a classification don't show as `…=""` in logs.
func logRejection(logger *slog.Logger, target, listenerID string, err error) {
	var ce *connectRejected
	if errors.As(err, &ce) {
		attrs := []any{"target", target, "error", err}
		if ce.ListenerID != "" {
			attrs = append(attrs, "listener_id", ce.ListenerID)
		}
		if ce.Code != "" {
			attrs = append(attrs, "code", ce.Code)
		}
		logger.Warn("listener refused connection", attrs...)
		return
	}
	attrs := []any{"target", target, "error", err}
	if listenerID != "" {
		attrs = append(attrs, "listener_id", listenerID)
	}
	if code, ok := relay.WSCloseCode(err); ok {
		attrs = append(attrs, "close_code", code)
	}
	logger.Warn("envelope exchange failed", attrs...)
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
