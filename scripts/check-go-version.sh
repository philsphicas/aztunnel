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

failed=0
error() {
  echo "error: $*" >&2
  failed=1
}

read_directive() {
  awk -v directive="$2" '$1 == directive { sub(/\r$/, "", $2); print $2; exit }' "$1"
}

baseline="$(read_directive go.work go)"
toolchain="$(read_directive go.work toolchain)"
toolchain_version="${toolchain#go}"

if [[ ! "$baseline" =~ ^1\.[0-9]+\.0$ ]]; then
  error "go.work must declare a major.minor.0 language baseline; found '$baseline'"
fi
if [[ ! "$toolchain" =~ ^go1\.[0-9]+\.[0-9]+$ ]]; then
  error "go.work must declare an exact stable toolchain; found '$toolchain'"
fi
if [[ "${baseline%.*}" != "${toolchain_version%.*}" ]]; then
  error "go.work baseline $baseline and preferred toolchain $toolchain must use the same feature version"
fi

mapfile -d '' module_files < <("${git_command[@]}" ls-files -z -- 'go.mod' ':(glob)**/go.mod')
if ((${#module_files[@]} == 0)); then
  error "no tracked go.mod files found"
fi
for module_file in "${module_files[@]}"; do
  module_baseline="$(read_directive "$module_file" go)"
  module_toolchain="$(read_directive "$module_file" toolchain)"
  if [[ "$module_baseline" != "$baseline" ]]; then
    error "$module_file declares Go $module_baseline; expected language baseline $baseline"
  fi
  if [[ -n "$module_toolchain" ]]; then
    error "$module_file declares $module_toolchain; the workspace is the single preferred-toolchain source"
  fi
done

version_scan_paths=(
  .
  ':(exclude)scripts/check-go-version.sh'
  ':(exclude)scripts/update-go-version.sh'
)

static_builder_count=0
while IFS= read -r image; do
  static_builder_count=$((static_builder_count + 1))
  image_version="${image#golang:}"
  image_version="${image_version%%-*}"
  if [[ "$image_version" != "$toolchain_version" ]]; then
    error "found Docker builder $image; expected exact toolchain Go $toolchain_version"
  fi
done < <(
  "${git_command[@]}" grep -hoE \
    'golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?' \
    -- "${version_scan_paths[@]}" || true
)
if ((static_builder_count == 0)); then
  error "no static Go Docker builder references found"
fi

while IFS= read -r reference; do
  if [[ "$reference" == 'golang:${{'* ]]; then
    continue
  fi
  if [[ "$reference" =~ ^golang:\$\([A-Z_][A-Z0-9_]*\)(-[[:alnum:]_.-]+)?$ ]]; then
    continue
  fi
  if [[ "$reference" =~ ^golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?$ ]]; then
    continue
  fi
  error "unsupported Go Docker reference '$reference'"
done < <(
  "${git_command[@]}" grep -hoE 'golang:[[:alnum:]$(){}_.:+/@-]+' \
    -- "${version_scan_paths[@]}" || true
)

while IFS= read -r -d '' workflow; do
  setup_count="$(grep -c 'uses: actions/setup-go@' "$workflow" || true)"
  if [[ "$workflow" == ".github/workflows/update-go.yml" ]]; then
    if ((setup_count != 1)); then
      error "$workflow must contain exactly one setup-go step"
    fi
    grep -q 'uses: actions/setup-go@v7' "$workflow" ||
      error "$workflow must use actions/setup-go@v7"
    grep -qE 'go-version:[[:space:]]+stable' "$workflow" ||
      error "$workflow must discover the stable Go version"
    grep -qE 'check-latest:[[:space:]]+true' "$workflow" ||
      error "$workflow must check for the latest stable release"
    continue
  fi

  source_count="$(grep -c 'go-version-file: go.work' "$workflow" || true)"
  v7_count="$(grep -c 'uses: actions/setup-go@v7' "$workflow" || true)"
  if ((setup_count != source_count || setup_count != v7_count)); then
    error "$workflow must pair every setup-go@v7 step with go-version-file: go.work"
  fi
  if grep -qE '^[[:space:]]+go-version:' "$workflow"; then
    error "$workflow must not select an explicit Go version outside the updater"
  fi
done < <("${git_command[@]}" ls-files -z -- '.github/workflows/*.yml' '.github/workflows/*.yaml')

if ! grep -qE 'BUILDER_IMAGE[[:space:]]*=[[:space:]]*golang:\$\{\{[[:space:]]*steps\.go\.outputs\.version[[:space:]]*\}\}-\$\{\{[[:space:]]*matrix\.builder_variant[[:space:]]*\}\}' \
  .github/workflows/publish-release.yml; then
  error "release publisher must derive Docker builders from the go.work toolchain"
fi

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
if [[ "$("${git_command[@]}" check-attr eol -- scripts/check-go-version.sh scripts/update-go-version.sh)" == *"unspecified"* ]]; then
  error "shell scripts must be forced to LF by .gitattributes"
fi

if ((failed != 0)); then
  exit 1
fi

echo "ok: Go baseline $baseline and toolchain $toolchain_version are synchronized"
