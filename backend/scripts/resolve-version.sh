#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACKEND_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(CDPATH= cd -- "$BACKEND_DIR/.." && pwd)"
VERSION_FILE="$BACKEND_DIR/cmd/server/VERSION"

# Prefer the exact release tag when building from a tagged checkout so
# source builds from zX.Y.Z-N or vX.Y.Z don't inherit upstream VERSION metadata.
if command -v git >/dev/null 2>&1; then
  FORK_TAG="$(git -C "$REPO_DIR" tag --points-at HEAD --list 'z*' 2>/dev/null | grep -E '^z(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-[1-9][0-9]*$' || true)"
  if [ -n "$FORK_TAG" ]; then
    if [ "$(printf '%s\n' "$FORK_TAG" | wc -l)" -ne 1 ]; then
      echo 'Multiple fork release tags identify this commit; refusing ambiguous version.' >&2
      exit 1
    fi
    printf '%s\n' "$FORK_TAG"
    exit 0
  fi
  TAG="$(
    git -C "$REPO_DIR" describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null || \
    git -C "$REPO_DIR" describe --tags --exact-match --match '[0-9]*' 2>/dev/null || \
    true
  )"
  if [ -n "$TAG" ]; then
    printf '%s\n' "${TAG#v}"
    exit 0
  fi
fi

printf '%s\n' "$(tr -d '\r\n' < "$VERSION_FILE")"
