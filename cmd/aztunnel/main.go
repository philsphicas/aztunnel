package main

import (
	"context"
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

// resolveAuth determines the endpoint and token provider from flags and
// environment variables.
//
// Resolution order for namespace:
//  1. relay flag (or hidden namespace alias)
//  2. AZTUNNEL_RELAY_NAME env var
//
// Resolution order for auth:
//  1. AZTUNNEL_KEY_NAME + AZTUNNEL_KEY → SAS auth (explicit override)
//  2. Otherwise → Entra ID auth via DefaultAzureCredential (default)
func resolveAuth(relayFlag, namespaceAlias, suffixFlag string) (endpoint string, tp relay.TokenProvider, err error) {
	ns := relayFlag
	if ns == "" {
		ns = namespaceAlias
	}
	if ns == "" {
		ns = os.Getenv("AZTUNNEL_RELAY_NAME")
	}
	if ns == "" {
		return "", nil, fmt.Errorf("relay namespace is required: use --relay or set AZTUNNEL_RELAY_NAME")
	}
	suffix := suffixFlag
	if suffix == "" {
		suffix = os.Getenv("AZTUNNEL_RELAY_SUFFIX")
	}
	if suffix == "" {
		suffix = relay.DefaultRelaySuffix
	}
	endpoint = relay.ParseRelayEndpoint(ns, suffix)
	if endpoint == "" {
		return "", nil, fmt.Errorf("invalid relay endpoint: %q", ns)
	}

	keyName := os.Getenv("AZTUNNEL_KEY_NAME")
	key := os.Getenv("AZTUNNEL_KEY")

	if keyName != "" && key != "" {
		return endpoint, &relay.SASTokenProvider{KeyName: keyName, Key: key}, nil
	}

	entra, err := relay.NewEntraTokenProvider()
	if err != nil {
		return "", nil, fmt.Errorf("no SAS credentials found (AZTUNNEL_KEY_NAME/AZTUNNEL_KEY) and Entra auth failed: %w", err)
	}
	return endpoint, entra, nil
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
