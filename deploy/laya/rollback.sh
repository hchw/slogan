#!/bin/sh
# Rolls the Laya sidecar back to the previously recorded release.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

PREVIOUS=$(cat .laya-release.previous 2>/dev/null || true)
if [ -z "$PREVIOUS" ]; then
  echo "no previous Laya release recorded" >&2
  exit 1
fi

LAYA_VERSION="$PREVIOUS" LAYA_IMAGE="slogan-laya:$PREVIOUS" docker compose --profile laya up -d --build laya

READY=0
for _ in $(seq 1 60); do
  if "$DIR/status.sh" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done
if [ "$READY" -ne 1 ]; then
  echo "rollback version failed readiness" >&2
  exit 1
fi

CURRENT=$(cat .laya-release.current 2>/dev/null || true)
printf '%s\n' "$CURRENT" >.laya-release.previous
printf '%s\n' "$PREVIOUS" >.laya-release.current
printf 'Rolled back to Laya %s. Re-run ./smoke.sh before admitting Gateway traffic.\n' "$PREVIOUS"
