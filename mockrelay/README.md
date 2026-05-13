# `aztunnel-relay` — mock relay for testing aztunnel

> **Scope.** This is a **development and testing tool**, not a
> production Azure Relay replacement. It speaks the subset of the
> Azure Relay Hybrid Connections wire protocol used by aztunnel's
> listener and sender, so you can exercise the full data path without
> an Azure account.
>
> It currently performs **no token validation, no listener auth, and
> no HA**. Anyone who can reach its TCP port can listen for or send
> connections to any entity. Use it for local dev, CI, and
> self-contained air-gapped tests — not as a public-facing service.
> If you need a hardened self-hosted relay, that is a separate effort
> that hasn't started.

## Why it exists as a separate module

This code lives in its own Go module
(`github.com/philsphicas/aztunnel/mockrelay`) inside the aztunnel
repo so that consumers of aztunnel-the-client don't pull in the
relay code or its deps. It has its own Dockerfile, Makefile, and
release surface — i.e. nothing about `aztunnel` itself depends on
this module.

## Quick start

The simplest setup is a plain-HTTP relay on localhost, with no auth
and no TLS. Ideal for tests, dev loops, and demos.

Terminal 1 — start the relay:

```sh
aztunnel-relay
```

(equivalent to `aztunnel-relay --bind 127.0.0.1:8080`; the loopback bind
is the default so the mock can't accidentally be reached from another
host.)

Terminal 2 — start a listener that forwards to a local SSH server:

```sh
aztunnel relay-listener \
  --relay ws://localhost:8080 \
  --hyco demo-hc \
  --relay-auth=none
```

Terminal 3 — start a port-forward sender:

```sh
aztunnel relay-sender port-forward \
  --relay ws://localhost:8080 \
  --hyco demo-hc \
  --relay-auth=none \
  --bind 127.0.0.1:2222 \
  127.0.0.1:22
```

Now `ssh -p 2222 user@127.0.0.1` will tunnel through the local relay.

Key flags explained:

| Flag                | Why                                                                                                                                 |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `--relay ws://...`  | URL with `ws://` scheme tells the client to dial plain HTTP, not TLS. Port is taken from the URL; the Azure suffix is not appended. |
| `--relay-auth=none` | Selects the no-op token provider — the client sends a dummy `sb-hc-token` value that the server ignores.                            |

## TLS

When clients reach the relay over an untrusted network, run with TLS.
Two options:

### Self-signed certificate (for local TLS testing)

```sh
aztunnel-relay --tls --bind 127.0.0.1:8443
```

The relay generates an ECDSA P-256 self-signed cert for `localhost`,
`127.0.0.1`, and `::1`, and logs its SHA-256 fingerprint at startup:

```text
WARN using self-signed TLS certificate — clients must trust it manually sha256_fingerprint=...
```

Clients connect with `--relay-insecure-tls` to skip certificate
verification (or import the printed fingerprint into a custom trust
store):

```sh
aztunnel relay-listener \
  --relay localhost:8443 \
  --hyco demo-hc \
  --relay-auth=none \
  --relay-insecure-tls
```

(Note: when the `--relay` value includes a port and no `ws://` prefix,
the default scheme is `wss`. So `localhost:8443` → `wss://localhost:8443`.)

### Provided certificate (e.g. for a non-loopback test bed)

```sh
aztunnel-relay \
  --tls \
  --tls-cert /etc/ssl/relay.example.com.crt \
  --tls-key  /etc/ssl/relay.example.com.key \
  --public-url https://relay.example.com \
  --bind 0.0.0.0:8443
```

Set `--public-url` to the externally-visible base URL of the relay.
This is what the server tells the listener to dial for the rendezvous
half of each connection. **Required behind a reverse proxy or when
binding to a non-loopback address** — otherwise the rendezvous URL is
built from the inbound `Host` header, which is sender-controlled and
not generally externally routable.

Clients then connect with their default settings (full TLS verification):

```sh
aztunnel relay-listener \
  --relay relay.example.com \
  --hyco demo-hc \
  --relay-auth=none
```

> Reminder: token validation is not implemented. A real certificate
> alone does not make this safe to expose publicly.

## CLI flags

