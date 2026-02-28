#!/usr/bin/env bash
# uniquestring.sh — Azure ARM/Bicep uniqueString() in pure bash
#
# Replicates the Bicep/ARM uniqueString() function exactly, producing the same
# 13-character base-32 output for any given inputs. Uses Microsoft's MurmurHash64
# variant (from Microsoft.PowerPlatform.ResourceStack) with custom constants.
#
# Arguments are joined with hyphens before hashing, matching Bicep behavior:
#   uniqueString('foo', 'bar')  ≡  uniquestring "foo" "bar"
#
# Output: 13 characters from [a-z2-7] (RFC 4648 base-32, lowercase)
#
# CAVEAT: The hash is case-sensitive. uniqueString('ABC') and uniqueString('abc')
# produce different results, even though Azure resource IDs, region names, and
# subscription IDs are case-insensitive. If you are hashing ARM resource IDs,
# normalize them to lowercase first, or you will get inconsistent results.
#
# Usage:
#   source uniquestring.sh
#   uniquestring "mySubscriptionId" "myResourceGroup"
#
# Or run directly to test:
#   ./uniquestring.sh --test
#   ./uniquestring.sh "arg1" "arg2" ...

# Only set shell options when executed directly, not when sourced.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
fi

uniquestring() {
  if [ $# -eq 0 ]; then
    echo "uniquestring: at least one argument required" >&2
    return 1
  fi

  local IFS='-'
  local input="$*"

  local -a bytes=()
  local hex
  hex=$(printf '%s' "$input" | od -An -tx1 -v | tr -d ' \n')
  local i
  for ((i = 0; i < ${#hex}; i += 2)); do
    bytes+=($((16#${hex:i:2})))
  done
  local length=${#bytes[@]}

  local C1=2869860233
  local C2=597399067
  local C3=2246822507
  local C4=3266489909
  local M32=4294967295

  _rotl32() {
    local x=$(($1 & M32))
    local r=$2
    echo $((((x << r) | (x >> (32 - r))) & M32))
  }

  _le32() {
    local off=$1
    echo $((${bytes[$off]} | (${bytes[$((off + 1))]} << 8) | (${bytes[$((off + 2))]} << 16) | (${bytes[$((off + 3))]} << 24)))
  }

  local h1=0 h2=0
  local idx=0
  local k1 k2 tmp

  while [ $((idx + 7)) -lt "$length" ]; do
    k1=$(_le32 $idx)
    k2=$(_le32 $((idx + 4)))

    k1=$(((k1 * C2) & M32))
    k1=$(_rotl32 $k1 15)
    k1=$(((k1 * C1) & M32))

    k2=$(((k2 * C1) & M32))
    k2=$(_rotl32 $k2 17)
    k2=$(((k2 * C2) & M32))

    h1=$(((h1 ^ k1) & M32))
    h1=$(_rotl32 $h1 19)
    h1=$(((h1 + h2) & M32))
    tmp=$h1
    if [ $tmp -ge 2147483648 ]; then
      tmp=$((tmp - 4294967296))
    fi
    h1=$(((tmp * 5 + 1444728091) & M32))

    h2=$(((h2 ^ k2) & M32))
    h2=$(_rotl32 $h2 13)
    h2=$(((h2 + h1) & M32))
    tmp=$h2
    if [ $tmp -ge 2147483648 ]; then
      tmp=$((tmp - 4294967296))
    fi
    h2=$(((tmp * 5 + 197830471) & M32))

    idx=$((idx + 8))
  done

  local rem=$((length - idx))
  if [ $rem -gt 0 ]; then
    k1=0
    if [ $rem -ge 4 ]; then
      k1=$(_le32 $idx)
    else
      k1=${bytes[$idx]}
      if [ $rem -ge 2 ]; then k1=$((k1 | (${bytes[$((idx + 1))]} << 8))); fi
      if [ $rem -ge 3 ]; then k1=$((k1 | (${bytes[$((idx + 2))]} << 16))); fi
    fi
    k1=$(((k1 * C2) & M32))
    k1=$(_rotl32 $k1 15)
    k1=$(((k1 * C1) & M32))
    h1=$(((h1 ^ k1) & M32))

    if [ $rem -gt 4 ]; then
      k2=0
      local off=$((idx + 4))
      local rem2=$((rem - 4))
      k2=${bytes[$off]}
      if [ $rem2 -ge 2 ]; then k2=$((k2 | (${bytes[$((off + 1))]} << 8))); fi
      if [ $rem2 -ge 3 ]; then k2=$((k2 | (${bytes[$((off + 2))]} << 16))); fi
      k2=$(((k2 * C1) & M32))
      k2=$(_rotl32 $k2 17)
      k2=$(((k2 * C2) & M32))
      h2=$(((h2 ^ k2) & M32))
    fi
  fi

  local r0=$(((h2 ^ length) & M32))
  local r1=$((((h1 ^ length) + r0) & M32))
  local r2=$(((r0 + r1) & M32))

  r1=$((((r1 ^ (r1 >> 16)) * C3) & M32))
  r1=$((((r1 ^ (r1 >> 13)) * C4) & M32))
  r1=$(((r1 ^ (r1 >> 16)) & M32))

  r2=$((((r2 ^ (r2 >> 16)) * C3) & M32))
  r2=$((((r2 ^ (r2 >> 13)) * C4) & M32))
  r2=$(((r2 ^ (r2 >> 16)) & M32))

  r1=$(((r1 + r2) & M32))
  r2=$(((r2 + r1) & M32))

  local h=$(((r2 << 32) | r1))

  local alpha="abcdefghijklmnopqrstuvwxyz234567"
  local result=""
  for ((i = 0; i < 13; i++)); do
    local idx5=$(((h >> 59) & 31))
    result="${result}${alpha:$idx5:1}"
    h=$((h << 5))
  done

  echo "$result"
  unset -f _rotl32 _le32
}

# =============================================================================
# Self-test: verified against Bicep uniqueString() output
# =============================================================================
_run_tests() {
  local pass=0 fail=0

  _check() {
    local expected="$1"
    shift
    local got
    got=$(uniquestring "$@")
    if [ "$got" = "$expected" ]; then
      pass=$((pass + 1))
      echo "  PASS  uniquestring($*) = $got"
    else
      fail=$((fail + 1))
      echo "  FAIL  uniquestring($*) = $got (expected $expected)"
    fi
  }

  echo "Running uniquestring tests..."
  _check "rbgf3xv4ufgzg" "test"
  _check "a7wkljktk6wne" "hello" "world"
  _check "cgtzqvhu4i23s" "abc"
  _check "bmg7c6xu4tw46" "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/my-rg"
  _check "xzi4ioaxtmth4" "foo" "bar" "baz"

  echo ""
  echo "$pass passed, $fail failed"
  if [ "$fail" -gt 0 ]; then
    return 1
  fi
}

# =============================================================================
# CLI: run directly for testing or one-off use
# =============================================================================
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  if [ $# -eq 0 ]; then
    echo "Usage: $(basename "$0") [--test] [ARG ...]" >&2
    echo "" >&2
    echo "Replicates Azure ARM/Bicep uniqueString() in pure bash." >&2
    echo "Arguments are joined with hyphens before hashing." >&2
    echo "" >&2
    echo "Examples:" >&2
    echo "  $(basename "$0") --test" >&2
    echo "  $(basename "$0") mySubscriptionId myResourceGroup" >&2
    exit 1
  fi

  if [ "$1" = "--test" ]; then
    _run_tests
  else
    uniquestring "$@"
  fi
fi
