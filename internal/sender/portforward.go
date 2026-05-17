// Package sender implements the relay-sender modes: port-forward,
// socks5-proxy, and connect (stdin/stdout).
//
// The port-forward and socks5-proxy modes use a persistent multiplexed
// session (smux over a single relay WebSocket) so that each new TCP session
// becomes a cheap smux stream rather than a full Azure Relay rendezvous.
// The mux session is dialed *lazily* on the first connection that needs
// one — so the very first TCP session through the sender still pays the
// full ~1-2 s rendezvous; subsequent connections that reuse an
// already-established mux session drop to milliseconds. When MuxSessions
// > 1 is configured and concurrent traffic forces the pool to grow, each
// additional session is also dialed lazily on its first connection,
// which pays a rendezvous too. Mux can be disabled per-config; the
// listener-side accepts both protocols and the sender automatically
// falls back to v1 if the listener it reaches doesn't speak v2
// (mixed-version rolling deployments).
//
// The connect (stdin/stdout) mode is intentionally v1 only — it carries
// exactly one connection, so multiplexing has no benefit.
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

// muxStreamAdmissionTimeout bounds the per-connection wait inside
// MuxPool.OpenStream so a saturated pool cannot hang the goroutine
// indefinitely on a process-level ctx (as used by the CLI port-forward
// and socks5-proxy paths). Sized to give realistic burst traffic time
// to land a slot while still surfacing genuine saturation through
// aztunnel_mux_pool_saturated_total on caller-deadline expiry. The
// envelope handshake and bridge phases run on the parent ctx so a
// working stream lives as long as the client keeps the connection
// open.
const muxStreamAdmissionTimeout = 60 * time.Second

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
	// DialBudget bounds the per-connection relay dial + retry
	// duration. Zero (the default) uses defaultDialBudget. See
	// issue #94: without a per-connection bound, retry continues
	// after the local app has closed its socket, producing ghost
	// rendezvous when a listener eventually appears.
	DialBudget time.Duration

	// Ready, if non-nil, is invoked once after the local bind succeeds
	// and before the accept loop starts. Tests use this to learn the
	// chosen bind address (when BindAddress is :0) without having to
	// probe with a real TCP dial that would consume a listener slot
	// under MaxConnections. Production callers leave this nil.
	Ready func(net.Addr)

	// MaxProtocolVersion is the highest relay protocol version this
	// sender is willing to attempt against a listener.
	//
	//   1 = legacy per-connection rendezvous. Every TCP session opens
	//       a fresh relay WebSocket; no mux pool is built.
	//   2 = stream multiplexing. Each TCP session becomes an smux
	//       stream over a small pool of long-lived rendezvous
	//       WebSockets (see MuxPool, MuxDialer).
	//
	// Zero is normalized to DefaultSenderMaxProtocolVersion at
	// PortForward entry; values outside [1, protocol.MuxVersion] are
	// clamped silently. A v2 sender against a v1-only listener falls
	// back automatically (sticky muxUnavailableTTL), so requesting v2
	// is always safe — this knob is a ceiling, not a floor.
	MaxProtocolVersion int

	// MuxSessions caps the number of persistent relay rendezvous
	// WebSockets the sender holds open. Larger values may spread
	// concurrent traffic across multiple HA listeners. Defaults to
	// DefaultMuxSessions. Only effective when MaxProtocolVersion >= 2.
	MuxSessions int

	// MaxStreamsPerSession bounds in-flight streams per mux session;
	// callers block (with ctx) when all sessions are at this cap and the
	// pool is at MuxSessions. Defaults to DefaultMaxStreamsPerSession.
	// Only effective when MaxProtocolVersion >= 2.
	MaxStreamsPerSession int

	// MuxStreamHandshakeTimeout caps the per-stream envelope+response
	// exchange. Must exceed the listener's --connect-timeout because
	// the listener dials the target before writing the response.
	// Zero falls back to the package default (60s).
	MuxStreamHandshakeTimeout time.Duration
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
	cfg.MaxProtocolVersion = NormalizeSenderMaxProtocolVersion(cfg.MaxProtocolVersion)
	muxEnabled := cfg.MaxProtocolVersion >= protocol.MuxVersion
	muxSessions := cfg.MuxSessions
	if muxSessions <= 0 {
		muxSessions = DefaultMuxSessions
	}
	maxStreamsPerSession := cfg.MaxStreamsPerSession
	if maxStreamsPerSession <= 0 {
		maxStreamsPerSession = DefaultMaxStreamsPerSession
	}
	cfg.Logger.Info("port-forward listening",
		"bind", ln.Addr(), "target", cfg.Target,
		"maxProtocolVersion", cfg.MaxProtocolVersion,
		"mux", muxEnabled,
		"muxSessions", muxSessions,
		"maxStreamsPerSession", maxStreamsPerSession,
	)
	if cfg.Ready != nil {
		cfg.Ready(ln.Addr())
	}

	var pool *MuxPool
	if muxEnabled {
		pool = NewMuxPool(ctx, MuxPoolOptions{
			Endpoint:             cfg.Endpoint,
			EntityPath:           cfg.EntityPath,
			TokenProvider:        cfg.TokenProvider,
			ClientOptions:        cfg.ClientOptions,
			Logger:               cfg.Logger,
			Metrics:              cfg.Metrics,
			MaxSessions:          cfg.MuxSessions,
			MaxStreamsPerSession: cfg.MaxStreamsPerSession,
		})
		defer pool.Close()
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
			_ = forwardConnection(ctx, conn, cfg.Target, cfg, pool)
		}()
	}
}

