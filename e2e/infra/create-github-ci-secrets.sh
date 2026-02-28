#!/usr/bin/env bash
# Configure GitHub environment secrets/variables and (optionally) Dependabot secrets.
#
# Reads credentials from Azure and sets them in the GitHub repository.
# Idempotent: safe to re-run (overwrites existing values).
#
# Usage:
#   ./e2e/infra/create-github-ci-secrets.sh              # environment secrets only
#   ./e2e/infra/create-github-ci-secrets.sh --dependabot # also set Dependabot secrets
#   ./e2e/infra/create-github-ci-secrets.sh --dry-run    # print gh commands instead of running them
#
# Flags can be combined: --dependabot --dry-run
#
# Environment variables:
#   RESOURCE_GROUP  Resource group name       (default: aztunnel-e2e)
#   RELAY_NAME      Relay namespace name      (default: auto-discovered from resource group)
#   ENTRA_APP        App registration name     (default: aztunnel-e2e-ci)
#   GITHUB_REPO     owner/repo               (default: auto-detected via gh)
#   GITHUB_ENV      GitHub environment name   (default: e2e-azure)
#
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-aztunnel-e2e}"
RELAY_NAME="${RELAY_NAME:-}"
ENTRA_APP="${ENTRA_APP:-aztunnel-e2e-ci}"
GITHUB_REPO="${GITHUB_REPO:-}"
GITHUB_ENV="${GITHUB_ENV:-e2e-azure}"
SET_DEPENDABOT="false"
DRY_RUN="false"

for arg in "$@"; do
  case "$arg" in
  --dependabot) SET_DEPENDABOT="true" ;;
  --dry-run) DRY_RUN="true" ;;
  *)
    echo "Unknown flag: $arg" >&2
    exit 1
    ;;
  esac
done

die() {
  echo "ERROR: $*" >&2
  exit 1
}

# In dry-run mode, gh is not required.
for cmd in az jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not found"
done
az account show -o json >/dev/null 2>&1 || die "az CLI not logged in — run 'az login'"

if [ "$DRY_RUN" = "false" ]; then
  command -v gh >/dev/null 2>&1 || die "gh is required but not found (use --dry-run to print commands)"
  gh auth status >/dev/null 2>&1 || die "gh CLI not authenticated — run 'gh auth login'"
fi

if [ -z "$GITHUB_REPO" ]; then
  if command -v gh >/dev/null 2>&1; then
    GITHUB_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)
  fi
  if [ -z "$GITHUB_REPO" ]; then
    die "GITHUB_REPO must be set (e.g. owner/repo)"
  fi
fi

if [ -z "$RELAY_NAME" ]; then
  if ! output=$(az relay namespace list -g "$RESOURCE_GROUP" -o json); then
    die "Failed to list relay namespaces in $RESOURCE_GROUP"
  fi
  count=$(echo "$output" | jq 'length')
  if [ "$count" -eq 0 ]; then
    die "No relay namespace found in $RESOURCE_GROUP — run create-relay.sh first"
  fi
  if [ "$count" -gt 1 ]; then
    die "Multiple relay namespaces found in $RESOURCE_GROUP — set RELAY_NAME explicitly"
  fi
  RELAY_NAME=$(echo "$output" | jq -r '.[0].name')
  echo "==> Auto-discovered relay namespace: $RELAY_NAME"
fi

echo "==> Looking up app registration: $ENTRA_APP"
if ! output=$(az ad app list --filter "displayName eq '$ENTRA_APP'" -o json); then
  die "Failed to look up app registration: $ENTRA_APP"
fi
count=$(echo "$output" | jq 'length')
if [ "$count" -eq 0 ]; then
  die "App registration not found: $ENTRA_APP — run create-entra-oidc-app.sh first"
fi
if [ "$count" -gt 1 ]; then
  die "Multiple app registrations found with displayName '$ENTRA_APP' — use a unique name"
fi
APP_ID=$(echo "$output" | jq -r '.[0].appId')
echo "    Client ID: $APP_ID"

echo ""
echo "==> Reading Azure account details"
if ! output=$(az account show -o json); then
  die "Failed to read account details"
fi
TENANT_ID=$(echo "$output" | jq -r '.tenantId')
SUBSCRIPTION_ID=$(echo "$output" | jq -r '.id')
echo "    Tenant: $TENANT_ID"
echo "    Subscription: $SUBSCRIPTION_ID"

