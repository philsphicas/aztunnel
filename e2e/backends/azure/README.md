# Azure Backend

Runs the shared `e2e/scenarios` suite against a real Azure Relay namespace.

## Setup

Local dev box:

```bash
az login
make e2e-setup            # provisions per-developer infra; writes e2e/.local.json
make e2e-azure            # runs the suite
```

Joining an existing namespace (teammate's RG or the shared CI namespace) without
re-provisioning:

```bash
make e2e-attach RESOURCE_GROUP=<rg> [RELAY_NAME=<ns>]
```

See `e2e/infra/README.md` for the setup CLI details (subscription discovery,
role-assignment fallback to SAS-only, cleanup commands).

## `e2e/.local.json`

Written by `make e2e-setup` / `make e2e-attach`; consumed by `TestMain` in
`testmain_test.go`. Schema in `localconfig.go`. **Never commit this file** — it
contains your subscription / tenant / UPN.

## Env-var overrides

`TestMain` resolves config in this order:

1. `AZURE_SUBSCRIPTION_ID` + `E2E_RESOURCE_GROUP` + `E2E_RELAY_NAME` env vars
   (CI / explicit override).
2. `e2e/.local.json` (local setup/attach flow).
3. Fail with a directive error.

Optional toggles:

| Variable                      | Description                                           |
| ----------------------------- | ----------------------------------------------------- |
| `E2E_AUTH`                    | Pin to `entra` or `sas`; unset runs both.             |
| `E2E_PROVISIONER_CONCURRENCY` | In-flight per-test hyco provisioning cap (default 4). |

## SAS-only fallback

If `make e2e-setup` provisioned infra but could not create the role assignment,
it records `sasOnly: true` in `e2e/.local.json`. `TestMain` then forces
`E2E_AUTH=sas` so Entra subtests skip cleanly. After an owner grants access:

```bash
make e2e-grant ASSIGNEE=<your-upn>
make e2e-attach                # rewrites e2e/.local.json without sasOnly
```

## Status / troubleshooting

```bash
make e2e-status
```

prints the resolved subscription / RG / namespace / sasOnly state and verifies
permanent SAS-rule readability.
