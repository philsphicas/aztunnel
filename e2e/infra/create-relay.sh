#!/usr/bin/env bash
# Create an Azure Relay namespace with hybrid connections for e2e tests.
#
# Idempotent: safe to re-run.
#
# The relay namespace name must be globally unique. If RELAY_NAME is not set,
# a deterministic name is generated from the subscription ID and resource group
# using the same algorithm as Bicep's uniqueString().
#
# Environment variables:
#   RESOURCE_GROUP  Resource group name     (default: aztunnel-e2e)
#   LOCATION        Azure region            (default: westus2)
#   RELAY_NAME      Relay namespace name    (default: aztunnel-<uniqueString>)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=uniquestring.sh
source "$SCRIPT_DIR/uniquestring.sh"

RESOURCE_GROUP="${RESOURCE_GROUP:-aztunnel-e2e}"
LOCATION="${LOCATION:-westus2}"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

for cmd in az jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not found"
done
az account show -o json >/dev/null 2>&1 || die "az CLI not logged in — run 'az login'"

if [ -z "${RELAY_NAME:-}" ]; then
  SUB_ID=$(az account show -o json | jq -r '.id')
  SUB_ID_LOWER=$(printf '%s' "$SUB_ID" | tr '[:upper:]' '[:lower:]')
  RG_LOWER=$(printf '%s' "$RESOURCE_GROUP" | tr '[:upper:]' '[:lower:]')
  RELAY_NAME="aztunnel-$(uniquestring "$SUB_ID_LOWER" "$RG_LOWER")"
fi

echo "==> Creating resource group: $RESOURCE_GROUP"
if ! output=$(az group create -n "$RESOURCE_GROUP" -l "$LOCATION" -o json); then
  die "Failed to create resource group"
fi
echo "$output" | jq -r '.id'

echo ""
echo "==> Creating relay namespace: $RELAY_NAME"
if ! output=$(az relay namespace create \
  -g "$RESOURCE_GROUP" \
  -n "$RELAY_NAME" \
  -l "$LOCATION" \
  --sku Standard \
  -o json); then
  die "Failed to create relay namespace"
fi
echo "$output" | jq '{name, location, provisioningState}'

echo ""
echo "==> Creating hybrid connection: e2e-entra"
if ! output=$(az relay hyco create \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  -n e2e-entra \
  --requires-client-authorization true \
  -o json); then
  die "Failed to create hybrid connection e2e-entra"
fi
echo "$output" | jq '{name}'

echo ""
echo "==> Creating hybrid connection: e2e-sas"
if ! output=$(az relay hyco create \
  -g "$RESOURCE_GROUP" \
  --namespace-name "$RELAY_NAME" \
  -n e2e-sas \
  --requires-client-authorization true \
  -o json); then
  die "Failed to create hybrid connection e2e-sas"
fi
echo "$output" | jq '{name}'

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  Relay created: $RELAY_NAME"
echo "════════════════════════════════════════════════════════════════════"
