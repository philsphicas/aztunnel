# Scenario: Multi-Hop Tunneling

Chain aztunnel with SSH jump hosts to reach machines in complex network
topologies — for example, a host that's only reachable from another host
that's only reachable via a relay.

```
┌─────────────┐       ┌──────────────┐       ┌──────────┐       ┌──────────┐
│ Workstation  │       │ Azure Relay  │       │ Jump     │       │ Target   │
│              │       │              │       │ Host     │       │          │
│  ssh ────────┤══WSS══│              │══WSS══│          │──SSH──│          │
│              │       │              │       │ 10.0.0.5 │       │ 10.1.0.5 │
└─────────────┘       └──────────────┘       └──────────┘       └──────────┘
```

## When to use multi-hop

- The target is in a subnet that only the jump host can reach
- You need to traverse multiple security boundaries
- Network policy prevents direct relay-to-target connectivity

> **Consider first**: If the listener can reach the target directly, you
> don't need multi-hop. The [gateway pattern](scenario-vnet-gateway.md) is
> simpler — one listener with a wide allowlist.

## Option 1: aztunnel + SSH jump host (`-J`)

The simplest approach — aztunnel tunnels to the jump host, then SSH jumps
from there to the target:

```sh
# SSH config
Host jump
    HostName 10.0.0.5
    User admin
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p

Host target
    HostName 10.1.0.5
    User admin
    ProxyJump jump
```

Then:

```sh
ssh target
```

This creates: workstation → aztunnel → Azure Relay → listener → jump host (SSH) → target.

## Option 2: aztunnel SOCKS5 + SSH jump

If you need to reach multiple targets behind the jump host, use SOCKS5 to
reach the jump host, then SSH from there:

```sh
# Start SOCKS5 proxy
aztunnel relay-sender socks5-proxy --relay my-relay-ns --hyco my-tunnel --bind 127.0.0.1:1080

# SSH to jump host through SOCKS5, then jump to target
ssh -o ProxyCommand="nc -x 127.0.0.1:1080 %h %p" -J admin@10.0.0.5 admin@10.1.0.5
```

## Option 3: aztunnel port-forward + SSH jump

Forward a local port to the jump host's SSH port, then jump:

```sh
# Forward to jump host SSH
aztunnel relay-sender port-forward --hyco my-tunnel --bind 127.0.0.1:2222 10.0.0.5:22

# Jump through the forwarded port
ssh -J admin@127.0.0.1:2222 admin@10.1.0.5
```

## SSH config for multi-hop

A complete `~/.ssh/config` for a multi-hop setup:

```
# Jump host — reachable via aztunnel relay
Host jump-*
    User admin
    ProxyCommand aztunnel relay-sender connect --relay my-relay-ns --hyco my-tunnel %h:%p

# Targets behind the jump host
Host internal-web
    HostName 10.1.0.5
    User deploy
    ProxyJump jump-10.0.0.5

Host internal-db
    HostName 10.1.0.6
    User dbadmin
    ProxyJump jump-10.0.0.5
```

Then:

```sh
ssh internal-web
ssh internal-db
```

## Multi-hop vs gateway

| Approach      | How it works                          | Best for                                |
| ------------- | ------------------------------------- | --------------------------------------- |
| **Multi-hop** | aztunnel → jump host → SSH → target   | Complex topologies, existing jump hosts |
| **Gateway**   | aztunnel → listener → target (direct) | Flat networks, simpler setup            |

If you can deploy a listener that reaches the target directly, the gateway
pattern avoids the extra SSH hop and is easier to manage.
