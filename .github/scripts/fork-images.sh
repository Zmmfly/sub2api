#!/usr/bin/env bash
set -euo pipefail
: "${RELEASE_VERSION:?}" "${RELEASE_SHA:?}" "${GITHUB_REPOSITORY:?}"
[[ "$RELEASE_VERSION" =~ ^z[0-9]+\.[0-9]+\.[0-9]+-[1-9][0-9]*$ ]]
[[ "$RELEASE_SHA" =~ ^[0-9a-f]{40}$ ]]
image="ghcr.io/${GITHUB_REPOSITORY,,}"
# Authentication/network errors are not proof that a tag is absent.
image_exists() {
  local result
  if result=$(docker buildx imagetools inspect "$1" 2>&1); then return 0; fi
  if [[ "$result" == *"not found"* || "$result" == *"manifest unknown"* ]]; then return 1; fi
  printf '%s\n' "$result" >&2
  exit 1
}
verify_image() {
  local ref="$1" arches="$2"
  docker buildx imagetools inspect "$ref" --format '{{json .Image}}' | \
    python .github/scripts/verify-fork-image.py "$RELEASE_VERSION" "$RELEASE_SHA" "$GITHUB_REPOSITORY" "$arches"
}
case "${1:-}" in
  build)
    # Reserved version tags are immutable. Resume missing architectures only;
    # never overwrite a successfully published image during an Actions retry.
    if image_exists "$image:$RELEASE_VERSION"; then
      verify_image "$image:$RELEASE_VERSION" amd64,arm64
      echo "Versioned image already exists; preserving it."
      exit 0
    fi
    for arch in amd64 arm64; do
      if ! image_exists "$image:$RELEASE_VERSION-$arch"; then
        docker buildx build --platform "linux/$arch" \
          --file ".release-context/$arch/Dockerfile" \
          --label "org.opencontainers.image.version=$RELEASE_VERSION" \
          --label "org.opencontainers.image.revision=$RELEASE_SHA" \
          --label "org.opencontainers.image.source=https://github.com/$GITHUB_REPOSITORY" \
          --tag "$image:$RELEASE_VERSION-$arch" --push ".release-context/$arch"
      fi
      verify_image "$image:$RELEASE_VERSION-$arch" "$arch"
    done
    docker buildx imagetools create --tag "$image:$RELEASE_VERSION" \
      "$image:$RELEASE_VERSION-amd64" "$image:$RELEASE_VERSION-arm64"
    ;;
  promote)
    tip=$(git ls-remote origin refs/heads/main | cut -f1)
    if [[ "$tip" != "$RELEASE_SHA" ]]; then
      echo "Main advanced to $tip; versioned artifacts preserved, latest promotion skipped." >> "$GITHUB_STEP_SUMMARY"
      exit 0
    fi
    verify_image "$image:$RELEASE_VERSION" amd64,arm64
    docker buildx imagetools create --tag "$image:latest" "$image:$RELEASE_VERSION"
    gh release edit "$RELEASE_VERSION" --repo "$GITHUB_REPOSITORY" --latest
    printf 'Released `%s` from `%s`; main image `%s:latest`.\n' "$RELEASE_VERSION" "$RELEASE_SHA" "$image" >> "$GITHUB_STEP_SUMMARY"
    ;;
  *) echo 'usage: fork-images.sh build|promote' >&2; exit 2 ;;
esac
