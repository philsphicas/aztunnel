# End-to-End Tests

These tests verify aztunnel against a real Azure Relay. They are gated behind
the `e2e` build tag. Each Azure-dependent test provisions its own pair of
ephemeral hybrid connections in the configured relay namespace and tears them
down via `t.Cleanup`, so tests run with `t.Parallel()` (concurrency capped by
`E2E_PROVISIONER_CONCURRENCY`, default 4) and concurrent `go test` pipelines
do not collide.

## Quick Start

```bash
# Contributor: deploy Azure infra + grant yourself RBAC, then run tests
az login                       # required: TestMain uses DefaultAzureCredential
make e2e-infra-setup
eval "$(make e2e-infra-env)"   # exports E2E_RELAY_NAME, E2E_RESOURCE_GROUP, AZURE_SUBSCRIPTION_ID
make e2e

# Maintainer: above + CI identity + GitHub secrets
make e2e-infra-ci
```

`AZURE_SUBSCRIPTION_ID` must be set when `TestMain` constructs the relay
`Provider`. `make e2e-infra-env` resolves it from the Azure CLI's default
subscription (`~/.azure/azureProfile.json`) so `az login` plus
`eval "$(make e2e-infra-env)"` is enough for most contributors. Set it
explicitly if you have multiple subscriptions and don't want to rely on
the CLI's default.

`TestMain` constructs a process-scoped `azrelay.Provider` that hands out
freshly-suffixed pairs (`e2e-entra-<hex>` for Entra ID auth and
`e2e-sas-<hex>` for SAS-key auth) to each test via `requireDedicatedHyco`.
Tests register a `t.Cleanup` that tears their pair down at the end of the
scenario. Benchmarks instead share a single pair leased on first use and
drained on `TestMain` exit, so benchstat runs do not pay per-sub-bench
provisioning. The orphan janitor workflow (`.github/workflows/e2e-janitor.yml`)
reaps anything left behind by killed runners.

## Environment Variables

| Variable                      | Required | Description                                                            |
| ----------------------------- | -------- | ---------------------------------------------------------------------- |
| `E2E_RELAY_NAME`              | Yes      | Azure Relay namespace name                                             |
| `E2E_RESOURCE_GROUP`          | Yes      | Resource group containing the relay namespace                          |
| `AZURE_SUBSCRIPTION_ID`       | Yes      | Subscription used to provision per-test hycos                          |
| `E2E_AUTH`                    | No       | `entra`, `sas`, or both (default)                                      |
| `E2E_PROVISIONER_CONCURRENCY` | No       | Cap on in-flight hyco provisions across `t.Parallel` tests (default 4) |
| `E2E_LARGE_TRANSFER`          | No       | Set to `1` to enable 100MB bulk transfer                               |
| `E2E_LONG_LIVED`              | No       | Set to `1` to enable >2 min keepalive test                             |

Authentication is via `DefaultAzureCredential`: `az login` for local
development, OIDC federated workload identity for GitHub Actions. No SAS
listener/sender keys need to be configured by hand — the namespace
holds two **permanent** namespace-scoped authorization rules,
`e2e-listener` (Listen-only) and `e2e-sender` (Send-only), provisioned
once by `e2e-infra setup` (or `e2e-infra ci`). `TestMain` reads their
keys via `azrelay.AcquireRunRules` (a `Microsoft.Relay/...ListKeys`
call against each permanent rule — no create / no delete) and reuses
them across every per-test SAS hyco. The rules outlive every test
invocation; the orphan janitor sweeps only hybrid connections, not
authorization rules.

## Test Scenarios

### Functional

- **TestPortForwardBasic** — echo round-trip through port-forward mode
- **TestSOCKS5Basic** — echo round-trip through SOCKS5 proxy mode
- **TestConnectStdio** — raw TCP through connect (stdin/stdout) mode
- **TestSSHProxyCommand** — real SSH session through the tunnel
- **TestSASKeyAuth** — port-forward with SAS key authentication
- **TestAllowlistAllow** — CIDR allowlist permits connection
- **TestAllowlistDeny** — connection rejected by allowlist
- **TestMaxConnections** — `--max-connections` limit enforced

### Data Integrity

- **TestSmallPayload** — 256 individual 1-byte writes
- **TestLargePayload** — 1MB payload with SHA256 verification
- **TestBulkTransfer** — 100MB streaming transfer (opt-in)
- **TestLongLivedConnection** — survives >2 min idle period (opt-in)

### Concurrency

