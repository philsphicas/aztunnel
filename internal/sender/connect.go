package sender

import (
	"context"
	"io"
	"log/slog"

	"github.com/philsphicas/aztunnel/internal/idgen"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// ConnectConfig holds configuration for the connect (stdin/stdout) mode.
type ConnectConfig struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	ClientOptions relay.ClientOptions
	Target        string // host:port
	Stdin         io.ReadCloser
	Stdout        io.WriteCloser
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // optional; nil disables metrics
}

// Connect performs a one-shot connection: dials the relay, sends the
// envelope, and bridges stdin/stdout with the tunnel. It returns when
// either side closes.
func Connect(ctx context.Context, cfg ConnectConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	bridgeID := idgen.NewBridgeID()
	logger := cfg.Logger.With("bridge_id", bridgeID)
	logger.Info("connection requested", "target", cfg.Target)

	ws, err := cfg.Metrics.InstrumentedDial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, cfg.ClientOptions, "sender", logger)
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	listenerID, err := sendEnvelopeAndCheck(ctx, ws, cfg.Target, bridgeID)
	if err != nil {
		logRejection(logger, cfg.Target, listenerID, err)
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		return err
	}
	logAccept(logger, cfg.Target, listenerID)

	stdio := &stdioConn{in: cfg.Stdin, out: cfg.Stdout}
	stats, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, stdio, "sender", cfg.Target)
	attrs := []any{
		"target", cfg.Target,
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
	return bridgeErr
}
