#!/usr/bin/env bash
# Maintain one tracking issue while any Go update is deferred.
#
# usage: track-go-deferral.sh <deferred> <security-deferred> <report-file>
#
# The issue's creation time is the lag clock, so it does not reset when a newer
# release supersedes the one originally deferred. Prints overdue=<true|false>.
# Environment: GH_TOKEN, GH_REPO, OWNER (mentioned on escalation), RUN_URL.

set -euo pipefail

if (($# != 3)); then
  echo "usage: $0 <true|false> <true|false> <report-file>" >&2
  exit 2
fi
deferred="$1"
security="$2"
report_file="$3"

label="go-update-deferred"
overdue_label="go-update-overdue"
security_hours=24
routine_hours=72

issue="$(gh issue list --label "$label" --state open --json number,createdAt,labels \
  --jq "sort_by(.createdAt) | .[0] | select(.) |
    \"\(.number) \(.createdAt) \([.labels[].name] | index(\"$overdue_label\") != null)\"")"

if [[ "$deferred" != "true" ]]; then
  if [[ -n "$issue" ]]; then
    read -r number _ _ <<<"$issue"
    gh issue close "$number" \
      --comment "No Go update is deferred any more (${RUN_URL:-latest updater run})." >&2
  fi
  echo "overdue=false"
  exit 0
fi

threshold_hours="$routine_hours"
if [[ "$security" == "true" ]]; then
  threshold_hours="$security_hours"
fi

body="$(
  cat <<EOF
The Go updater is holding back these releases:

$(cat "$report_file")

The updater retries daily and closes this issue once nothing is deferred.
Escalation threshold: **${threshold_hours}h** from this issue's creation (${security_hours}h when a deferred release has security fixes, otherwise ${routine_hours}h).

If a security release stays blocked on a lagging builder image, decide whether to:
1. Wait. Check exposure with \`make vulncheck\`.
2. Release without the lagging image variant (a manual, temporary change to the release image matrix).

Last checked: ${RUN_URL:-unknown run}
EOF
)"

gh label create "$label" --color FBCA04 --force \
  --description "A Go toolchain update is waiting on builder images or golangci-lint" >&2
gh label create "$overdue_label" --color B60205 --force \
  --description "A deferred Go toolchain update has exceeded its escalation threshold" >&2

if [[ -z "$issue" ]]; then
  url="$(gh issue create --title "Go toolchain update deferred" --label "$label" --body "$body")"
  number="${url##*/}"
  created="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  flagged=false
else
  read -r number created flagged <<<"$issue"
  gh issue edit "$number" --body "$body" >&2
fi

age_hours=$((($(date -u +%s) - $(date -u -d "$created" +%s)) / 3600))
if ((age_hours < threshold_hours)); then
  echo "overdue=false"
  exit 0
fi
if [[ "$flagged" != "true" ]]; then
  gh issue edit "$number" --add-label "$overdue_label" >&2
  gh issue comment "$number" --body \
    "@${OWNER:?} this Go update has been deferred for ${age_hours}h (threshold ${threshold_hours}h) and needs a decision." >&2
fi
echo "overdue=true"
echo "issue=$number"
