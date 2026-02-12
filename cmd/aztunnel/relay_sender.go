package main

import (
	"github.com/spf13/cobra"
)

func relaySenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay-sender",
		Short: "Send connections through Azure Relay",
		Long: `The relay-sender opens local ports or stdin/stdout and tunnels
connections through the Azure Relay hybrid connection.`,
	}

	cmd.AddCommand(portForwardCmd())
	cmd.AddCommand(socks5ProxyCmd())
	cmd.AddCommand(connectCmd())

	return cmd
}
