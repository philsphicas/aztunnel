// Package listener implements the relay-listener: it accepts connections
// from the Azure Relay control channel, reads the connect envelope,
// optionally checks the target against an allowlist, dials the target,
// sends the response, and bridges data bidirectionally.
//
// The listener accepts both v1 (single connection per relay rendezvous)
// and v2 (multiplexed) senders. It inspects the first WebSocket message
// to decide which path to run; this allows mixed-version rolling upgrades.
package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/idgen"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/muxconfig"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/xtaci/smux"
)

// muxStreamReadTimeout bounds reading a per-stream ConnectEnvelope. We
// intentionally allow more time here than the WebSocket-level envelope
// read because each smux stream is opened on demand and may be slow to
// transmit its first frame under load.
const muxStreamReadTimeout = 30 * time.Second

// muxStreamWriteTimeout bounds every WriteStreamResponse on a smux
// stream. Without this, a misbehaving peer that stops reading (or
// fills smux's per-stream send window and never drains it) could
// block the listener indefinitely on its rejection / handshake write —
// either pinning a per-stream goroutine + its pendingSem slot, or
// (on the accept-loop fast-reject path) stalling the accept loop
// itself. 5s is generous for an authenticated peer over Azure Relay
// and far below muxStreamReadTimeout, so a stuck write surfaces as
// an error long before the read-side timeout would.
const muxStreamWriteTimeout = 5 * time.Second

// muxRejectInflightCap bounds the number of concurrent in-flight
// rejection goroutines (rejection write + close) spawned by the
// accept-loop fast-reject path (pendingSem-overflow). Each rejection
// goroutine is bounded individually by muxStreamWriteTimeout, but
// without this cap a peer that keeps opening streams while
// back-pressuring smux's transport (so writes block on the 5s
// deadline) could accumulate one goroutine per opened stream,
// defeating the envelope-pending cap that's meant to control exactly
// this abuse. When the cap is full, the accept loop tears down the
// entire mux session — the only bounded response: we cannot close
// the overflow stream inline (smux's internal 30s openCloseTimeout
// is not bounded by SetWriteDeadline, so close-on-back-pressured-
// transport stalls the accept loop) and we cannot dispatch unbounded
// close goroutines either (each lives up to ~30s; sustained attack
// accumulates them faster than they drain). Session teardown
// triggers defer sess.Close() → wg.Wait(): sess.Close unblocks all
// in-flight stream ops (smux Read/Write/Close observe session-die
// and return immediately), so wg drains promptly. Legitimate bursts
// complete in microseconds (peers that read responses don't block
// on the write deadline) and never fill the cap.
// 32 is large enough for legitimate bursts above pendingSem and
// small enough to bound the rejection goroutine/timer footprint to
// ~256 KiB per session worst-case.
const muxRejectInflightCap = 32

// listenerControlWriteTimeout bounds the time spent writing
// ConnectResponse envelopes on the per-connection rendezvous
// WebSocket (handled by sendResponse). The handler ctx is tied to the
// rendezvous WS lifetime, so without a bounded write deadline a peer
// that stops reading can stall every sendResponse call — including
// capacity-rejection paths that need to release their listener slot
// quickly. 5s is generous over Azure Relay's typical <100 ms write
// latency, far below the ConnectTimeout envelope-read deadline
// (default 30s) so the read-side budget dominates legitimate traffic,
// and consistent with muxStreamWriteTimeout. The timeout applies to
// every sendResponse caller: rejection paths discard the error
// (intent), OK paths observe it and return immediately, releasing
// slots.
const listenerControlWriteTimeout = 5 * time.Second

