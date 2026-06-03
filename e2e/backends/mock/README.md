# Mock Backend

Runs the shared `e2e/scenarios` suite against an in-process mock relay server.
Fast, deterministic, no Azure or network setup required.

```bash
make e2e-mock
```

Runs anywhere Go runs. No subscription, no `az login`, no per-developer infra.

`TestE2E_Mock` runs over two independent, composable dimensions, mirroring the
Azure backend:

- **Auth method** (`E2E_AUTH`): unset runs both `sas` and `entra`; pin one with
  `E2E_AUTH=sas` or `E2E_AUTH=entra`. The `entra` cell drives a real
  `EntraTokenProvider` (backed by a fake credential) so the production token
  cache is exercised end-to-end, pays a one-off cold token-acquisition cost on
  the first dial, and — because the fake credential now mints a JWT-shaped token
  — also pays the mock relay's per-request Entra validation cost
  (`EntraValidate`) on every connect, modelling the recurring server-side "warm
  tax" the real Azure Relay charges under Entra. The `sas` cell pays only the
  cheap local SAS validation (`AuthInternal`).
- **Delay profile** (`E2E_DELAY`): unset uses the wire-faithful `default`
  profile so timing thresholds calibrated against Azure also fire here. The
  profile also owns the entra cold-acquisition cost (`TokenAcquire`), so the
  `zero` profile is instant everywhere — entra included.

```bash
make e2e-mock                         # both auth methods × default profile
make e2e-mock-fast                    # both auth methods × zero profile (fastest)
make e2e-mock-matrix                  # both auth methods × curated functional delay set (zero + default)
E2E_DELAY=zero make e2e-mock          # zero profile, no synthetic relay delay
E2E_DELAY=all make e2e-mock           # fan out over the curated functional delay set (zero + default)
E2E_DELAY=zero,default make e2e-mock  # explicit profile subset
E2E_AUTH=entra make e2e-mock          # pin the entra auth method
```

A dimension with a single value adds no sub-test layer; two or more nest
scenarios under `TestE2E_Mock/<auth>/<profile>/<scenario>` (auth outermost).
Per-profile latency thresholds scale with the profile's predicted cost, and the
entra cold-start budget widens only when the profile models a token-acquisition
cost. An unrecognised name fails the test loudly and prints the known names.

`E2E_DELAY=all` runs only the curated functional delay set (`zero` + `default`).
The relay-placement profiles form the full sender×listener grid — nine cells
named `<sender><listener>` with single-letter distance codes (`n`ear/`m`id/`f`ar,
sender first), e.g. `nf` = sender near, listener far. They model sender/listener
distance from the relay and are resolvable by name — e.g. `E2E_DELAY=nn,nf,fn,ff`
— but are intentionally excluded from `all` so a perf sweep does not expand the
functional suite. Sweep the whole grid via `make perf-placement`, whose rows
land in the always-on per-run history file (tagged with `PERF_MATRIX_BACKEND`)
and render with `go run ./cmd/perfreport`.

The reporter is decoupled from execution: every run appends a timestamped file
to one store, `perf-artifacts/history/<backend>-<run id>.jsonl`, and `make
perf-table` / `make perf-grid` re-render the merged history without re-running.
Because the reporter merges every file it is given and collapses to the newest
row per cell, running `make perf-placement` (mock) and `make
perf-placement-azure` produces one comparison table with a `backend` column. The
3×3 grid view needs a single backend, scenario, and mode — narrow the merged
history with `make perf-grid BACKEND=mock SCENARIO=<name>`.

### Named axes — compare any dimension

Each row records its matrix cell as **named axis dimensions** (e.g.
`axes: {"auth":"sas","delay":"nn"}`) resolved from the backend's `Axes()`,
not just a flat string. The reporter renders one column per axis and accepts
repeatable `--filter key=value` (where `key` is any axis name, or the virtual
`backend`/`scenario`/`mode`), so you can pivot on a single dimension:

```sh
make perf-axes-mock                       # run mock UNPINNED: auth × delay both vary
# the footer prints the exact history file it wrote, e.g.
go run ./cmd/perfreport --filter delay=ff --mode PortForward \
  perf-artifacts/history/mock-<run id>.jsonl   # compare entra vs sas at one placement
```

A run that _pins_ an axis (e.g. `make perf-placement` pins `E2E_AUTH=sas`)
omits that axis from its rows; merging such a run with an unpinned one yields
rows with differing axis key sets, and the reporter prints a warning because a
`--filter` on the missing key would silently skip them. To compare across an
axis, leave it unpinned so every row carries it.

### Comparing runs — before/after a change

