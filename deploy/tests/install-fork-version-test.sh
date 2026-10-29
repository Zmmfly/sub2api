#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# Source helpers only, never execute installation or make network requests.
source <(head -n -1 "$ROOT_DIR/deploy/install.sh")
github_api_curl() { printf 200; }
print_info() { :; }
[[ "$GITHUB_REPO" == Zmmfly/sub2api ]]
[[ "$(validate_version z0.2.19-10)" == z0.2.19-10 ]]
[[ "$(validate_version 0.2.19-10)" == z0.2.19-10 ]]
[[ "$(validate_version v0.2.19)" == v0.2.19 ]]
TEMP_TEST=$(mktemp -d)
trap 'rm -rf "$TEMP_TEST"' EXIT
INSTALL_DIR="$TEMP_TEST"
printf '#!/bin/sh\necho "sub2api z0.2.19-10 commit test"\n' > "$INSTALL_DIR/sub2api"
chmod +x "$INSTALL_DIR/sub2api"
[[ "$(get_current_version)" == z0.2.19-10 ]]
echo 'fork installer version checks passed'
