#!/usr/bin/env bash
# Setup script for aztunnel e2e test infrastructure.
#
# Deploys an Azure Relay namespace with hybrid connections and grants the
# signed-in user Relay Listener + Sender RBAC roles for local e2e testing.
#
# With --ci: also creates an Entra ID app registration with OIDC federation
# for GitHub Actions and configures the GitHub environment secrets/variables.
#
# Idempotent: safe to re-run.
#
# Prerequisites: az (logged in), jq
# With --ci:     also gh (authenticated)
#
# Usage:
#   bash infra/setup-e2e.sh             # contributor: Azure infra + RBAC
#   bash infra/setup-e2e.sh --ci        # maintainer:  above + GitHub Actions
#
# Environment variables:
#   RESOURCE_GROUP  Resource group name          (default: aztunnel-e2e)
#   LOCATION        Azure region                 (default: westus2)
#   RELAY_NAME      Relay namespace name          (default: auto-generated)
#   GITHUB_REPO     owner/repo for OIDC           (required for --ci)
#   GITHUB_ENV      GitHub environment name       (default: e2e-azure)
#   APP_NAME        Entra ID app registration     (default: aztunnel-e2e-ci)
#
set -euo pipefail

# ─── DEFAULTS (override via environment variables) ───────────────────────────
RESOURCE_GROUP="${RESOURCE_GROUP:-aztunnel-e2e}"
LOCATION="${LOCATION:-westus2}"
RELAY_NAME="${RELAY_NAME:-}"            # empty = auto-generated via Bicep uniqueString
GITHUB_REPO="${GITHUB_REPO:-}"          # owner/repo for OIDC federation (--ci)
GITHUB_ENV="${GITHUB_ENV:-e2e-azure}"   # GitHub environment name (--ci)
APP_NAME="${APP_NAME:-aztunnel-e2e-ci}" # Entra ID app registration name (--ci)
# ─────────────────────────────────────────────────────────────────────────────

CI_MODE="false"
if [[ "${1:-}" == "--ci" ]]; then
  CI_MODE="true"
fi

die() {
  echo "ERROR: $*" >&2
  exit 1
}

# Preflight checks.
for cmd in az jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not found"
done
az account show -o json | jq -r '.user.name' >/dev/null 2>&1 || die "az CLI not logged in — run 'az login'"

if [[ "$CI_MODE" == "true" ]]; then
  command -v gh >/dev/null 2>&1 || die "gh is required for --ci mode but not found"
  gh auth status >/dev/null 2>&1 || die "gh CLI not authenticated — run 'gh auth login'"
  GITHUB_REPO="${GITHUB_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)}"
  [[ -n "$GITHUB_REPO" ]] || die "GITHUB_REPO must be set for --ci mode (e.g. owner/repo)"
fi

# Get current user's object ID for RBAC.
echo "==> Detecting signed-in user"
USER_IDS=()
USER_OID=$(az ad signed-in-user show -o json 2>/dev/null | jq -r '.id // empty')
if [[ -n "$USER_OID" ]]; then
  echo "    User object ID: $USER_OID"
  USER_IDS+=("$USER_OID")
else
  echo "    Could not determine user object ID (service principal login?) — skipping user RBAC"
fi

# CI mode: create app registration, service principal, and federated credential.
SP_IDS=()
APP_ID=""
if [[ "$CI_MODE" == "true" ]]; then
  echo ""
  echo "==> Entra ID app registration: $APP_NAME"
  APP_ID=$(az ad app list --filter "displayName eq '$APP_NAME'" -o json | jq -r '.[0].appId // empty')
  if [[ -z "$APP_ID" ]]; then
    APP_ID=$(az ad app create --display-name "$APP_NAME" -o json | jq -r '.appId')
    echo "    Created app: $APP_ID"
  else
    echo "    Already exists: $APP_ID"
  fi

  echo ""
  echo "==> Service principal"
  SP_OID=$(az ad sp list --filter "appId eq '$APP_ID'" -o json | jq -r '.[0].id // empty')
  if [[ -z "$SP_OID" ]]; then
    SP_OID=$(az ad sp create --id "$APP_ID" -o json | jq -r '.id')
    echo "    Created SP: $SP_OID"
  else
    echo "    Already exists: $SP_OID"
  fi
  SP_IDS+=("$SP_OID")

  echo ""
  echo "==> Federated credential for GitHub Actions"
  EXISTING_CRED=$(az ad app federated-credential list --id "$APP_ID" -o json | jq -r '.[].name')
  if echo "$EXISTING_CRED" | grep -qx "github-actions-e2e"; then
    echo "    Already exists"
  else
    CRED_PARAMS=$(jq -n \
      --arg name "github-actions-e2e" \
      --arg issuer "https://token.actions.githubusercontent.com" \
      --arg subject "repo:${GITHUB_REPO}:environment:${GITHUB_ENV}" \
      '{name: $name, issuer: $issuer, subject: $subject, audiences: ["api://AzureADTokenExchange"]}')
    az ad app federated-credential create --id "$APP_ID" \
      --parameters "$CRED_PARAMS" -o json | jq '{name, issuer, subject}'
  fi
fi

