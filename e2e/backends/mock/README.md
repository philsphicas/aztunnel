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
  cache is exercised end-to-end, and pays a one-off cold token-acquisition cost
  on the first dial.
- **Delay profile** (`E2E_DELAY`): unset uses the wire-faithful `default`
  profile so timing thresholds calibrated against Azure also fire here. The
  profile also owns the entra cold-acquisition cost (`TokenAcquire`), so the
  `zero` profile is instant everywhere — entra included.

```bash
make e2e-mock                         # both auth methods × default profile
make e2e-mock-fast                    # both auth methods × zero profile (fastest)
make e2e-mock-matrix                  # both auth methods × every registered profile
E2E_DELAY=zero make e2e-mock          # zero profile, no synthetic relay delay
E2E_DELAY=all make e2e-mock           # fan out over every registered profile
E2E_DELAY=zero,default make e2e-mock  # explicit profile subset
E2E_AUTH=entra make e2e-mock          # pin the entra auth method
```

A dimension with a single value adds no sub-test layer; two or more nest
scenarios under `TestE2E_Mock/<auth>/<profile>/<scenario>` (auth outermost).
Per-profile latency thresholds scale with the profile's predicted cost, and the
entra cold-start budget widens only when the profile models a token-acquisition
cost. An unrecognised name fails the test loudly and prints the known names.

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
