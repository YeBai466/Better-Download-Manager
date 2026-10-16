#!/usr/bin/env sh
set -eu
baseline=4907a7a
branch=${1:-codex/rollback-download-pipeline-$(date -u +%Y%m%d%H%M%S)}
previous=$(git branch --show-current)
if [ -n "$(git status --porcelain)" ]; then
  echo 'Commit or stash local changes before switching revisions.' >&2
  exit 1
fi
git switch -c "$branch" "$baseline"
printf 'Restored source revision: %s\n' "$(git rev-parse HEAD)"
printf 'Optimized branch retained: %s\n' "$previous"
printf '%s\n' 'Download data and existing build artifacts are retained. Rebuild this revision to run the baseline executable.'