Emission is **always-on** and there is a single store: every e2e/perf run
appends a timestamped per-run file under `perf-artifacts/history/`, named
`<backend>-<run id>.jsonl`. Every view (`perf-table`, `perf-grid`,
`perf-history`, `perf-compare`) reads that one directory. Each row carries a
`run` id — a sortable UTC timestamp plus a short random suffix — so repeated
runs of the same cell accumulate as history instead of overwriting or
colliding. Set `PERF_MATRIX_RUN_ID` to pin the id (e.g. sharded runs share one,
and the generation targets set it so their footer can render the exact file they
just wrote); the default is generated per run. Setting `PERF_MATRIX_JSONL=<path>`
additionally exports that one run to an explicit file — an opt-in escape hatch
(e.g. CI shard upload), not part of the default workflow.

```sh
make perf-placement                 # run once (baseline)
# …make a change…
make perf-placement                 # run again (candidate)
make perf-history                   # list the runs captured so far
make perf-compare                   # diff the two newest: BASE=previous CAND=latest
```

`make perf-compare` matches cells across two runs and prints baseline,
candidate, and the percent delta for `warm_p50`, `cold_p50`, and `est`
(negative = faster). `BASE`/`CAND` accept a run id, an unambiguous id prefix,
or the words `latest`/`previous`; `SCENARIO`/`MODE`/`BACKEND` narrow the
comparison. A cell present in only one run renders as a dash so missing
coverage is visible rather than silently dropped — note that comparisons match
on the full cell `(backend, axes, scenario, mode)`, so both runs must pin the
same axes for a row to line up. Back-to-back runs of identical code show the
measurement-noise floor (typically a few percent); a real regression has to
beat that to be meaningful. By default the plain table collapses a merged
history to the newest run per cell; pass `--all-runs` to see every run with a
`run` column. `make perf-clean-history` clears the accumulated files.

### Comparing any dimension — entra vs sas, near vs far, mock vs azure

The same delta view pivots on **any** dimension, not just runs. Set `DIM` to a
named axis (`auth`, `delay`, …), `backend`, `scenario`, `mode`, or the legacy
flat `axis`, and give `BASE`/`CAND` the two values to compare:

```sh
make perf-compare DIM=auth  BASE=sas CAND=entra   # entra vs sas, same run
make perf-compare DIM=delay BASE=nn  CAND=ff      # near vs far placement
```

