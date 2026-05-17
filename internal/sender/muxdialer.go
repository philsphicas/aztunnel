package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/muxconfig"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/xtaci/smux"
)

const (
	// muxHandshakeTimeout bounds the v2 handshake (envelope + response)
	// on the outer WebSocket. Independent of per-stream operations.
	muxHandshakeTimeout = 15 * time.Second

	// muxStreamHandshakeTimeout is the default budget for the per-stream
	// envelope+response exchange after a fresh smux stream is opened on
	// an established mux session. Without this bound, a peer that
	// accepts the smux stream but never replies (crash, hang, malicious
	// actor) would pin the sender's pool slot indefinitely on
	// ReadStreamResponse. The deadline is cleared once the response
	// arrives so the bridge can run with no time bound.
	//
	// This cap MUST exceed the listener's worst-case target-dial budget
	// (cfg.ConnectTimeout, default 30s in listener.Config), because the
	// listener dials the target before writing the response. Operators
	// who configure the listener with `--connect-timeout` above the
	// default should also raise the sender's
	// `--mux-stream-handshake-timeout` (env
	// AZTUNNEL_MUX_STREAM_HANDSHAKE_TIMEOUT) to match.
	muxStreamHandshakeTimeout = 60 * time.Second

	// muxUnavailableTTL is how long the dialer remembers that a listener
	// rejected the v2 handshake. During this window OpenStream returns
	// ErrMuxUnsupported immediately so the caller falls back to v1
	// without re-dialing the relay.
	muxUnavailableTTL = 60 * time.Second
)

// ErrMuxUnsupported is returned by MuxDialer.OpenStream when the listener
// the sender reached does not understand the v2 mux protocol. Callers
// should retry the same logical TCP session through the v1 single-
// connection path.
var ErrMuxUnsupported = errors.New("mux: peer does not support mux protocol")

// ErrMuxDialerClosed is returned by MuxDialer.OpenStream after the dialer's
// parentCtx has been cancelled (for example, by MuxPool eviction during
// graceful rotation). It signals "this dialer is permanently dead; do not
// retry on me".
var ErrMuxDialerClosed = errors.New("mux: dialer closed")

// ErrMuxDialFailed is wrapped around dial/handshake errors that originate
// from the underlying relay dial (i.e. were already recorded as a connection
// error by MuxDialer.connectLocked via DialReason — the mux path calls
// relay.DialWithRetry directly and emits ConnectionError itself rather than
// going through metrics.InstrumentedDial, so it can suppress the dial-error
// metric when the cause is parentCtx cancellation). Upstream callers should
// check errors.Is(err, ErrMuxDialFailed) before recording their own
// ConnectionError, to avoid double-counting a single dial failure.
var ErrMuxDialFailed = errors.New("mux: relay dial failed")

// MuxDialer maintains a persistent smux session over a single relay
// WebSocket. Each call to OpenStream returns a new logical net.Conn
// (smux stream) that carries one tunneled TCP session.
//
// The session is established lazily on the first OpenStream call and
// re-established transparently if the underlying WebSocket dies. If the
// listener does not support the v2 mux protocol, OpenStream returns
// ErrMuxUnsupported for muxUnavailableTTL so callers can fall back to v1
// without re-paying the rendezvous cost.
type MuxDialer struct {
	mu         sync.Mutex
	session    *smux.Session
	ws         *websocket.Conn
	parentCtx  context.Context
	endpoint   string
	entityPath string
	tp         relay.TokenProvider
	clientOpts relay.ClientOptions
	logger     *slog.Logger
	metrics    *metrics.Metrics
	// muxUnavailUntil holds the sticky v1-fallback expiry as unix-nano
	// timestamps so Unavailable() can be a lock-free read. Stored as
	// wall-clock time (not monotonic); the 60s TTL means clock jumps
	// have negligible practical impact.
	muxUnavailUntil atomic.Int64
}

