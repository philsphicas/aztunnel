package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultDialTimeout = 30 * time.Second
	dialRetryBase      = 1 * time.Second
	dialRetryMax       = 30 * time.Second
)

// Dial connects to the Azure Relay as a sender, establishing a rendezvous
// WebSocket connection that will be paired with a listener.
func Dial(ctx context.Context, endpoint, entityPath string, tp TokenProvider) (*websocket.Conn, error) {
	resURI := ResourceURI(endpoint, entityPath)
	token, err := tp.GetToken(ctx, resURI)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	wssBase := EndpointToWSS(endpoint)
	connectURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=connect&sb-hc-token=%s",
		wssBase, url.PathEscape(entityPath), url.QueryEscape(token))

	dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, connectURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", sanitizeErr(err))
	}
	return ws, nil
}

// DialWithTimeout dials the relay, retrying with exponential backoff (1s→2s→4s,
// capped at 30s) until dialTimeout is exhausted or the context is cancelled.
// dialTimeout=0 means a single attempt with no retries. onRetry is called
// before each retry attempt; it may be nil.
func DialWithTimeout(ctx context.Context, endpoint, entityPath string, tp TokenProvider, dialTimeout time.Duration, onRetry func(), logger *slog.Logger) (*websocket.Conn, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Zero timeout: single attempt, no retries.
	if dialTimeout == 0 {
		return Dial(ctx, endpoint, entityPath, tp)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	delay := dialRetryBase
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			logger.Debug("retrying relay dial", "attempt", attempt, "delay", delay)
			if onRetry != nil {
				onRetry()
			}
			select {
			case <-timeoutCtx.Done():
				return nil, lastErr // budget exhausted before retry
			case <-time.After(delay):
			}
			delay = min(delay*2, dialRetryMax)
		}
		ws, err := Dial(timeoutCtx, endpoint, entityPath, tp)
		if err == nil {
			return ws, nil
		}
		lastErr = err
		logger.Debug("relay dial attempt failed", "attempt", attempt+1, "error", err)
		if timeoutCtx.Err() != nil {
			break // budget exhausted during dial
		}
	}
	// lastErr is set by each failed dial attempt. Fall back to the context
	// error in the defensive case where the budget expired before any attempt
	// could record a dial error (e.g., context expired between iterations).
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, timeoutCtx.Err()
}
