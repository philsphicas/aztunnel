#!/usr/bin/env bash

set -euo pipefail

if (($# != 1)); then
  echo "usage: $0 <go-version>" >&2
  exit 2
fi

version="${1#go}"
if [[ ! "$version" =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: expected a full stable Go version, got '$1'" >&2
  exit 2
fi

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

mapfile -d '' workspace_files < <("${git_command[@]}" ls-files -z -- 'go.work' ':(glob)**/go.work')
if ((${#workspace_files[@]} == 0)); then
  echo "error: no tracked go.work files found" >&2
  exit 1
fi

for workspace_file in "${workspace_files[@]}"; do
  (
    cd "$(dirname "$workspace_file")"
    go work edit -go "$version"
  )
done

mapfile -d '' module_files < <("${git_command[@]}" ls-files -z -- 'go.mod' ':(glob)**/go.mod')
if ((${#module_files[@]} == 0)); then
  echo "error: no tracked go.mod files found" >&2
  exit 1
fi

for module_file in "${module_files[@]}"; do
  (
    cd "$(dirname "$module_file")"
    GOWORK=off go get "go@$version"
    GOWORK=off go mod tidy
  )
done

mapfile -d '' builder_files < <(
  "${git_command[@]}" grep -lzE \
    'golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?' -- || true
)
if ((${#builder_files[@]} == 0)); then
  echo "error: no static Go Docker builder references found" >&2
  exit 1
fi

for builder_file in "${builder_files[@]}"; do
  if [[ "$builder_file" == .github/workflows/* ]]; then
    continue
  fi
  sed -E -i \
    "s|golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?|golang:${version}\2|g" \
    "$builder_file"
done

bash scripts/check-go-version.sh
