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
	Relay            string `help:"Azure Relay namespace name, FQDN, or URI."`
	Namespace        string `name:"namespace" help:"Azure Relay namespace name (alias for --relay)." hidden:""`
	Hyco             string `help:"Hybrid connection name."`
	RelaySuffix      string `name:"relay-suffix" help:"Namespace suffix for sovereign clouds." default:""`
	RelayInsecureTLS bool   `name:"relay-insecure-tls" help:"Skip TLS certificate verification (mock/self-hosted only)."`
}

// BindFlags holds local bind flags shared across port-forward and socks5 commands.
type BindFlags struct {
	Bind         string        `short:"b" help:"Local bind address:port." default:"127.0.0.1:0"`
	Gateway      bool          `help:"Bind to 0.0.0.0 instead of 127.0.0.1."`
	TCPKeepAlive time.Duration `name:"tcp-keepalive" help:"TCP keepalive interval." default:"30s"`
}

// MuxFlags holds stream-multiplexing flags shared across port-forward and
// socks5-proxy. When mux is enabled (default), the sender establishes a
// persistent relay WebSocket *lazily* on the first connection that needs
// one — that first connection still pays the full ~1-2s rendezvous, and
// subsequent connections that reuse an already-established mux session
// are carried as logical smux streams that skip the per-connection
// rendezvous. When `--mux-sessions > 1` is set and concurrent traffic
// forces the pool to grow, each additional session is also dialed lazily
// on its first connection, which pays a rendezvous too.
// The sender automatically falls back to the v1 path if the listener does
// not support v2.
type MuxFlags struct {
	NoMux                     bool          `name:"no-mux" env:"AZTUNNEL_NO_MUX" help:"Disable stream multiplexing; use a fresh relay rendezvous per connection. Slower but matches older versions exactly."`
	MuxSessions               int           `name:"mux-sessions" env:"AZTUNNEL_MUX_SESSIONS" help:"Maximum number of persistent relay rendezvous WebSockets to maintain. Larger values may spread concurrent traffic across multiple HA listeners; Azure Relay listener selection is opaque, so empirical testing recommended." default:"2"`
	MaxStreamsPerSession      int           `name:"max-streams-per-session" env:"AZTUNNEL_MAX_STREAMS_PER_SESSION" help:"Maximum concurrent streams per mux session before back-pressure kicks in. New connections wait for a slot; the port-forward and SOCKS5 paths cap that wait at an internal 60s mux admission timeout, after which the connection is dropped (and aztunnel_mux_pool_saturated_total increments)." default:"256" hidden:""`
	MuxStreamHandshakeTimeout time.Duration `name:"mux-stream-handshake-timeout" env:"AZTUNNEL_MUX_STREAM_HANDSHAKE_TIMEOUT" help:"Per-stream envelope+response timeout. Must exceed the listener's --connect-timeout because the listener dials the target before writing the response." default:"60s" hidden:""`
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