// Config holds relay-listener configuration.
type Config struct {
	Endpoint       string
	EntityPath     string
	TokenProvider  relay.TokenProvider
	ClientOptions  relay.ClientOptions
	AllowList      []string // Optional target allowlist (CIDR:port patterns)
	MaxConnections int
	ConnectTimeout time.Duration
	TCPKeepAlive   time.Duration
	Logger         *slog.Logger
	Metrics        *metrics.Metrics // optional; nil disables metrics

	// ListenerID is the per-listener-process correlation identifier
	// stamped onto every ConnectResponse this listener sends. Callers
	// should leave this empty; ListenAndServe mints a fresh value at
	// startup. Tests that drive handleConnection directly may set it
	// to a known string for deterministic assertions.
	ListenerID string

	// MaxProtocolVersion is the highest relay protocol version this
	// listener will accept from a sender.
	//
	//   1 = legacy per-connection rendezvous only. Incoming v2
	//       MuxHandshake messages are rejected with the same
	//       "unsupported protocol version" string a pre-mux listener
	//       would emit, so a v2 sender's mux-unavailable fallback path
	//       (internal/sender/muxdialer.go:isMuxUnsupportedRejection)
	//       triggers cleanly and the connection drops back to v1.
	//   2 = also accepts v2 (stream multiplexing) sessions.
	//
	// Zero is normalized to DefaultListenerMaxProtocolVersion in
	// applyDefaults. The flag has no environment-variable binding (see
	// cmd/aztunnel/relay_listener.go) to prevent a stray
	// AZTUNNEL_MAX_PROTOCOL_VERSION env intended for senders from
	// accidentally pinning a listener fleet to v1.
	//
	// Production callers normally leave this at the default. The
	// 0.4.0 default is protocol.MuxVersion (2) so every listener is
	// mux-capable from day one; an operator who needs to roll a
	// listener fleet back to v1 (e.g. emergency mitigation for a
	// v2-only bug) sets MaxProtocolVersion = 1.
	MaxProtocolVersion int

	// dialContext optionally overrides target dialing. When nil,
	// handleConnection uses a net.Dialer honouring ConnectTimeout.
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// RenewInterval is how often the listener renews its SAS/Entra
	// token over the control channel. Zero selects the relay
	// package default (45m). Set a short value in tests that want to
	// exercise a real renew round-trip within an assertion budget.
	RenewInterval time.Duration
}

// applyDefaults fills in zero-valued config fields with their
// runtime defaults and mints a ListenerID if the caller didn't
// provide one. Both ListenAndServe and tests that drive
// handleConnection directly call this so production and test traffic
// walk the same startup path.
//
// applyDefaults also wraps the Logger so every subsequent log line
// emitted by the listener (including those from the relay control
// loop) automatically carries the listener_id attribute. Operators
// reading sender logs can grep the listener log on the same
// listener_id to confirm which listener answered.
func applyDefaults(cfg *Config) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}
	if cfg.TCPKeepAlive == 0 {
		cfg.TCPKeepAlive = 30 * time.Second
	}
	if cfg.ListenerID == "" {
		cfg.ListenerID = idgen.NewListenerID()
	}
	if cfg.MaxProtocolVersion == 0 {
		cfg.MaxProtocolVersion = DefaultListenerMaxProtocolVersion
	}
	cfg.Logger = cfg.Logger.With("listener_id", cfg.ListenerID)
}

// ListenAndServe starts the relay-listener. It blocks until ctx is cancelled.
func ListenAndServe(ctx context.Context, cfg Config) error {
	applyDefaults(&cfg)

	if len(cfg.AllowList) == 0 {
		cfg.Logger.Warn("no allowlist configured, all targets will be permitted")
	}

	// Global stream semaphore: caps total in-flight target connections
	// across all paths (v1 and v2 mux) at MaxConnections. Without a
	// global cap, a v2 mux session could grow up to MaxConnections
	// streams per session and multiple sessions would multiply the
	// budget; this restores v1's "MaxConnections == total target
	// connections" semantics under v2 multi-session deployments.
	//
	// Note: there is also a separate per-rendezvous control-channel cap
	// (ctrlCfg.MaxConnections below) that limits the number of
	// concurrent accepted relay WebSockets. In v1 the two caps fire
	// simultaneously (1 rendezvous = 1 target stream). In v2 mux they
	// are decoupled: with N senders each opening MuxSessions
	// rendezvous, the control cap may bind first (rejecting new
	// rendezvous) even when streamSem has idle slots. Both caps are
	// kept at MaxConnections as a conservative default — operators
	// running large mux-aware deployments may want to size them
	// independently in future.
	streamSem := newConnSemaphore(cfg.MaxConnections)

	// pendingSem bounds the concurrent mux streams that have been
	// accepted but have not yet completed envelope validation. Without
	// this, a malicious peer could open many smux streams that
	// withhold the per-stream envelope, each pinning a goroutine + a
	// read-deadline timer for up to muxStreamReadTimeout. Sized at 2x
	// MaxConnections to give envelope-pending streams headroom over the
	// active-stream cap (so legitimate slow envelopes can't starve the
	// active path). Bypassed when MaxConnections is 0 (unlimited).
	pendingMax := 0
	if cfg.MaxConnections > 0 {
		pendingMax = cfg.MaxConnections * 2
	}
	pendingSem := newConnSemaphore(pendingMax)

	ctrlCfg := relay.ControlConfig{
		Endpoint:       cfg.Endpoint,
		EntityPath:     cfg.EntityPath,
		TokenProvider:  cfg.TokenProvider,
		Options:        cfg.ClientOptions,
		MaxConnections: cfg.MaxConnections,
		Logger:         cfg.Logger,
		RenewInterval:  cfg.RenewInterval,
		Handler: func(ctx context.Context, ws *websocket.Conn) {
			handleConnection(ctx, ws, cfg, streamSem, pendingSem)
		},
	}
	ctrlCfg.OnConnect = func() { cfg.Metrics.SetControlChannelConnected(true) }
	ctrlCfg.OnDisconnect = func() { cfg.Metrics.SetControlChannelConnected(false) }

	return relay.ListenAndServe(ctx, ctrlCfg)
}