echo ""
echo "==> Retrieving SAS keys"
if ! output=$(az relay hyco authorization-rule keys list \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  --hybrid-connection-name e2e-sas \
  -n e2e-listener \
  -o json); then
  die "Failed to read listener SAS key — run create-relay-sas-auth-rules.sh first"
fi
LISTENER_KEY=$(echo "$output" | jq -r '.primaryKey')

if ! output=$(az relay hyco authorization-rule keys list \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  --hybrid-connection-name e2e-sas \
  -n e2e-sender \
  -o json); then
  die "Failed to read sender SAS key — run create-relay-sas-auth-rules.sh first"
fi
SENDER_KEY=$(echo "$output" | jq -r '.primaryKey')

echo "    ✓ Listener key retrieved"
echo "    ✓ Sender key retrieved"

# Helper: run or print a gh command.
# In dry-run mode, redacts -b values to avoid leaking secrets.
run_gh() {
  if [ "$DRY_RUN" = "true" ]; then
    local args=() redact_next=false
    for arg in "$@"; do
      if [ "$redact_next" = "true" ]; then
        args+=("***")
        redact_next=false
      elif [ "$arg" = "-b" ]; then
        args+=("$arg")
        redact_next=true
      else
        args+=("$arg")
      fi
    done
    echo "  ${args[*]}"
  else
    "$@"
  fi
}

echo ""
if [ "$DRY_RUN" = "true" ]; then
  echo "==> gh commands for GitHub environment: $GITHUB_ENV"
else
  echo "==> Configuring GitHub environment: $GITHUB_ENV"
fi

# Ensure the GitHub environment exists.
run_gh gh api -X PUT "repos/${GITHUB_REPO}/environments/${GITHUB_ENV}" -q '.name'

# Environment variables (not secret).
run_gh gh variable set E2E_RELAY_NAME -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$RELAY_NAME"
run_gh gh variable set E2E_ENTRA_HYCO_NAME -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "e2e-entra"
run_gh gh variable set E2E_SAS_HYCO_NAME -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "e2e-sas"
echo "    ✓ Environment variables"

# Environment secrets.
run_gh gh secret set AZURE_CLIENT_ID -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$APP_ID"
run_gh gh secret set AZURE_TENANT_ID -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$TENANT_ID"
run_gh gh secret set AZURE_SUBSCRIPTION_ID -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$SUBSCRIPTION_ID"
run_gh gh secret set E2E_SAS_LISTENER_KEY_NAME -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "e2e-listener"
run_gh gh secret set E2E_SAS_LISTENER_KEY -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$LISTENER_KEY"
run_gh gh secret set E2E_SAS_SENDER_KEY_NAME -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "e2e-sender"
run_gh gh secret set E2E_SAS_SENDER_KEY -e "$GITHUB_ENV" --repo "$GITHUB_REPO" -b "$SENDER_KEY"
echo "    ✓ Environment secrets"

if [ "$SET_DEPENDABOT" = "true" ]; then
  echo ""
  if [ "$DRY_RUN" = "true" ]; then
    echo "==> gh commands for Dependabot secrets"
  else
    echo "==> Configuring Dependabot secrets"
  fi
  run_gh gh secret set AZURE_CLIENT_ID --app dependabot --repo "$GITHUB_REPO" -b "$APP_ID"
  run_gh gh secret set AZURE_TENANT_ID --app dependabot --repo "$GITHUB_REPO" -b "$TENANT_ID"
  run_gh gh secret set AZURE_SUBSCRIPTION_ID --app dependabot --repo "$GITHUB_REPO" -b "$SUBSCRIPTION_ID"
  run_gh gh secret set E2E_SAS_LISTENER_KEY_NAME --app dependabot --repo "$GITHUB_REPO" -b "e2e-listener"
  run_gh gh secret set E2E_SAS_LISTENER_KEY --app dependabot --repo "$GITHUB_REPO" -b "$LISTENER_KEY"
  run_gh gh secret set E2E_SAS_SENDER_KEY_NAME --app dependabot --repo "$GITHUB_REPO" -b "e2e-sender"
  run_gh gh secret set E2E_SAS_SENDER_KEY --app dependabot --repo "$GITHUB_REPO" -b "$SENDER_KEY"
  echo "    ✓ Dependabot secrets"
fi

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  GitHub configured: $GITHUB_REPO"
echo "  Environment:       $GITHUB_ENV"
echo "  Dependabot:        $SET_DEPENDABOT"
echo "════════════════════════════════════════════════════════════════════"
