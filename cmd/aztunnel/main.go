package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	// Automatically set GOMEMLIMIT based on cgroup memory limits (container
	// or systemd MemoryMax=). If no cgroup limit is detected, GOMEMLIMIT is
	// left at the Go default.
	"github.com/KimMachineGun/automemlimit/memlimit"

	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/spf13/cobra"
)

var version = "dev"

func init() {
	_, _ = memlimit.SetGoMemLimitWithOpts(memlimit.WithLogger(nil))
}

func main() {
	rootCmd := &cobra.Command{
		Use:          "aztunnel",
		Short:        "Azure Relay Hybrid Connection tunnel",
		Long:         "Tunnel TCP connections through Azure Relay Hybrid Connections.",
		SilenceUsage: true,
	}

	// Global flags.
	rootCmd.PersistentFlags().String("log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().String("metrics-addr", "", "address for Prometheus metrics server (e.g. :9090); disabled if empty")
	rootCmd.PersistentFlags().Int("metrics-max-targets", 500, "max unique target labels in metrics (0 = unlimited)")

	rootCmd.AddCommand(relayListenerCmd())
	rootCmd.AddCommand(relaySenderCmd())
	rootCmd.AddCommand(arcCmd())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version)
		},
	}
}

// addAuthFlags adds the credential flags to a command.
func addAuthFlags(cmd *cobra.Command) {
	cmd.Flags().String("relay", "", "Azure Relay namespace name, FQDN, or URI")
	cmd.Flags().String("namespace", "", "Azure Relay namespace name (alias for --relay)")
	_ = cmd.Flags().MarkHidden("namespace")
	cmd.Flags().String("hyco", "", "hybrid connection name")
	cmd.Flags().String("relay-suffix", "", "namespace suffix for sovereign clouds (default: .servicebus.windows.net)")
}

// resolveMetrics creates a Metrics instance and starts the HTTP server if
// --metrics-addr or AZTUNNEL_METRICS_ADDR is set. Returns nil if metrics are
// disabled. The provided context controls the server's lifetime — when
// cancelled the server shuts down gracefully.
func resolveMetrics(ctx context.Context, cmd *cobra.Command, logger *slog.Logger) (*metrics.Metrics, error) {
	addr, _ := cmd.Flags().GetString("metrics-addr")
	if addr == "" {
		addr = os.Getenv("AZTUNNEL_METRICS_ADDR")
	}
	if addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen on %s: %w", addr, err)
	}
	m := metrics.New()
	maxTargets, _ := cmd.Flags().GetInt("metrics-max-targets")
	if maxTargets < 0 {
		return nil, fmt.Errorf("--metrics-max-targets must be >= 0, got %d", maxTargets)
	}
	m.MaxTargets = maxTargets
	go func() {
		if err := m.Serve(ctx, ln, logger); err != nil {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	return m, nil
}

// resolveHyco returns the hybrid connection name from --hyco flag, env var, or positional arg.
func resolveHyco(cmd *cobra.Command, args []string) (string, error) {
	if hyco, _ := cmd.Flags().GetString("hyco"); hyco != "" {
		return hyco, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	if hyco := os.Getenv("AZTUNNEL_HYCO_NAME"); hyco != "" {
		return hyco, nil
	}
	return "", fmt.Errorf("hybrid connection name is required: use --hyco or set AZTUNNEL_HYCO_NAME")
}

// resolveAuth determines the endpoint and token provider from CLI flags
// and environment variables.
//
// Resolution order for namespace:
//  1. --relay flag (or hidden --namespace alias)
//  2. AZTUNNEL_RELAY_NAME env var
//
// Resolution order for auth:
//  1. AZTUNNEL_KEY_NAME + AZTUNNEL_KEY → SAS auth
//  2. Otherwise → Entra ID auth (DefaultAzureCredential)
func resolveAuth(cmd *cobra.Command) (endpoint string, tp relay.TokenProvider, err error) {
	ns, _ := cmd.Flags().GetString("relay")
	if ns == "" {
		ns, _ = cmd.Flags().GetString("namespace")
	}
	if ns == "" {
		ns = os.Getenv("AZTUNNEL_RELAY_NAME")
	}
	if ns == "" {
		return "", nil, fmt.Errorf("relay namespace is required: use --relay or set AZTUNNEL_RELAY_NAME")
	}
	suffix, _ := cmd.Flags().GetString("relay-suffix")
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
