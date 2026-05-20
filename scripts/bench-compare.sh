#!/usr/bin/env bash
#
# Run the e2e benchmark suite against two git refs and produce a
# benchstat comparison. Designed for characterising PR #47 once it
# merges:
#
#   make bench-compare BASE=<pre-47-sha> HEAD=<post-47-sha>
#
# Equivalent direct invocation:
#
#   scripts/bench-compare.sh <pre-47-sha> [<post-47-sha>]
#
# Both refs MUST contain the benchmark suite (i.e. both must be at or
# after issue #54). The script does not stub the suite into older
# refs; if BASE predates the benchmarks the run at BASE will produce
# no output and benchstat will fail to match.
#
# Each ref is checked out into its own `git worktree`, so the caller's
# working tree is never modified and the script itself is read once
# at the start (no self-overwriting hazard during checkout games).
#
# Environment knobs:
#
#   COUNT      -count= passed to `go test -bench` (default: 5).
#   BENCH      -bench= regex passed to `go test` (default: .).
#   BACKEND    "mock" (default) or "azure". Azure benchmarks require
#              `make e2e-setup`
#              first; pin E2E_AUTH for benchstat name stability.
#   BENCHTIME  -benchtime= passed to `go test` (default: 1s for mock,
#              10x for azure — each azure iteration is a real relay
#              round-trip).
#   ARTIFACTS  Optional directory to keep base.txt and head.txt; if
#              unset, a mktemp dir is used and its path is printed on
#              exit.

set -euo pipefail

BASE_REF=${1:?usage: $0 BASE_REF [HEAD_REF]}
HEAD_REF=${2:-HEAD}

COUNT=${COUNT:-5}
BENCH=${BENCH:-.}
BACKEND=${BACKEND:-mock}

case "$BACKEND" in
mock)
  BENCHTIME=${BENCHTIME:-1s}
  ;;
azure)
  BENCHTIME=${BENCHTIME:-10x}
  ;;
*)
  echo "unknown BACKEND=$BACKEND (expected: mock, azure)" >&2
  exit 1
  ;;
esac

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

BASE_SHA=$(git rev-parse "$BASE_REF")
HEAD_SHA=$(git rev-parse "$HEAD_REF")
BASE_SHORT=$(git rev-parse --short "$BASE_SHA")
HEAD_SHORT=$(git rev-parse --short "$HEAD_SHA")

if [ -n "${ARTIFACTS:-}" ]; then
  out_dir=$ARTIFACTS
  mkdir -p "$out_dir"
else
  out_dir=$(mktemp -d -t aztunnel-bench.XXXXXX)
fi

work_root=$(mktemp -d -t aztunnel-bench-wt.XXXXXX)
base_wt=$work_root/base
head_wt=$work_root/head

cleanup() {
  for wt in "$base_wt" "$head_wt"; do
    if [ -d "$wt" ]; then
      git worktree remove --force "$wt" >/dev/null 2>&1 || true
    fi
  done
  rmdir "$work_root" >/dev/null 2>&1 || true
  echo "==> artifacts: $out_dir"
}
trap cleanup EXIT

if ! command -v benchstat >/dev/null 2>&1; then
  echo "==> installing benchstat"
  GOBIN="$(go env GOPATH)/bin" go install golang.org/x/perf/cmd/benchstat@latest
  PATH="$(go env GOPATH)/bin:$PATH"
fi

echo "==> adding worktree base@$BASE_SHORT"
git worktree add --detach --quiet "$base_wt" "$BASE_SHA"
echo "==> adding worktree head@$HEAD_SHORT"
git worktree add --detach --quiet "$head_wt" "$HEAD_SHA"

if [ "$BACKEND" = "azure" ]; then
  if [ ! -f "$repo_root/e2e/.local.json" ]; then
    echo "error: BACKEND=azure requires $repo_root/e2e/.local.json (run \`make e2e-setup\` first)" >&2
    exit 1
  fi
  cp "$repo_root/e2e/.local.json" "$base_wt/e2e/.local.json"
  cp "$repo_root/e2e/.local.json" "$head_wt/e2e/.local.json"
fi

run_bench() {
  local wt=$1
  local out=$2
  local label=$3
  echo "==> running benchmarks @$label ($BACKEND backend, count=$COUNT, benchtime=$BENCHTIME)"
  case "$BACKEND" in
  mock)
    (
      cd "$wt/mockrelay"
      go test -run='^$' -bench="$BENCH" -benchmem \
        -count="$COUNT" -benchtime="$BENCHTIME" \
        ./testharness/mockbackend/...
    ) | tee "$out"
    ;;
  azure)
    (
      cd "$wt"
      go test -tags=e2e -run='^$' -bench="$BENCH" -benchmem \
        -count="$COUNT" -benchtime="$BENCHTIME" -timeout=60m \
        ./e2e/...
    ) | tee "$out"
    ;;
  esac
}

run_bench "$base_wt" "$out_dir/base.txt" "$BASE_SHORT"
run_bench "$head_wt" "$out_dir/head.txt" "$HEAD_SHORT"

echo
echo "==> benchstat $BASE_SHORT -> $HEAD_SHORT"
benchstat "$out_dir/base.txt" "$out_dir/head.txt"