The compared dimension drops out of the identity columns and becomes the
`base`/`cand`/`Δ%` split; every other dimension still lines the rows up. Unlike
run comparison, a non-run compare matches **within a single run** (the run id
stays part of a cell's identity), so it is apples-to-apples — when the history
holds several runs you get one comparison row per run, surfaced by a `run`
column. Because of that same-run rule, comparing `backend=mock..azure` only
works when both backends were measured under one run id; mock and azure
captured as separate runs have no overlapping cell and the tool says so rather
than printing meaningless dashes. Filtering on the compared dimension (e.g.
`--filter auth=sas` while comparing `auth`) is rejected, since it would delete
one side of the comparison. The underlying flags are `--compare-by <dim>` with
`--compare base..cand`.

### Streaming workload — the second metric family

Most perf scenarios drive a request/response **RTT** workload (an echo or a
fixed-size respond) and report the cold/warm establishment distribution.
Long-lived **streaming** scenarios measure something the RTT family cannot:
how a server-paced trickle of chunks fans out across many concurrent
streams. They use a separate `StreamShape` and a connect-all **start
barrier** — every stream dials and completes its SOCKS5 CONNECT first, then
all are released together — so the metrics reflect steady-state behaviour
under simultaneous load rather than dial/rendezvous ordering. Streaming is
SOCKS5-only (the fan-out targets distinct workload servers).

Streaming rows carry `metric_family: "stream"` (RTT rows omit the field).
The reporter keeps the two families on separate tables and never pairs one
against the other. The streaming columns are:

- `first_resp_p50` / `first_resp_p95` — client-side time to the first server
  output, from the barrier release. With a zero-think-time server this is just
  a warm round-trip; a non-zero server `ProcessingDelay` adds its initial think
  time. Reported for context, **not gated** (it tracks a warm RTT plus an
  injected constant, neither of which is a tunnel regression).
- `gap_p95` — pooled inter-chunk gap p95 (uniform pacing degradation).
- `max_stream_gap_p95` — the worst single stream's inter-chunk gap p95, the
  starvation-sensitive **jitter** signal (gated).
- `max_gap` — the single largest inter-chunk gap (diagnostic for a stall).
- `final_chunk_spread` — max−min of the streams' last-chunk arrivals, i.e.
  delivery **fairness** across the fan-out (gated).
- `goodput` — total payload bytes over the active stream window (release →
  last chunk delivered), not the full round wall.

`make perf-compare` renders a parallel **streaming** comparison table whose
deltas are **absolute** durations, not percentages: an injected trickle
interval inflates a percentage denominator and would hide a real fairness
or jitter regression. The gate covers `max_stream_gap_p95` (jitter) and
`final_chunk_spread` (fairness) only; the other streaming columns are reported
but not gated. Because streaming metrics are noisier than RTT p50s, the
streaming gate never trips below a 50ms absolute floor (even if
`--fail-min-abs` is lower), and a comparison that paired streaming cells but
produced no comparable gated metric is a hard failure rather than a silent
pass.

### Validation tiers — where perf fits next to correctness

Perf is a **measurement lens on the e2e suite**, not a separate suite: the perf
scenarios are `t.Run` subtests inside `TestE2E_Mock` / `TestE2E_Azure`, and
every e2e/perf run emits history (emission is always-on). That gives four
distinct uses, in increasing cost:

1. **Correctness (PR-gating).** `ci.yml` + `e2e.yml` already run the functional
   suite — connection setup, SHA-256 byte parity, auth, fallback, and the
   mock-resembles-azure log-shape parity gate. Perf numbers are produced as a
   side effect but **nothing is gated on them**. This is the only tier that
   blocks a merge.
2. **Opt-in PR perf** (`perf-pr.yml`). Add the `perf` label to a PR (or use the
   Actions "Run workflow" button) to sweep the mock placement grid and render
   the matrix into the job summary, with the history uploaded as an artifact.
   Always green — it informs, it never blocks. Runs read-only with no cloud
   credentials.
3. **Nightly gross-regression gate** (`perf-nightly.yml`). A cron job sweeps the
   mock grid, restores the previous nightly's history artifact as a baseline,
   and runs `make perf-gate` (default `FAIL_OVER=30`, i.e. fail only on a
   warm/cold p50 cell that regressed by **both** >30% **and** >20ms, plus
   any streaming `max_stream_gap_p95`/`final_chunk_spread` cell that regressed past
   the 50ms absolute floor). Mock-only:
   the real Azure relay's rendezvous spikes (~3–6s) swamp any threshold. The
   baseline is re-uploaded only on success, so a regressing night never
   overwrites the known-good reference.
4. **Developer-local before/after.** `make perf-placement` twice around a change,
   then `make perf-compare` (or `make perf-gate FAIL_OVER=<pct>` to get the same
   pass/fail verdict the nightly uses).

`make perf-gate` is the shared tripwire for tiers 3 and 4: it diffs the two
newest runs and exits non-zero only on a gross regression, treats a tiny
absolute delta as noise (`FAIL_MIN_ABS`, default `20ms`), and **skips** (exit 0)
when only one run is present so a bootstrap night is not a failure. It requires
the two newest runs to be the same scenario shape (both produced by
`make perf-placement`); a partial cell-set difference is a warning, but zero
overlap is a hard error so the gate can never silently pass on nothing.

## What it is

`backend.go` exports `MockBackend`, an implementation of
`e2e/scenarios.Backend` that stands up:

- The mock relay server (`mockrelay/server`) on a loopback `httptest` listener.
- A real aztunnel listener attached to the mock's control channel.
- A real aztunnel sender (port-forward or SOCKS5) bound to a loopback port.

All three live in the same process; the mock relay is reached over a
loopback TLS listener (`httptest.NewTLSServer`), so the bytes never leave
the host but still traverse the kernel's TCP stack and the real TLS
handshake — the same wire shape aztunnel uses against Azure. Scenarios
that pass against this backend exercise the same aztunnel code paths
exercised against Azure Relay.

## Files

- `backend.go` — `MockBackend` and its `Setup`/`Cell`/`Axes` implementation,
  including the composable auth + delay matrix (`NewMatrixBackend`).
- `e2e_test.go` — `TestE2E_Mock`, runs `scenarios.RunAllScenarios`.
- `env_test.go` — parses `E2E_AUTH` / `E2E_DELAY` into the matrix backend.
- `emulates_test.go` — `TestMockEmulates_*` tests asserting wire-level parity
  with Azure on specific scenarios.
- `entracred.go` / `entracred_test.go` — the fake Entra credential and the
  `TestMockEmulates_EntraTokenAcquisitionDelay` cold-start proof.
- `features_test.go` — `TestMockFeature_*` tests asserting mock-only knobs
  (e.g. the delay-profile matrix wiring).

## Build tag

All `_test.go` files under this directory carry `//go:build e2e`. `backend.go`
itself does not — it is a library importable from any test.

## When to add tests here vs. `e2e/scenarios/`

If the assertion exercises the mock's wire output, add it here as
`TestMockEmulates_*` or `TestMockFeature_*` and (when appropriate) add a paired
backend-agnostic scenario in `e2e/scenarios/`. If the assertion is about
aztunnel's behavior given the mock's wire output, write the test in
`e2e/scenarios/` so it runs against Azure too.
