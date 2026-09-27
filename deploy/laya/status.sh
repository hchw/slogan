#!/bin/sh
# Reports Laya process state, checkpoint readiness and the pinned release.
#
# Process liveness is not readiness: a container can be running while its
# checkpoint is still loading, in which case semantic classification must stay
# disabled and the Gateway must report degraded rather than classifier-ready.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

docker compose --profile laya ps laya

if [ -s .laya-release.current ]; then
  printf 'pinned release: %s\n' "$(cat .laya-release.current)"
else
  printf 'pinned release: unknown (no .laya-release.current; record the image digest)\n'
fi

if ! docker compose --profile laya ps --status running --services | grep -qx laya; then
  echo "Laya process is not running" >&2
  exit 1
fi

HEALTH=$(docker compose --profile laya exec -T laya curl -fsS http://127.0.0.1:8000/health || true)
if [ -z "$HEALTH" ]; then
  echo "Laya /health is unreachable" >&2
  exit 1
fi
printf 'health: %s\n' "$HEALTH"

case "$HEALTH" in
  *'"model_loaded":false'*|*'"classifier_ready":false'*)
    echo "process alive, checkpoint NOT ready: report degraded, keep semantic classification off" >&2
    exit 1
    ;;
  *'"model_loaded":true'*|*'"classifier_ready":true'*)
    echo "classifier ready (checkpoint loaded)"
    ;;
  *)
    echo "health body does not state checkpoint readiness; run ./smoke.sh before treating the node as classifier-ready" >&2
    ;;
esac
