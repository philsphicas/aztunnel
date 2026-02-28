#!/usr/bin/env bash
# Create SAS authorization rules on the e2e-sas hybrid connection.
#
# Creates separate listener (Listen) and sender (Send) keys.
# Idempotent: safe to re-run.
#
# Environment variables:
#   RESOURCE_GROUP  Resource group name     (default: aztunnel-e2e)
#   RELAY_NAME      Relay namespace name    (default: auto-discovered from resource group)
#
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-aztunnel-e2e}"
RELAY_NAME="${RELAY_NAME:-}"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

for cmd in az jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not found"
done
az account show -o json >/dev/null 2>&1 || die "az CLI not logged in — run 'az login'"

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

echo ""
echo "==> Creating SAS auth rule: e2e-listener (Listen)"
if ! output=$(az relay hyco authorization-rule create \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  --hybrid-connection-name e2e-sas \
  -n e2e-listener \
  --rights Listen \
  -o json); then
  die "Failed to create e2e-listener auth rule"
fi
echo "$output" | jq '{name, rights}'

echo ""
echo "==> Creating SAS auth rule: e2e-sender (Send)"
if ! output=$(az relay hyco authorization-rule create \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  --hybrid-connection-name e2e-sas \
  -n e2e-sender \
  --rights Send \
  -o json); then
  die "Failed to create e2e-sender auth rule"
fi
echo "$output" | jq '{name, rights}'

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  SAS rules created on $RELAY_NAME/e2e-sas"
echo "════════════════════════════════════════════════════════════════════"
