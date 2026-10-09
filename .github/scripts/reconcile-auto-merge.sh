#!/usr/bin/env bash
# Reconcile auto-merge on the open PR for an automation branch.
#
# usage: reconcile-auto-merge.sh <branch> <requires-review> [--disable-only]
#
# Runs on every updater pass (not just when the branch changed) so a failed
# toggle self-heals. --disable-only is for use before pushing new contents, so
# review-required changes can never ride an older auto-merge request.

set -euo pipefail

if (($# < 2 || $# > 3)) || [[ "$2" != "true" && "$2" != "false" ]] ||
  [[ $# -eq 3 && "$3" != "--disable-only" ]]; then
  echo "usage: $0 <branch> <true|false> [--disable-only]" >&2
  exit 2
fi
branch="$1"
requires_review="$2"
disable_only="${3:-}"

pr="$(gh pr list --head "$branch" --state open --json number,autoMergeRequest \
  --jq '.[0] | select(.) | "\(.number) \(.autoMergeRequest != null)"')"
if [[ -z "$pr" ]]; then
  echo "no open pull request for $branch"
  exit 0
fi
read -r number auto_merge <<<"$pr"

if [[ "$requires_review" == "true" ]]; then
  if [[ "$auto_merge" == "true" ]]; then
    gh pr merge --disable-auto "$number"
  fi
  echo "#$number requires review; auto-merge disabled"
elif [[ -z "$disable_only" && "$auto_merge" == "false" ]]; then
  # Enabling fails when the PR is already mergeable with no pending checks;
  # surface that without failing the updater.
  if ! gh pr merge --auto --squash "$number"; then
    echo "::warning::Could not enable auto-merge for #$number; merge it manually once checks pass."
  fi
fi