- **TestConcurrentSameTarget** — 50 simultaneous port-forward connections
- **TestConcurrentDistinctTargets** — 50 SOCKS5 connections to distinct targets

### Metrics

- **TestMetricsEndpoint** — `/metrics` returns valid Prometheus data
- **TestMetricsConnectionCount** — connection counters accurate after traffic
- **TestMetricsErrorReason** — allowlist rejection recorded with correct reason
- **TestMetricsDialDuration** — dial duration histogram populated

### Multi-Instance

- **TestMultiListenerPortForwardSmoke** — two listeners on the same hyco serve
  a single port-forward sender; asserts all flows round-trip and neither
  listener emits an error-level log line. Per-listener distribution is logged,
  not asserted, because Azure Relay's listener-selection is not specified as
  round-robin.

## Test Harness Conventions

The Phase 1 harness pass (see `helpers_test.go` + `multi_test.go`) introduces
a small set of helpers that new tests should prefer over ad-hoc sleeps and
manual process management:

- `(*aztunnelProcess).Stop(t)` — idempotent kill + wait. Safe to call from a
  test that also has the implicit `t.Cleanup` Stop registered.
- `(*aztunnelProcess).MetricsAddr(t, timeout)` — block until the subprocess
  logs `metrics server listening`, return the bound `host:port`. Use with
  `--metrics-addr 127.0.0.1:0`.
- `waitForMetric(t, addr, name, predicate, timeout)` — poll `/metrics` until
  `predicate(sumMetric(name))` is true. Prefer over `time.Sleep` before
  scraping counters.
- `waitForMetricsContains(t, addr, substr, timeout)` — same shape but for
  label-keyed assertions (e.g., `reason="dial_failed"`).
- `scrapeMetricsBest(addr)` — non-fatal scrape returning `""` on error;
  intended for use inside polling loops where transient failures are
  expected.

## Running Specific Tests

```bash
# Run only metrics tests.
go test -tags=e2e -timeout=10m -v -run TestMetrics ./e2e/...

# Run with opt-in tests.
E2E_LARGE_TRANSFER=1 E2E_LONG_LIVED=1 make e2e
```

## Infrastructure Setup

The maintainer-facing tool is `./e2e/infra/cmd/e2e-infra`, exposed via
`make e2e-infra-*` targets. It replaces the prior shell scripts and shells
out to no external tooling (no `az`, no `gh`, no `jq`).

### CLI Subcommands

| Make target         | CLI subcommand                                 | Purpose                                                                           |
| ------------------- | ---------------------------------------------- | --------------------------------------------------------------------------------- |
| `e2e-infra-setup`   | `e2e-infra setup`                              | Create RG + namespace + permanent SAS rules + grant yourself `Azure Relay Owner`. |
| `e2e-infra-ci`      | `e2e-infra ci`                                 | Above + Entra app + federated credential + GitHub secrets.                        |
| `e2e-infra-clean`   | `e2e-infra clean --yes`                        | Delete the resource group (and everything in it).                                 |
| `e2e-infra-env`     | `e2e-infra env`                                | Print `export` statements for `E2E_*` and `AZURE_*` vars.                         |
| `e2e-infra-janitor` | `e2e-infra janitor [--max-age 4h] [--dry-run]` | Delete orphan `e2e-{entra,sas}-<hex>` hycos older than max-age.                   |
| (none)              | `e2e-infra grant --self\|--user\|--sp …`       | Grant `Azure Relay Owner` to a principal.                                         |

### Environment Variable Overrides

| Variable         | Default                          | Notes                                   |
| ---------------- | -------------------------------- | --------------------------------------- |
| `RESOURCE_GROUP` | `aztunnel-e2e`                   | Resource group name                     |
| `LOCATION`       | `westus2`                        | Azure region for created resources      |
| `RELAY_NAME`     | `aztunnel-<short hash>`          | Auto-generated from sub + RG when unset |
| `ENTRA_APP`      | `aztunnel-e2e-ci`                | CI app registration display name        |
| `GITHUB_REPO`    | auto-detected from `.git/config` | `owner/name` of the GitHub repo         |
| `GITHUB_ENV`     | `e2e-azure`                      | GitHub environment name                 |

### Migrating From the Old Shell Scripts

If you previously ran `create-relay.sh` / `uniquestring.sh` to deploy a
namespace, the new `e2e-infra setup` is not bit-compatible with the
historic Bash `uniqueString` helper — different hash function, different
length — so it does **not** synthesize the same name from a given
(subscription, resource group) pair.

Two options to migrate without rebuilding:

