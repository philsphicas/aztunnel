# Stream multiplexing

Each new TCP session through aztunnel normally pays a full Azure Relay
rendezvous: a TLS handshake, a WSS upgrade, a control-channel `accept` to
the listener, the listener dialing its own rendezvous WebSocket, and the
two sides being paired. End-to-end this typically costs **1–2 seconds per
connection**, which is painful in SOCKS5 mode where a browser, `kubectl`,
or `curl` workload opens many short-lived TCP sessions.

To avoid paying that cost on every new connection, aztunnel maintains one
or more **persistent multiplexed sessions** (smux v2 over a single relay
WebSocket). Each new TCP session becomes a cheap smux stream rather than a
full rendezvous, dropping per-connection setup time from ~1–2 s to
~milliseconds.

Sessions are **dialed lazily on the first request that needs one**, so the
very first TCP connection through the sender still pays one full
rendezvous (and is no faster than the v1 path). The speedup applies to
every subsequent connection that reuses the established session.

```
Without mux:                       With mux:
┌──────┐                           ┌──────┐
│ ssn1 │──[full rendezvous 1-2s]   │ ssn1 │──┐
└──────┘                           └──────┘  │
┌──────┐                           ┌──────┐  │
│ ssn2 │──[full rendezvous 1-2s]   │ ssn2 │──┼──[single persistent WS]
└──────┘                           └──────┘  │
┌──────┐                           ┌──────┐  │
│ ssn3 │──[full rendezvous 1-2s]   │ ssn3 │──┘
└──────┘                           └──────┘
```

## When mux is used

Multiplexing is **on by default** in:

- `relay-sender port-forward`
- `relay-sender socks5-proxy`

These are the modes where many short-lived connections benefit most. The
one-shot `relay-sender connect` (used for SSH `ProxyCommand` etc.) stays on
the unmultiplexed v1 path because it only opens a single session per
invocation.

## Configuration

