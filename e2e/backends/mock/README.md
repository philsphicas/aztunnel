# Mock Backend

Runs the shared `e2e/scenarios` suite against an in-process mock relay server.
Fast, deterministic, no Azure or network setup required.

```bash
make e2e-mock
```

Runs anywhere Go runs. No subscription, no `az login`, no per-developer infra.

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

- `backend.go` — `MockBackend` and its `Setup`/`Cell`/`Axes` implementation.
- `e2e_test.go` — `TestE2E_Mock`, runs `scenarios.RunAllScenarios`.
- `bench_test.go` — `BenchmarkE2E_Mock`, runs `scenarios.RunAllBenchmarks`.
- `emulates_test.go` — `TestMockEmulates_*` tests asserting wire-level parity
  with Azure on specific scenarios (added in follow-up work).
- `features_test.go` — `TestMockFeature_*` tests asserting mock-only knobs
  (e.g. rendezvous-delay overrides; added in follow-up work).

## Build tag

All `_test.go` files under this directory carry `//go:build e2e`. `backend.go`
itself does not — it is a library importable from any test.

## When to add tests here vs. `e2e/scenarios/`

If the assertion exercises the mock's wire output, add it here as
`TestMockEmulates_*` or `TestMockFeature_*` and (when appropriate) add a paired
backend-agnostic scenario in `e2e/scenarios/`. If the assertion is about
aztunnel's behavior given the mock's wire output, write the test in
`e2e/scenarios/` so it runs against Azure too.
