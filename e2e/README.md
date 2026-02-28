# End-to-End Tests

These tests verify aztunnel against a real Azure Relay. They are gated behind
the `e2e` build tag and require at least one auth method configured (Entra ID
and/or SAS keys). Functional tests run against all available auth methods.

## Quick Start

```bash
# Option 1: Auto-discover from Azure (after make e2e-infra-setup)
. e2e/infra/env.sh
make e2e

# Option 2: Entra ID only
export E2E_RELAY_NAME=my-relay-namespace
export E2E_ENTRA_HYCO_NAME=e2e-entra
az login
make e2e

# Option 3: SAS keys only (no az login required)
export E2E_RELAY_NAME=my-relay-namespace
export E2E_SAS_HYCO_NAME=e2e-sas
export E2E_SAS_LISTENER_KEY_NAME=e2e-listener
export E2E_SAS_LISTENER_KEY=<key>
export E2E_SAS_SENDER_KEY_NAME=e2e-sender
export E2E_SAS_SENDER_KEY=<key>
make e2e
```

## Environment Variables

| Variable                    | Required    | Description                                |
| --------------------------- | ----------- | ------------------------------------------ |
| `E2E_RELAY_NAME`            | Yes         | Azure Relay namespace name                 |
| `E2E_ENTRA_HYCO_NAME`       | Either/both | Hybrid connection name (Entra ID auth)     |
| `E2E_SAS_HYCO_NAME`         | Either/both | Hybrid connection name (SAS key auth)      |
| `E2E_SAS_LISTENER_KEY_NAME` | With SAS    | SAS listener key name (Listen-only)        |
| `E2E_SAS_LISTENER_KEY`      | With SAS    | SAS listener key                           |
| `E2E_SAS_SENDER_KEY_NAME`   | With SAS    | SAS sender key name (Send-only)            |
| `E2E_SAS_SENDER_KEY`        | With SAS    | SAS sender key                             |
| `E2E_AUTH`                  | No          | `entra`, `sas`, or both (default)          |
| `E2E_LARGE_TRANSFER`        | No          | Set to `1` to enable 100MB bulk transfer   |
| `E2E_LONG_LIVED`            | No          | Set to `1` to enable >2 min keepalive test |

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

## Running Specific Tests

```bash
# Run only metrics tests.
go test -tags=e2e -timeout=10m -v -run TestMetrics ./e2e/...

# Run with opt-in tests.
E2E_LARGE_TRANSFER=1 E2E_LONG_LIVED=1 make e2e
```

## Infrastructure Setup

One-time setup for the Azure resources needed by e2e tests.

### Quick Start

```bash
# Contributor: deploy Azure infra + grant yourself RBAC for local testing
make e2e-infra-setup
. e2e/infra/env.sh
make e2e

# If you don't have permission to create role assignments:
SKIP_RBAC=1 make e2e-infra-setup
# (ask an admin to run: ./e2e/infra/grant-relay-access.sh --user you@contoso.com)

# Maintainer: above + CI identity + GitHub secrets (including Dependabot)
make e2e-infra-ci
```

### Scripts

Each script is independently runnable, idempotent, and can be customized via
environment variables. See the header of each script for details.

| Script                           | Purpose                                       | Prerequisites |
| -------------------------------- | --------------------------------------------- | ------------- |
| `create-relay.sh`                | Resource group, relay namespace, hybrid conns | `az`          |
| `create-relay-sas-auth-rules.sh` | SAS auth rules on `e2e-sas`                   | `az`          |
| `create-entra-oidc-app.sh`       | Entra ID app + SP + OIDC federated credential | `az`          |
| `grant-relay-access.sh`          | RBAC Relay Listener + Sender on namespace     | `az`          |
| `create-github-ci-secrets.sh`    | GitHub environment secrets/vars + Dependabot  | `az`, `gh`    |
| `env.sh`                         | Export e2e env vars (source or execute)       | `az`          |

### Environment Variable Overrides

| Variable         | Default                      | Used by                                  |
| ---------------- | ---------------------------- | ---------------------------------------- |
| `RESOURCE_GROUP` | `aztunnel-e2e`               | all scripts                              |
| `LOCATION`       | `westus2`                    | `create-relay.sh`                        |
| `RELAY_NAME`     | `aztunnel-<uniqueString(…)>` | all scripts (auto-discovered if not set) |
| `ENTRA_APP`      | `aztunnel-e2e-ci`            | identity, grant, configure               |
| `GITHUB_REPO`    | auto-detected                | identity, configure                      |
| `GITHUB_ENV`     | `e2e-azure`                  | identity, configure                      |

### Granting Access

`grant-relay-access.sh` accepts one principal per invocation:

```bash
# Grant yourself
./e2e/infra/grant-relay-access.sh --self

# Grant a service principal by app name
./e2e/infra/grant-relay-access.sh --sp aztunnel-e2e-ci

# Grant another user by UPN
./e2e/infra/grant-relay-access.sh --user alice@contoso.com
```

### Configuring GitHub

```bash
# Environment secrets only
./e2e/infra/create-github-ci-secrets.sh

# Environment + Dependabot secrets
./e2e/infra/create-github-ci-secrets.sh --dependabot

# Use a different identity for Dependabot
ENTRA_APP=aztunnel-e2e-dependabot ./e2e/infra/create-github-ci-secrets.sh --dependabot
```

### Running Scripts Individually

```bash
# Step 1: Create relay resources
./e2e/infra/create-relay.sh

# Step 2: Create SAS auth rules (optional — only needed for SAS tests)
./e2e/infra/create-relay-sas-auth-rules.sh

# Step 3: Create CI identity (maintainer only)
./e2e/infra/create-entra-oidc-app.sh

# Step 4: Grant RBAC access
./e2e/infra/grant-relay-access.sh --self
./e2e/infra/grant-relay-access.sh --sp aztunnel-e2e-ci

# Step 5: Configure GitHub (maintainer only)
./e2e/infra/create-github-ci-secrets.sh --dependabot
```

### Costs

- **Idle**: $0 (no listeners connected)
- **During tests**: fractions of a penny (pro-rated per listener-hour, ~$0.014/hr)
- **Monthly**: $0 if only used for CI runs

### Tear Down

```bash
# Delete the resource group (relay, SAS rules, RBAC assignments)
make e2e-infra-clean

# To also remove the Entra app registration (maintainer only):
APP_ID=$(az ad app list --filter "displayName eq 'aztunnel-e2e-ci'" -o json | jq -r '.[0].appId')
az ad app delete --id "$APP_ID"
```