func forwardConnection(ctx context.Context, conn net.Conn, target string, cfg PortForwardConfig, pool *MuxPool) error {
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	bridgeID := idgen.NewBridgeID()
	logger := cfg.Logger.With("bridge_id", bridgeID)
	logger.Info("connection requested", "target", target)

	if pool != nil {
		// Bound the mux open/admission wait so a saturated pool can't
		// hang the goroutine indefinitely. The CLI passes the listener-
		// loop ctx here (effectively process-level, no deadline), so
		// without this an OpenStream blocked in the pool's
		// notifyCh/select wait would never release if every session is
		// at MaxStreamsPerSession AND the pool is at MaxSessions — and
		// aztunnel_mux_pool_saturated_total would never fire because
		// the ctx has no deadline. After OpenStream returns, the
		// envelope handshake (capped by MuxStreamHandshakeTimeout) and
		// the bridge use the parent ctx so a working stream lives as
		// long as the client keeps it open.
		admitCtx, admitCancel := context.WithTimeout(ctx, muxStreamAdmissionTimeout)
		stream, err := pool.OpenStream(admitCtx)
		// Capture the admission-ctx state BEFORE admitCancel() — once
		// we cancel, admitCtx.Err() is always non-nil and useless for
		// telling "admission expired / parent done" apart from
		// "internal handshake/setup deadline fired on a sub-context".
		admitDone := admitCtx.Err() != nil
		admitCancel()
		switch {
		case err == nil:
			defer stream.Close() //nolint:errcheck // best-effort cleanup
			listenerID, err := sendEnvelopeOverStream(ctx, stream, target, bridgeID, cfg.MuxStreamHandshakeTimeout)
			if err != nil {
				logRejection(logger, target, listenerID, err)
				cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
				return fmt.Errorf("mux envelope: %w", err)
			}
			logAccept(logger, target, listenerID)
			result, bridgeErr := cfg.Metrics.TrackedStreamBridge(ctx, stream, conn, "sender", target)
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
			if bridgeErr != nil {
				errAttrs := append([]any{"error", bridgeErr}, attrs...)
				logger.Warn("forward failed", errAttrs...)
			} else {
				logger.Debug("bridge ended", attrs...)
			}
			return bridgeErr
		case errors.Is(err, ErrMuxUnsupported):
			logger.Debug("mux unavailable, using v1 path", "target", target)
			// fall through to v1
		default:
			// Filter what we record so we don't double-count or
			// mislabel:
			//   - context.Canceled/DeadlineExceeded BUT ONLY when
			//     admitDone is true — admitCtx fired (admission
			//     timeout) OR the parent ctx already cancelled
			//     before we cancelled admitCtx ourselves. Either
			//     way it's the caller giving up / real saturation,
			//     not a connection error. (An *internal*
			//     mux-handshake timeout also wraps
			//     context.DeadlineExceeded but admitCtx was still
			//     alive when OpenStream returned, so admitDone is
			//     false and the error gets recorded.)
			//   - ErrMuxPoolClosed (admitDone): the pool's
			//     poolCtx fired during caller shutdown — same
			//     class as ctx.Canceled, not a real failure.
			//   - ErrMuxDialFailed: the underlying relay dial
			//     failed and was already recorded by
			//     MuxDialer.connectLocked (which calls
			//     relay.DialWithRetry directly and emits
			//     ConnectionError via DialReason itself, rather
			//     than going through metrics.InstrumentedDial so
			//     it can suppress parent-cancellation cases).
			// Everything else is a genuine "we couldn't open a
			// mux stream" (smux setup, handshake parse,
			// listener rejection that isn't the v1-fallback
			// marker, internal handshake timeout) — record as
			// ReasonMuxOpenFailed.
			callerCancelled := admitDone &&
				(errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) ||
					errors.Is(err, ErrMuxPoolClosed))
			if !callerCancelled && !errors.Is(err, ErrMuxDialFailed) {
				cfg.Metrics.ConnectionError("sender", metrics.ReasonMuxOpenFailed)
			}
			logger.Warn("forward failed", "error", err)
			return fmt.Errorf("open mux stream: %w", err)
		}
	}

	return forwardConnectionV1(ctx, conn, target, cfg, bridgeID, logger)
}

// forwardConnectionV1 is the original per-connection rendezvous path. It
// is used when mux is disabled by config or when the listener has been
// observed to not support v2. The caller is expected to have already
// minted a bridgeID and bound it on logger so logs from this path
// share the same correlation key.
func forwardConnectionV1(ctx context.Context, conn net.Conn, target string, cfg PortForwardConfig, bridgeID string, logger *slog.Logger) error {
	// Per-connection dial budget caps retry duration so a stale
	// local socket can't keep retrying indefinitely (issue #94).
	// The bridge below intentionally uses the original ctx, not
	// dialCtx, so a successful dial isn't torn down by cancelDial.
	dialCtx, cancelDial := context.WithTimeout(ctx, dialBudget(cfg.DialBudget))
	ws, err := cfg.Metrics.InstrumentedDial(dialCtx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, cfg.ClientOptions, "sender", logger)
	cancelDial()
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

// sendEnvelopeAndCheck sends a v1 ConnectEnvelope as a WebSocket text
// message and reads back the ConnectResponse. v1 uses WS message framing
// (one envelope per message), so banner-overread is impossible.
//
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
