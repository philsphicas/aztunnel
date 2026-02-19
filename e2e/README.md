# End-to-End Tests

These tests verify aztunnel against a real Azure Relay. They are gated behind
the `e2e` build tag and skipped when the required environment variables are not
set.

## Quick Start

```bash
# Set your Azure Relay configuration.
export AZTUNNEL_RELAY_NAME=my-relay-namespace
export AZTUNNEL_HYCO_NAME=e2e-entra

# Authenticate (Entra ID).
az login

# Run tests.
make e2e
```

## Environment Variables

| Variable                         | Required | Description                                   |
| -------------------------------- | -------- | --------------------------------------------- |
| `AZTUNNEL_RELAY_NAME`            | Yes      | Azure Relay namespace name                    |
| `AZTUNNEL_HYCO_NAME`             | Yes      | Hybrid connection name (Entra ID auth)        |
| `AZTUNNEL_SAS_HYCO_NAME`         | No       | Hybrid connection name for SAS key auth tests |
| `AZTUNNEL_SAS_LISTENER_KEY_NAME` | No       | SAS listener key name (Listen-only)           |
| `AZTUNNEL_SAS_LISTENER_KEY`      | No       | SAS listener key                              |
| `AZTUNNEL_SAS_SENDER_KEY_NAME`   | No       | SAS sender key name (Send-only)               |
| `AZTUNNEL_SAS_SENDER_KEY`        | No       | SAS sender key                                |
| `E2E_LARGE_TRANSFER`             | No       | Set to `1` to enable 100MB bulk transfer test |
| `E2E_LONG_LIVED`                 | No       | Set to `1` to enable >2 min keepalive test    |

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

See [infra/README.md](../infra/README.md) for one-time Azure resource
provisioning and GitHub Actions OIDC configuration.
