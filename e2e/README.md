# End-to-End Tests

These tests verify aztunnel against a real Azure Relay namespace. They run behind
the `e2e` build tag, and each Azure-dependent test provisions its own ephemeral
hyco pair (`e2e-entra-<hex>`, `e2e-sas-<hex>`) via `t.Cleanup`-scoped lifetimes,
so parallel runs do not collide.

## Quick Start (local dev)

```bash
az login
make e2e-setup
make e2e
```

Maintainers who need CI identity + GitHub secret setup run:

```bash
make e2e-ci
```

## Per-developer isolation

`make e2e-setup` defaults to `aztunnel-e2e-<alias>`, where `<alias>` is derived
from your signed-in Azure user. This keeps local test infra isolated from CI and
from other developers.

- Override alias: `ALIAS=foo make e2e-setup`
- Override RG directly: `RESOURCE_GROUP=my-rg make e2e-setup`

The setup command writes `e2e/.local.json`, which `make e2e` reads automatically.
No `eval` flow and no shell-level env export are required.

## Join an existing setup

If you already have access to an existing namespace (for example a teammate's or
the shared CI RG), record it locally without provisioning:

```bash
make e2e-attach RESOURCE_GROUP=aztunnel-e2e
```

`RELAY_NAME=<ns>` is optional when the RG has exactly one namespace.

## Running tests

`make e2e` resolves config in this order:

1. `AZURE_SUBSCRIPTION_ID` + `E2E_RESOURCE_GROUP` env vars (CI / explicit override)
2. `e2e/.local.json` (local setup/attach flow)
3. Fail with a directive error

Optional toggles:

| Variable                      | Description                                 |
| ----------------------------- | ------------------------------------------- |
| `E2E_AUTH`                    | `entra`, `sas`, or unset (both)             |
| `E2E_PROVISIONER_CONCURRENCY` | In-flight provisioning cap (default 4)      |
| `E2E_LARGE_TRANSFER`          | Set `1` to run the 100MB transfer test      |
| `E2E_LONG_LIVED`              | Set `1` to run the >2 minute keepalive test |

## Contributor-only fallback (`sasOnly`)

If setup can provision infra but cannot create role assignments
(`AuthorizationFailed` on `Microsoft.Authorization/roleAssignments/write`), setup
completes and records `sasOnly: true` in `e2e/.local.json`.

When `sasOnly` is set and `E2E_AUTH` is not explicitly set, `TestMain` forces
`E2E_AUTH=sas`, so Entra subtests skip cleanly instead of timing out.

After an owner grants access, re-record the local config without
re-attempting the role assignment so `sasOnly` flips back to false:

```bash
make e2e-grant RESOURCE_GROUP=<their-rg> RELAY_NAME=<their-namespace> ASSIGNEE=<your-upn>
make e2e-attach RESOURCE_GROUP=<their-rg> RELAY_NAME=<their-namespace>
```

## Status / troubleshooting

Run this first when local e2e runs look wrong:

```bash
make e2e-status
```

It prints the resolved source/subscription/resource-group/namespace/sas-only mode
and verifies permanent SAS-rule readability.

## Cleanup

`make e2e-clean` only deletes RGs that were created by `make e2e-setup`
(validated via ownership tags). It will not delete unrelated or attached RGs.

To forget an attached or stale local config without deleting infra:

```bash
rm e2e/.local.json
```

## CI environment overrides

CI continues to inject config via workflow `env` blocks. The default shared RG
stays `aztunnel-e2e`.

| Variable                | Purpose                               |
| ----------------------- | ------------------------------------- |
| `AZURE_SUBSCRIPTION_ID` | Subscription for ARM calls            |
| `E2E_RESOURCE_GROUP`    | Shared CI RG (default `aztunnel-e2e`) |
| `E2E_RELAY_NAME`        | Relay namespace to target             |

## Test matrix highlights

- Functional: port-forward, SOCKS5, connect, SSH, allowlist, max-connections
- Data integrity: small/large payloads, opt-in bulk + long-lived
- Concurrency: same-target and distinct-target fanout
- Metrics: endpoint, counters, error reasons, dial histograms
- Multi-instance smoke: two listeners on one hyco with flow distribution logging

<details>
<summary>Implementation notes</summary>

The implementation lives under `e2e/infra/cmd/e2e-infra`; Make targets are the
stable user-facing surface.

| Make target   | CLI subcommand                   |
| ------------- | -------------------------------- |
| `e2e-setup`   | `e2e-infra setup`                |
| `e2e-attach`  | `e2e-infra attach`               |
| `e2e-status`  | `e2e-infra status`               |
| `e2e-clean`   | `e2e-infra clean`                |
| `e2e-grant`   | `e2e-infra grant --assignee ...` |
| `e2e-ci`      | `e2e-infra ci`                   |
| `e2e-janitor` | `e2e-infra janitor`              |

</details>
