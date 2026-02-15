package main

import (
	"fmt"
	"os"

	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:   "aztunnel",
		Short: "Azure Relay Hybrid Connection tunnel",
		Long:  "Tunnel TCP connections through Azure Relay Hybrid Connections.",
	}

	// Global flags.
	rootCmd.PersistentFlags().String("log-level", "info", "log level (debug, info, warn, error)")

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
	cmd.Flags().String("relay", "", "Azure Relay namespace name")
	cmd.Flags().String("namespace", "", "Azure Relay namespace name (alias for --relay)")
	_ = cmd.Flags().MarkHidden("namespace")
	cmd.Flags().String("hyco", "", "hybrid connection name")
}

const relaySuffix = ".servicebus.windows.net"

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
	endpoint = "sb://" + ns + relaySuffix

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
