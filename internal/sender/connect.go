package sender

import (
	"context"
	"io"
	"log/slog"

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
}

// Connect performs a one-shot connection: dials the relay, sends the
// envelope, and bridges stdin/stdout with the tunnel. It returns when
// either side closes.
func Connect(ctx context.Context, cfg ConnectConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ws, err := relay.Dial(ctx, cfg.Endpoint, cfg.EntityPath, cfg.TokenProvider)
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()

	if err := sendEnvelopeAndCheck(ctx, ws, cfg.Target); err != nil {
		return err
	}

	cfg.Logger.Debug("connected", "target", cfg.Target)

	stdio := &stdioConn{in: cfg.Stdin, out: cfg.Stdout}
	return relay.Bridge(ctx, ws, stdio)
}
