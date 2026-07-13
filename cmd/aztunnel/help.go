package main

import (
	"io"

	"github.com/alecthomas/kong"
)

func customHelpPrinter(options kong.HelpOptions, ctx *kong.Context) error {
	if ctx.Selected() != nil {
		return kong.DefaultHelpPrinter(options, ctx)
	}
	_, err := io.WriteString(ctx.Stdout, topLevelHelp)
	return err
}

const topLevelHelp = `aztunnel - Tunnel TCP connections through Azure Relay Hybrid Connections.

Aztunnel creates encrypted TCP tunnels through Azure Relay Hybrid
Connections. It has two modes:

  relay mode    Bring your own Azure Relay namespace. A relay-listener
                runs near the target and a relay-sender runs near the
                client.

  arc mode      Use Azure Arc's automatically provisioned relay. Works
                with any Arc-connected machine that has the
                HybridConnectivity endpoint enabled — no listener needed.

Usage:
  aztunnel relay-listener [flags]
  aztunnel relay-sender port-forward <host:port> [flags]
  aztunnel relay-sender socks5-proxy [flags]
  aztunnel relay-sender connect <host:port> [flags]
  aztunnel arc connect [flags]
  aztunnel arc port-forward [flags]
  aztunnel arc aad-cert [flags]

Global Options:
      --log-level string            Log level: debug, info, warn, error (default "info")
      --metrics-addr string         Prometheus metrics server address (e.g. :9090); disabled if empty
      --metrics-max-targets int     Max unique target labels in metrics; 0 = unlimited (default 500)
      --help, -h                    Show this help message
      --version                     Print version and exit

Relay Listener:
  Start a relay-listener that accepts connections from the Azure Relay
  hybrid connection and forwards them to the targets specified in the
  connect envelopes. Optionally restrict allowed targets with --allow.

      --relay string                Azure Relay namespace name, FQDN, or URI
      --hyco string                 Hybrid connection name
      --relay-suffix string         Namespace suffix for sovereign clouds
      --allow strings               Allowed targets (host:port, CIDR:port, CIDR:*)
      --max-connections int         Max concurrent connections; 0 = unlimited (default 0)
      --connect-timeout duration    Timeout for dialing targets (default 30s)
      --tcp-keepalive duration      TCP keepalive interval (default 30s)

Relay Sender - Port Forward:
  Start a local TCP listener and forward each connection through the
  Azure Relay to the specified target host:port.

      --relay string                Azure Relay namespace name, FQDN, or URI
      --hyco string                 Hybrid connection name
      --relay-suffix string         Namespace suffix for sovereign clouds
  -b, --bind string                 Local bind address:port (default "127.0.0.1:0")
      --gateway                     Bind to 0.0.0.0 instead of 127.0.0.1
      --tcp-keepalive duration      TCP keepalive interval (default 30s)

Relay Sender - Connect:
  Connect to the relay, tell the listener to dial host:port, then bridge
  stdin/stdout with the tunnel. Exits when the connection closes.
  Designed for use as an SSH ProxyCommand.

      --relay string                Azure Relay namespace name, FQDN, or URI
      --hyco string                 Hybrid connection name
      --relay-suffix string         Namespace suffix for sovereign clouds

Relay Sender - SOCKS5 Proxy:
  Start a local SOCKS5 proxy server. The target for each connection is
  determined from the SOCKS5 handshake and forwarded through the relay.

      --relay string                Azure Relay namespace name, FQDN, or URI
      --hyco string                 Hybrid connection name
      --relay-suffix string         Namespace suffix for sovereign clouds
  -b, --bind string                 Local bind address:port (default "127.0.0.1:0")
      --gateway                     Bind to 0.0.0.0 instead of 127.0.0.1
      --tcp-keepalive duration      TCP keepalive interval (default 30s)

Arc Connect:
  Connect to an Azure Arc-enrolled machine through the automatically
  provisioned Azure Relay. Bridges stdin/stdout with the tunnel, then
  exits when the connection closes. Designed for use as an SSH
  ProxyCommand.

      --resource-id string          ARM resource ID of the Arc-connected machine
      --port int                    Remote port the service listens on (default 22)
      --service string              Service name: SSH or WAC (default "SSH")

Arc Port Forward:
  Start a local TCP listener and forward each connection through the
  Azure Arc managed relay to the remote service.

      --resource-id string          ARM resource ID of the Arc-connected machine
      --port int                    Remote port the service listens on (default 22)
      --service string              Service name: SSH or WAC (default "SSH")
  -b, --bind string                 Local bind address:port (default "127.0.0.1:0")
      --gateway                     Bind to 0.0.0.0 instead of 127.0.0.1
      --tcp-keepalive duration      TCP keepalive interval (default 30s)

Arc AAD SSH Certificate:
  Mint a short-lived Microsoft Entra ID SSH certificate for machines using
  the AADSSHLoginForLinux extension. Designed for an SSH config "Match final
  exec" directive so plain "ssh" acquires a certificate automatically. The
  login username (the certificate's first principal) is printed to stderr.

      --dir string                  Directory holding this connection's key/cert (point at ~/.ssh/arc/%C)
      --user string                 SSH login name (ssh %r); selects the cached Entra account
      --identity string             Private key path, created if missing (alternative to --dir)
      --cert string                 Certificate output path (default "<identity>-cert.pub")
      --token-cache string          MSAL token cache path for silent renewal
      --client-id string            OAuth public client ID (default Azure CLI)
      --tenant string               Entra ID tenant, or organizations/common (default "organizations")
      --scope string                Token scope yielding the certificate (default AADSSHLoginForLinux)
      --min-valid duration          Reuse an existing certificate valid at least this long (default 5m)

Authentication:
  Relay commands authenticate to the Azure Relay namespace:

  1. Entra ID (default): Uses DefaultAzureCredential automatically
                         (az login, managed identity, workload identity).
  2. SAS credentials:    Override by setting both AZTUNNEL_KEY_NAME and
                         AZTUNNEL_KEY env vars. Only needed when Entra ID
                         is unavailable.

  Arc commands authenticate via DefaultAzureCredential to the Azure
  Resource Manager API. No relay credentials are needed — Azure provides
  short-lived SAS tokens automatically.

  arc aad-cert additionally signs in to Microsoft Entra ID interactively
  (browser) the first time to obtain the SSH certificate, then caches a
  refresh token for silent renewals. This is separate from the ARM
  DefaultAzureCredential used by the tunnel transport.

Environment Variables:
  AZTUNNEL_RELAY_NAME        Relay namespace (fallback for --relay)
  AZTUNNEL_RELAY_SUFFIX      Namespace suffix (fallback for --relay-suffix)
  AZTUNNEL_HYCO_NAME         Hybrid connection name (fallback for --hyco)
  AZTUNNEL_KEY_NAME          SAS authorization rule name (optional, overrides Entra)
  AZTUNNEL_KEY               SAS key value (optional, overrides Entra)
  AZTUNNEL_ARC_RESOURCE_ID   Arc resource ID (fallback for --resource-id)
  AZTUNNEL_METRICS_ADDR      Metrics server address (fallback for --metrics-addr)

Examples:
  # Start a relay listener allowing only SSH and HTTPS targets
  aztunnel relay-listener --relay my-ns --hyco tunnel \
    --allow 10.0.0.0/8:22 --allow 10.0.0.0/8:443

  # Forward local port 2222 to remote host through the relay
  aztunnel relay-sender port-forward --relay my-ns --hyco tunnel \
    -b 127.0.0.1:2222 db-server:5432

  # Use as SSH ProxyCommand (relay mode)
  ssh -o ProxyCommand="aztunnel relay-sender connect \
    --relay my-ns --hyco tunnel %h:%p" user@host

  # Use as SSH ProxyCommand (arc mode)
  ssh -o ProxyCommand="aztunnel arc connect \
    --resource-id /subscriptions/.../machines/myVM" user@host

  # Automatic Entra ID (AADSSHLoginForLinux) SSH via ~/.ssh/config
  # (requires OpenSSH 9.6+ for safe Match exec token expansion):
  #   Host /subscriptions/*
  #       StrictHostKeyChecking accept-new
  #       UserKnownHostsFile ~/.ssh/arc/%C/known_hosts
  #       IdentityFile ~/.ssh/arc/%C/id
  #       CertificateFile ~/.ssh/arc/%C/id-cert.pub
  #       Match final exec "aztunnel arc aad-cert --resource-id %n --user %r --dir ~/.ssh/arc/%C"
  #       ProxyCommand aztunnel arc connect --resource-id %n --port %p
  # Then: ssh <entra-upn>@/subscriptions/.../machines/myVM

  # Forward a local port through Arc
  aztunnel arc port-forward \
    --resource-id /subscriptions/.../machines/myVM \
    -b 127.0.0.1:2222
  ssh -p 2222 user@127.0.0.1

  # Run a SOCKS5 proxy for dynamic forwarding
  aztunnel relay-sender socks5-proxy --relay my-ns --hyco tunnel -b 127.0.0.1:1080
  curl --proxy socks5h://127.0.0.1:1080 http://internal-service:8080

Run "aztunnel <command> --help" for full flag details.
`