func handleConnection(ctx context.Context, ws *websocket.Conn, cfg Config, streamSem, pendingSem *connSemaphore) {
	logger := cfg.Logger
	// Defensive default: tests call handleConnection directly,
	// bypassing applyDefaults. Zero is normalized to the production
	// default so an uninitialised Config still dispatches v2 the way
	// ListenAndServe would.
	if cfg.MaxProtocolVersion == 0 {
		cfg.MaxProtocolVersion = DefaultListenerMaxProtocolVersion
	}

	// Read the first message with a timeout. Both v1 and v2 senders open
	// with a single text JSON message; only the contents differ.
	readCtx, readCancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer readCancel()
	_, data, err := ws.Read(readCtx)
	if err != nil {
		attrs := []any{"error", err}
		if code, ok := relay.WSCloseCode(err); ok {
			attrs = append(attrs, "close_code", code)
		}
		logger.Warn("failed to read envelope", attrs...)
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	// Forward-compatible dispatch: parse as FirstMessage first to learn the
	// version/mode, then re-parse as the appropriate concrete type. JSON's
	// extra-field tolerance means a v1 ConnectEnvelope and a v2 MuxHandshake
	// both decode into FirstMessage cleanly.
	var first protocol.FirstMessage
	if err := json.Unmarshal(data, &first); err != nil {
		logger.Warn("invalid envelope", "error", err)
		_ = sendResponse(ctx, ws, cfg, false, "invalid envelope")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	if first.IsMux() {
		if cfg.MaxProtocolVersion < protocol.MuxVersion {
			logger.Info("rejecting v2 mux handshake (listener max protocol version)",
				"max", cfg.MaxProtocolVersion)
			_ = sendResponse(ctx, ws, cfg, false, "unsupported protocol version")
			cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
			return
		}
		handleMuxSession(ctx, ws, cfg, streamSem, pendingSem)
		return
	}

	// v1 path.
	var env protocol.ConnectEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		logger.Warn("invalid envelope", "error", err)
		_ = sendResponse(ctx, ws, cfg, false, "invalid envelope")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}
	if env.Version != protocol.CurrentVersion {
		logger.Warn("unsupported protocol version", "version", env.Version)
		_ = sendResponse(ctx, ws, cfg, false, "unsupported protocol version")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}
	handleSingleConnection(ctx, ws, env, cfg, streamSem)
}

// handleSingleConnection runs the v1 path: one rendezvous WebSocket, one
// target connection, one bidirectional bridge.
func handleSingleConnection(ctx context.Context, ws *websocket.Conn, env protocol.ConnectEnvelope, cfg Config, streamSem *connSemaphore) {
	logger := cfg.Logger

	if env.Target == "" {
		_ = sendResponse(ctx, ws, cfg, false, "missing target")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	// Bind the sender-minted bridge correlation ID onto the request-
	// scoped logger so every log line on this listener for this bridge
	// carries the same value the sender logs against. An empty value
	// indicates a pre-P5 sender; we bind it anyway (slog emits
	// bridge_id="") so operators see explicit evidence of mixed-version
	// traffic rather than a silently absent attribute.
	logger = logger.With("bridge_id", env.BridgeID)

	logger.Info("connection requested", "target", env.Target)

	if len(cfg.AllowList) > 0 && !isAllowed(env.Target, cfg.AllowList) {
		logger.Warn("target not allowed", "target", env.Target)
		_ = sendResponse(ctx, ws, cfg, false, "target not allowed")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonAllowlistRejected)
		return
	}

	// Reserve a slot in the listener-wide stream cap before dialing.
	// This holds the slot for the entire bridge lifetime, mirroring the
	// per-stream accounting on the v2 path.
	if !streamSem.tryAcquire() {
		logger.Warn("listener at max concurrent streams, rejecting connection")
		_ = sendResponse(ctx, ws, cfg, false, "listener at max concurrent streams")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonListenerAtCapacity)
		return
	}
	defer streamSem.release()

	// Dial the target.
	dial := cfg.dialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: cfg.ConnectTimeout}
		dial = dialer.DialContext
	}
	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	dialStart := time.Now()
	conn, err := dial(dialCtx, "tcp", env.Target)
	cfg.Metrics.ObserveDialDuration("listener", time.Since(dialStart).Seconds())
	if err != nil {
		code := classifyDialError(err)
		logger.Warn("dial target failed", "target", env.Target, "error", err, "code", code)
		_ = sendResponseWithCode(ctx, ws, cfg, false, "connection failed", code)
		cfg.Metrics.ConnectionError("listener", metrics.DialReason(err, metrics.ReasonDialFailed))
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Send success response.
	if err := sendResponse(ctx, ws, cfg, true, ""); err != nil {
		logger.Warn("failed to send response", "error", err)
		return
	}

	// Bridge data.
	result, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "listener", env.Target)
	attrs := []any{
		"target", env.Target,
		"cause", result.EndCause,
		"tcp_to_ws", result.Stats.TCPToWS,
		"ws_to_tcp", result.Stats.WSToTCP,
	}
	if bridgeErr != nil {
		attrs = append(attrs, "error", bridgeErr)
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
	logger.Debug("bridge ended", attrs...)
}

