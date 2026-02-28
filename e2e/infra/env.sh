#!/usr/bin/env bash
# Discover e2e environment variables from Azure.
#
# When sourced, exports variables into the caller's shell.
# When executed, prints export statements to stdout.
#
# Usage:
#   . e2e/infra/env.sh                    # source: exports directly
#   ./e2e/infra/env.sh                     # execute: prints exports (keys redacted)
#   ./e2e/infra/env.sh --show-secrets      # execute: prints exports (keys visible)
#   eval "$(./e2e/infra/env.sh --show-secrets)"  # execute + apply
#
# Environment variables:
#   RESOURCE_GROUP  Resource group name     (default: aztunnel-e2e)
#   RELAY_NAME      Relay namespace name    (default: auto-discovered from resource group)

_e2e_env_collect() {
  local rg="${RESOURCE_GROUP:-aztunnel-e2e}"
  local relay="${RELAY_NAME:-}"

  for cmd in az jq; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "ERROR: $cmd is required but not found" >&2
      return 1
    fi
  done

  if ! az account show -o json >/dev/null 2>&1; then
    echo "ERROR: az CLI not logged in — run 'az login'" >&2
    return 1
  fi

  if [ -z "$relay" ]; then
    local output count
    if ! output=$(az relay namespace list -g "$rg" -o json); then
      echo "ERROR: Failed to list relay namespaces in $rg" >&2
      return 1
    fi
    count=$(echo "$output" | jq 'length')
    if [ "$count" -eq 0 ]; then
      echo "ERROR: No relay namespace found in $rg — run create-relay.sh first" >&2
      return 1
    fi
    if [ "$count" -gt 1 ]; then
      echo "ERROR: Multiple relay namespaces found in $rg — set RELAY_NAME explicitly" >&2
      return 1
    fi
    relay=$(echo "$output" | jq -r '.[0].name')
  fi

  echo "==> Reading credentials from $relay" >&2

  local output
  if ! output=$(az relay hyco authorization-rule keys list \
    -g "$rg" \
    --namespace-name "$relay" \
    --hybrid-connection-name e2e-sas \
    -n e2e-listener \
    -o json); then
    echo "ERROR: Failed to read listener SAS key — run create-relay-sas-auth-rules.sh first" >&2
    return 1
  fi
  local listener_key
  listener_key=$(echo "$output" | jq -r '.primaryKey')

  if ! output=$(az relay hyco authorization-rule keys list \
    -g "$rg" \
    --namespace-name "$relay" \
    --hybrid-connection-name e2e-sas \
    -n e2e-sender \
    -o json); then
    echo "ERROR: Failed to read sender SAS key — run create-relay-sas-auth-rules.sh first" >&2
    return 1
  fi
  local sender_key
  sender_key=$(echo "$output" | jq -r '.primaryKey')

  printf 'export E2E_RELAY_NAME=%q\n' "$relay"
  printf 'export E2E_ENTRA_HYCO_NAME=%q\n' "e2e-entra"
  printf 'export E2E_SAS_HYCO_NAME=%q\n' "e2e-sas"
  printf 'export E2E_SAS_LISTENER_KEY_NAME=%q\n' "e2e-listener"
  printf 'export E2E_SAS_LISTENER_KEY=%q\n' "$listener_key"
  printf 'export E2E_SAS_SENDER_KEY_NAME=%q\n' "e2e-sender"
  printf 'export E2E_SAS_SENDER_KEY=%q\n' "$sender_key"
}

if ! _e2e_vars=$(_e2e_env_collect); then
  unset _e2e_vars
  unset -f _e2e_env_collect
  # shellcheck disable=SC2317 # exit is reachable when executed (not sourced)
  return 1 2>/dev/null || exit 1
fi
unset -f _e2e_env_collect

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  _e2e_show_secrets=false
  for _e2e_arg in "$@"; do
    case "$_e2e_arg" in
    --show-secrets) _e2e_show_secrets=true ;;
    esac
  done
  if [ "$_e2e_show_secrets" = "true" ]; then
    echo "$_e2e_vars"
  else
    # shellcheck disable=SC2001 # multi-line substitution, can't use ${//}
    echo "$_e2e_vars" | sed "s/\(E2E_SAS_\(LISTENER\|SENDER\)_KEY=\).*/\1'***'/"
  fi
  unset _e2e_show_secrets _e2e_arg
else
  eval "$_e2e_vars"
fi
unset _e2e_vars

echo "    ✓ Ready — run 'make e2e'" >&2
