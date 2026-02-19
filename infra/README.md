# E2E Infrastructure Setup

One-time setup for the Azure resources needed by e2e tests.

## Quick Start

The automated setup script handles everything:

```bash
# Contributor: deploy Azure infra + grant yourself RBAC roles
bash infra/setup-e2e.sh

# Maintainer: above + configure GitHub Actions CI
GITHUB_REPO=owner/repo bash infra/setup-e2e.sh --ci
```

See `setup-e2e.sh` for available environment variable overrides.

## Manual Setup

The steps below are provided for reference. Prefer the setup script above.

### Prerequisites

- Azure CLI (`az`) installed and logged in
- An Azure subscription

### Deploy

```bash
# Create a resource group (if needed).
az group create -n aztunnel-e2e -l <region>

# Deploy the relay namespace and hybrid connections.
# Grant yourself Relay Listener + Sender roles.
USER_OID=$(az ad signed-in-user show -o json | jq -r '.id')
az deployment group create \
  -g aztunnel-e2e \
  -f infra/e2e.bicep \
  -p userIds="[\"$USER_OID\"]" \
  -o json | jq '.properties.outputs'
```

### Configure OIDC for GitHub Actions

1. Create an App Registration in Entra ID:

   ```bash
   az ad app create --display-name "aztunnel-e2e-ci"
   APP_ID=$(az ad app list --display-name "aztunnel-e2e-ci" -o json | jq -r '.[0].appId')
   ```

2. Create a service principal:

   ```bash
   az ad sp create --id $APP_ID
   ```

3. Add a federated credential for GitHub Actions:

   ```bash
   az ad app federated-credential create --id $APP_ID --parameters '{
     "name": "github-actions-e2e",
     "issuer": "https://token.actions.githubusercontent.com",
     "subject": "repo:<owner>/<repo>:environment:e2e-azure",
     "audiences": ["api://AzureADTokenExchange"]
   }'
   ```

4. Grant the service principal **Azure Relay Listener** and **Azure Relay Sender** by
   redeploying the Bicep template with the `servicePrincipalIds` parameter:

   ```bash
   SP_OID=$(az ad sp show --id $APP_ID -o json | jq -r '.id')
   az deployment group create \
     -g aztunnel-e2e \
     -f infra/e2e.bicep \
     -p servicePrincipalIds="[\"$SP_OID\"]" \
     -o json | jq '.properties.outputs'
   ```

5. Configure GitHub repository:
   - Create an environment named `e2e-azure` in Settings > Environments
   - Add these secrets to the environment:
     - `AZURE_CLIENT_ID` — the App Registration's Application (client) ID
     - `AZURE_TENANT_ID` — your Entra ID tenant ID
     - `AZURE_SUBSCRIPTION_ID` — your Azure subscription ID

6. Get the SAS keys for SAS auth tests (separate listener/sender keys):

   ```bash
   # Listener key (Listen-only)
   az relay hyco authorization-rule keys list \
     -g aztunnel-e2e \
     --namespace-name aztunnel-e2e-relay \
     --hybrid-connection-name e2e-sas \
     -n e2e-listener \
     -o json | jq -r '.primaryKey'

   # Sender key (Send-only)
   az relay hyco authorization-rule keys list \
     -g aztunnel-e2e \
     --namespace-name aztunnel-e2e-relay \
     --hybrid-connection-name e2e-sas \
     -n e2e-sender \
     -o json | jq -r '.primaryKey'
   ```

   Add these as environment secrets:
   - `AZTUNNEL_SAS_LISTENER_KEY_NAME` = `e2e-listener`
   - `AZTUNNEL_SAS_LISTENER_KEY` = the listener primary key
   - `AZTUNNEL_SAS_SENDER_KEY_NAME` = `e2e-sender`
   - `AZTUNNEL_SAS_SENDER_KEY` = the sender primary key

## Costs

- **Idle**: $0 (no listeners connected)
- **During tests**: fractions of a penny (pro-rated per listener-hour, ~$0.014/hr)
- **Monthly**: $0 if only used for CI runs

## Tear Down

```bash
az group delete -n aztunnel-e2e --yes
```
