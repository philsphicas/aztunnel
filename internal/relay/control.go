package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	tokenExpiry    = 1 * time.Hour
	renewInterval  = 45 * time.Minute
	pingInterval   = 30 * time.Second
	pingTimeout    = 10 * time.Second
	reconnectMin   = 1 * time.Second
	reconnectMax   = 30 * time.Second
	reconnectReset = 2 // multiplier
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
	// OnConnect is called when the control channel connects. Optional.
	OnConnect func()
	// OnDisconnect is called when the control channel disconnects. Optional.
	OnDisconnect func()
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

func runControlLoop(ctx context.Context, cfg ControlConfig) (connected bool, err error) {
	resURI := ResourceURI(cfg.Endpoint, cfg.EntityPath)
	token, err := cfg.TokenProvider.GetToken(ctx, resURI)
	if err != nil {
		return false, fmt.Errorf("get token: %w", err)
	}

	wssBase := EndpointToWSS(cfg.Endpoint)
	listenURL := fmt.Sprintf("%s/$hc/%s?sb-hc-action=listen&sb-hc-token=%s",
		wssBase, url.PathEscape(cfg.EntityPath), url.QueryEscape(token))

	dialCtx, dialCancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, listenURL, nil)
	if err != nil {
		return false, fmt.Errorf("dial control: %w", sanitizeErr(err))
	}
	defer func() { _ = ws.CloseNow() }()

	cfg.Logger.Info("control channel connected", "entityPath", cfg.EntityPath)
	if cfg.OnConnect != nil {
		cfg.OnConnect()
	}

	// Cancel used by ping/renew failure to force reconnect.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	sem := newConnSemaphore(cfg.MaxConnections)

	var wg sync.WaitGroup
	defer wg.Wait()

	// Token renewal goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		renewLoop(loopCtx, ws, resURI, cfg.TokenProvider, cfg.Logger, loopCancel)
	}()

	// Ping heartbeat goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pingLoop(loopCtx, ws, cfg.Logger, loopCancel)
	}()

	// Read accept messages from the control channel.
	for {
		_, data, err := ws.Read(loopCtx)
		if err != nil {
			return true, fmt.Errorf("read control: %w", err)
		}

		var msg struct {
			Accept *struct {
				Address        string            `json:"address"`
				ID             string            `json:"id"`
				ConnectHeaders map[string]string `json:"connectHeaders"`
			} `json:"accept"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			cfg.Logger.Warn("invalid control message", "error", err)
			continue
		}
		if msg.Accept == nil {
			continue
		}

		if !sem.tryAcquire(loopCtx) {
			cfg.Logger.Warn("max connections reached, dropping accept")
			continue
		}

		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			defer sem.release()
			if err := handleAccept(loopCtx, addr, cfg); err != nil {
				cfg.Logger.Warn("accept failed", "error", err)
			}
		}(msg.Accept.Address)
	}
}

func handleAccept(ctx context.Context, addr string, cfg ControlConfig) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, addr, nil)
	if err != nil {
		return fmt.Errorf("dial rendezvous: %w", sanitizeErr(err))
	}
	defer func() { _ = ws.CloseNow() }()

	cfg.Handler(ctx, ws)
	_ = ws.Close(websocket.StatusNormalClosure, "done")
	return nil
}

const maxRenewRetries = 3

func renewLoop(ctx context.Context, ws *websocket.Conn, resURI string, tp TokenProvider, logger *slog.Logger, cancel context.CancelFunc) {
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renewOnce(ctx, ws, resURI, tp, logger); err != nil {
				logger.Warn("token renewal failed, forcing reconnect", "error", err)
				cancel()
				return
			}
		}
	}
}

func renewOnce(ctx context.Context, ws *websocket.Conn, resURI string, tp TokenProvider, logger *slog.Logger) error {
	var lastErr error
	for attempt := range maxRenewRetries {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}

		token, err := tp.GetToken(ctx, resURI)
		if err != nil {
			lastErr = err
			logger.Warn("token renewal attempt failed", "attempt", attempt+1, "error", err)
			continue
		}
		msg := map[string]interface{}{
			"renewToken": map[string]string{
				"token": token,
			},
		}
		data, _ := json.Marshal(msg)
		if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
			return err // write failure = connection problem, no retry
		}
		logger.Debug("token renewed")
		return nil
	}
	return lastErr
}

func pingLoop(ctx context.Context, ws *websocket.Conn, logger *slog.Logger, cancel context.CancelFunc) {
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
				logger.Warn("ping failed, forcing reconnect", "error", err)
				cancel()
				return
			}
		}
	}
}