- **Implicit discovery (recommended for single-namespace RGs):** if your
  resource group contains exactly one relay namespace,
  `e2e-infra setup` / `ci` / `env` / `janitor` all auto-discover it and
  reuse it. Just run `make e2e-infra-setup` (or `make e2e-infra-ci`) and
  the CLI prints `(reusing existing namespace …)` instead of generating
  a new name.
- **Explicit:** pass the existing namespace name on the command line or
  via `RELAY_NAME=…`. For example:
  `RELAY_NAME=aztunnel-e2e-relay make e2e-infra-setup`.

If the RG contains more than one relay namespace, every subcommand
errors out asking for `--relay-name` (or `RELAY_NAME=…`); pick one
explicitly.

### RBAC

The CI service principal (and your developer account) need the built-in
**Azure Relay Owner** role at the relay namespace scope. This single role
covers control-plane operations (create/delete hybrid connections, manage
SAS auth rules, ListKeys) and data-plane operations (Listen, Send). The
`e2e-infra setup` and `e2e-infra ci` subcommands grant it automatically.

To grant access to another user or SP:

```bash
# Yourself (idempotent)
cd e2e/infra && go run ./cmd/e2e-infra grant --self

# Another user by UPN
cd e2e/infra && go run ./cmd/e2e-infra grant --user alice@contoso.com

# A service principal by app display name
cd e2e/infra && go run ./cmd/e2e-infra grant --sp aztunnel-e2e-ci
```

### Configuring GitHub

The `ci` subcommand needs a GitHub token in `GITHUB_TOKEN` with `repo` +
`admin:org_environment` scopes (or equivalents).

```bash
# Environment secrets only (maintainer)
cd e2e/infra && GITHUB_TOKEN=$(gh auth token) go run ./cmd/e2e-infra ci

# Environment + Dependabot repo secrets
cd e2e/infra && GITHUB_TOKEN=$(gh auth token) go run ./cmd/e2e-infra ci --dependabot
```

### Costs

- **Idle**: $0 (no listeners connected)
- **During tests**: fractions of a penny (pro-rated per listener-hour, ~$0.014/hr)
- **Monthly**: $0 if only used for CI runs

Healthy `make e2e` invocations clean up their hybrid connections via
`t.Cleanup` registered by `requireDedicatedHyco`. A daily `E2E Janitor`
workflow (`make e2e-infra-janitor`) reaps orphans left behind by killed
runners or panicking tests, so the namespace does not accumulate billable
resources over time.

### Tear Down

```bash
# Delete the resource group (relay, SAS rules, RBAC assignments)
make e2e-infra-clean

# To also remove the Entra app registration (maintainer only):
APP_ID=$(az ad app list --filter "displayName eq 'aztunnel-e2e-ci'" -o json | jq -r '.[0].appId')
az ad app delete --id "$APP_ID"
```

## Concurrency Model

PR pipelines no longer use a workflow `concurrency` group, because
collisions between concurrent invocations are eliminated at the hyco level:

- `TestMain` constructs the shared `azrelay.Provider` once. Each test
  (and the shared benchmark lease) calls `Provider.Provision`, which
  generates a fresh suffix and creates `e2e-entra-<suffix>` +
  `e2e-sas-<suffix>`. `t.Parallel()` tests run with their hyco
  lifetime scoped to a single `t.Cleanup`.
- Static hyco names from prior versions (`e2e-entra`, `e2e-sas`) are no
  longer required and are intentionally not provisioned by the new setup.
- Hyco names are matched against `^e2e-(entra|sas)-[0-9a-f]{12}$` for the
  janitor, so any unrelated hycos in the namespace are not touched.
- SAS authorization rules live at **namespace scope**, not per hyco,
  and are permanent fixtures of the namespace: two rules
  (`e2e-listener` with Listen-only, `e2e-sender` with Send-only) are
  provisioned once by `e2e-infra setup` and reused across every
  `go test` invocation. `TestMain` calls `azrelay.AcquireRunRules` to
  read their primary keys via `ListKeys` — no create, no delete, no
  per-test rule churn. Every per-test hyco signs SAS tokens with these
  shared permanent keys. The two-rule split preserves the contract
  that a listener key cannot send and vice versa
  (asserted by `TestWrongSASClaim`).
- Permanent SAS rules keep the e2e-owned authorization-rule count
  fixed at two (`e2e-listener` + `e2e-sender`), well below the
  Azure Relay 12-rules-per-namespace cap, regardless of how many
  `go test` invocations are running concurrently.
- The relay namespace itself is shared across pipelines; only hybrid
  connections are per-test (authorization rules are permanent).
