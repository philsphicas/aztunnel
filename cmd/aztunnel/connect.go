package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/spf13/cobra"
)

func connectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect <host:port>",
		Short: "One-shot stdin/stdout connection through the relay",
		Long: `Connect to the relay, tell the listener to dial host:port, then
bridge stdin/stdout with the tunnel. Exits when the connection closes.
Designed for use as an SSH ProxyCommand.

Example:
  ssh -o ProxyCommand="aztunnel relay-sender connect --relay my-ns --hyco my-hyco %%h:%%p" user@host`,
		Args: cobra.ExactArgs(1),
		RunE: runConnect,
	}

	addAuthFlags(cmd)
	cmd.Flags().Int("dial-retries", 3, "number of relay dial retry attempts on failure (0 = no retries)")
	return cmd
}

func runConnect(cmd *cobra.Command, args []string) error {
	hyco, err := resolveHyco(cmd, nil)
	if err != nil {
		return err
	}
	target := args[0]

	endpoint, tp, err := resolveAuth(cmd)
	if err != nil {
		return err
	}

	logLevel, _ := cmd.Flags().GetString("log-level")
	logger := newLogger(logLevel)
	dialRetries, _ := cmd.Flags().GetInt("dial-retries")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := sender.ConnectConfig{
		Endpoint:      endpoint,
		EntityPath:    hyco,
		TokenProvider: tp,
		Target:        target,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Logger:        logger,
		DialRetries:   dialRetries,
	}
	if cfg.Metrics, err = resolveMetrics(ctx, cmd, logger); err != nil {
		return err
	}

	return sender.Connect(ctx, cfg)
}
