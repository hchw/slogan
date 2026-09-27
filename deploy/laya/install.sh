#!/bin/sh
# Installs (or re-installs) the pinned Laya sidecar on a clean host.
#
# The sidecar is never published on a public port: it is only reachable from the
# private Compose network. The checkpoint cache lives in the persistent
# `hf_models` volume, so re-running this script reuses already-downloaded
# weights instead of fetching them again.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

"$DIR/preflight.sh" ${LAYA_PREFLIGHT_ARGS:-}

if [ ! -s secrets/laya_api_key.txt ]; then
  echo "create deploy/secrets/laya_api_key.txt first" >&2
  exit 1
fi

VERSION=${LAYA_VERSION:-0.3.20}
LAYA_VERSION="$VERSION" LAYA_IMAGE="slogan-laya:$VERSION" docker compose --profile laya build laya
LAYA_VERSION="$VERSION" LAYA_IMAGE="slogan-laya:$VERSION" docker compose --profile laya up -d laya

# Process start is not readiness: wait for the checkpoint before reporting
# success, and never report ready from container state alone.
READY=0
for _ in $(seq 1 90); do
  if "$DIR/status.sh" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done
if [ "$READY" -ne 1 ]; then
  echo "Laya did not reach checkpoint readiness; run ./status.sh for detail" >&2
  exit 1
fi

"$DIR/smoke.sh"

printf '%s\n' "$VERSION" >.laya-release.current
if docker image inspect "slogan-laya:$VERSION" >/dev/null 2>&1; then
  printf 'image digest: %s\n' "$(docker image inspect --format '{{index .RepoDigests 0}}' "slogan-laya:$VERSION" 2>/dev/null || echo 'local build, record the image ID')"
fi
printf 'Installed Laya %s (weights reused from the hf_models cache when present).\n' "$VERSION"
printf 'Record the image digest, package version, checkpoint revision and smoke result in the release manifest.\n'