// newConnectResponse builds a protocol.ConnectResponse stamped with the
// listener's ListenerID. Both transport paths (WebSocket-framed v1 via
// sendResponse* and stream-framed v2 via WriteStreamResponse) go through
// this helper so every response, including capacity-reject and dial-fail
// rejections, carries the listener correlation ID.
func newConnectResponse(cfg Config, ok bool, errMsg, code string) protocol.ConnectResponse {
	return protocol.ConnectResponse{
		Version:    protocol.CurrentVersion,
		OK:         ok,
		Error:      errMsg,
		Code:       code,
		ListenerID: cfg.ListenerID,
	}
}

func sendResponse(ctx context.Context, ws *websocket.Conn, cfg Config, ok bool, errMsg string) error {
	return sendResponseWithCode(ctx, ws, cfg, ok, errMsg, "")
}

// handleMuxSession runs the v2 path: one rendezvous WebSocket carries an
// smux session; each accepted stream is an independent target connection.
//
// Streams are gated on two semaphores:
//   - pendingSem (envelope-read DoS bound) is acquired in the accept loop
//     before dispatching to handleMuxStream; rejected if at cap.
//   - streamSem (MaxConnections cap on in-flight target connections) is
//     acquired inside handleMuxStream AFTER envelope validation + allowlist
//     so that slow/withheld envelopes cannot starve legitimate connections
//     of the active-stream budget.
func handleMuxSession(ctx context.Context, ws *websocket.Conn, cfg Config, streamSem, pendingSem *connSemaphore) {
	logger := cfg.Logger
	logger.Info("mux session started")

	// Acknowledge mux support. From this point the WS is owned by smux —
	// nothing else may read/write/ping it directly.
	if err := sendResponse(ctx, ws, cfg, true, ""); err != nil {
		logger.Warn("failed to confirm mux", "error", err)
		return
	}

	netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	sess, err := smux.Server(netConn, muxconfig.SmuxConfig())
	if err != nil {
		logger.Warn("smux server creation failed", "error", err)
		return
	}

	// Defer ordering: registered as (wg.Wait, sess.Close); LIFO means
	// sess.Close runs FIRST on return and wg.Wait runs second. Closing
	// the smux session unblocks any in-flight Read/Write/Close on its
	// streams (they observe sess.die and return io.ErrClosedPipe
	// immediately), so wg drains promptly. This matters for the
	// rejectSem-exhaustion teardown branch in the accept loop below,
	// where we deliberately return to tear the session down; for
	// natural exits (peer-initiated session close) the session is
	// already closed when the accept loop returns, so order is a
	// no-op.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer sess.Close() //nolint:errcheck // best-effort cleanup

	// rejectSem caps concurrent in-flight rejection writers so a peer
	// that back-pressures smux's transport can't accumulate one
	// goroutine per overflow stream. See muxRejectInflightCap for the
	// rationale.
	rejectSem := make(chan struct{}, muxRejectInflightCap)

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if ctx.Err() != nil || sess.IsClosed() {
				logger.Debug("mux session ended")
			} else {
				logger.Warn("mux accept failed", "error", err)
			}
			return
		}

		if !pendingSem.tryAcquire() {
			logger.Warn("listener at envelope-pending cap, rejecting mux stream")
			cfg.Metrics.ConnectionError("listener", metrics.ReasonListenerAtCapacity)
			// Dispatch the rejection write + close to a goroutine so a
			// misbehaving peer that fills smux's per-stream send window
			// (or stops reading) cannot stall the accept loop. Bounded
			// by muxStreamWriteTimeout per write and rejectSem
			// (muxRejectInflightCap) across concurrent writers; tracked
			// via wg so session shutdown drains in-flight rejections.
			// If rejectSem is full, tear down the entire mux session —
			// see muxRejectInflightCap for the rationale.
			select {
			case rejectSem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-rejectSem }()
					defer stream.Close() //nolint:errcheck // best-effort cleanup
					writeMuxRejection(stream, cfg, "listener busy")
				}()
			default:
				logger.Warn("listener rejection cap exhausted, tearing down mux session")
				return
			}
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer stream.Close() //nolint:errcheck // best-effort cleanup
			handleMuxStream(ctx, stream, cfg, streamSem, pendingSem)
		}()
	}
}

