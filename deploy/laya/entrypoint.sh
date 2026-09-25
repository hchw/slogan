#!/bin/sh
set -eu
if [ -n "${LAYA_API_KEY_FILE:-}" ]; then
  if [ ! -r "$LAYA_API_KEY_FILE" ]; then
    echo "LAYA_API_KEY_FILE is not readable" >&2
    exit 1
  fi
  LAYA_API_KEY="$(tr -d '\r\n' < "$LAYA_API_KEY_FILE")"
  export LAYA_API_KEY
fi
exec "$@"