```text
aztunnel-relay [flags]

  --bind="127.0.0.1:8080"       Address:port to bind on.
  --tls                         Enable TLS. If --tls-cert/--tls-key are
                                unset, generate a self-signed cert.
  --tls-cert=PATH               PEM-encoded TLS certificate.
  --tls-key=PATH                PEM-encoded TLS private key.
  --public-url=URL              Base URL used for rendezvous addresses.
                                Required behind a reverse proxy.
  --log-level=info              debug | info | warn | error.
  --max-connections=0           Cap on concurrent rendezvous (0 = none).
  --listener-idle-timeout=2m    Close idle listener control channels.
  --rendezvous-timeout=30s      Max wait for listener to dial rendezvous URL.
  --metrics-addr                Prometheus metrics address (e.g. :9090).
```

All flags have matching environment variables (`AZTUNNEL_RELAY_BIND`,
`AZTUNNEL_RELAY_TLS`, `AZTUNNEL_RELAY_PUBLIC_URL`, etc.).

## Client-side flags

The aztunnel client adds two flags to all relay-\* commands for use
against the mock relay:

| Flag                   | Env var                         | Effect                                                                      |
| ---------------------- | ------------------------------- | --------------------------------------------------------------------------- |
| `--relay-auth=MODE`    | `AZTUNNEL_RELAY_AUTH`           | `auto` (default), `none`, `sas`, or `entra`. Pick `none` for the mock case. |
| `--relay-insecure-tls` | `AZTUNNEL_RELAY_INSECURE_TLS=1` | Skip TLS verification (use only for self-signed local/test certs).          |

The `--relay` value drives scheme and suffix decisions:

| `--relay` value                  | Scheme | Suffix appended?                |
| -------------------------------- | ------ | ------------------------------- |
| `my-ns`                          | wss    | yes (`.servicebus.windows.net`) |
| `my-ns.example.com`              | wss    | no                              |
| `relay.example.com:8443`         | wss    | no (port present)               |
| `localhost:8080`                 | wss    | no (port present)               |
| `ws://localhost:8080`            | ws     | no                              |
| `https://relay.example.com:8443` | wss    | no                              |

This means `--relay-auth=none` and `--relay ws://localhost:8080` are
the only required additions for the typical mock scenario.

## Architecture

```text
   ┌──────────────┐      control WS       ┌──────────────────┐      control WS      ┌──────────────┐
   │   listener   │ ───────────────────►  │  aztunnel-relay  │  ◄──────────────────  │    sender    │
   │              │ ◄────accept────────── │                  │ ──── 404 if no LSR ── │              │
   └──────┬───────┘                       └────────┬─────────┘                       └──────┬───────┘
          │ dial rendezvous URL                    │ bridge                                  │
          ▼                                        ▼                                          ▼
       rendezvous WS  ───────────────────  byte-for-byte copy ──────────────────►  rendezvous WS
```

1. The listener opens a long-lived **control WebSocket** to the relay.
2. The sender opens a **connect WebSocket** to the relay; this becomes
   the sender's half of a rendezvous. If no listener is registered, the
   relay returns HTTP 404 pre-upgrade — the sender retries with backoff.
3. The relay writes an `accept` message back to the listener's control
   channel, containing a random rendezvous URL and ID.
4. The listener dials the rendezvous URL — this becomes the listener's
   half.
5. The relay bridges the two halves byte-for-byte, preserving WebSocket
   message boundaries.

## Limitations

v1 deliberately omits the following — file issues if you need them:

- **No token validation.** Anyone who can reach the relay's TCP port
  can listen or send for any entity. Rendezvous URLs carry a 128-bit
  random ID with a short timeout (30s default), but they are not
  cryptographically bound to a particular sender/listener pair beyond
  that.
- **No HA / clustering.** A single `aztunnel-relay` process is the
  source of truth for the listeners it knows about. Running multiple
  replicas is fine for independent traffic, but a listener registered
  with replica A is not visible to replica B.
- **No authentication of listeners or senders against each other.**
  Multiple listeners on the same entity round-robin sender connections.
- **No structured Prometheus metrics yet.** `--metrics-addr` serves
  `/healthz` and a placeholder `/metrics` page; relay-specific counters
  will land in a follow-up.

## Development / CI

The easiest way to exercise the end-to-end aztunnel data path in tests
is to run `aztunnel-relay` as a subprocess in your test setup:

```sh
aztunnel-relay --bind 127.0.0.1:0 --log-level=warn &
```

…then point your aztunnel client at it with `--relay ws://...
--relay-auth=none`.

For an in-tree, in-process example, see
`mockrelay/server/integration_test.go`, which wires
`listener.ListenAndServe` and `sender.PortForward` against the server
package (called from the same module) and round-trips data through a
local echo server.
