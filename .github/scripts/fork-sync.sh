#!/usr/bin/env bash
set -euo pipefail
# Execute only in the dedicated Actions checkout, never in a user's working tree.
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git fetch --no-tags origin \
  '+refs/heads/main:refs/remotes/origin/main' \
  '+refs/heads/dev/traffic-stat:refs/remotes/origin/dev/traffic-stat'
git fetch --no-tags https://github.com/Wei-Shaw/sub2api.git refs/heads/main
upstream_sha=$(git rev-parse FETCH_HEAD)
git switch --create fork-sync-main refs/remotes/origin/main
# Non-force merge deliberately retains this fork's workflows on main.
# Any conflict fails before pushing either branch.
if ! git merge --no-edit "$upstream_sha"; then
  echo '::error::Upstream/main merge conflict requires semantic agent repair preserving custom traffic-statistics and compression support; do not use blanket ours/theirs. Remote branches unchanged.'
  git diff --name-only --diff-filter=U
  exit 1
fi
main_sha=$(git rev-parse HEAD)
git switch --create fork-sync-traffic refs/remotes/origin/dev/traffic-stat
if ! git merge --no-edit "$main_sha"; then
  echo '::error::Traffic-stat merge conflict requires semantic agent repair preserving custom traffic-statistics and compression support; do not use blanket ours/theirs. Remote branches unchanged.'
  git diff --name-only --diff-filter=U
  exit 1
fi
source_sha=$(git rev-parse HEAD)
changed=false
if [[ "$source_sha" != "$(git rev-parse refs/remotes/origin/dev/traffic-stat)" ]]; then changed=true; fi
# All-or-nothing update; concurrent non-fast-forward changes fail safely.
# No fallback to non-atomic or force push.
git push --atomic origin \
  "$main_sha:refs/heads/main" \
  "$source_sha:refs/heads/dev/traffic-stat"
printf 'changed=%s\n' "$changed" >> "$GITHUB_OUTPUT"
printf 'sha=%s\n' "$source_sha" >> "$GITHUB_OUTPUT"
printf 'Upstream `%s` merged into main `%s`, then traffic-stat `%s`.\n' \
  "$upstream_sha" "$main_sha" "$source_sha" >> "$GITHUB_STEP_SUMMARY"
