// Package listener implements the relay-listener: it accepts connections
// from the Azure Relay control channel, reads the connect envelope,
// optionally checks the target against an allowlist, dials the target,
// sends the response, and bridges data bidirectionally.
package listener

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// Config holds relay-listener configuration.
type Config struct {
	Endpoint       string
	EntityPath     string
	TokenProvider  relay.TokenProvider
	AllowList      []string // Optional target allowlist (CIDR:port patterns)
	MaxConnections int
	ConnectTimeout time.Duration
	TCPKeepAlive   time.Duration
	Logger         *slog.Logger
	Metrics        *metrics.Metrics // optional; nil disables metrics
}

// ListenAndServe starts the relay-listener. It blocks until ctx is cancelled.
func ListenAndServe(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}
	if cfg.TCPKeepAlive == 0 {
		cfg.TCPKeepAlive = 30 * time.Second
	}

	if len(cfg.AllowList) == 0 {
		cfg.Logger.Warn("no allowlist configured, all targets will be permitted")
	}

	ctrlCfg := relay.ControlConfig{
		Endpoint:       cfg.Endpoint,
		EntityPath:     cfg.EntityPath,
		TokenProvider:  cfg.TokenProvider,
		MaxConnections: cfg.MaxConnections,
		Logger:         cfg.Logger,
		Handler: func(ctx context.Context, ws *websocket.Conn) {
			handleConnection(ctx, ws, cfg)
		},
	}
	ctrlCfg.OnConnect = func() { cfg.Metrics.SetControlChannelConnected(true) }
	ctrlCfg.OnDisconnect = func() { cfg.Metrics.SetControlChannelConnected(false) }

	return relay.ListenAndServe(ctx, ctrlCfg)
}

func handleConnection(ctx context.Context, ws *websocket.Conn, cfg Config) {
	logger := cfg.Logger

	// Read the connect envelope with a timeout.
	readCtx, readCancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer readCancel()
	_, data, err := ws.Read(readCtx)
	if err != nil {
		logger.Warn("failed to read envelope", "error", err)
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	var env protocol.ConnectEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		logger.Warn("invalid envelope", "error", err)
		_ = sendResponse(ctx, ws, false, "invalid envelope")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}
	if env.Version != protocol.CurrentVersion {
		logger.Warn("unsupported protocol version", "version", env.Version)
		_ = sendResponse(ctx, ws, false, "unsupported protocol version")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}
	if env.Target == "" {
		_ = sendResponse(ctx, ws, false, "missing target")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

	logger.Info("connection requested", "target", env.Target)

	// Check allowlist.
	if len(cfg.AllowList) > 0 && !isAllowed(env.Target, cfg.AllowList) {
		logger.Warn("target not allowed", "target", env.Target)
		_ = sendResponse(ctx, ws, false, "target not allowed")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonAllowlistRejected)
		return
	}

	// Dial the target.
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout}
	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	dialStart := time.Now()
	conn, err := dialer.DialContext(dialCtx, "tcp", env.Target)
	cfg.Metrics.ObserveDialDuration("listener", time.Since(dialStart).Seconds())
	if err != nil {
		logger.Warn("dial target failed", "target", env.Target, "error", err)
		_ = sendResponse(ctx, ws, false, "connection failed")
		cfg.Metrics.ConnectionError("listener", metrics.DialReason(err, metrics.ReasonDialFailed))
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	// Set TCP keepalive.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Send success response.
	if err := sendResponse(ctx, ws, true, ""); err != nil {
		logger.Warn("failed to send response", "error", err)
		return
	}

	// Bridge data.
	_, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "listener", env.Target)
	if bridgeErr != nil {
		logger.Debug("bridge ended", "target", env.Target, "error", bridgeErr)
	}
}

func sendResponse(ctx context.Context, ws *websocket.Conn, ok bool, errMsg string) error {
	resp := protocol.ConnectResponse{
		Version: protocol.CurrentVersion,
		OK:      ok,
		Error:   errMsg,
	}
	data, _ := json.Marshal(resp) // simple struct, cannot fail
	return ws.Write(ctx, websocket.MessageText, data)
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
