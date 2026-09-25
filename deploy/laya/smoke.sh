#!/bin/sh
# Authenticated typed-decision smoke test for the local Laya sidecar.
#
#   ./smoke.sh                       # uses deploy/secrets/laya_api_key.txt
#   LAYA_SMOKE_INPUT='...' ./smoke.sh
#
# Passing this is required before a Gateway node may report classifier-ready;
# process health alone only proves the container is running.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

if [ ! -s secrets/laya_api_key.txt ]; then
  echo "create deploy/secrets/laya_api_key.txt first" >&2
  exit 1
fi

INPUT=${LAYA_SMOKE_INPUT:-'帮我用 Go 写一个并发安全的缓存'}
QUESTION=${LAYA_SMOKE_QUESTION:-intent}

HEALTH=$(docker compose --profile laya exec -T laya curl -fsS http://127.0.0.1:8000/health || true)
if [ -z "$HEALTH" ]; then
  echo "Laya /health is unreachable: process is not serving" >&2
  exit 1
fi
printf 'health: %s\n' "$HEALTH"
case "$HEALTH" in
  *'"model_loaded":false'*|*'"classifier_ready":false'*)
    echo "process is alive but the checkpoint is not loaded: semantic classification must stay disabled" >&2
    exit 1
    ;;
esac

BODY=$(printf '{"input":"%s","question":"%s"}' "$INPUT" "$QUESTION")
RESPONSE=$(docker compose --profile laya exec -T laya sh -c \
  "curl -fsS -X POST http://127.0.0.1:8000/v1/systemone -H 'Content-Type: application/json' -H \"Authorization: Bearer \$(tr -d '\\r\\n' < /run/secrets/laya_api_key)\" -d '$BODY'")
printf 'typed-decision: %s\n' "$RESPONSE"
case "$RESPONSE" in
  *'"choice"'*|*'"label"'*|*'"intent"'*) ;;
  *) echo "smoke response does not match the pinned /v1/systemone contract" >&2; exit 1 ;;
esac
printf 'smoke ok: record this run (with the image digest) before enabling semantic classification.\n'
