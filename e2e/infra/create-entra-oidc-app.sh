#!/usr/bin/env bash
# Create an Entra ID app registration with OIDC federation for GitHub Actions.
#
# Creates the app registration, service principal, and a federated credential
# scoped to the specified GitHub environment.
# Idempotent: safe to re-run.
#
# Environment variables:
#   ENTRA_APP      App registration name     (default: aztunnel-e2e-ci)
#   GITHUB_REPO   owner/repo for OIDC       (default: auto-detected via gh)
#   GITHUB_ENV    GitHub environment name    (default: e2e-azure)
#
set -euo pipefail

ENTRA_APP="${ENTRA_APP:-aztunnel-e2e-ci}"
GITHUB_REPO="${GITHUB_REPO:-}"
GITHUB_ENV="${GITHUB_ENV:-e2e-azure}"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

for cmd in az jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not found"
done
az account show -o json >/dev/null 2>&1 || die "az CLI not logged in — run 'az login'"

if [ -z "$GITHUB_REPO" ]; then
  if command -v gh >/dev/null 2>&1; then
    GITHUB_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)
  fi
  if [ -z "$GITHUB_REPO" ]; then
    die "GITHUB_REPO must be set (e.g. owner/repo)"
  fi
fi

echo "==> Entra ID app registration: $ENTRA_APP"
if ! output=$(az ad app list --filter "displayName eq '$ENTRA_APP'" -o json); then
  die "Failed to query app registrations"
fi
count=$(echo "$output" | jq 'length')
if [ "$count" -gt 1 ]; then
  die "Multiple app registrations found with displayName '$ENTRA_APP' — use a unique name"
fi
APP_ID=$(echo "$output" | jq -r '.[0].appId // empty')
if [ -z "$APP_ID" ]; then
  if ! output=$(az ad app create --display-name "$ENTRA_APP" -o json); then
    die "Failed to create app registration"
  fi
  APP_ID=$(echo "$output" | jq -r '.appId')
  echo "    Created app: $APP_ID"
else
  echo "    Already exists: $APP_ID"
fi

echo ""
echo "==> Service principal"
if ! output=$(az ad sp list --filter "appId eq '$APP_ID'" -o json); then
  die "Failed to query service principals"
fi
SP_OID=$(echo "$output" | jq -r '.[0].id // empty')
if [ -z "$SP_OID" ]; then
  if ! output=$(az ad sp create --id "$APP_ID" -o json); then
    die "Failed to create service principal"
  fi
  SP_OID=$(echo "$output" | jq -r '.id')
  echo "    Created SP: $SP_OID"
else
  echo "    Already exists: $SP_OID"
fi

echo ""
echo "==> Federated credential for GitHub Actions"
CRED_NAME="github-actions-${GITHUB_ENV}"
if ! output=$(az ad app federated-credential list --id "$APP_ID" -o json); then
  die "Failed to list federated credentials"
fi
SUBJECT="repo:${GITHUB_REPO}:environment:${GITHUB_ENV}"
if echo "$output" | jq -e --arg name "$CRED_NAME" 'any(.[]; .name == $name)' >/dev/null; then
  echo "    Already exists: $CRED_NAME"
elif echo "$output" | jq -e --arg sub "$SUBJECT" 'any(.[]; .subject == $sub)' >/dev/null; then
  existing_name=$(echo "$output" | jq -r --arg sub "$SUBJECT" '.[] | select(.subject == $sub) | .name')
  echo "    Already exists with subject match: $existing_name"
else
  CRED_PARAMS=$(jq -n \
    --arg name "$CRED_NAME" \
    --arg issuer "https://token.actions.githubusercontent.com" \
    --arg subject "$SUBJECT" \
    '{name: $name, issuer: $issuer, subject: $subject, audiences: ["api://AzureADTokenExchange"]}')
  if ! output=$(az ad app federated-credential create --id "$APP_ID" \
    --parameters "$CRED_PARAMS" -o json); then
    die "Failed to create federated credential"
  fi
  echo "$output" | jq '{name, issuer, subject}'
fi

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  Identity: $ENTRA_APP"
echo "  App ID:   $APP_ID"
echo "  SP OID:   $SP_OID"
echo "════════════════════════════════════════════════════════════════════"
