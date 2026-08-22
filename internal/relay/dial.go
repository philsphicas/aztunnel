package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

const defaultDialTimeout = 30 * time.Second

// Retry parameters for DialWithRetry.
const (
	retryAttemptTimeout = 10 * time.Second
	retryInitial        = 1 * time.Second
	retryMax            = 5 * time.Second
	retryMultiplier     = 2
)

type retryConfig struct {
	attemptTimeout time.Duration
	initialDelay   time.Duration
	maxDelay       time.Duration
	multiplier     int
}

// Dial connects to the Azure Relay as a sender, establishing a rendezvous
// WebSocket connection that will be paired with a listener.
func Dial(ctx context.Context, endpoint, entityPath string, tp TokenProvider, opts ClientOptions) (*websocket.Conn, error) {
	resURI := ResourceURI(endpoint, entityPath)
	token, err := tp.GetToken(ctx, resURI)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	connectURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=connect&sb-hc-token=%s",
		opts.wssBase(endpoint), url.PathEscape(entityPath), url.QueryEscape(token))

	dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, connectURL, opts.dialOptions())
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", sanitizeErr(err))
	}
	return ws, nil
}

// IsRetryableStatus returns true for HTTP status codes that indicate
// the listener is not yet available and the dial should be retried.
func IsRetryableStatus(code int) bool {
	return code == http.StatusNotFound || code == http.StatusServiceUnavailable
}

// DialWithRetry is like Dial but retries on transient HTTP 404/503 errors
// (no active listener) and pre-response attempt deadlines with exponential
// backoff until ctx expires.
func DialWithRetry(ctx context.Context, endpoint, entityPath string, tp TokenProvider, opts ClientOptions, logger *slog.Logger) (*websocket.Conn, error) {
	return dialWithRetry(ctx, endpoint, entityPath, tp, opts, logger, retryConfig{
		attemptTimeout: retryAttemptTimeout,
		initialDelay:   retryInitial,
		maxDelay:       retryMax,
		multiplier:     retryMultiplier,
	})
}

func dialWithRetry(ctx context.Context, endpoint, entityPath string, tp TokenProvider, opts ClientOptions, logger *slog.Logger, cfg retryConfig) (*websocket.Conn, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("dialing relay", "entityPath", entityPath)

	delay := cfg.initialDelay
	for {
		resURI := ResourceURI(endpoint, entityPath)
		token, err := tp.GetToken(ctx, resURI)
		if err != nil {
			logger.Warn("relay dial failed", "error", err)
			return nil, fmt.Errorf("get token: %w", err)
		}

		connectURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=connect&sb-hc-token=%s",
			opts.wssBase(endpoint), url.PathEscape(entityPath), url.QueryEscape(token))

		dialCtx, cancel := context.WithTimeout(ctx, cfg.attemptTimeout)
		var trace *dialTrace
		if logger.Enabled(ctx, slog.LevelDebug) {
			dialCtx, trace = newDialTrace(dialCtx, time.Now())
		}
		ws, resp, dialErr := websocket.Dial(dialCtx, connectURL, opts.dialOptions())
		cancel()

		if dialErr == nil {
			trace.log(ctx, logger, "relay rendezvous trace")
			logger.Debug("relay connected", "entityPath", entityPath)
			return ws, nil
		}

		// Emit the trace on failure too: a dial that exceeded its
		// deadline or failed before the HTTP 101 is exactly when the
		// phase split (local dial time vs relay hold) is most useful.
		trace.log(ctx, logger, "relay rendezvous trace (dial failed)")

		retryableStatus := resp != nil && IsRetryableStatus(resp.StatusCode)
		retryableAttemptDeadline := resp == nil &&
			ctx.Err() == nil &&
			errors.Is(dialErr, context.DeadlineExceeded)
		// Azure can abandon an upgrade before registration and before returning
		// HTTP 101. Retrying that observed pre-registration failure is useful,
		// but the protocol has no idempotency key and this is not a universal
		// exactly-once guarantee if a response is lost after registration.
		if !retryableStatus && !retryableAttemptDeadline {
			logger.Warn("relay dial failed", "error", sanitizeErr(dialErr))
			return nil, fmt.Errorf("dial relay: %w", sanitizeErr(dialErr))
		}

		if resp != nil {
			logger.Warn("relay dial failed (retrying)", "status", resp.StatusCode, "delay", delay, "error", sanitizeErr(dialErr))
		} else {
			logger.Warn("relay dial failed (retrying)", "delay", delay, "error", sanitizeErr(dialErr))
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dial relay: %w", ctx.Err())
		case <-time.After(delay):
		}

		delay = min(delay*time.Duration(cfg.multiplier), cfg.maxDelay)
	}
}

// DialWithLogger is like Dial but logs the connection attempt and applies
// DialWithRetry's bounded retry policy.
func DialWithLogger(ctx context.Context, endpoint, entityPath string, tp TokenProvider, opts ClientOptions, logger *slog.Logger) (*websocket.Conn, error) {
	return DialWithRetry(ctx, endpoint, entityPath, tp, opts, logger)
}