// newConnSemaphore is the listener-local copy of relay.newConnSemaphore;
// it accepts max==0 as "unlimited" to preserve MaxConnections semantics.
// We don't reach into relay.connSemaphore because it's unexported.
type connSemaphore struct {
	ch chan struct{}
}

func newConnSemaphore(max int) *connSemaphore {
	if max <= 0 {
		return &connSemaphore{}
	}
	return &connSemaphore{ch: make(chan struct{}, max)}
}

// tryAcquire is non-blocking: it returns true if a slot was reserved,
// false if the semaphore is at capacity. Callers that want to reject
// fast at capacity (the only use today) should call this directly.
func (s *connSemaphore) tryAcquire() bool {
	if s.ch == nil {
		return true
	}
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *connSemaphore) release() {
	if s.ch == nil {
		return
	}
	<-s.ch
}

// held returns the number of slots currently acquired. It exists for
// tests that need to synchronize on semaphore state instead of
// fixed-duration sleeps (a sleep can falsely pass on a slow listener
// without exercising the intended pending state).
func (s *connSemaphore) held() int {
	if s.ch == nil {
		return 0
	}
	return len(s.ch)
}

// handleMuxStream handles one stream of an established mux session. It uses
// length-prefixed framing for the per-stream envelope exchange to avoid
// json.Decoder over-read into immediately-following bytes (e.g. server
// banners like SSH or SMTP, or client-first startup payloads).
//
// The caller (handleMuxSession) acquires pendingSem before dispatching.
// This function is responsible for releasing it — either on early return
// (envelope/version/target/allowlist failure) or as soon as the active-
// stream slot (streamSem, the MaxConnections cap) is acquired. The
// streamSem reservation is held for the entire bridge lifetime so that
// MaxConnections genuinely bounds in-flight target connections; envelope-
// pending streams do not consume from it.
func handleMuxStream(ctx context.Context, stream *smux.Stream, cfg Config, streamSem, pendingSem *connSemaphore) {
	logger := cfg.Logger

	pendingHeld := true
	defer func() {
		if pendingHeld {
			pendingSem.release()
		}
	}()

	_ = stream.SetReadDeadline(time.Now().Add(muxStreamReadTimeout))
	env, err := protocol.ReadStreamEnvelope(stream)
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil {
		logger.Warn("mux stream: failed to read envelope", "error", err)
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}
	if env.Version != protocol.CurrentVersion {
		logger.Warn("mux stream: unsupported version", "version", env.Version)
		writeMuxRejection(stream, cfg, "unsupported protocol version")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	// Bind the sender-minted bridge correlation ID onto the per-stream
	// logger so every log line on this listener for this bridge carries
	// the same value the sender logs against. An empty value indicates
	// a pre-P5 sender on the mux path; we bind it anyway so operators
	// see explicit evidence of mixed-version traffic rather than a
	// silently absent attribute.
	logger = logger.With("bridge_id", env.BridgeID)

	if env.Target == "" {
		writeMuxRejection(stream, cfg, "missing target")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	if len(cfg.AllowList) > 0 && !isAllowed(env.Target, cfg.AllowList) {
		logger.Warn("mux stream: target not allowed", "target", env.Target)
		writeMuxRejection(stream, cfg, "target not allowed")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonAllowlistRejected)
		return
	}

	// Acquire the active-stream slot AFTER envelope validation and
	// allowlist enforcement. This way slow/withheld envelopes (gated
	// by pendingSem) cannot consume MaxConnections budget; only
	// connections that are actually about to dial a target do.
	if !streamSem.tryAcquire() {
		logger.Warn("mux stream: listener at max concurrent streams, rejecting", "target", env.Target)
		writeMuxRejection(stream, cfg, "listener at max concurrent streams")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonListenerAtCapacity)
		return
	}
	defer streamSem.release()

	// We're past the envelope-pending phase — free the pendingSem slot
	// so another stream can start its envelope read while this one
	// occupies an active-stream slot for the bridge lifetime.
	pendingSem.release()
	pendingHeld = false

	logger.Info("connection requested", "target", env.Target)

	// Dial the target via the same seam used by the v1 path so tests
	// can stub one entry point for both transports.
	dial := cfg.dialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: cfg.ConnectTimeout}
		dial = dialer.DialContext
	}
	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	dialStart := time.Now()
	conn, err := dial(dialCtx, "tcp", env.Target)
	cfg.Metrics.ObserveDialDuration("listener", time.Since(dialStart).Seconds())
	if err != nil {
		code := classifyDialError(err)
		logger.Warn("dial target failed", "target", env.Target, "error", err, "code", code)
		writeMuxRejectionWithCode(stream, cfg, "connection failed", code)
		cfg.Metrics.ConnectionError("listener", metrics.DialReason(err, metrics.ReasonDialFailed))
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Bound the success-response write so a peer that fills smux's
	// send window cannot pin this goroutine + its streamSem slot
	// before the bridge takes over. Clear the deadline once the OK
	// is on the wire so the bridge's own deadline logic owns the
	// stream from here on.
	_ = stream.SetWriteDeadline(time.Now().Add(muxStreamWriteTimeout))
	if err := protocol.WriteStreamResponse(stream, newConnectResponse(cfg, true, "", "")); err != nil {
		logger.Warn("mux stream: failed to send response", "error", err)
		return
	}
	_ = stream.SetWriteDeadline(time.Time{})

	result, bridgeErr := cfg.Metrics.TrackedStreamBridge(ctx, stream, conn, "listener", env.Target)
	attrs := []any{
		"target", env.Target,
		"cause", result.EndCause,
		"tcp_to_ws", result.Stats.TCPToWS,
		"ws_to_tcp", result.Stats.WSToTCP,
	}
	if bridgeErr != nil {
		attrs = append(attrs, "error", bridgeErr)
	}
	if result.TCPToWS != nil {
		attrs = append(attrs, "tcp_to_ws_err", result.TCPToWS)
	}
	if result.WSToTCP != nil {
		attrs = append(attrs, "ws_to_tcp_err", result.WSToTCP)
	}
	logger.Debug("bridge ended", attrs...)
}

