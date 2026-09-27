#!/bin/sh
# Preflight for the Laya sidecar host. Fails loudly before a rollout instead of
# discovering resource or GPU problems after the checkpoint cache is warmed.
#
#   ./preflight.sh            # CPU baseline (default)
#   ./preflight.sh --gpu      # optional NVIDIA path: device + driver + runtime
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY=$(dirname "$DIR")
cd "$DEPLOY"

MODE=cpu
case "${1:-}" in
  --gpu) MODE=gpu ;;
  "") ;;
  *) echo "usage: $0 [--gpu]" >&2; exit 2 ;;
esac

fail=0
note() { printf '%s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1" >&2; fail=1; }

if ! command -v docker >/dev/null 2>&1; then bad "docker is required"; fi
if ! docker compose version >/dev/null 2>&1; then bad "docker compose v2 is required"; fi

if [ ! -s secrets/laya_api_key.txt ]; then
  bad "create deploy/secrets/laya_api_key.txt (private caller credential) first"
fi

# CPU is the compatibility baseline; upstream quickstart figures are not a
# production guarantee, so require headroom and state it explicitly.
CPU_CORES=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)
MEM_MB=$(awk '/MemTotal/ {printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
DISK_MB=$(df -Pm . | awk 'NR==2 {printf "%d", $4}')
MIN_CPU=${LAYA_MIN_CPU:-2}
MIN_MEM_MB=${LAYA_MIN_MEM_MB:-8192}
MIN_DISK_MB=${LAYA_MIN_DISK_MB:-10240}
note "cpu=${CPU_CORES} mem=${MEM_MB}MiB free_disk=${DISK_MB}MiB"
[ "${CPU_CORES:-0}" -ge "$MIN_CPU" ] || bad "need >= ${MIN_CPU} CPU cores (override LAYA_MIN_CPU once measured)"
[ "${MEM_MB:-0}" -ge "$MIN_MEM_MB" ] || bad "need >= ${MIN_MEM_MB} MiB RAM (override LAYA_MIN_MEM_MB once measured)"
[ "${DISK_MB:-0}" -ge "$MIN_DISK_MB" ] || bad "need >= ${MIN_DISK_MB} MiB free disk for checkpoint cache (override LAYA_MIN_DISK_MB once measured)"

if [ "$MODE" = gpu ]; then
  # GPU is optional: absence must be an explicit, visible failure here rather
  # than a silent CPU fallback that changes classification latency.
  if ! command -v nvidia-smi >/dev/null 2>&1; then
    bad "nvidia-smi not found: install a compatible NVIDIA driver or use the CPU profile"
  elif ! nvidia-smi -L | grep -q '^GPU'; then
    bad "no NVIDIA GPU visible to the host"
  else
    note "gpu: $(nvidia-smi -L | head -n 1)"
  fi
  if ! docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
    bad "docker has no nvidia runtime: install nvidia-container-toolkit"
  fi
  note "GPU mode requested; start with: LAYA_DEVICE=cuda docker compose -f docker-compose.yml -f laya/compose.gpu.yml --profile laya up -d laya"
else
  note "CPU mode: verified by the default Compose profile"
fi

# Offline weights: a missing pinned checkpoint must fail readiness instead of
# triggering an unpinned download.
if [ "${HF_HUB_OFFLINE:-0}" = "1" ]; then
  if docker volume inspect "${COMPOSE_PROJECT_NAME:-deploy}_hf_models" >/dev/null 2>&1; then
    note "offline weights requested; cache volume present"
  else
    bad "HF_HUB_OFFLINE=1 but the hf_models cache volume does not exist yet: pre-seed the pinned checkpoint"
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo "preflight failed; do not start Laya or attach Gateway traffic" >&2
  exit 1
fi
printf 'preflight ok (%s mode). Preload set: %s\n' "$MODE" "${LAYA_MODELS:-<runtime default>}"
