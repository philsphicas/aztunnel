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
	// DialBudget bounds the per-connection relay dial + retry
	// duration. Zero (the default) uses defaultDialBudget. See
	// issue #94: without a per-connection bound, retry continues
	// after the local app has closed its socket, producing ghost
	// rendezvous when a listener eventually appears.
	DialBudget time.Duration

	// Ready, if non-nil, is invoked once after the local bind succeeds
	// and before the accept loop starts. Tests use this to learn the
	// chosen bind address (when BindAddress is :0) without having to
	// open a probe TCP connection. Production callers leave this nil.
	Ready func(net.Addr)

	// MaxProtocolVersion is the highest relay protocol version this
	// sender is willing to attempt. See PortForwardConfig for full
	// semantics and the version meanings.
	MaxProtocolVersion int

	// MuxSessions and MaxStreamsPerSession are the mux pool sizing knobs.
	// See PortForwardConfig for semantics. Only effective when
	// MaxProtocolVersion >= 2.
	MuxSessions          int
	MaxStreamsPerSession int

	// MuxStreamHandshakeTimeout caps the per-stream envelope+response
	// exchange. See PortForwardConfig.MuxStreamHandshakeTimeout for
	// semantics.
	MuxStreamHandshakeTimeout time.Duration
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
	cfg.Logger.Info("socks5-proxy listening",
		"bind", ln.Addr(),
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
			// handleSOCKS5 logs its own per-bridge errors with the
			// bridge_id-bound logger (or with the unbound logger for
			// failures that happen before the SOCKS5 handshake reveals
			// a target). The returned error is surfaced for tests and
			// metrics, not for top-level logging.
			_ = handleSOCKS5(ctx, conn, cfg, pool)
		}()
	}
}

func handleSOCKS5(ctx context.Context, conn net.Conn, cfg SOCKS5Config, pool *MuxPool) error {
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

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
	_ = conn.SetReadDeadline(time.Time{})

	// Mint the bridge_id once the target is known so the per-bridge
	// logger carries both attributes from this point on.
	bridgeID := idgen.NewBridgeID()
	logger := cfg.Logger.With("bridge_id", bridgeID)
	logger.Info("connection requested", "target", target)

	if pool != nil {
		// Bound the mux open/admission wait so a saturated pool can't
		// hang the SOCKS5 goroutine indefinitely on a process-level
		// ctx (the CLI socks5-proxy path uses the listener-loop ctx,
		// which has no deadline). See the longer rationale in
		// portforward.go's forwardConnection.
		admitCtx, admitCancel := context.WithTimeout(ctx, muxStreamAdmissionTimeout)
		stream, err := pool.OpenStream(admitCtx)
		// Capture admit state before admitCancel() — see the matching
		// note in portforward.go.
		admitDone := admitCtx.Err() != nil
		admitCancel()
		switch {
		case err == nil:
			defer stream.Close() //nolint:errcheck // best-effort cleanup
			listenerID, err := sendEnvelopeOverStream(ctx, stream, target, bridgeID, cfg.MuxStreamHandshakeTimeout)
			if err != nil {
				_ = socks5.SendReply(conn, socks5RepForError(err), nil)
				logRejection(logger, target, listenerID, err)
				cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
				return fmt.Errorf("mux envelope: %w", err)
			}
			logAccept(logger, target, listenerID)
			tcpAddr, _ := conn.LocalAddr().(*net.TCPAddr)
			_ = socks5.SendReply(conn, socks5.RepSuccess, tcpAddr)
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
				logger.Warn("socks5 failed", errAttrs...)
			} else {
				logger.Debug("bridge ended", attrs...)
			}
			return bridgeErr
		case errors.Is(err, ErrMuxUnsupported):
			logger.Debug("mux unavailable, using v1 path", "target", target)
			// fall through to v1
		default:
			_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
			// Filter what we record so we don't double-count or
			// mislabel — see the matching note in
			// portforward.go's forwardConnection. The `admitDone`
			// gate is critical: an *internal* mux-handshake
			// timeout also wraps context.DeadlineExceeded, but
			// admitCtx was still alive when OpenStream returned,
			// so admitDone is false and the error gets recorded.
			// ErrMuxPoolClosed is treated the same as a ctx
			// cancellation when admitDone — it's the pool's
			// poolCtx firing during caller shutdown, not a real
			// connection failure.
			callerCancelled := admitDone &&
				(errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) ||
					errors.Is(err, ErrMuxPoolClosed))
			if !callerCancelled && !errors.Is(err, ErrMuxDialFailed) {
				cfg.Metrics.ConnectionError("sender", metrics.ReasonMuxOpenFailed)
			}
			logger.Warn("socks5 failed", "error", err)
			return fmt.Errorf("open mux stream: %w", err)
		}
	}

	return handleSOCKS5V1(ctx, conn, target, cfg, bridgeID, logger)
}

// handleSOCKS5V1 is the per-connection rendezvous path used when mux is
// disabled or unsupported. The caller is expected to have already
// minted a bridgeID and bound it on logger so logs from this path
// share the same correlation key.
func handleSOCKS5V1(ctx context.Context, conn net.Conn, target string, cfg SOCKS5Config, bridgeID string, logger *slog.Logger) error {
	// Per-connection dial budget caps retry duration so a stale
	// local socket can't keep retrying indefinitely (issue #94).
	// The bridge below intentionally uses the original ctx, not
	// dialCtx, so a successful dial isn't torn down by cancelDial.
	dialCtx, cancelDial := context.WithTimeout(ctx, dialBudget(cfg.DialBudget))
	ws, err := cfg.Metrics.InstrumentedDial(dialCtx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, cfg.ClientOptions, "sender", logger)
	cancelDial()
	if err != nil {
		_ = socks5.SendReply(conn, socks5.RepGeneralFailure, nil)
		logger.Warn("socks5 failed", "error", err)
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	// Send envelope and check response.
	listenerID, err := sendEnvelopeAndCheck(ctx, ws, target, bridgeID)
	if err != nil {
		// logRejection already emits a contextual WARN with target,
		// code, and listener_id; do not log "socks5 failed" on top
		// of it (the doubled WARN obscures rather than clarifies).
		logRejection(logger, target, listenerID, err)
		_ = socks5.SendReply(conn, socks5RepForError(err), nil)
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		return err
	}
	logAccept(logger, target, listenerID)

	tcpAddr, _ := conn.LocalAddr().(*net.TCPAddr)
	_ = socks5.SendReply(conn, socks5.RepSuccess, tcpAddr)

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
		logger.Warn("socks5 failed", errAttrs...)
	} else {
		logger.Debug("bridge ended", attrs...)
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