| Flag                                      | Env                                     | Default | Notes                                                                                                                                    |
| ----------------------------------------- | --------------------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `--no-mux`                                | `AZTUNNEL_NO_MUX`                       | `false` | Disable mux; revert to one rendezvous per connection.                                                                                    |
| `--mux-sessions N`                        | `AZTUNNEL_MUX_SESSIONS`                 | `2`     | Cap on persistent rendezvous WebSockets.                                                                                                 |
| `--max-streams-per-session N`             | `AZTUNNEL_MAX_STREAMS_PER_SESSION`      | `256`   | Per-session in-flight stream cap. Advanced; rarely needed.                                                                               |
| `--mux-stream-handshake-timeout DURATION` | `AZTUNNEL_MUX_STREAM_HANDSHAKE_TIMEOUT` | `60s`   | Sender-side cap on the per-stream envelope+response exchange. Hidden; see [Tuning the handshake timeout](#tuning-the-handshake-timeout). |

### `--mux-sessions` sizing

Each "session" is one long-lived relay rendezvous WebSocket. The pool
grows lazily to `--mux-sessions` whenever every existing session has at
least one in-flight stream, so a serial workload stays on session 0 and a
bursty workload spreads across new sessions.

Guidance:

- **Default (`2`)** covers typical workloads: single-stream callers
  keep using session 0 (no extra rendezvous cost), and bursty
  multi-stream callers get a second session lazily for parallel
  fan-out and a chance at HA-listener spread.
- **Strict single-rendezvous behavior**: set `--mux-sessions 1`. Use
  this when you want a deterministic single WebSocket per sender and
  are not concerned about head-of-line blocking under concurrent
  load.
- **High-availability listener fleet (many listeners)**: raise
  `--mux-sessions` toward the number of listeners you run. Each new
  session opens a fresh rendezvous, which **may** land on a different
  listener — but Azure Relay's listener selection is opaque and we do
  not promise true load balancing. Test empirically.
- **Long-lived heavy traffic**: leave the default unless you observe
  `aztunnel_mux_pool_saturated_total` incrementing in metrics, which
  indicates callers blocking on `--max-streams-per-session`. The CLI
  port-forward and SOCKS5 paths bound each per-connection mux open
  wait at 60s (an internal `muxStreamAdmissionTimeout`); on saturation
  the timeout fires and increments the counter, then the connection
  is dropped (or falls back to v1 if mux is unsupported). Raise
  `--mux-sessions` and/or `--max-streams-per-session` before the
  counter becomes a sustained signal in your dashboards.

### Tuning the handshake timeout

The hidden `--mux-stream-handshake-timeout` flag (default `60s`) caps the
per-stream envelope+response exchange between sender and listener. It
needs to be greater than the listener's target-dial budget because the
listener starts dialing the upstream target before writing the envelope
response; if the sender timeout is too short, the sender gives up while
the listener's target dial is still in progress, never seeing the
response that was about to be written.

Operators almost never need to touch this. Raise it only if you have
also raised the listener's `--connect-timeout` above its `30s` default
(e.g. for sluggish upstream targets like cold-start VMs) — in that case
set the sender's `--mux-stream-handshake-timeout` to at least the
listener's `--connect-timeout` plus a few seconds of headroom. Otherwise
v2 mux streams will time out at the 60s default while v1 single-stream
connections would have waited the full listener budget.

The flag is intentionally hidden from `--help` because the default is
correct for every standard configuration; it exists for the slow-dial
edge case only.

### Listener `--max-connections` and envelope-pending streams

With mux enabled, a single accepted control-channel WebSocket can
carry many concurrent smux streams. The listener guards two distinct
resources independently:

- The **active-stream cap** (`--max-connections`, applied via
  `streamSem` after envelope+allowlist validation) bounds in-flight
  target connections. This matches the v1 cap exactly.
- The **envelope-pending cap** (`pendingSem`, sized at 2× the active
  cap) bounds smux streams that have been accepted but have not yet
  completed envelope validation, so that slow or malicious peers
  withholding envelopes can't pin unbounded goroutines and
  read-deadline timers (up to `muxStreamReadTimeout`) inside the
  listener.

When `--max-connections` is `0` (its default, meaning **unlimited**),
**both** caps are unlimited. Operators running mux-aware listeners in
untrusted-sender environments should set an explicit
`--max-connections` so that the envelope-pending cap takes effect —
otherwise a single authenticated sender can open as many
envelope-pending streams as its smux peer is willing to keep open.
The v1 path is also unbounded at `--max-connections 0`, but in v2 the
per-WebSocket multiplier makes the asymmetry worth calling out
explicitly.

## Session rotation

aztunnel rotates each mux session every 6 hours by default:

1. At `t + 6h` (`RotateAfter`), the session is marked **draining**.
   The pool stops assigning new streams to it.
2. In-flight streams are allowed to finish naturally for up to 5 minutes
   (`RotateGrace`).
3. If streams are still active at the end of the grace window, the
   session is force-closed and a brief disruption follows.

### Why rotation exists

Empirical probing of Azure Relay shows that sender rendezvous
WebSockets **survive past their SAS token's `se=` expiry indefinitely**
as long as keepalives flow — token validation happens at handshake
only. A 2-minute SAS token sustained traffic for at least 22 minutes
past expiry in testing with no relay-side teardown.

So rotation is **not** a correctness requirement to dodge token-expiry
WS reaping. It is purely defensive:

- **Fleet rebalancing**: each rotation re-dials, which is another
  opportunity for Azure Relay to pick a different listener instance
  in a multi-listener HA setup.
- **Defence in depth**: against unknown long-term relay limits or
  silent quotas not exposed in documentation.

Draining sessions do **not** count toward `--mux-sessions`, so a
rotation in progress never stalls new streams on the sender side: the
pool can temporarily exceed the cap by the number of draining sessions.
Note this is a sender-pool guarantee only — if the listener is running
with `--max-connections` near `--mux-sessions`, the draining session
still holds a listener control-channel slot until closed, so a
replacement session can still be rejected or delayed by the listener's
control cap. Size the listener's `--max-connections` with headroom for
in-flight drains when running close to the cap.

### Impact on long-lived streams

Long-lived streams (interactive SSH sessions, `kubectl port-forward`,
HTTP long-polling) reaching the 5-minute grace window during rotation
will be force-closed. Plan around this: if you need a stream to live
more than ~5 minutes uninterrupted across a 6h boundary, use
`--no-mux` and accept the per-connection setup cost. For most
workloads (browsers, short DB queries, repeated curl calls) rotation
is invisible.

## Backwards compatibility

A mux-aware sender can talk to either:

- a mux-aware listener (v2 — uses the persistent path), or
- an older listener (v1 — falls back to per-connection rendezvous).

When the sender first dials a v1 listener, the handshake is rejected
with a v1-style error (`unsupported protocol version` on listeners
that recognize the v2 version field, or `missing target` / `invalid
envelope` on older listeners that parse the v2 handshake as a v1
`ConnectEnvelope` and reject it). The sender treats any of these as a
v1-fallback signal, remembers it for 60 seconds, and falls through to
v1 for new connections during that window without re-dialing. After
the window, it retries the mux path in case the listener pool was
rolled.

With `--mux-sessions > 1`, the first logical connection to an all-v1
listener fleet can pay up to `MuxSessions` failed mux rendezvous
before the 60-second sticky fallback is cached: the pool grows lazily
and each new session has to learn the v1 verdict independently.
Subsequent connections during the 60-second window short-circuit to
v1 without paying that cost. Operators raising `--mux-sessions`
during mixed-version rollouts should expect this one-time latency
spike on the cold pool.

If you operate a **mixed-version fleet** during a rolling upgrade, this
fallback works automatically; preferably upgrade listeners first.

## Metrics

If `--metrics-addr` is set, mux-specific metrics are exposed. All mux
metrics below are emitted by the sender path (`MuxDialer` / `MuxPool`)
with `role="sender"`; listener-only mux session metrics are not
currently implemented, so these series will not appear on a
listener-only `/metrics` endpoint.

| Metric                              | Type      | Labels           | Description                                                                                                                                             |
| ----------------------------------- | --------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `aztunnel_mux_sessions_active`      | gauge     | `role`           | Number of currently active mux sessions.                                                                                                                |
| `aztunnel_mux_stream_open_seconds`  | histogram | `role`           | Time from `OpenStream` entry to smux stream open (pool admission + smux SYN; excludes the per-stream envelope handshake).                               |
| `aztunnel_mux_session_age_seconds`  | histogram | `role`           | Distribution of session ages at rotation or eviction (includes `unsupported`, `pool_closed`, and `open_failed` shutdowns).                              |
| `aztunnel_mux_rotations_total`      | counter   | `role`, `reason` | Mux session lifecycle exits by reason: `scheduled`, `force_after_grace` (rotations), `unsupported`, `pool_closed`, `open_failed` (evictions/shutdowns). |
| `aztunnel_mux_pool_saturated_total` | counter   | `role`           | Callers that ctx-expired while waiting for a free slot.                                                                                                 |

`aztunnel_connection_errors_total` also gains a `mux_open_failed` reason
for connections that failed opening a mux stream (e.g. smux setup error,
handshake parse failure, listener rejection that isn't the v1-fallback
marker), and a `listener_at_capacity` reason emitted by the listener
when it rejects an incoming connection because it has hit its own
streamSem or pendingSem cap (operators can tell "we're at our
configured `--max-connections`" apart from "Azure had a relay outage").
Relay-dial failures inside the mux path are recorded under the
`dial_timeout` and `relay_failed` reasons by `MuxDialer.connectLocked`,
which calls `relay.DialWithRetry` directly and emits `ConnectionError`
via `DialReason(err, ReasonRelayFailed)` itself (rather than going
through `metrics.InstrumentedDial`, so it can suppress the dial-error
metric when the cause is parentCtx cancellation during eviction). The
`dial_failed` reason is reserved for the listener target-dial path and
does not appear on sender mux dials. No double-counting, and context
cancellation while waiting on a saturated pool is not recorded as a
connection error.

## Disabling mux

If you hit a bug or want the old behaviour for a specific session:

```sh
aztunnel relay-sender socks5-proxy --no-mux --bind 127.0.0.1:1080
# or
AZTUNNEL_NO_MUX=1 aztunnel relay-sender port-forward 10.0.0.5:5432
```

Symptoms that might warrant `--no-mux`:

- An unusual protocol that depends on TCP framing characteristics not
  preserved by smux (rare).
- A stream that needs to survive a rotation boundary by more than the
  5-minute grace window (rotation defaults to every 6h; see "Session
  rotation" above for details and the "Impact on long-lived streams"
  guidance).
- Diagnosing whether a regression is mux-related.
