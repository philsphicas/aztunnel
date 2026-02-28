#!/usr/bin/env bash
# Grant Azure Relay Listener + Sender roles on the relay namespace.
#
# Grants at the namespace level so the principal can access all hybrid
# connections. For a test namespace this is appropriate; for production
# you would scope to individual hybrid connections.
#
# Usage:
#   ./e2e/infra/grant-relay-access.sh --self                      # current user
#   ./e2e/infra/grant-relay-access.sh --sp aztunnel-e2e-ci        # service principal by app name
#   ./e2e/infra/grant-relay-access.sh --user alice@contoso.com    # user by UPN/email
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

# Resolve the principal to grant.
ASSIGNEE=""
ASSIGNEE_TYPE=""
LABEL=""

case "${1:-}" in
--self)
  if ! output=$(az ad signed-in-user show -o json); then
    die "Could not determine signed-in user"
  fi
  ASSIGNEE=$(echo "$output" | jq -r '.id // empty')
  ASSIGNEE_TYPE="User"
  LABEL=$(echo "$output" | jq -r '.userPrincipalName // "current user"')
  ;;
--user)
  if [ -z "${2:-}" ]; then die "--user requires an argument (e.g. alice@contoso.com)"; fi
  if ! output=$(az ad user show --id "$2" -o json); then
    die "User not found: $2"
  fi
  ASSIGNEE=$(echo "$output" | jq -r '.id // empty')
  ASSIGNEE_TYPE="User"
  LABEL="$2"
  ;;
--sp)
  if [ -z "${2:-}" ]; then die "--sp requires an argument (e.g. aztunnel-e2e-ci)"; fi
  if ! output=$(az ad app list --filter "displayName eq '$2'" -o json); then
    die "Failed to look up app registration: $2"
  fi
  app_count=$(echo "$output" | jq 'length')
  if [ "$app_count" -eq 0 ]; then die "App registration not found: $2"; fi
  if [ "$app_count" -gt 1 ]; then
    die "Multiple app registrations found with displayName '$2' — use a unique name"
  fi
  APP_ID=$(echo "$output" | jq -r '.[0].appId')
  if ! output=$(az ad sp list --filter "appId eq '$APP_ID'" -o json); then
    die "Failed to look up service principal for app: $2"
  fi
  ASSIGNEE=$(echo "$output" | jq -r '.[0].id // empty')
  ASSIGNEE_TYPE="ServicePrincipal"
  LABEL="$2"
  ;;
*)
  die "Usage: grant-relay-access.sh --self | --sp <name> | --user <upn>"
  ;;
esac

if [ -z "$ASSIGNEE" ]; then
  die "Could not resolve principal"
fi

if ! output=$(az relay namespace show -g "$RESOURCE_GROUP" -n "$RELAY_NAME" -o json); then
  die "Failed to look up relay namespace $RELAY_NAME"
fi
SCOPE=$(echo "$output" | jq -r '.id')

echo "==> Granting Relay roles to: $LABEL ($ASSIGNEE)"

for ROLE in "Azure Relay Listener" "Azure Relay Sender"; do
  ARGS=(--assignee-object-id "$ASSIGNEE" --role "$ROLE" --scope "$SCOPE")
  if [ -n "$ASSIGNEE_TYPE" ]; then
    ARGS+=(--assignee-principal-type "$ASSIGNEE_TYPE")
  fi

  if az role assignment list --assignee "$ASSIGNEE" --role "$ROLE" --scope "$SCOPE" -o json | jq -e 'length > 0' >/dev/null; then
    echo "    ✓ $ROLE (already assigned)"
  elif az role assignment create "${ARGS[@]}" -o none; then
    echo "    ✓ $ROLE"
  else
    die "Failed to assign $ROLE"
  fi
done

echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  RBAC granted on: $RELAY_NAME"
echo "════════════════════════════════════════════════════════════════════"
