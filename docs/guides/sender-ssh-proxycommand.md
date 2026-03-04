# Sender: SSH ProxyCommand

Use `aztunnel relay-sender connect` as an SSH `ProxyCommand` for transparent
tunneling. Each SSH session opens a dedicated WebSocket through Azure Relay —
no local port needed.

```
┌──────────┐       ┌─────────────────────────┐       ┌──────────┐
│   ssh    │──────▶│ aztunnel relay-sender    │══WSS══│ listener │──▶ sshd
│          │ stdio │ connect <host>:<port>    │       │          │
└──────────┘       └─────────────────────────┘       └──────────┘
```

## Prerequisites

- A running aztunnel listener on the remote side (see
  [listener guides](README.md#listener-remote-side))
- Azure Relay **Sender** credentials (SAS key or Entra ID)
- SSH client (OpenSSH)

## Basic usage

Pass `aztunnel relay-sender connect` as the SSH `ProxyCommand`. The `%h:%p`
tokens are replaced by SSH with the target hostname and port:

```sh
ssh -o ProxyCommand="aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p" user@10.0.0.5
```

## Using environment variables

Set credentials once and keep the command short:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel
export AZTUNNEL_KEY_NAME=send-policy
export AZTUNNEL_KEY='<your-sas-key>'

ssh -o ProxyCommand="aztunnel relay-sender connect %h:%p" user@10.0.0.5
```

With Entra ID (e.g., after `az login`), omit the key variables entirely:

```sh
export AZTUNNEL_RELAY_NAME=my-relay-ns
export AZTUNNEL_HYCO_NAME=my-tunnel

ssh -o ProxyCommand="aztunnel relay-sender connect %h:%p" user@10.0.0.5
```

## SSH config patterns

Add entries to `~/.ssh/config` so you never have to type the ProxyCommand
manually.

### Single host

The simplest case — one relay, one hybrid connection, one VM. `%h` expands
to the `HostName` value, so the listener receives `10.0.0.5:22` as the
target to dial:

```
Host myvm
    HostName 10.0.0.5
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p
```

Then just: `ssh myvm`

If the hybrid connection already maps to a single VM (one hyco per machine),
the target address doesn't matter for routing — but it still needs to match
the listener's allowlist. Use `HostName` to set the address the listener
will dial and check against `--allow`.

### Wildcard pattern for a group of hosts

If multiple VMs share the same hybrid connection (e.g., behind one gateway
listener), use a wildcard. Here `%h` expands to what the user types (e.g.,
`remote-web`), so the listener must be able to resolve and allow that name:

```
Host remote-*
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p
```

`ssh remote-10.0.0.5` sends `10.0.0.5:22` to the listener. If you use
hostnames like `ssh remote-webserver`, the listener's allowlist must include
that hostname (e.g., `--allow "remote-webserver:22"`).

### Per-host hybrid connection

If each VM has its own hybrid connection (recommended for targeting specific
machines). Since each hyco maps to exactly one listener, the target just
needs to match the allowlist:

```
Host vm1
    HostName 127.0.0.1
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco tunnel-vm1 %h:%p

Host vm2
    HostName 127.0.0.1
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco tunnel-vm2 %h:%p
```

Here `HostName 127.0.0.1` means the listener dials `127.0.0.1:22` (its own
SSH server). The hyco name selects which VM you reach.

### Different relays

If VMs are spread across different relay namespaces:

```
Host prod-vm
    HostName 10.0.0.5
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay prod-relay --hyco prod-tunnel %h:%p

Host dev-vm
    HostName 10.1.0.5
    User azureuser
    ProxyCommand aztunnel relay-sender connect --relay dev-relay --hyco dev-tunnel %h:%p
```

## SCP, rsync, and other SSH-based tools

Any tool that uses SSH benefits automatically from the ProxyCommand:

```sh
# Copy a file
scp myvm:~/data.csv .

# Rsync
rsync -avz myvm:/var/log/app/ ./logs/

# Git over SSH
git clone ssh://azureuser@10.0.0.5/~/repo.git
```

## Agent forwarding

SSH agent forwarding works normally through the tunnel:

```sh
ssh -A -o ProxyCommand="aztunnel relay-sender connect %h:%p" user@10.0.0.5
```

Or in `~/.ssh/config`:

```
Host myvm
    HostName 10.0.0.5
    User azureuser
    ForwardAgent yes
    ProxyCommand aztunnel relay-sender connect %h:%p
```

## Debugging

If the connection fails, increase the log level:

```sh
ssh -o ProxyCommand="aztunnel relay-sender connect --log-level debug %h:%p" user@10.0.0.5
```

aztunnel logs to stderr, so SSH still works (it reads stdout). Common issues:

| Symptom                | Likely cause                                                    |
| ---------------------- | --------------------------------------------------------------- |
| `auth failed`          | Wrong SAS key, expired token, or missing RBAC role              |
| `no active listener`   | Listener isn't running or is on a different hybrid connection   |
| `allowlist rejected`   | The listener's `--allow` doesn't include the target `host:port` |
| `connection timed out` | The listener can't reach the target service                     |

## How it works

`relay-sender connect` bridges stdin/stdout to a WebSocket connection through
Azure Relay. SSH treats it like a direct TCP connection:

1. SSH launches `aztunnel relay-sender connect 10.0.0.5:22` as a subprocess
2. aztunnel opens a WebSocket to Azure Relay, sending the target in the
   protocol envelope
3. The listener receives the connection, dials `10.0.0.5:22`, and bridges
   the two sides
4. SSH communicates over stdin/stdout of the aztunnel process
5. When SSH disconnects, the WebSocket closes

Each SSH session gets its own WebSocket — there's no multiplexing, which
keeps things simple and avoids head-of-line blocking.
