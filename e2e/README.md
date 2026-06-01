# End-to-End Tests

Aztunnel's e2e suite has one shared library of backend-agnostic scenarios that
runs against multiple backends.

```
e2e/
├── scenarios/        ← backend-agnostic scenarios (behavior tests)
├── backends/
│   ├── azure/        ← scenarios run against a real Azure Relay namespace
│   └── mock/         ← scenarios run against the in-process mock relay
├── azrelay/          ← per-test hyco provisioner used by the Azure backend
└── infra/            ← `make e2e-setup` CLI (separate Go module)
```

## Make targets

| Target                 | Backend     | Setup required                | Use when                                            |
| ---------------------- | ----------- | ----------------------------- | --------------------------------------------------- |
| `make e2e-mock`        | mock        | none                          | local iteration; CI sanity gate (both auth methods) |
| `make e2e-mock-fast`   | mock        | none                          | quickest mock signal: zero delay, no token cost     |
| `make e2e-mock-matrix` | mock        | none                          | full auth × delay-profile matrix                    |
| `make e2e-azure`       | Azure Relay | `make e2e-setup` + `az login` | smoke against the real relay control plane          |
| `make e2e`             | both        | both                          | full local validation before opening a PR           |

`make e2e` runs both targets. They share no infra (mock is in-process; Azure
provisions per-test hycos), so `make -j2 e2e` runs them in parallel and finishes
in roughly the walltime of the slower backend.

### Environment knobs

| Variable    | Backend      | Values                                                               | Effect                                                                                                                                                                                                                                                                                                  |
| ----------- | ------------ | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `E2E_AUTH`  | Azure + mock | `entra`, `sas`, unset (both)                                         | Pins the auth axis to one method; unset runs both. For the mock, the `entra` cell additionally models the cold token-acquisition cost (see `E2E_DELAY`).                                                                                                                                                |
| `E2E_DELAY` | mock         | unset (`default`), `all`, or a comma-separated list of profile names | Selects which mockrelay synthetic-delay profiles `TestE2E_Mock` runs. Names come from the registry in `mockrelay/server/delay_profile.go` (e.g. `zero`, `default`). Two or more profiles fan the suite out over a `delay` matrix axis. The `zero` profile also zeroes the entra token-acquisition cost. |

```bash
# Run the mock scenarios with no synthetic relay delay (and no entra
# token-acquisition cost) — the fastest signal. Same as `make e2e-mock-fast`:
E2E_DELAY=zero make e2e-mock

# Fan the whole suite out over every registered profile (`make e2e-mock-matrix`):
E2E_DELAY=all make e2e-mock

# Run an explicit subset (sub-test paths gain a /<profile>/ layer):
E2E_DELAY=zero,default make e2e-mock

# Pin a single auth method (applies to both mock and Azure):
E2E_AUTH=entra make e2e-mock
```

`E2E_AUTH` and `E2E_DELAY` are independent dimensions that compose into one
matrix: `TestE2E_Mock` runs once per (auth, delay) cell. A dimension with a
single selected value (including the unset defaults — both auth methods, the
`default` profile) adds no sub-test layer for that dimension, so
`-run TestE2E_Mock/Performance/...` selectors keep matching. A dimension with two
or more values nests scenarios under `TestE2E_Mock/<auth>/<profile>/<scenario>`
(auth outermost, mirroring the Azure backend). Per-profile latency thresholds
scale with the profile's predicted cost, so slower profiles don't mask
regressions, and the entra cold-start budget widens only when the profile models
a token-acquisition cost.

## Adding tests

Use the testing-discipline taxonomy below to pick where a new test lives.

| Category              | Location                                | When to use                                                                                           | Naming                        |
| --------------------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------- | ----------------------------- |
| **Behavior scenario** | `e2e/scenarios/`                        | Test asserts aztunnel behavior given backend output. Backend-agnostic (uses the `Backend` interface). | `Scenario<Topic>_<Specifics>` |
| **Mock emulation**    | `e2e/backends/mock/emulates_test.go`    | Test asserts the mock matches Azure's wire-level output. Mock-only by nature.                         | `TestMockEmulates_<Topic>`    |
| **Mock feature**      | `e2e/backends/mock/features_test.go`    | Test asserts a mock-only knob (fault injection, timing override). No Azure equivalent.                | `TestMockFeature_<Topic>`     |
| **Azure-only**        | `e2e/backends/azure/azure_only_test.go` | Behaviors unique to real Azure: Entra plumbing, real RBAC, soak tests.                                | `TestAzureOnly_<Topic>`       |
| **Mock-server unit**  | `mockrelay/server/*_test.go`            | Mock relay's own protocol tests in isolation (no aztunnel import).                                    | `Test<Topic>`                 |
| **CLI unit**          | `cmd/aztunnel/*_test.go` (no e2e tag)   | aztunnel CLI parsing / process-startup with no network.                                               | `TestCLI_<Topic>`             |

Decision tree:

1. Does the test need to exercise aztunnel-relay wire behavior? **No** → CLI
   unit or mock-server unit.
2. Does it assert something only one backend can do? **Yes** → backend-specific
   test in that backend's directory.
3. Otherwise → behavior scenario in `e2e/scenarios/`. Default.

When adding a mock emulation test, also check whether a paired behavior
scenario exists for aztunnel's response to the same wire condition. If not,
add one too — the scenario keeps the behavior covered against Azure.

## Backend-specific setup

See `e2e/backends/azure/README.md` for Azure (subscription, RG, `make e2e-setup`).
See `e2e/backends/mock/README.md` for mock (no setup; runs anywhere Go runs).

## Implementation pointers

- `e2e/scenarios/backend.go` defines the `Backend`, `Tunnel`, `Listener`,
  `Sender`, and `SetupOptions` types every scenario writes against.
- `e2e/scenarios/scenarios.go` is the entry point: `RunAllScenarios(t, b)`
  iterates every scenario across the backend's `Axes()`.
- `e2e/azrelay/` is the per-test hyco provisioner used by the Azure backend.
  CI calls it from `azure_backend_test.go`; setup commands call it from
  `e2e/infra/`.
