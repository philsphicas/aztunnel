package sender

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// ConnectConfig holds configuration for the connect (stdin/stdout) mode.
type ConnectConfig struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	Target        string // host:port
	Stdin         io.ReadCloser
	Stdout        io.WriteCloser
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // optional; nil disables metrics
	DialTimeout   time.Duration    // total retry budget for the relay dial (0 = single attempt)
}

// Connect performs a one-shot connection: dials the relay, sends the
// envelope, and bridges stdin/stdout with the tunnel. It returns when
// either side closes.
func Connect(ctx context.Context, cfg ConnectConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ws, err := cfg.Metrics.InstrumentedDial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider, "sender", cfg.DialTimeout, cfg.Logger)
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	if err := sendEnvelopeAndCheck(ctx, ws, cfg.Target); err != nil {
		cfg.Metrics.ConnectionError("sender", metrics.ReasonEnvelopeError)
		return err
	}

	cfg.Logger.Debug("connected", "target", cfg.Target)

	stdio := &stdioConn{in: cfg.Stdin, out: cfg.Stdout}
	_, bridgeErr := cfg.Metrics.TrackedBridge(ctx, ws, stdio, "sender", cfg.Target)
	return bridgeErr
}
