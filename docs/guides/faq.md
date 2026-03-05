# Frequently Asked Questions

## What is bgtask and how does it relate to aztunnel?

[bgtask](https://github.com/philsphicas/bgtask) is a companion tool for
running long-lived processes in the background with named tasks, log capture,
and lifecycle management. It's a better alternative to `nohup`, `&`, or
`tmux` for ad-hoc processes.

aztunnel listeners and senders are long-running processes that you often want
to run in the background — especially during development, testing, or on
machines without systemd/Kubernetes. bgtask is ideal for this:

```sh
# Start a listener as a named background task
bgtask run --name listener -- aztunnel relay-listener --allow "localhost:8080"

# Start a sender
bgtask run --name sender -- aztunnel relay-sender port-forward -b 127.0.0.1:8080 localhost:80

# Check status
bgtask ls

# Follow logs
bgtask logs -f listener

# Stop when done
bgtask stop listener
bgtask stop sender
```

**Why not just `&`?** When you background a process with `&`, you lose the
output, the process dies if you close your terminal, and finding it again
means hunting through `ps`. bgtask gives you named tasks with structured
logs, auto-restart, and simple lifecycle commands.

**When to use what:**

| Deployment            | Tool                                                |
| --------------------- | --------------------------------------------------- |
| Production VM         | [systemd](listener-systemd.md)                      |
| Kubernetes            | [sidecar container](listener-kubernetes-sidecar.md) |
| Development / testing | [bgtask](https://github.com/philsphicas/bgtask)     |
| Quick one-off test    | Foreground (Ctrl+C to stop)                         |

Install bgtask:

```sh
go install github.com/philsphicas/bgtask/cmd/bgtask@latest
```

See the [bgtask README](https://github.com/philsphicas/bgtask) for full
documentation.

## Can I use Entra ID instead of SAS keys?

Yes, and it's recommended for production. aztunnel uses
[DefaultAzureCredential](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication),
which automatically picks up:

- **Managed Identity** (VMs, ACI, AKS with Workload Identity)
- **Azure CLI** (`az login`) for development
- **Service Principal** (via `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
  `AZURE_CLIENT_SECRET`)

When using Entra ID, you don't need `AZTUNNEL_KEY_NAME` or `AZTUNNEL_KEY` —
just the relay namespace name and the appropriate RBAC role assignment.

The guides use SAS keys in examples because they're simpler to set up for
testing, but always switch to Entra ID for production. See the
[authentication section](../../README.md#authentication) for details.

## One hybrid connection per VM, or one for everything?

**One per VM** when you need to reach specific machines. Each VM runs its own
listener on its own hybrid connection, and you specify `--hyco` on the sender
to choose which one.

**One shared hybrid connection** when you want a gateway pattern — one
listener that can forward to many targets. The sender uses SOCKS5 to choose
the target at connection time.

See [VNet gateway](scenario-vnet-gateway.md) for the gateway pattern and
the [Azure setup guide](../azure-setup.md#hybrid-connections-and-multiple-listeners)
for platform limits.

## How does DNS resolution work with the SOCKS5 proxy?

The sender passes the target address **as-is** to the listener — no DNS
resolution happens on the sender side. The listener checks the allowlist
against the exact string it received, then dials the target (which is when
Go's stdlib resolves DNS if needed).

This means:

- `curl --socks5h` sends the **hostname** → listener needs a hostname
  allowlist entry (e.g., `--allow "mydb:5432"`)
- `curl --socks5` resolves DNS **locally** and sends the **IP** → listener
  needs a CIDR or IP allowlist entry (e.g., `--allow "10.0.0.0/8:*"`)

**Use `--socks5h` in most cases** — the listener is on the remote network
and can resolve internal hostnames that your workstation can't.

See [Sender: SOCKS5 Proxy — DNS resolution](sender-socks5-proxy.md#dns-resolution---socks5-vs---socks5h)
for the full explanation.

## What ports does aztunnel need open?

**Outbound HTTPS (443) only.** Both the listener and sender connect outbound
to `*.servicebus.windows.net` over WebSocket (WSS). No inbound ports are
needed on either side.

## How do I monitor aztunnel in production?

Pass `--metrics-addr :9090` to expose Prometheus metrics at `/metrics`. Key
metrics to watch:

- `aztunnel_active_connections` — current connection count
- `aztunnel_control_channel_connected` — is the listener connected to the
  relay? (should be 1)
- `aztunnel_connection_errors_total` — errors by reason (dial_failed,
  allowlist_rejected, auth_failed)

See the [README metrics section](../../README.md#metrics) for the full list.

## Can I run aztunnel in a container?

Yes. Pre-built images are available on GitHub Container Registry:

```
ghcr.io/philsphicas/aztunnel:latest
```

| Tag                | Base                   | Description                   |
| ------------------ | ---------------------- | ----------------------------- |
| `:latest`          | `scratch`              | Static binary, smallest image |
| `:latest-alpine`   | `alpine`               | Includes shell and apk        |
| `:latest-bookworm` | `debian:bookworm-slim` | Includes bash and apt         |

See [Listener: Kubernetes sidecar](listener-kubernetes-sidecar.md) for
Kubernetes deployment patterns.
