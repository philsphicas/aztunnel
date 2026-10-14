#!/usr/bin/env bash
# The go directive in go.work and every go.mod names the same exact Go release,
# and nothing else pins a Go version: workflows read it with setup-go, and
# Docker builds receive it as GO_VERSION.

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

failed=0
error() {
  echo "error: $*" >&2
  failed=1
}

read_directive() {
  awk -v directive="$2" '$1 == directive { sub(/\r$/, "", $2); print $2; exit }' "$1"
}

version="$(read_directive go.work go)"
if [[ ! "$version" =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  error "go.work must declare an exact Go release (go 1.X.Y); found '$version'"
fi

mapfile -d '' module_files < <("${git_command[@]}" ls-files -z -- 'go.mod' ':(glob)**/go.mod')
for file in go.work "${module_files[@]}"; do
  if [[ "$file" != go.work && "$(read_directive "$file" go)" != "$version" ]]; then
    error "$file declares go $(read_directive "$file" go); expected $version (go.work)"
  fi
  if [[ -n "$(read_directive "$file" toolchain)" ]]; then
    error "$file must not declare a toolchain"
  fi
done

while IFS= read -r reference; do
  error "pass GO_VERSION instead of pinning a Go image version: $reference"
done < <(
  "${git_command[@]}" grep -nE 'golang:[0-9]+\.[0-9]' -- . ':(exclude)scripts/check-go-version.sh' || true
)

while IFS= read -r -d '' workflow; do
  setup_count="$(grep -c 'uses: actions/setup-go@' "$workflow" || true)"
  source_count="$(grep -c 'go-version-file: go.work' "$workflow" || true)"
  if ((setup_count != source_count)); then
    error "$workflow must pair every setup-go step with go-version-file: go.work"
  fi
  if grep -qE '^[[:space:]]+go-version:' "$workflow"; then
    error "$workflow must not select an explicit Go version"
  fi
done < <("${git_command[@]}" ls-files -z -- '.github/workflows/*.yml' '.github/workflows/*.yaml')

golangci_version="$(tr -d '\r\n' < .golangci-version)"
if [[ ! "$golangci_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  error ".golangci-version must pin a full stable release; found '$golangci_version'"
fi
golangci_actions="$(grep -c 'uses: golangci/golangci-lint-action@' .github/workflows/ci.yml || true)"
golangci_sources="$(grep -c 'version: \${{ steps.golangci.outputs.version }}' .github/workflows/ci.yml || true)"
if ((golangci_actions == 0 || golangci_actions != golangci_sources)); then
  error "every golangci-lint action must source .golangci-version"
fi

if ! grep -q 'bash scripts/check-go-version.sh' Makefile; then
  error "Makefile must expose the repository consistency check"
fi
if [[ "$("${git_command[@]}" check-attr eol -- scripts/check-go-version.sh scripts/update-go.sh)" == *"unspecified"* ]]; then
  error "shell scripts must be forced to LF by .gitattributes"
fi

if ((failed != 0)); then
  exit 1
fi

echo "ok: every go directive is $version"
