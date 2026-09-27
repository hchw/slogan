#!/bin/sh
# Upgrades the Laya sidecar to a reviewed, pinned version.
#
# A failed upgrade keeps the previous version serving: the previous release is
# recorded before the switch, and any readiness/smoke failure rolls back.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

VERSION=${1:-}
if [ -z "$VERSION" ]; then
  echo "usage: $0 <reviewed-laya-version>" >&2
  exit 2
fi
if [ ! -s secrets/laya_api_key.txt ]; then
  echo "create deploy/secrets/laya_api_key.txt first" >&2
  exit 1
fi

"$DIR/preflight.sh" ${LAYA_PREFLIGHT_ARGS:-}

CURRENT=$(cat .laya-release.current 2>/dev/null || true)
if [ -n "$CURRENT" ]; then
  printf '%s\n' "$CURRENT" >.laya-release.previous
fi

LAYA_VERSION="$VERSION" LAYA_IMAGE="slogan-laya:$VERSION" docker compose --profile laya build laya
LAYA_VERSION="$VERSION" LAYA_IMAGE="slogan-laya:$VERSION" docker compose --profile laya up -d laya

# The new image may reuse the cached checkpoint; readiness still has to pass.
READY=0
for _ in $(seq 1 90); do
  if "$DIR/status.sh" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done

if [ "$READY" -eq 1 ] && "$DIR/smoke.sh" >/dev/null 2>&1; then
  printf '%s\n' "$VERSION" >.laya-release.current
  printf 'Upgraded to Laya %s. Roll Gateway nodes one at a time after the held-out evaluation is recorded.\n' "$VERSION"
  exit 0
fi

echo "new Laya version failed readiness/smoke; keeping the previous version" >&2
"$DIR/rollback.sh"
exit 1
