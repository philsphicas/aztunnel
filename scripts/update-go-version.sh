#!/usr/bin/env bash

set -euo pipefail

if (($# != 1)); then
  echo "usage: $0 <go-version>" >&2
  exit 2
fi

version="${1#go}"
if [[ ! "$version" =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: expected an exact stable Go version, got '$1'" >&2
  exit 2
fi

installed_version="$(go env GOVERSION)"
installed_version="${installed_version#go}"
if [[ "$installed_version" != "$version" ]]; then
  echo "error: installed Go is $installed_version; update requires Go $version" >&2
  exit 1
fi

baseline="${version%.*}.0"
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

go work edit -go="$baseline" -toolchain="go$version"

mapfile -d '' module_files < <("${git_command[@]}" ls-files -z -- 'go.mod' ':(glob)**/go.mod')
if ((${#module_files[@]} == 0)); then
  echo "error: no tracked go.mod files found" >&2
  exit 1
fi
for module_file in "${module_files[@]}"; do
  (
    cd "$(dirname "$module_file")"
    GOWORK=off go mod edit -go="$baseline" -toolchain=none
    GOWORK=off go mod tidy
  )
done

version_scan_paths=(
  .
  ':(exclude)scripts/check-go-version.sh'
  ':(exclude)scripts/update-go-version.sh'
)
mapfile -d '' builder_files < <(
  "${git_command[@]}" grep -lzE \
    'golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?' \
    -- "${version_scan_paths[@]}" || true
)
if ((${#builder_files[@]} == 0)); then
  echo "error: no static Go Docker builder references found" >&2
  exit 1
fi

if sed --version >/dev/null 2>&1; then
  sed_in_place=(-E -i)
else
  sed_in_place=(-E -i '')
fi
for builder_file in "${builder_files[@]}"; do
  sed "${sed_in_place[@]}" \
    "s|golang:[0-9]+(\.[0-9]+){0,2}(-[[:alnum:]_.-]+)?|golang:${version}\2|g" \
    "$builder_file"
done

bash scripts/check-go-version.sh