// writeMuxRejection writes a non-OK ConnectResponse on a per-stream
// rejection path, capped by muxStreamWriteTimeout. Without the
// deadline, a peer that stops reading (or fills smux's per-stream
// send window) can pin the calling goroutine — and, on the accept
// loop's fast-reject path, the accept loop itself — until the smux
// session closes. Errors are intentionally ignored: the caller is
// already on a rejection path and the stream will be closed
// immediately after this returns.
func writeMuxRejection(stream *smux.Stream, cfg Config, errMsg string) {
	writeMuxRejectionWithCode(stream, cfg, errMsg, "")
}

// writeMuxRejectionWithCode is the variant of writeMuxRejection that
// includes a machine-readable code so the sender can map listener-side
// dial failures onto client-visible status (e.g. SOCKS5 REP bytes).
// Used on the v2 dial-failure path so that error-code propagation
// reaches mux-mode senders just like v1 senders. The response carries
// cfg.ListenerID via newConnectResponse so rejection bookkeeping on
// the sender side can correlate which listener answered.
func writeMuxRejectionWithCode(stream *smux.Stream, cfg Config, errMsg, code string) {
	_ = stream.SetWriteDeadline(time.Now().Add(muxStreamWriteTimeout))
	_ = protocol.WriteStreamResponse(stream, newConnectResponse(cfg, false, errMsg, code))
}

