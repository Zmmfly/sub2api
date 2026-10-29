#!/usr/bin/env bash
set -euo pipefail
# Dedicated Actions checkout only. Never discard fork changes to resolve conflicts.
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
git fetch --no-tags https://github.com/Wei-Shaw/sub2api.git refs/heads/main
upstream_sha=$(git rev-parse FETCH_HEAD)
git switch --create fork-sync-main refs/remotes/origin/main
if ! git merge --no-edit "$upstream_sha"; then
  echo '::error::Upstream/main merge conflict requires semantic repair preserving fork traffic statistics, compression and release workflows. Do not use blanket ours/theirs. Remote main unchanged.'
  git diff --name-only --diff-filter=U
  exit 1
fi
source_sha=$(git rev-parse HEAD)
changed=false
if [[ "$source_sha" != "$(git rev-parse refs/remotes/origin/main)" ]]; then changed=true; fi
# A concurrent remote advance rejects this non-force push safely.
git push origin "$source_sha:refs/heads/main"
printf 'changed=%s\nsha=%s\n' "$changed" "$source_sha" >> "$GITHUB_OUTPUT"
printf 'Upstream `%s` merged into fork main `%s`.\n' "$upstream_sha" "$source_sha" >> "$GITHUB_STEP_SUMMARY"
