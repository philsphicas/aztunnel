#!/usr/bin/env bash
# Moves every go directive to the newest Go release and bumps each module that
# govulncheck reports with a fixed version, so a Go security release and its
# golang.org/x/... fixes land in one change that passes vulncheck. Writes the
# commit message to the given file (default: stdout).

set -euo pipefail
# Abort when a command inside $(...) fails, e.g. a govulncheck scan.
shopt -s inherit_errexit
cd "$(dirname "${BASH_SOURCE[0]}")/.."

message="${1:-/dev/stdout}"
export GOTOOLCHAIN=auto

current="$(awk '$1 == "go" { sub(/\r$/, "", $2); print $2; exit }' go.work)"
latest="$(go list -m -f '{{.Version}}' go@latest)"
mapfile -t modules < <(git ls-files -- go.mod ':(glob)**/go.mod' | xargs -n1 dirname)

# Release builds use golang:<version>-<variant>; Docker Hub publishes those a
# few hours after a Go release. The Microsoft Go image is allowed to lag.
images_published() {
  local variant platform output
  for variant in bookworm alpine; do
    if ! output="$(docker buildx imagetools inspect "golang:${1}-${variant}" 2>&1)"; then
      grep -qiE 'not found|manifest unknown' <<< "$output" && return 1
      echo "error: cannot inspect golang:${1}-${variant}: ${output}" >&2
      exit 1
    fi
    for platform in linux/amd64 linux/arm64; do
      grep -qE "^[[:space:]]*Platform:[[:space:]]+${platform}(/v[0-9]+)?$" <<< "$output" || return 1
    done
  done
}

target="$current"
if [[ "$latest" != "$current" ]]; then
  if images_published "$latest"; then
    target="$latest"
  else
    echo "Go ${latest} Docker images are not published yet; staying on ${current}." >&2
  fi
fi

go work edit -go="$target"
for module in "${modules[@]}"; do
  (cd "$module" && GOWORK=off go mod edit -go="$target")
done

tidy() {
  for module in "${modules[@]}"; do
    (cd "$module" && GOWORK=off go mod tidy)
  done
}

# Dependencies of a module that govulncheck reports with a fix available.
fixable() {
  local report
  report="$(cd "$1" && GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@latest -format json ./...)"
  jq -r 'select(.finding) | .finding | select(.fixed_version)
    | .trace[0].module // empty | select(. != "stdlib" and . != "toolchain")' \
    <<< "$report" | sort -u
}

tidy
bumped=()
for _ in 1 2 3; do
  # Scan every module before bumping any: a bump in one module leaves the
  # others untidy until the next tidy.
  plan=()
  for module in "${modules[@]}"; do
    found="$(fixable "$module")"
    for dependency in $found; do
      plan+=("$module $dependency")
    done
  done
  ((${#plan[@]})) || break
  # The latest release always includes the fix.
  for line in "${plan[@]}"; do
    read -r module dependency <<< "$line"
    (cd "$module" && GOWORK=off go get "${dependency}@latest")
    version="$(cd "$module" && GOWORK=off go list -m -f '{{.Version}}' "$dependency")"
    bumped+=("${dependency} ${version} ${module}")
  done
  tidy
done

bash scripts/check-go-version.sh >&2

{
  if [[ "$target" != "$current" ]]; then
    echo "Update Go to ${target}"
  else
    echo "Update vulnerable Go modules"
  fi
  echo
  if [[ "$target" != "$current" ]]; then
    echo "Update every go directive from ${current} to ${target}."
  fi
  if ((${#bumped[@]})); then
    [[ "$target" == "$current" ]] || echo
    echo "Update the modules govulncheck reports as vulnerable to their latest version:"
    echo
    # One line per module version, listing the workspace modules it updates.
    printf '%s\n' "${bumped[@]}" | sort -u | awk '
      { key = $1 " " $2; dirs[key] = (key in dirs) ? dirs[key] ", " $3 : $3 }
      END { for (k in dirs) print "  " k " (" dirs[k] ")" }' | LC_ALL=C sort
  fi
} > "$message"
