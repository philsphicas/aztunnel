package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/philsphicas/aztunnel/internal/idgen"
)

const (
	tokenExpiry          = 1 * time.Hour
	defaultRenewInterval = 45 * time.Minute
	pingInterval         = 30 * time.Second
	pingTimeout          = 10 * time.Second
	reconnectMin         = 1 * time.Second
	reconnectMax         = 30 * time.Second
	reconnectReset       = 2 // multiplier
)

// AcceptHandler is called for each accepted rendezvous connection.
// The handler receives the rendezvous WebSocket and is responsible for
// the envelope exchange and bridging. The WebSocket will be closed after
// the handler returns.
type AcceptHandler func(ctx context.Context, ws *websocket.Conn)

// ControlConfig holds parameters for the listener control channel.
type ControlConfig struct {
	Endpoint       string
	EntityPath     string
	TokenProvider  TokenProvider
	Handler        AcceptHandler
	MaxConnections int // 0 = unlimited
	DialTimeout    time.Duration
	Logger         *slog.Logger
	// Options controls transport (scheme, TLS) for the control channel
	// dial and the listener's outbound rendezvous dial. The zero value
	// is real-Azure-compatible (wss + http.DefaultClient).
	Options ClientOptions
	// OnConnect is called when the control channel connects. Optional.
	OnConnect func()
	// OnDisconnect is called when the control channel disconnects. Optional.
	OnDisconnect func()
	// RenewInterval is how often the listener renews its SAS/Entra
	// token over the control channel. Zero selects defaultRenewInterval
	// (45m). Tests set a short value to drive a real renew round-trip
	// within an assertion budget.
	RenewInterval time.Duration
}

// ListenAndServe connects to the Azure Relay control channel and accepts
// incoming connections. It blocks until the context is cancelled.
func ListenAndServe(ctx context.Context, cfg ControlConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 30 * time.Second
	}
	delay := reconnectMin
	for {
		start := time.Now()
		connected, err := runControlLoop(ctx, cfg)
		if ctx.Err() != nil {
			// Graceful shutdown: ensure OnDisconnect is called if
			// OnConnect fired inside runControlLoop.
			if connected && cfg.OnDisconnect != nil {
				cfg.OnDisconnect()
			}
			return ctx.Err()
		}
		// Reset backoff if the connection was up for a meaningful duration.
		if time.Since(start) > reconnectMax {
			delay = reconnectMin
		}
		cfg.Logger.Warn("control channel disconnected, reconnecting", "error", err, "delay", delay)
		if connected && cfg.OnDisconnect != nil {
			cfg.OnDisconnect()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		// Exponential backoff capped at reconnectMax.
		delay = min(delay*reconnectReset, reconnectMax)
	}
}

// loopState carries the deferred-emit inputs for control_ended out of
// the goroutines that detect a forced-reconnect cause. renewLoop,
// pingLoop, and the read loop all call setEnd before cancelling the
// loop context; the deferred control_ended emission in runControlLoop
// reads the stored (cause, err) pair so the reported reason and error
// reflect the actual failure rather than the read-loop's wrapped
// context-cancellation error. Cause and error are coupled in one
// atomic write so concurrent failures can never produce a mismatched
// (renew_failed, <ping error>)-style pair.
type loopState struct {
	end atomic.Value // *endResult; first-writer-wins
}

// endResult is the coupled (cause, err) record stored on loopState.
// err may be nil — used by call sites whose loop-return error is
// already the right value to report (e.g. dial failures wrap the
// returned err themselves), in which case the deferred emit falls
// back to the loop's return err.
type endResult struct {
	cause string
	err   error
}

func (s *loopState) setEnd(cause string, err error) {
	// First-writer-wins: when renew_failed forces the cancel and
	// pingLoop then notices the cancelled context and would set
	// ping_failed, the operator-visible answer should remain the
	// underlying renew failure. The single CAS keeps cause and err
	// in sync — a slow ping cannot overwrite the renew error after
	// renew has stamped its cause.
	s.end.CompareAndSwap(nil, &endResult{cause: cause, err: err})
}

