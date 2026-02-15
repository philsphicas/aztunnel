# aztunnel

[![CI](https://github.com/philsphicas/aztunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/philsphicas/aztunnel/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/philsphicas/aztunnel)](https://goreportcard.com/report/github.com/philsphicas/aztunnel)
[![Go Reference](https://pkg.go.dev/badge/github.com/philsphicas/aztunnel.svg)](https://pkg.go.dev/github.com/philsphicas/aztunnel)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Tunnel TCP connections through [Azure Relay Hybrid Connections](https://learn.microsoft.com/en-us/azure/azure-relay/relay-what-is-it) using WebSockets. Useful for reaching services behind firewalls or NATs without opening inbound ports or setting up a VPN.

```
┌──────────┐       ┌─────────────┐       ┌──────────┐       ┌────────┐
│  Client  │──TCP──│   Sender    │──WSS──│ Listener │──TCP──│ Target │
│          │       │ (your side) │       │ (remote) │       │        │
└──────────┘       └─────────────┘       └──────────┘       └────────┘
                          │                    │
                          └───── Azure Relay ──┘
```

## Features

- **Port forward** — bind a local port and forward connections to a fixed remote target
- **SOCKS5 proxy** — run a local SOCKS5 server for dynamic target selection
- **SSH ProxyCommand** — bridge stdin/stdout for use with `ssh -o ProxyCommand`
- **Azure Arc support** — connect to Arc-enrolled machines through automatically provisioned relays
- **Allowlist enforcement** — restrict which targets the listener can reach (CIDR, host:port, wildcard)
- **Two auth modes** — SAS keys or Entra ID (DefaultAzureCredential)

## Install

### From source

```sh
go install github.com/philsphicas/aztunnel/cmd/aztunnel@latest
```

### Build locally

```sh
git clone https://github.com/philsphicas/aztunnel.git
cd aztunnel
make build        # outputs bin/aztunnel
make install      # installs to $GOPATH/bin
```

### Docker

```sh
docker run --rm ghcr.io/philsphicas/aztunnel:dev --help
```

Image variants:

| Tag               | Base                   | Description                   |
| ----------------- | ---------------------- | ----------------------------- |
| `:dev`, `:latest` | `scratch`              | Static binary, smallest image |
| `:dev-alpine`     | `alpine`               | Includes shell and apk        |
| `:dev-bookworm`   | `debian:bookworm-slim` | Includes bash and apt         |

Build locally:

```sh
make docker            # scratch (default)
make docker-alpine     # alpine variant
make docker-bookworm   # bookworm variant
```

## Prerequisites

You need an [Azure Relay namespace](https://learn.microsoft.com/en-us/azure/azure-relay/relay-create-namespace-portal) with a Hybrid Connection. See **[Azure Relay Setup Guide](docs/azure-setup.md)** for step-by-step provisioning instructions, including VM deployment with a systemd unit.

## Authentication

aztunnel supports two authentication methods. **Entra ID with Managed Identity
is strongly recommended** — it eliminates secret management entirely.

| Method                     | How to configure                                                                                                                                                        | Best for                                  |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------- |
| **Entra ID** (recommended) | Automatic via [DefaultAzureCredential](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication) — managed identity, `az login`, service principal | Production VMs, containers, development   |
| SAS key                    | Set `AZTUNNEL_KEY_NAME` and `AZTUNNEL_KEY` env vars                                                                                                                     | Quick testing, environments without Entra |

### Entra ID (recommended)

Assign Azure RBAC roles on the hybrid connection:

| Role                     | Assign to                                            |
| ------------------------ | ---------------------------------------------------- |
| **Azure Relay Listener** | The listener's managed identity or service principal |
| **Azure Relay Sender**   | The sender's user account or service principal       |

On a VM with a system-assigned managed identity, aztunnel picks up credentials
automatically — no configuration needed beyond the namespace name. See
[Deploying a listener on a VM](docs/azure-setup.md#2a-deploy-a-listener-on-an-azure-vm-with-managed-identity)
for a complete walkthrough with a systemd unit.

For local development, `az login` is sufficient.

### SAS keys

When using SAS keys, create **hybrid connection–level** policies with
least-privilege claims (`Listen` for the listener, `Send` for the sender).
Never use `RootManageSharedAccessKey` in production. See
[SAS key setup](docs/azure-setup.md#3-authentication-with-sas-keys)
for detailed instructions.

### Namespace

The relay namespace name is always required:

```sh
export AZTUNNEL_RELAY_NAME="mynamespace"
```

Or pass `--relay mynamespace` to any command.

## Quick start

### Port forward

Forward local port 2222 to a remote SSH server at `10.0.0.5:22`:

```sh
# On the remote side (where 10.0.0.5 is reachable):
aztunnel relay-listener --relay my-ns --hyco my-hyco --allow "10.0.0.5:22"

# On your machine:
aztunnel relay-sender port-forward --relay my-ns --hyco my-hyco 10.0.0.5:22 -b 127.0.0.1:2222

# Connect:
ssh -p 2222 user@127.0.0.1
```

### SOCKS5 proxy

Run a local SOCKS5 proxy on port 1080, forwarding any target through the relay:

```sh
# Remote:
aztunnel relay-listener --relay my-ns --hyco my-hyco --allow "10.0.0.0/8:*"

# Local:
aztunnel relay-sender socks5-proxy --relay my-ns --hyco my-hyco

# Use it:
curl --socks5 127.0.0.1:1080 http://10.0.0.5:8080/health
```

### SSH ProxyCommand

Use aztunnel as an SSH proxy command for transparent tunneling:

```sh
ssh -o ProxyCommand="aztunnel relay-sender connect --relay my-ns --hyco my-hyco %h:%p" user@10.0.0.5
```

Or in `~/.ssh/config` (with env vars set):

```
Host remote-*
    ProxyCommand aztunnel relay-sender connect %h:%p
```

## Azure Arc

aztunnel can connect to [Azure Arc-enrolled machines](https://learn.microsoft.com/en-us/azure/azure-arc/servers/overview) through the Azure Relay that Azure provisions automatically when the OpenSSH extension is installed. No separate relay namespace or listener is needed — the Arc agent on the VM acts as the listener.

### Prerequisites

- The target machine must be Arc-enrolled (`Microsoft.HybridCompute/machines`)
- The `Microsoft.HybridConnectivity` resource provider must be registered on the subscription
- You need [DefaultAzureCredential](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication) access to the machine's ARM resource (e.g., via `az login`)
- SSH must be running on the target machine

### Arc SSH ProxyCommand

```sh
ssh -o ProxyCommand="aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM" user@myVM
```

Or in `~/.ssh/config`:

```
Host arc-*
    ProxyCommand aztunnel arc connect --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/%n
```

### Arc port forward

Forward local port 2222 to SSH on an Arc VM:

```sh
aztunnel arc port-forward --resource-id /subscriptions/SUB/resourceGroups/RG/providers/Microsoft.HybridCompute/machines/myVM -b 127.0.0.1:2222

# Then connect:
ssh -p 2222 user@127.0.0.1
```

### Custom port

If SSH listens on a non-standard port (e.g., 2222):

```sh
aztunnel arc connect --resource-id /subscriptions/.../machines/myVM --port 2222
```

## CLI reference

```
aztunnel [command] [flags]

Commands:
  relay-listener                        Listen on Azure Relay and forward to targets
  relay-sender port-forward             Forward a local port through the relay
  relay-sender socks5-proxy             Run a local SOCKS5 proxy through the relay
  relay-sender connect                  One-shot stdin/stdout connection (ProxyCommand)
  arc connect                           One-shot connection through an Arc relay (ProxyCommand)
  arc port-forward                      Forward a local port through an Arc relay
  version                               Print the version

Global flags:
  --log-level string   Log level: debug, info, warn, error (default "info")
```

### relay-listener

```
aztunnel relay-listener [hyco] [flags]

Flags:
  --relay string         Azure Relay namespace name
  --hyco string              Hybrid connection name
  --allow strings            Allowed targets (repeatable, see Allowlist below)
  --max-connections int      Max concurrent connections (0 = unlimited)
  --connect-timeout duration Timeout for dialing targets (default 30s)
  --tcp-keepalive duration   TCP keepalive interval (default 30s)
```

### relay-sender port-forward

```
aztunnel relay-sender port-forward <host:port> [flags]

Flags:
  --relay string       Azure Relay namespace name
  --hyco string            Hybrid connection name
  -b, --bind string        Local bind address:port (default "127.0.0.1:0")
  --gateway                Bind to 0.0.0.0 instead of 127.0.0.1
  --tcp-keepalive duration TCP keepalive interval (default 30s)
```

### relay-sender socks5-proxy

```
aztunnel relay-sender socks5-proxy [hyco] [flags]

Flags:
  --relay string       Azure Relay namespace name
  --hyco string            Hybrid connection name
  -b, --bind string        Local bind address:port (default "127.0.0.1:1080")
  --gateway                Bind to 0.0.0.0 instead of 127.0.0.1
  --tcp-keepalive duration TCP keepalive interval (default 30s)
```

### relay-sender connect

```
aztunnel relay-sender connect <host:port> [flags]

Flags:
  --relay string   Azure Relay namespace name
  --hyco string        Hybrid connection name
```

### arc connect

```
aztunnel arc connect [flags]

Flags:
  --resource-id string   ARM resource ID of the Arc-connected machine
  --port int             Remote port the service listens on (default 22)
  --service string       Service name: SSH or WAC (default "SSH")
```

### arc port-forward

```
aztunnel arc port-forward [flags]

Flags:
  --resource-id string       ARM resource ID of the Arc-connected machine
  --port int                 Remote port the service listens on (default 22)
  --service string           Service name: SSH or WAC (default "SSH")
  -b, --bind string          Local bind address:port (default "127.0.0.1:0")
  --gateway                  Bind to 0.0.0.0 instead of 127.0.0.1
  --tcp-keepalive duration   TCP keepalive interval (default 30s)
```

## Allowlist

The listener's `--allow` flag restricts which targets can be dialed. Entries are matched against the target `host:port` requested by the sender.

| Format      | Example          | Matches                           |
| ----------- | ---------------- | --------------------------------- |
| `host:port` | `10.0.0.5:22`    | Exact match only                  |
| `CIDR:port` | `10.0.0.0/24:22` | Any IP in the CIDR on port 22     |
| `CIDR:*`    | `10.0.0.0/8:*`   | Any IP in the CIDR on any port    |
| `*`         | `*`              | Everything (same as no allowlist) |

If no `--allow` flags are given, **all targets are permitted** (a warning is logged).

Hostnames are matched literally — no DNS resolution is performed. Use CIDR notation for IP-based restrictions.

## Environment variables

| Variable                  | Description                                  |
| ------------------------- | -------------------------------------------- |
| `AZTUNNEL_RELAY_NAME`     | Azure Relay namespace name                   |
| `AZTUNNEL_HYCO_NAME`      | Hybrid connection name                       |
| `AZTUNNEL_KEY_NAME`       | SAS policy name                              |
| `AZTUNNEL_KEY`            | SAS key value                                |
| `AZTUNNEL_ARC_RESOURCE_ID`| ARM resource ID of the Arc-connected machine |

## License

[MIT](LICENSE)