# Deploy Azure resources.
echo ""
echo "==> Creating resource group: $RESOURCE_GROUP"
az group create -n "$RESOURCE_GROUP" -l "$LOCATION" -o json | jq -r '.id'
echo ""
echo "==> Deploying Bicep template"
if [ ${#USER_IDS[@]} -eq 0 ]; then
  USER_IDS_JSON='[]'
else
  USER_IDS_JSON=$(jq -n '$ARGS.positional' --args -- "${USER_IDS[@]}")
fi
if [ ${#SP_IDS[@]} -eq 0 ]; then
  SP_IDS_JSON='[]'
else
  SP_IDS_JSON=$(jq -n '$ARGS.positional' --args -- "${SP_IDS[@]}")
fi
BICEP_PARAMS=(-p userIds="$USER_IDS_JSON" -p servicePrincipalIds="$SP_IDS_JSON")
if [[ -n "$RELAY_NAME" ]]; then
  BICEP_PARAMS+=(-p relayName="$RELAY_NAME")
fi

OUTPUTS=$(az deployment group create \
  -g "$RESOURCE_GROUP" \
  -f infra/e2e.bicep \
  "${BICEP_PARAMS[@]}" \
  -o json | jq '.properties.outputs')
echo "$OUTPUTS" | jq .

RELAY_NS=$(echo "$OUTPUTS" | jq -r '.relayNamespaceName.value')
HYCO_ENTRA=$(echo "$OUTPUTS" | jq -r '.hycoEntraName.value')
HYCO_SAS=$(echo "$OUTPUTS" | jq -r '.hycoSasName.value')

[[ -n "$RELAY_NS" && "$RELAY_NS" != "null" ]] || die "Failed to read deployment outputs"

echo ""
echo "==> Retrieving SAS keys"
LISTENER_KEY=$(az relay hyco authorization-rule keys list \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NS" \
  --hybrid-connection-name "$HYCO_SAS" \
  -n e2e-listener \
  -o json | jq -r '.primaryKey')

SENDER_KEY=$(az relay hyco authorization-rule keys list \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NS" \
  --hybrid-connection-name "$HYCO_SAS" \
  -n e2e-sender \
  -o json | jq -r '.primaryKey')

# CI mode: configure GitHub environment.
if [[ "$CI_MODE" == "true" ]]; then
  TENANT_ID=$(az account show -o json | jq -r '.tenantId')
  SUBSCRIPTION_ID=$(az account show -o json | jq -r '.id')

  echo ""
  echo "==> Configuring GitHub environment: $GITHUB_ENV"
  gh api -X PUT "repos/${GITHUB_REPO}/environments/${GITHUB_ENV}" --silent

  # Environment variables (not secret)
  gh variable set AZTUNNEL_RELAY_NAME -e "$GITHUB_ENV" -b "$RELAY_NS"
  gh variable set AZTUNNEL_HYCO_NAME -e "$GITHUB_ENV" -b "$HYCO_ENTRA"
  gh variable set AZTUNNEL_SAS_HYCO_NAME -e "$GITHUB_ENV" -b "$HYCO_SAS"

  # Environment secrets
  gh secret set AZURE_CLIENT_ID -e "$GITHUB_ENV" -b "$APP_ID"
  gh secret set AZURE_TENANT_ID -e "$GITHUB_ENV" -b "$TENANT_ID"
  gh secret set AZURE_SUBSCRIPTION_ID -e "$GITHUB_ENV" -b "$SUBSCRIPTION_ID"
  gh secret set AZTUNNEL_SAS_LISTENER_KEY_NAME -e "$GITHUB_ENV" -b "e2e-listener"
  gh secret set AZTUNNEL_SAS_LISTENER_KEY -e "$GITHUB_ENV" -b "$LISTENER_KEY"
  gh secret set AZTUNNEL_SAS_SENDER_KEY_NAME -e "$GITHUB_ENV" -b "e2e-sender"
  gh secret set AZTUNNEL_SAS_SENDER_KEY -e "$GITHUB_ENV" -b "$SENDER_KEY"
  echo "    Done"
fi

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  Setup complete!"
echo "════════════════════════════════════════════════════════════════════"
echo ""
echo "── Local testing ──"
echo "  export AZTUNNEL_RELAY_NAME=$RELAY_NS"
echo "  export AZTUNNEL_HYCO_NAME=$HYCO_ENTRA"
echo "  export AZTUNNEL_SAS_HYCO_NAME=$HYCO_SAS"
echo "  export AZTUNNEL_SAS_LISTENER_KEY_NAME=e2e-listener"
echo "  export AZTUNNEL_SAS_LISTENER_KEY=\$(az relay hyco authorization-rule keys list -g $RESOURCE_GROUP --namespace-name $RELAY_NS --hybrid-connection-name $HYCO_SAS -n e2e-listener -o json | jq -r '.primaryKey')"
echo "  export AZTUNNEL_SAS_SENDER_KEY_NAME=e2e-sender"
echo "  export AZTUNNEL_SAS_SENDER_KEY=\$(az relay hyco authorization-rule keys list -g $RESOURCE_GROUP --namespace-name $RELAY_NS --hybrid-connection-name $HYCO_SAS -n e2e-sender -o json | jq -r '.primaryKey')"
echo "  make e2e"