// NewMuxDialer constructs a MuxDialer. The provided context governs the
// lifetime of the eventual smux session (not individual OpenStream calls);
// cancelling it tears down the session and frees all in-flight streams.
func NewMuxDialer(ctx context.Context, endpoint, entityPath string, tp relay.TokenProvider, opts relay.ClientOptions, logger *slog.Logger, m *metrics.Metrics) *MuxDialer {
	if logger == nil {
		logger = slog.Default()
	}
	return &MuxDialer{
		parentCtx:  ctx,
		endpoint:   endpoint,
		entityPath: entityPath,
		tp:         tp,
		clientOpts: opts,
		logger:     logger,
		metrics:    m,
	}
}

// OpenStream returns a new smux stream over the persistent mux session,
// connecting the session lazily if needed. The supplied ctx bounds the
// connect handshake but does not govern the lifetime of the returned
// stream — the caller's bridge logic owns that.
//
// Returns ErrMuxUnsupported if the listener rejected the v2 handshake;
// the rejection is sticky for muxUnavailableTTL so concurrent callers
// fall back to v1 quickly.
func (d *MuxDialer) OpenStream(ctx context.Context) (net.Conn, error) {
	// Fast path: parentCtx cancellation makes this dialer permanently
	// dead. The pool cancels each session's ctx when evicting (during
	// rotation or after ErrMuxUnsupported), so any in-flight OpenStream
	// that raced past the pool's selector must NOT reconnect on this
	// opener — that would create an untracked zombie relay rendezvous.
	if err := d.parentCtx.Err(); err != nil {
		return nil, ErrMuxDialerClosed
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Re-check after acquiring the lock to close a TOCTOU race with
	// concurrent ctx cancellation.
	if err := d.parentCtx.Err(); err != nil {
		return nil, ErrMuxDialerClosed
	}

	if d.isUnavailableNow() {
		return nil, ErrMuxUnsupported
	}

	if d.session != nil && !d.session.IsClosed() {
		if stream, err := d.session.OpenStream(); err == nil {
			// Final parentCtx re-check before returning. Pool
			// eviction cancels parentCtx WITHOUT needing d.mu,
			// so it can fire between the lock-acquire check
			// above and here. Returning a stream from a
			// logically-evicted session would surface as an
			// opaque smux read/write failure to the caller
			// instead of triggering the pool's retry-on-
			// ErrMuxDialerClosed loop.
			if err := d.parentCtx.Err(); err != nil {
				_ = stream.Close()
				return nil, ErrMuxDialerClosed
			}
			// Honor the caller's per-call ctx too: the smux
			// OpenStream call above did not consult it (smux
			// has no ctx-aware OpenStream), so an admission
			// deadline that expired during that call would
			// otherwise return a stream past the deadline.
			// Close the stream and surface ctx.Err() so the
			// pool's retry/admission accounting can recognize
			// it (see muxpool.go's ctx-typed-error branch).
			if err := ctx.Err(); err != nil {
				_ = stream.Close()
				return nil, err
			}
			return stream, nil
		} else {
			d.logger.Debug("mux session broken, reconnecting", "error", err)
			d.closeSessionLocked()
		}
	} else if d.session != nil {
		// Session was remotely closed (IsClosed() == true). Tear down
		// our refs so the new connect doesn't leak the
		// mux_sessions_active gauge (closeSessionLocked decrements it)
		// or hold the dead WebSocket reference.
		d.closeSessionLocked()
	}

	if err := d.connectLocked(ctx); err != nil {
		return nil, err
	}

	stream, err := d.session.OpenStream()
	if err != nil {
		d.closeSessionLocked()
		return nil, fmt.Errorf("open stream after connect: %w", err)
	}
	// Same parentCtx re-check as the fast path: the session we just
	// created could have been evicted between connectLocked's final
	// guard and this OpenStream call.
	if err := d.parentCtx.Err(); err != nil {
		_ = stream.Close()
		return nil, ErrMuxDialerClosed
	}
	// Caller-ctx re-check, mirroring the fast path. connectLocked
	// honors ctx during the relay dial/handshake, but the smux
	// OpenStream call above does not — so a ctx that expired between
	// connectLocked returning and OpenStream completing would
	// otherwise produce a stream past the caller's deadline.
	if err := ctx.Err(); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

// Close tears down the underlying mux session and WebSocket. Safe to call
// multiple times.
func (d *MuxDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeSessionLocked()
}

// Unavailable reports whether the dialer is in its sticky v1-fallback
// window — i.e. a recent handshake was rejected with a v1-style error.
// Pool implementations use this to skip the dialer when selecting a
// session for a new stream.
//
// This is a lock-free atomic read. The pool calls Unavailable() while
// holding its own p.mu, and the dialer mutex (d.mu) may be held for
// the duration of a long relay dial; making this lock-free prevents
// the pool from blocking behind in-flight rendezvous.
func (d *MuxDialer) Unavailable() bool {
	return d.isUnavailableNow()
}

// isUnavailableNow returns true iff the sticky v1-fallback marker is
// set and has not yet expired. Lock-free.
func (d *MuxDialer) isUnavailableNow() bool {
	until := d.muxUnavailUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}

// markUnavailable arms the sticky v1-fallback marker for muxUnavailableTTL
// from now. Called when a listener rejects the v2 handshake.
func (d *MuxDialer) markUnavailable() {
	d.muxUnavailUntil.Store(time.Now().Add(muxUnavailableTTL).UnixNano())
}

func (d *MuxDialer) closeSessionLocked() {
	if d.session != nil {
		_ = d.session.Close()
		d.session = nil
		d.metrics.MuxSessionClosed("sender")
	}
	if d.ws != nil {
		_ = d.ws.CloseNow()
		d.ws = nil
	}
}

func (d *MuxDialer) connectLocked(ctx context.Context) (retErr error) {
	// Tie the dial/handshake ctx to BOTH the caller's ctx and
	// d.parentCtx. Without this, an eviction (which cancels
	// d.parentCtx via the pool's sessCancel) cannot abort an
	// in-flight dial — opener.Close() would block behind d.mu until
	// the caller's ctx expired, and a late-completing dial could
	// publish an untracked session into d.session while the dialer
	// has already been evicted by the pool.
	dialCtx, dialCancel := mergeCancelCtx(ctx, d.parentCtx)
	defer dialCancel()

	// Rewrite any error returned from dial/handshake into
	// ErrMuxDialerClosed if d.parentCtx is cancelled. The pool's
	// retry loop in OpenStream only treats ErrMuxDialerClosed as a
	// retryable eviction signal, so a parent-cancellation that
	// surfaces as e.g. "dial relay: ... context canceled" would
	// otherwise be returned to the caller as a hard failure instead
	// of triggering a retry against a fresh session.
	defer func() {
		if retErr != nil && retErr != ErrMuxUnsupported && d.parentCtx.Err() != nil {
			retErr = ErrMuxDialerClosed
		}
	}()

	// Dial via relay.DialWithRetry directly so we can suppress the
	// dial-error metric when the failure is caused by parentCtx
	// cancellation (internal eviction/rotation/shutdown). Using
	// metrics.InstrumentedDial here would emit a connection_errors_total
	// sample with reason=relay_failed BEFORE we have a chance to
	// reclassify the parent-cancel case via the defer above.
	dialStart := time.Now()
	ws, err := relay.DialWithRetry(dialCtx, d.endpoint, d.entityPath, d.tp, d.clientOpts, d.logger)
	d.metrics.ObserveDialDuration("sender", time.Since(dialStart).Seconds())
	if err != nil {
		if d.parentCtx.Err() == nil {
			// Real dial failure — record so it's not lost when
			// the defer below leaves ErrMuxDialFailed unwrapped
			// to the caller.
			d.metrics.ConnectionError("sender", metrics.DialReason(err, metrics.ReasonRelayFailed))
		}
		// Wrap with ErrMuxDialFailed so upstream callers can
		// distinguish "already counted" from other open failures
		// (e.g. handshake parse). Avoids double-counting connection
		// errors.
		return fmt.Errorf("%w: %w", ErrMuxDialFailed, err)
	}

	// Outer handshake (MuxHandshake + ConnectResponse) uses WebSocket
	// message framing — smux is not yet running on this conn. Per-stream
	// envelopes will use length-prefixed framing inside each smux stream.
	hsCtx, cancel := context.WithTimeout(dialCtx, muxHandshakeTimeout)
	defer cancel()

	hs := protocol.MuxHandshake{
		Version: protocol.MuxVersion,
		Mode:    protocol.MuxMode,
	}
	hsData, _ := json.Marshal(hs)
	if err := ws.Write(hsCtx, websocket.MessageText, hsData); err != nil {
		_ = ws.CloseNow()
		return fmt.Errorf("send mux handshake: %w", err)
	}

	_, respData, err := ws.Read(hsCtx)
	if err != nil {
		_ = ws.CloseNow()
		return fmt.Errorf("read mux response: %w", err)
	}
	var resp protocol.ConnectResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		_ = ws.CloseNow()
		return fmt.Errorf("parse mux response: %w", err)
	}
	if !resp.OK {
		_ = ws.CloseNow()
		if isMuxUnsupportedRejection(resp.Error) {
			d.markUnavailable()
			d.logger.Warn("listener does not support mux protocol; falling back to v1",
				"error", resp.Error, "fallbackTTL", muxUnavailableTTL)
			return ErrMuxUnsupported
		}
		return fmt.Errorf("mux handshake rejected: %s", resp.Error)
	}

	// Hand the WS over to smux. From this point nothing else may read,
	// write, or ping the underlying WS — smux owns it.
	netConn := websocket.NetConn(d.parentCtx, ws, websocket.MessageBinary)
	sess, err := smux.Client(netConn, muxconfig.SmuxConfig())
	if err != nil {
		_ = ws.CloseNow()
		return fmt.Errorf("create smux client: %w", err)
	}

	// One final parentCtx check before publishing the session into
	// d.session. If the pool evicted this dialer while we were
	// dialing/handshaking, we must NOT publish — otherwise we leak
	// the gauge and create an untracked session that the pool no
	// longer references.
	if err := d.parentCtx.Err(); err != nil {
		_ = sess.Close()
		_ = ws.CloseNow()
		return ErrMuxDialerClosed
	}

	d.session = sess
	d.ws = ws
	d.metrics.MuxSessionOpened("sender")
	d.logger.Info("mux session established")

	// Spawn an async-close watcher so the mux_sessions_active gauge is
	// decremented promptly when the smux session dies on its own (smux
	// keepalive timeout, peer-initiated close, transport-level read
	// failure). Without this, an idle MuxDialer with a dead session
	// would report active==1 until the next OpenStream call (which
	// takes the IsClosed() branch and decrements via
	// closeSessionLocked) or until pool eviction. The watcher is
	// idempotent with respect to the synchronous close paths: if
	// closeSessionLocked has already replaced d.session by the time
	// the watcher fires, the d.session==sess check skips the
	// double-decrement.
	go d.watchSession(sess)

	return nil
}

// watchSession waits for the smux session to die (peer-initiated close,
// transport read/write error, smux keepalive timeout, or local Close)
// and then decrements the active-session gauge — provided d.session
// still references the same session (i.e. closeSessionLocked from
// another path hasn't already handled it).
//
// We use AcceptStream rather than CloseChan because smux's CloseChan
// only fires on an explicit Close() call; internal transport errors
// (detected by recvLoop) set chSocketReadError and break AcceptStream,
// but do NOT close die / fire CloseChan. AcceptStream is the only
// public signal that responds to BOTH the explicit-close and async
// transport-failure paths.
//
// Our protocol is asymmetric — only the sender (this side) opens
// streams. A peer-initiated stream is therefore a protocol violation;
// we close it defensively and continue watching.
func (d *MuxDialer) watchSession(sess *smux.Session) {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			break
		}
		_ = stream.Close()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == sess {
		d.closeSessionLocked()
	}
}

