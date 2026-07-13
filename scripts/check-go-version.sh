#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

git_command=(git)
if ! "${git_command[@]}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if command -v git.exe >/dev/null 2>&1 &&
    git.exe rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git_command=(git.exe)
  else
    echo "error: repository metadata is not accessible from this shell" >&2
    exit 1
  fi
fi

version_scan_paths=(
  .
  ':(exclude)scripts/check-go-version.sh'
  ':(exclude)scripts/update-go-version.sh'
)

failed=0

error() {
  echo "error: $*" >&2
  failed=1
}

read_go_version() {
  awk '$1 == "go" { sub(/\r$/, "", $2); print $2; exit }' "$1"
}

workspace_version="$(read_go_version go.work)"
if [[ ! "$workspace_version" =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  error "go.work must declare a full stable Go version; found '$workspace_version'"
fi

mapfile -d '' workspace_files < <("${git_command[@]}" ls-files -z -- 'go.work' ':(glob)**/go.work')
if ((${#workspace_files[@]} == 0)); then
  error "no tracked go.work files found"
fi

for workspace_file in "${workspace_files[@]}"; do
  declared_version="$(read_go_version "$workspace_file")"
  if [[ "$declared_version" != "$workspace_version" ]]; then
    error "$workspace_file declares Go $declared_version; expected $workspace_version from go.work"
  fi
done

mapfile -d '' module_files < <("${git_command[@]}" ls-files -z -- 'go.mod' ':(glob)**/go.mod')
if ((${#module_files[@]} == 0)); then
  error "no tracked go.mod files found"
fi

for module_file in "${module_files[@]}"; do
  module_version="$(read_go_version "$module_file")"
  if [[ "$module_version" != "$workspace_version" ]]; then
    error "$module_file declares Go $module_version; expected $workspace_version from go.work"
  fi
done

static_builder_count=0
while IFS= read -r image; do
  static_builder_count=$((static_builder_count + 1))
  if [[ "$image" =~ ^golang:([0-9]+(\.[0-9]+){0,2})(-|$) ]]; then
    image_version="${BASH_REMATCH[1]}"
    if [[ "$image_version" != "$workspace_version" ]]; then
      error "found Docker builder $image; expected Go $workspace_version"
    fi
  else
    error "cannot parse Go version from Docker builder $image"
  fi
done < <(
  "${git_command[@]}" grep -hoE \
    'golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?' \
    -- "${version_scan_paths[@]}" || true
)

if ((static_builder_count == 0)); then
  error "no static Go Docker builder references found"
fi

workflow_builder_pins="$(
  "${git_command[@]}" grep -nE 'golang:[0-9]' -- .github/workflows || true
)"
if [[ -n "$workflow_builder_pins" ]]; then
  error "workflow Docker builders must derive their version from go.work:"
  echo "$workflow_builder_pins" >&2
fi

digest_pins="$(
  "${git_command[@]}" grep -nE 'golang:[^[:space:]]+@sha256:' \
    -- "${version_scan_paths[@]}" || true
)"
if [[ -n "$digest_pins" ]]; then
  error "Go Docker digest pins are not supported by the updater:"
  echo "$digest_pins" >&2
fi

while IFS= read -r reference; do
  if [[ "$reference" == 'golang:${{'* ]]; then
    continue
  fi
  if [[ "$reference" =~ ^golang:\$\([A-Z_][A-Z0-9_]*\)(-[[:alnum:]_.-]+)?$ ]]; then
    continue
  fi
  if [[ "$reference" =~ ^golang:\$\{[[:alnum:]_]+\}(-[[:alnum:]_.-]+)?$ ]]; then
    continue
  fi
  if [[ "$reference" =~ ^golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?$ ]]; then
    continue
  fi
  error "unsupported Go Docker reference '$reference'; use the synchronized version or a checked variable"
done < <(
  "${git_command[@]}" grep -hoE 'golang:[[:alnum:]$(){}_.:+/@-]+' \
    -- "${version_scan_paths[@]}" || true
)

normal_setup_count=0
normal_source_count=0
while IFS= read -r -d '' workflow; do
  if [[ "$workflow" == ".github/workflows/update-go.yml" ]]; then
    continue
  fi

  setup_count="$(grep -c 'uses: actions/setup-go@' "$workflow" || true)"
  source_count="$(grep -c 'go-version-file: go.work' "$workflow" || true)"
  explicit_count="$(grep -c 'go-version:' "$workflow" || true)"

  normal_setup_count=$((normal_setup_count + setup_count))
  normal_source_count=$((normal_source_count + source_count))

  if ((setup_count != source_count)); then
    error "$workflow has $setup_count setup-go step(s) but $source_count go.work version source(s)"
  fi
  if ((explicit_count != 0)); then
    error "$workflow contains an explicit go-version; normal workflows must read go.work"
  fi
done < <(find .github/workflows -type f \( -name '*.yml' -o -name '*.yaml' \) -print0)

if ((normal_setup_count == 0 || normal_setup_count != normal_source_count)); then
  error "normal workflow setup-go configuration is incomplete"
fi

if ! grep -qE 'go-version:[[:space:]]+stable' .github/workflows/update-go.yml; then
  error ".github/workflows/update-go.yml must discover the stable Go version"
fi
if ! grep -qE 'check-latest:[[:space:]]+true' .github/workflows/update-go.yml; then
  error ".github/workflows/update-go.yml must check for the latest stable release"
fi

golangci_version="$(tr -d '\r\n' < .golangci-version)"
if [[ ! "$golangci_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  error ".golangci-version must pin a full stable release; found '$golangci_version'"
fi

golangci_uses="$(grep -c 'version: \${{ steps.golangci.outputs.version }}' .github/workflows/ci.yml || true)"
if ((golangci_uses != 4)); then
  error ".github/workflows/ci.yml must source all four golangci-lint versions from .golangci-version"
fi

if ((failed != 0)); then
  exit 1
fi

echo "ok: Go $workspace_version is synchronized across modules, workflows, and Docker builders"
