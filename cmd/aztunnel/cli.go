package main

import (
	"fmt"
	"time"

	"github.com/alecthomas/kong"
	"github.com/willabides/kongplete"
)

// CLI defines the top-level command structure.
var CLI struct {
	Globals

	RelayListener RelayListenerCmd             `cmd:"" name:"relay-listener" help:"Listen on Azure Relay and forward connections to local targets."`
	RelaySender   RelaySenderCmd               `cmd:"" name:"relay-sender" help:"Send connections through Azure Relay."`
	Arc           ArcCmd                       `cmd:"" help:"Connect through Azure Arc managed relays."`
	Version       VersionFlag                  `name:"version" help:"Print version and exit."`
	Completion    kongplete.InstallCompletions `cmd:"" help:"Output shell completion script." hidden:""`
}

// Globals holds flags inherited by all commands.
type Globals struct {
	LogLevel          string `name:"log-level" help:"Log level (debug, info, warn, error)." default:"info"`
	MetricsAddr       string `name:"metrics-addr" help:"Address for Prometheus metrics server (e.g. :9090); disabled if empty."`
	MetricsMaxTargets int    `name:"metrics-max-targets" help:"Max unique target labels in metrics (0 = unlimited)." default:"500"`
}

// VersionFlag prints the version and exits when used as --version.
type VersionFlag bool

// BeforeApply prints the version and exits.
func (v VersionFlag) BeforeApply(app *kong.Kong) error {
	_, _ = fmt.Fprintln(app.Stdout, version)
	app.Exit(0)
	return nil
}

// AuthFlags holds Azure Relay authentication flags shared across relay commands.
type AuthFlags struct {
	Relay       string `help:"Azure Relay namespace name, FQDN, or URI."`
	Namespace   string `name:"namespace" help:"Azure Relay namespace name (alias for --relay)." hidden:""`
	Hyco        string `help:"Hybrid connection name."`
	RelaySuffix string `name:"relay-suffix" help:"Namespace suffix for sovereign clouds." default:""`
}

// BindFlags holds local bind flags shared across port-forward and socks5 commands.
type BindFlags struct {
	Bind         string        `short:"b" help:"Local bind address:port." default:"127.0.0.1:0"`
	Gateway      bool          `help:"Bind to 0.0.0.0 instead of 127.0.0.1."`
	TCPKeepAlive time.Duration `name:"tcp-keepalive" help:"TCP keepalive interval." default:"30s"`
}

// RelaySenderCmd is a grouping command for relay sender subcommands.
type RelaySenderCmd struct {
	PortForward PortForwardCmd `cmd:"" name:"port-forward" help:"Forward a local port through the relay to a specific target."`
	Connect     ConnectCmd     `cmd:"" help:"One-shot stdin/stdout connection through the relay."`
	Socks5Proxy Socks5ProxyCmd `cmd:"" name:"socks5-proxy" help:"Run a local SOCKS5 proxy that forwards through the relay."`
}

// ArcCmd is the parent command for Azure Arc subcommands.
type ArcCmd struct {
	ResourceID string `name:"resource-id" help:"ARM resource ID of the Arc-connected machine."`
	Port       int    `help:"Remote port the service listens on." default:"22"`
	Service    string `help:"Service name (SSH or WAC)." default:"SSH"`

	Connect     ArcConnectCmd     `cmd:"" help:"One-shot stdin/stdout connection through an Arc relay."`
	PortForward ArcPortForwardCmd `cmd:"" name:"port-forward" help:"Forward a local port through an Arc relay."`
}