// sendResponseWithCode is the variant of sendResponse that includes a
// machine-readable code so the sender can map listener-side dial
// failures onto client-visible status (e.g. SOCKS5 REP bytes).
func sendResponseWithCode(ctx context.Context, ws *websocket.Conn, cfg Config, ok bool, errMsg, code string) error {
	data, _ := json.Marshal(newConnectResponse(cfg, ok, errMsg, code)) // simple struct, cannot fail
	writeCtx, cancel := context.WithTimeout(ctx, listenerControlWriteTimeout)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageText, data)
}

// classifyDialError maps a net.Dial error to one of the protocol Code
// constants. Empty string when no classification applies — the sender
// treats that the same as "generic failure".
//
// The order matters:
//
//   - context.DeadlineExceeded wins so an operator-cancelled dial keeps
//     CodeTimeout regardless of which layer the error originated in.
//   - *net.DNSError is checked before the generic netErr.Timeout()
//     branch because *net.DNSError satisfies net.Error and its
//     Timeout() returns IsTimeout; without this ordering a DNS timeout
//     would be misclassified as the generic CodeTimeout.
//   - Other timeouts (OS-level connect timeouts) are classified by
//     surface error type rather than by errno, because the underlying
//     syscall errno can vary by platform on timeouts.
func classifyDialError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.CodeTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return protocol.CodeDNSTimeout
		}
		return protocol.CodeDNSNotFound
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return protocol.CodeTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return protocol.CodeConnectionRefused
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return protocol.CodeHostUnreachable
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return protocol.CodeNetworkUnreachable
	}
	return ""
}

// isAllowed checks if the target matches the allowlist.
// Allowlist entries can be:
//   - "host:port" — exact string match (no DNS resolution)
//   - "CIDR:port" — CIDR match with exact port
//   - "CIDR:*" — CIDR match with any port
//   - "*" — allow everything
//
// Note: hostname entries are matched literally. Use CIDR notation for
// IP-based restrictions to avoid bypass via IP/hostname mismatch.
func isAllowed(target string, allowList []string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}

	targetIP := net.ParseIP(host)

	for _, entry := range allowList {
		if entry == "*" {
			return true
		}

		aHost, aPort, err := splitAllowEntry(entry)
		if err != nil {
			continue
		}

		// Check port.
		if aPort != "*" && aPort != port {
			continue
		}

		// Check host: try CIDR first, then exact match.
		if _, cidr, err := net.ParseCIDR(aHost); err == nil {
			if targetIP != nil && cidr.Contains(targetIP) {
				return true
			}
		} else if host == aHost {
			return true
		}
	}
	return false
}

// splitAllowEntry parses "host:port" or "CIDR:port" from allowlist format.
// CIDR entries like "10.0.0.0/8:*" need special handling since they
// contain a colon in the CIDR notation.
func splitAllowEntry(entry string) (host, port string, err error) {
	// Find the last colon — the port separator.
	lastColon := -1
	for i := len(entry) - 1; i >= 0; i-- {
		if entry[i] == ':' {
			lastColon = i
			break
		}
	}
	if lastColon < 0 {
		return "", "", fmt.Errorf("no port in allowlist entry: %s", entry)
	}
	return entry[:lastColon], entry[lastColon+1:], nil
}
