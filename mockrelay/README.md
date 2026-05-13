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

Terminal 1 — start the relay (TLS is required because aztunnel only
dials TLS-protected relays):

```sh
aztunnel-relay --tls
```

(equivalent to `aztunnel-relay --tls --bind 127.0.0.1:8080`; the
loopback bind is the default so the mock can't accidentally be reached
from another host. `--tls` generates a self-signed cert at startup —
see [TLS](#tls) for the details.)

Terminal 2 — start a listener that forwards to a local SSH server. The
mock validates SAS, so the client needs to know the dummy key (the relay
prints it on startup as `AZTUNNEL_KEY_NAME`/`AZTUNNEL_KEY` log fields):

```sh
export AZTUNNEL_KEY_NAME=dev
export AZTUNNEL_KEY=dev-secret-do-not-use-in-prod

aztunnel relay-listener \
  --relay localhost:8080 \
  --hyco demo-hc \
  --relay-insecure-tls
```

Terminal 3 — start a port-forward sender (same env vars):

```sh
aztunnel relay-sender port-forward \
  --relay localhost:8080 \
  --hyco demo-hc \
  --bind 127.0.0.1:2222 \
  --relay-insecure-tls \
  127.0.0.1:22
```

Now `ssh -p 2222 user@127.0.0.1` will tunnel through the local relay.

Key flags explained:

| Flag                                     | Why                                                                                                                                                                       |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--relay localhost:8080`                 | Bare `host:port` is treated as `wss://localhost:8080`. The Azure suffix is not appended when a port is present.                                                           |
| `--relay-insecure-tls`                   | Skip TLS verification for the mock's self-signed cert. Required only for local/testing setups where the cert isn't trusted by the system store.                           |
| `AZTUNNEL_KEY_NAME` / `AZTUNNEL_KEY` env | The mock SAS credentials. Defaults are `dev` / `dev-secret-do-not-use-in-prod`. Override with `--auth-key-name` / `--auth-key` on the relay if you want different values. |

## TLS

aztunnel always dials its relay over TLS — plain `ws://` / `http://`
is rejected at parse time. The mock relay's `--tls` flag generates a
self-signed cert by default, which is fine for local development and
CI; the rest of this section covers what to do for real test beds.

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
  --relay-insecure-tls
```

(Note: when the `--relay` value includes a port and no scheme, it is
treated as `wss://`. So `localhost:8443` → `wss://localhost:8443`.)

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
  --hyco demo-hc
```

> Reminder: this is still a mock. Real production deployments need real
> authentication; the SAS validation here only proves the client knows
> a fixed dummy key.

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
  --auth-key-name=dev           SAS key name accepted from clients.
  --auth-key=dev-secret-...     SAS key value accepted from clients.
  --no-auth                     Disable SAS validation entirely (tests only).
```

All flags have matching environment variables (`AZTUNNEL_RELAY_BIND`,
`AZTUNNEL_RELAY_TLS`, `AZTUNNEL_RELAY_PUBLIC_URL`, etc.).

## Client-side flags

The aztunnel client adds one flag to all relay-\* commands for use
against the mock relay:

| Flag                   | Env var                         | Effect                                                             |
| ---------------------- | ------------------------------- | ------------------------------------------------------------------ |
| `--relay-insecure-tls` | `AZTUNNEL_RELAY_INSECURE_TLS=1` | Skip TLS verification (use only for self-signed local/test certs). |

To authenticate against the mock, set `AZTUNNEL_KEY_NAME` and
`AZTUNNEL_KEY` to the values printed by `aztunnel-relay` on startup
(defaults: `dev` / `dev-secret-do-not-use-in-prod`). The aztunnel client
uses its normal SAS code path — there is no client-side bypass for the
mock case.

The `--relay` value drives scheme and suffix decisions. Plain `ws://`
and `http://` URLs are rejected — aztunnel only dials TLS.

| `--relay` value                  | Scheme | Suffix appended?                |
| -------------------------------- | ------ | ------------------------------- |
| `my-ns`                          | wss    | yes (`.servicebus.windows.net`) |
| `my-ns.example.com`              | wss    | no                              |
| `relay.example.com:8443`         | wss    | no (port present)               |
| `localhost:8080`                 | wss    | no (port present)               |
| `wss://my-ns`                    | wss    | yes                             |
| `https://relay.example.com:8443` | wss    | no                              |
| `ws://localhost:8080`            | —      | rejected (use port-only form)   |

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

This is a test fixture, not a production relay — file issues if you
need any of the following:

- **SAS validation only.** The relay accepts any SAS token signed with
  the configured key (default: a hard-coded dev key). It does NOT check
  the audience (`sr`) against the request URL, so a token signed for
  one entity is accepted for any entity. Anyone who can reach the
  relay's TCP port AND knows the key can listen or send for any entity.
  Rendezvous URLs carry a 128-bit random ID with a short timeout (30s
  default), but they are not cryptographically bound to a particular
  sender/listener pair beyond that.
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
aztunnel-relay --tls --bind 127.0.0.1:0 --log-level=warn &
```

…then point your aztunnel client at the relay's bound `host:port` with
`--relay-insecure-tls` (to accept the self-signed cert) and the default
`AZTUNNEL_KEY_NAME=dev` / `AZTUNNEL_KEY=dev-secret-do-not-use-in-prod`
SAS credentials.

For an in-tree, in-process example, see
`mockrelay/server/integration_test.go`, which wires
`listener.ListenAndServe` and `sender.PortForward` against the server
package (called from the same module) and round-trips data through a
local echo server.