func (s *loopState) load() (cause string, err error) {
	v := s.end.Load()
	if v == nil {
		return "", nil
	}
	r := v.(*endResult)
	return r.cause, r.err
}

func runControlLoop(ctx context.Context, cfg ControlConfig) (connected bool, err error) {
	// Bind a fresh control_session_id onto the per-session logger
	// before any work that could log: token fetch, dial, and the
	// per-loop helpers (renewLoop, pingLoop) all log against this
	// logger so operators can mechanically separate the lines from
	// one control-loop run from the lines of the next.
	//
	// The outer reconnect loop in ListenAndServe keeps using
	// cfg.Logger directly, so its "control channel disconnected,
	// reconnecting" line stays out of any session — that line marks
	// the boundary between sessions and is intentionally untagged.
	sessionID := idgen.NewControlSessionID()
	logger := cfg.Logger.With("control_session_id", sessionID)

	loopStart := time.Now()
	state := &loopState{}

	defer func() {
		cause, stateErr := state.load()
		if cause == "" {
			cause = classifyControlEndReason(err, ctx.Err())
		}
		// When renew/ping/read stash a coupled (cause, err) pair on
		// loopState, prefer their underlying err. The read loop's
		// return-err is "read control: context canceled" after a
		// forced cancel — operationally useless. Dial/token-fetch
		// callers pass nil for err and let the loop's own wrapped
		// return-err surface here.
		reportErr := err
		if stateErr != nil {
			reportErr = stateErr
		}
		// Pass the error value (not the formatted string) so slog
		// handlers can apply their own formatting and so the
		// "error" attribute matches the package-wide convention.
		// Omit the attribute entirely on graceful shutdown so
		// successful loop exits don't carry an empty error field.
		attrs := []any{
			"reason", cause,
			"duration_seconds", time.Since(loopStart).Seconds(),
		}
		if reportErr != nil {
			attrs = append(attrs, "error", reportErr)
		}
		logger.Info(EventControlEnded, attrs...)
	}()

	resURI := ResourceURI(cfg.Endpoint, cfg.EntityPath)
	token, err := cfg.TokenProvider.GetToken(ctx, resURI)
	if err != nil {
		if ctx.Err() == nil {
			state.setEnd(ControlEndedTokenFetchFailed, nil)
		}
		return false, fmt.Errorf("get token: %w", err)
	}

	wssBase := cfg.Options.wssBase(cfg.Endpoint)
	listenURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=listen&sb-hc-token=%s",
		wssBase, url.PathEscape(cfg.EntityPath), url.QueryEscape(token))

	dialCtx, dialCancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer dialCancel()
	ws, resp, dialErr := websocket.Dial(dialCtx, listenURL, cfg.Options.dialOptions())
	if dialErr != nil {
		// Operator-driven cancellation propagated through dialCtx
		// is classified as context_cancelled (no setEnd call —
		// the classifier sees ctx.Err and maps it). Other dial
		// errors split into auth_failed vs dial_failed.
		switch {
		case ctx.Err() != nil:
		case dialAuthFailed(resp):
			state.setEnd(ControlEndedAuthFailed, nil)
		default:
			state.setEnd(ControlEndedDialFailed, nil)
		}
		return false, fmt.Errorf("dial control: %w", sanitizeErr(dialErr))
	}
	defer func() { _ = ws.CloseNow() }()

	// control_started fires here, after the dial has succeeded —
	// this is the operational milestone every other listener
	// readiness signal (metric, OnConnect callback, parity test
	// waitForLog) hangs off. On dial failure, only control_ended
	// fires with reason=dial_failed/auth_failed; that single
	// signal is sufficient to tell an operator the loop terminated
	// at dial.
	logger.Info(EventControlStarted,
		"relay_url", wssBase,
		"listener_name", cfg.EntityPath)

	if cfg.OnConnect != nil {
		cfg.OnConnect()
	}
	connected = true

	// Cancel used by ping/renew failure to force reconnect.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	sem := newConnSemaphore(cfg.MaxConnections)

	var wg sync.WaitGroup
	defer wg.Wait()

	renewInterval := cfg.RenewInterval
	if renewInterval == 0 {
		renewInterval = defaultRenewInterval
	}

	// Token renewal goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		renewLoop(loopCtx, ws, resURI, cfg.TokenProvider, logger, loopCancel, state, renewInterval)
	}()

	// Ping heartbeat goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pingLoop(loopCtx, ws, logger, loopCancel, state)
	}()

	// Read accept messages from the control channel.
	for {
		_, data, readErr := ws.Read(loopCtx)
		if readErr != nil {
			// Cancel renew/ping immediately so the deferred
			// wg.Wait below unblocks promptly; otherwise it
			// would sit until the ping ticker (~30s) noticed
			// the dead socket and called loopCancel itself.
			if loopCtx.Err() == nil {
				state.setEnd(ControlEndedReadFailed, readErr)
			}
			loopCancel()
			return true, fmt.Errorf("read control: %w", readErr)
		}

		var msg struct {
			Accept *struct {
				Address        string            `json:"address"`
				ID             string            `json:"id"`
				ConnectHeaders map[string]string `json:"connectHeaders"`
			} `json:"accept"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			logger.Warn("invalid control message", "error", err)
			continue
		}
		if msg.Accept == nil {
			continue
		}

		// Mint a short-lived accept_id before the semaphore check so
		// the drop path carries the same correlation key as the
		// accepted path. The accept_id flows through acceptLogger
		// onto every subsequent log line for this accept; the
		// control_session_id binding on logger means each accept
		// line carries both attributes.
		acceptID := idgen.NewAcceptID()
		acceptLogger := logger.With("accept_id", acceptID)

		acceptLogger.Info(EventAcceptAttempted)

		if !sem.tryAcquire(loopCtx) {
			acceptLogger.Warn(EventAcceptDropped, "reason", AcceptDroppedSemaphoreFull)
			continue
		}
		acceptLogger.Debug("accept acquired")

		wg.Add(1)
		go func(addr string, logger *slog.Logger) {
			defer wg.Done()
			defer func() {
				sem.release()
				logger.Debug("accept released")
			}()
			handleAccept(loopCtx, addr, cfg, logger)
		}(msg.Accept.Address, acceptLogger)
	}
}

// classifyControlEndReason maps a runControlLoop return error onto one
// of the ControlEnded* enum values. Used only as a fallback when the
// renew/ping goroutines have NOT already stored a more specific cause
// on loopState — those goroutines win because their context-cancel
// reason is operationally more useful than the read-loop's wrapped
// context.Canceled return.
func classifyControlEndReason(loopErr, outerCtxErr error) string {
	if outerCtxErr != nil {
		return ControlEndedContextCancelled
	}
	if loopErr == nil {
		return ControlEndedContextCancelled
	}
	if errors.Is(loopErr, context.Canceled) || errors.Is(loopErr, context.DeadlineExceeded) {
		return ControlEndedContextCancelled
	}
	return ControlEndedReadFailed
}

// dialAuthFailed reports whether a failed control-channel dial got an
// HTTP response that indicates token rejection. Azure Relay returns
// 401 for invalid SAS tokens and 403 for tokens with insufficient
// permissions; both map to control_ended.reason="auth_failed".
func dialAuthFailed(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func handleAccept(ctx context.Context, addr string, cfg ControlConfig, logger *slog.Logger) {
	logger.Debug("accept dial started")
	dialCtx, dialCancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer dialCancel()
	ws, resp, err := websocket.Dial(dialCtx, addr, cfg.Options.dialOptions())
	if err != nil {
		reason := AcceptDroppedDialFailed
		if dialAuthFailed(resp) {
			reason = AcceptDroppedAuthFailed
		}
		logger.Warn(EventAcceptDropped, "reason", reason, "error", sanitizeErr(err))
		return
	}
	logger.Debug("accept dial complete", "ok", true)
	defer func() { _ = ws.CloseNow() }()

	logger.Info(EventAcceptOK)

	cfg.Handler(ctx, ws)
	_ = ws.Close(websocket.StatusNormalClosure, "done")
}

const maxRenewRetries = 3

func renewLoop(ctx context.Context, ws *websocket.Conn, resURI string, tp TokenProvider, logger *slog.Logger, cancel context.CancelFunc, state *loopState, interval time.Duration) {
	// tokenMintedAt drives the expires_in_seconds attribute on
	// renew_attempted and the new_expires_in_seconds attribute on
	// renew_ok. The initial value is the entry time of this
	// goroutine, which is within milliseconds of the runControlLoop
	// dial — close enough for an operator-visible "how many seconds
	// until the current token expires" estimate.
	//
	// The estimate is computed against the SAS-token validity window
	// (tokenExpiry = 1h, the value passed to NewSASToken). For Entra
	// credentials the actual on-wire token lifetime comes from the
	// credential and may differ from 1h; the attribute is therefore
	// best understood as "seconds until the listener will rotate"
	// rather than "seconds until the bearer credential is invalid".
	tokenMintedAt := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newMint, err := renewOnce(ctx, ws, resURI, tp, logger, tokenMintedAt)
			if err != nil {
				if ctx.Err() == nil {
					state.setEnd(ControlEndedRenewFailed, err)
				}
				cancel()
				return
			}
			tokenMintedAt = newMint
		}
	}
}

// renewOnce executes one renew round-trip with up to maxRenewRetries
// attempts. It emits renew_attempted before each attempt and a single
// terminal renew_ok or renew_failed event when the round-trip
// completes. Returns the time the successful renew's token was minted
// (used by the caller to update tokenMintedAt for the next pass).
func renewOnce(ctx context.Context, ws *websocket.Conn, resURI string, tp TokenProvider, logger *slog.Logger, currentTokenMintedAt time.Time) (time.Time, error) {
	start := time.Now()
	var lastErr error
	attempt := 0
	for attempt < maxRenewRetries {
		attempt++
		if attempt > 1 {
			select {
			case <-ctx.Done():
				logger.Warn(EventRenewFailed,
					"attempt", attempt-1,
					"elapsed_ms", time.Since(start).Milliseconds(),
					"error", ctx.Err(),
					"code", RenewFailedContextCancel)
				return time.Time{}, ctx.Err()
			case <-time.After(time.Duration(attempt-1) * 5 * time.Second):
			}
		}

		expiresInSec := int64(time.Until(currentTokenMintedAt.Add(tokenExpiry)).Seconds())
		logger.Info(EventRenewAttempted,
			"attempt", attempt,
			"expires_in_seconds", expiresInSec)

		token, err := tp.GetToken(ctx, resURI)
		if err != nil {
			lastErr = err
			continue
		}
		msg := map[string]interface{}{
			"renewToken": map[string]string{"token": token},
		}
		data, _ := json.Marshal(msg)
		if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
			logger.Warn(EventRenewFailed,
				"attempt", attempt,
				"elapsed_ms", time.Since(start).Milliseconds(),
				"error", err,
				"code", RenewFailedConnectionLost)
			return time.Time{}, err
		}
		now := time.Now()
		logger.Info(EventRenewOK,
			"attempt", attempt,
			"new_expires_in_seconds", int64(tokenExpiry.Seconds()),
			"elapsed_ms", time.Since(start).Milliseconds())
		return now, nil
	}
	code := RenewFailedTokenFetchFail
	if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
		code = RenewFailedContextCancel
	}
	logger.Warn(EventRenewFailed,
		"attempt", attempt,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"error", lastErr,
		"code", code)
	return time.Time{}, lastErr
}

func pingLoop(ctx context.Context, ws *websocket.Conn, logger *slog.Logger, cancel context.CancelFunc, state *loopState) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, pingTimeout)
			err := ws.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// No prose log line here: the ping failure is
				// surfaced via control_ended{reason=ping_failed,
				// error=<ws.Ping err>} once the deferred emit
				// runs in runControlLoop. The coupled setEnd
				// stashes (cause, err) atomically so the emit
				// reads a consistent pair.
				state.setEnd(ControlEndedPingFailed, err)
				cancel()
				return
			}
		}
	}
}
