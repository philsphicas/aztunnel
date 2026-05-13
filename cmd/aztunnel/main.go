package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	// Automatically set GOMEMLIMIT based on cgroup memory limits (container
	// or systemd MemoryMax=). If no cgroup limit is detected, GOMEMLIMIT is
	// left at the Go default.
	"github.com/KimMachineGun/automemlimit/memlimit"

	"github.com/alecthomas/kong"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/willabides/kongplete"
)

var version = "dev"

func init() {
	_, _ = memlimit.SetGoMemLimitWithOpts(memlimit.WithLogger(nil))
}

func main() {
	parser := kong.Must(&CLI,
		kong.Name("aztunnel"),
		kong.Description("Tunnel TCP connections through Azure Relay Hybrid Connections."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Help(customHelpPrinter),
	)

	kongplete.Complete(parser)

	ctx, err := parser.Parse(os.Args[1:])
	parser.FatalIfErrorf(err)

	parser.FatalIfErrorf(ctx.Run(&CLI.Globals))
}

// resolveMetrics creates a Metrics instance and starts the HTTP server if
// metricsAddr or AZTUNNEL_METRICS_ADDR is set. Returns nil if metrics are
// disabled. The provided context controls the server's lifetime — when
// cancelled the server shuts down gracefully.
func resolveMetrics(ctx context.Context, metricsAddr string, maxTargets int, logger *slog.Logger) (*metrics.Metrics, error) {
	addr := metricsAddr
	if addr == "" {
		addr = os.Getenv("AZTUNNEL_METRICS_ADDR")
	}
	if addr == "" {
		return nil, nil
	}
	if maxTargets < 0 {
		return nil, fmt.Errorf("--metrics-max-targets must be >= 0, got %d", maxTargets)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen on %s: %w", addr, err)
	}
	m := metrics.New()
	m.MaxTargets = maxTargets
	go func() {
		if err := m.Serve(ctx, ln, logger); err != nil {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	return m, nil
}

// resolveHyco returns the hybrid connection name from flag or env var.
func resolveHyco(hycoFlag string) (string, error) {
	if hycoFlag != "" {
		return hycoFlag, nil
	}
	if hyco := os.Getenv("AZTUNNEL_HYCO_NAME"); hyco != "" {
		return hyco, nil
	}
	return "", fmt.Errorf("hybrid connection name is required: use --hyco or set AZTUNNEL_HYCO_NAME")
}

// resolveAuth determines the endpoint, transport options, and token
// provider from flags and environment variables.
//
// Endpoint precedence: --relay > AZTUNNEL_RELAY_NAME (env). The hidden
// --namespace alias is honored as a synonym for --relay.
//
// Suffix precedence: --relay-suffix > AZTUNNEL_RELAY_SUFFIX (env) >
// DefaultRelaySuffix. Suffix is only applied to bare hostnames with no
// dot AND no colon (port). Inputs that include a port — typical for
// mock/self-hosted relays — are used verbatim.
//
// Transport scheme is derived from the --relay value: URL form
// (ws://, http://) → ws; URL form with https/wss/sb or bare host →
// wss. --relay-insecure-tls (or AZTUNNEL_RELAY_INSECURE_TLS=1) skips
// TLS verification when scheme is wss.
//
// Auth precedence for --relay-auth: CLI flag > AZTUNNEL_RELAY_AUTH (env,
// bound by kong) > "auto" default. Modes:
//   - auto: SAS env vars (AZTUNNEL_KEY_NAME + AZTUNNEL_KEY) → SAS;
//     otherwise → Entra via DefaultAzureCredential. This is the
//     historical default.
//   - sas: require AZTUNNEL_KEY_NAME and AZTUNNEL_KEY.
//   - entra: force Entra; fail if credentials are unavailable.
//
// To exercise aztunnel against the in-tree mock (aztunnel-relay), use
// SAS with the mock's printed dummy key — there is no longer a no-auth
// shortcut on the client side.
func resolveAuth(af AuthFlags, logger *slog.Logger) (endpoint string, opts relay.ClientOptions, tp relay.TokenProvider, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := af.Relay
	if ns == "" {
		ns = af.Namespace
	}
	if ns == "" {
		ns = os.Getenv("AZTUNNEL_RELAY_NAME")
	}
	if ns == "" {
		return "", relay.ClientOptions{}, nil, fmt.Errorf("relay namespace is required: use --relay or set AZTUNNEL_RELAY_NAME")
	}
	suffix := af.RelaySuffix
	if suffix == "" {
		suffix = os.Getenv("AZTUNNEL_RELAY_SUFFIX")
	}
	if suffix == "" {
		suffix = relay.DefaultRelaySuffix
	}

	host, scheme := relay.ParseRelay(ns, suffix)
	if host == "" {
		return "", relay.ClientOptions{}, nil, fmt.Errorf("invalid relay endpoint: %q", ns)
	}
	endpoint = host
	opts.Scheme = scheme

	if af.RelayInsecureTLS || os.Getenv("AZTUNNEL_RELAY_INSECURE_TLS") == "1" {
		// Only meaningful for wss: plain ws:// doesn't do TLS at all,
		// so attaching a TLSConfig is a no-op and the warning would be
		// misleading.
		if opts.Scheme == relay.SchemeWSS {
			opts.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in by user for mock/self-hosted
			logger.Warn("relay TLS certificate verification disabled — do NOT use against production Azure Relay")
		} else {
			logger.Debug("ignoring --relay-insecure-tls because scheme is not wss",
				"scheme", opts.Scheme)
		}
	}

	mode := strings.ToLower(af.RelayAuth)
	if mode == "" {
		// Defense in depth: kong's env: tag and default:"auto" already
		// guarantee a non-empty value here, but fall back if the field
		// is cleared programmatically (e.g. by a test).
		mode = "auto"
	}

	keyName := os.Getenv("AZTUNNEL_KEY_NAME")
	key := os.Getenv("AZTUNNEL_KEY")

	switch mode {
	case "sas":
		if keyName == "" || key == "" {
			return "", relay.ClientOptions{}, nil, fmt.Errorf("--relay-auth=sas requires AZTUNNEL_KEY_NAME and AZTUNNEL_KEY")
		}
		return endpoint, opts, &relay.SASTokenProvider{KeyName: keyName, Key: key}, nil
	case "entra":
		entra, err := relay.NewEntraTokenProvider()
		if err != nil {
			return "", relay.ClientOptions{}, nil, fmt.Errorf("--relay-auth=entra: %w", err)
		}
		return endpoint, opts, entra, nil
	case "auto":
		if keyName != "" && key != "" {
			return endpoint, opts, &relay.SASTokenProvider{KeyName: keyName, Key: key}, nil
		}
		entra, err := relay.NewEntraTokenProvider()
		if err != nil {
			return "", relay.ClientOptions{}, nil, fmt.Errorf("no SAS credentials found (AZTUNNEL_KEY_NAME/AZTUNNEL_KEY) and Entra auth failed: %w", err)
		}
		return endpoint, opts, entra, nil
	default:
		return "", relay.ClientOptions{}, nil, fmt.Errorf("unknown --relay-auth value: %q (want auto, sas, or entra)", mode)
	}
}

// resolveResourceID returns the resource ID from flag or AZTUNNEL_ARC_RESOURCE_ID env var.
func resolveResourceID(resourceID string) (string, error) {
	if resourceID != "" {
		return resourceID, nil
	}
	if rid := os.Getenv("AZTUNNEL_ARC_RESOURCE_ID"); rid != "" {
		return rid, nil
	}
	return "", fmt.Errorf("resource ID is required: use --resource-id or set AZTUNNEL_ARC_RESOURCE_ID")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
