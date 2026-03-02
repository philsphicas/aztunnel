package relay

import (
	"context"
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
	retryInitial    = 1 * time.Second
	retryMax        = 5 * time.Second
	retryMultiplier = 2
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

// IsRetryableStatus returns true for HTTP status codes that indicate
// the listener is not yet available and the dial should be retried.
func IsRetryableStatus(code int) bool {
	return code == http.StatusNotFound || code == http.StatusServiceUnavailable
}

// DialWithRetry is like Dial but retries on transient HTTP 404/503 errors
// (no active listener) with exponential backoff until ctx expires.
func DialWithRetry(ctx context.Context, endpoint, entityPath string, tp TokenProvider, logger *slog.Logger) (*websocket.Conn, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("dialing relay", "entityPath", entityPath)

	delay := retryInitial
	for {
		resURI := ResourceURI(endpoint, entityPath)
		token, err := tp.GetToken(ctx, resURI)
		if err != nil {
			logger.Warn("relay dial failed", "error", err)
			return nil, fmt.Errorf("get token: %w", err)
		}

		wssBase := EndpointToWSS(endpoint)
		connectURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=connect&sb-hc-token=%s",
			wssBase, url.PathEscape(entityPath), url.QueryEscape(token))

		dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
		ws, resp, dialErr := websocket.Dial(dialCtx, connectURL, nil)
		cancel()

		if dialErr == nil {
			logger.Debug("relay connected", "entityPath", entityPath)
			return ws, nil
		}

		// Only retry on 404/503 (no active listener / listener transitioning).
		if resp == nil || !IsRetryableStatus(resp.StatusCode) {
			logger.Warn("relay dial failed", "error", sanitizeErr(dialErr))
			return nil, fmt.Errorf("dial relay: %w", sanitizeErr(dialErr))
		}

		logger.Warn("relay dial failed (retrying)", "status", resp.StatusCode, "delay", delay, "error", sanitizeErr(dialErr))

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dial relay: %w", ctx.Err())
		case <-time.After(delay):
		}

		delay = min(delay*retryMultiplier, retryMax)
	}
}

// DialWithLogger is like Dial but logs the connection attempt and retries
// on transient 404/503 errors.
func DialWithLogger(ctx context.Context, endpoint, entityPath string, tp TokenProvider, logger *slog.Logger) (*websocket.Conn, error) {
	return DialWithRetry(ctx, endpoint, entityPath, tp, logger)
}
