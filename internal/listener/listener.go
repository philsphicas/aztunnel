// Package listener implements the relay-listener: it accepts connections
// from the Azure Relay control channel, reads the connect envelope,
// optionally checks the target against an allowlist, dials the target,
// sends the response, and bridges data bidirectionally.
package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/idgen"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
)

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
	cfg.Logger = cfg.Logger.With("listener_id", cfg.ListenerID)
}

// ListenAndServe starts the relay-listener. It blocks until ctx is cancelled.
func ListenAndServe(ctx context.Context, cfg Config) error {
	applyDefaults(&cfg)

	if len(cfg.AllowList) == 0 {
		cfg.Logger.Warn("no allowlist configured, all targets will be permitted")
	}

	ctrlCfg := relay.ControlConfig{
		Endpoint:       cfg.Endpoint,
		EntityPath:     cfg.EntityPath,
		TokenProvider:  cfg.TokenProvider,
		Options:        cfg.ClientOptions,
		MaxConnections: cfg.MaxConnections,
		Logger:         cfg.Logger,
		RenewInterval:  cfg.RenewInterval,
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
		attrs := []any{"error", err}
		if code, ok := relay.WSCloseCode(err); ok {
			attrs = append(attrs, "close_code", code)
		}
		logger.Warn("failed to read envelope", attrs...)
		cfg.Metrics.ConnectionError("listener", metrics.ReasonEnvelopeError)
		return
	}

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

	// Check allowlist.
	if len(cfg.AllowList) > 0 && !isAllowed(env.Target, cfg.AllowList) {
		logger.Warn("target not allowed", "target", env.Target)
		_ = sendResponse(ctx, ws, cfg, false, "target not allowed")
		cfg.Metrics.ConnectionError("listener", metrics.ReasonAllowlistRejected)
		return
	}

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

	// Set TCP keepalive.
	relay.SetTCPKeepAlive(conn, cfg.TCPKeepAlive)

	// Send success response.
	if err := sendResponse(ctx, ws, cfg, true, ""); err != nil {
		logger.Warn("failed to send response", "error", err)
		return
	}

	// Bridge data.
	stats, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, conn, "listener", env.Target)
	attrs := []any{
		"target", env.Target,
		"cause", stats.Cause,
		"tcp_to_ws", stats.TCPToWS,
		"ws_to_tcp", stats.WSToTCP,
	}
	if bridgeErr != nil {
		attrs = append(attrs, "error", bridgeErr)
	}
	if code, ok := relay.WSCloseCode(bridgeErr); ok {
		attrs = append(attrs, "close_code", code)
	}
	logger.Debug("bridge ended", attrs...)
}

func sendResponse(ctx context.Context, ws *websocket.Conn, cfg Config, ok bool, errMsg string) error {
	return sendResponseWithCode(ctx, ws, cfg, ok, errMsg, "")
}

// sendResponseWithCode is the variant of sendResponse that includes a
// machine-readable code so the sender can map listener-side dial
// failures onto client-visible status (e.g. SOCKS5 REP bytes).
func sendResponseWithCode(ctx context.Context, ws *websocket.Conn, cfg Config, ok bool, errMsg, code string) error {
	resp := protocol.ConnectResponse{
		Version:    protocol.CurrentVersion,
		OK:         ok,
		Error:      errMsg,
		Code:       code,
		ListenerID: cfg.ListenerID,
	}
	data, _ := json.Marshal(resp) // simple struct, cannot fail
	return ws.Write(ctx, websocket.MessageText, data)
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