// mergeCancelCtx returns a new context that is cancelled when EITHER
// of the input contexts is cancelled. The returned cancel func MUST be
// called to release the watcher goroutine.
//
// Used to scope the dial/handshake to both the caller's ctx and the
// dialer's parentCtx, so that pool eviction (which cancels parentCtx)
// aborts a slow in-flight dial instead of waiting for the caller to
// give up.
func mergeCancelCtx(a, b context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	var once sync.Once
	return merged, func() {
		once.Do(func() { close(stop) })
		cancel()
	}
}

// isMuxUnsupportedRejection inspects a listener's ConnectResponse.Error to
// decide whether the rejection means the listener doesn't speak v2 (and so
// the sender should fall back to v1). A v1 listener parses MuxHandshake
// as a ConnectEnvelope, sees Target=="" and responds "missing target", and
// also rejects unknown protocol versions with "unsupported protocol version".
func isMuxUnsupportedRejection(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "unsupported protocol version") ||
		strings.Contains(low, "missing target") ||
		strings.Contains(low, "invalid envelope")
}

// sendEnvelopeOverStream performs the per-stream handshake using length-
// prefixed framing. CRITICAL: do NOT use json.NewDecoder here — its
// internal buffer would consume bytes that arrive immediately after the
// response (server banners like SSH or SMTP, or client-first startup
// payloads). See protocol.ReadStreamResponse.
//
// The envelope exchange is bounded by handshakeTimeout (or the caller's
// ctx deadline, whichever is sooner) and by ctx cancellation. If
// handshakeTimeout is zero, the default muxStreamHandshakeTimeout const
// is used. Without this bound, a peer that accepts the smux stream but
// never replies could block ReadStreamResponse indefinitely and pin the
// pool slot until process shutdown. On success the deadline is cleared
// so the bridge runs without a time bound.
//
// The cap must exceed the listener's worst-case target-dial budget
// (cfg.ConnectTimeout, default 30s in listener.Config) because the
// listener dials the target before writing the response. Operators
// who configure the listener with `--connect-timeout` above the default
// should also raise the sender's mux-stream-handshake-timeout to match.
//
// bridgeID is propagated to the listener via ConnectEnvelope.BridgeID so
// logs on both ends carry the same value. The returned listenerID is the
// value from ConnectResponse.ListenerID: non-empty for current-version
// listeners (success or rejection), empty for pre-listener_id listeners
// or for failures that occur before any response was read.
func sendEnvelopeOverStream(ctx context.Context, stream net.Conn, target, bridgeID string, handshakeTimeout time.Duration) (string, error) {
	if handshakeTimeout <= 0 {
		handshakeTimeout = muxStreamHandshakeTimeout
	}
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := stream.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("set envelope deadline: %w", err)
	}

	// Watchdog: propagate ctx cancellation by collapsing the stream
	// deadline so any in-flight Read/Write returns immediately. We
	// MUST join the watchdog before clearing the deadline on the
	// success path; otherwise a late ctx cancellation could race the
	// clear and leave the bridge with a past-now deadline.
	done := make(chan struct{})
	watchdogExited := make(chan struct{})
	go func() {
		defer close(watchdogExited)
		select {
		case <-ctx.Done():
			// Past-now deadline forces immediate timeout.
			_ = stream.SetDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	stopped := false
	stopWatchdog := func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
		<-watchdogExited
	}
	defer stopWatchdog()

	env := protocol.ConnectEnvelope{
		Version:  protocol.CurrentVersion,
		Target:   target,
		BridgeID: bridgeID,
	}
	if err := protocol.WriteStreamEnvelope(stream, env); err != nil {
		return "", fmt.Errorf("send envelope: %w", err)
	}
	resp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if !resp.OK {
		return resp.ListenerID, &connectRejected{Message: resp.Error, Code: resp.Code, ListenerID: resp.ListenerID}
	}
	// Stop and join the watchdog before clearing the deadline. After
	// stopWatchdog returns the watchdog goroutine has exited, so no
	// other goroutine can race the SetDeadline(zero) below.
	stopWatchdog()
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return "", fmt.Errorf("clear envelope deadline: %w", err)
	}
	return resp.ListenerID, nil
}
