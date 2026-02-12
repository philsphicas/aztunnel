package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

const defaultDialTimeout = 30 * time.Second

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

// DialWithLogger is like Dial but logs the connection attempt.
func DialWithLogger(ctx context.Context, endpoint, entityPath string, tp TokenProvider, logger *slog.Logger) (*websocket.Conn, error) {
	logger.Debug("dialing relay", "entityPath", entityPath)
	ws, err := Dial(ctx, endpoint, entityPath, tp)
	if err != nil {
		logger.Warn("relay dial failed", "error", err)
		return nil, err
	}
	logger.Debug("relay connected", "entityPath", entityPath)
	return ws, nil
}
