# Laya local classification sidecar

This directory builds the third-party Python `NandhaKishorM/laya` runtime. It is **not** the Go gateway or the score-ranking service. The Gateway calls it over the private Compose network; the default semantic feature flag remains off until the Chinese held-out evaluation gate is recorded as passed.

## Pinning and first run

- Baseline package version: `0.3.20` (re-verify the upstream package/API before production release).
- Runtime image tag: `slogan-laya:0.3.20`; production release records the immutable image digest, Python package version, checkpoint revision and intent-question-set revision together.
- Never use `latest` or install from upstream `main` in production.
- Compose mounts the Hugging Face cache at `hf_models` and injects `LAYA_API_KEY` from `deploy/secrets/laya_api_key.txt`. The secret directory is git-ignored; replace the development placeholder before use.

Build and run the sidecar on the private network:

```sh
cd deploy
# Prepare secure local secret files first; never commit them.
docker compose --profile laya build laya
docker compose --profile laya up -d laya
docker compose --profile laya exec laya curl -fsS http://127.0.0.1:8000/health
```

The service must also pass a typed-decision smoke request against the pinned `/v1/systemone` contract before its Gateway node reports classifier-ready. Process liveness alone does not mean checkpoints are loaded.

## CPU/GPU and offline weights

CPU is the compatibility baseline. Measure actual RAM, disk, cold start and classification P95 for the selected checkpoint; upstream quickstart resource figures are not production guarantees. NVIDIA deployment is optional and requires a compatible NVIDIA driver/container runtime. Keep device selection explicit. To enable offline inference, pre-seed and verify the pinned checkpoint in the persistent cache, then set `HF_HUB_OFFLINE=1`; a missing weight must fail readiness, not trigger an unpinned download.

## Upgrade/rollback

1. Build a new versioned image and record its digest without removing the old image/cache.
2. Start the new sidecar on an isolated/private test node; verify `/health`, authenticated `/v1/systemone`, schema and the fixed Chinese smoke/held-out suite.
3. Roll Gateway nodes one at a time. Switch a node only after the new runtime is ready; retain the prior tag and checkpoint cache until all nodes pass.
4. If any check fails, point the Gateway back to the previous image/config and confirm its health before returning the node to service.

The scripts `install.sh`, `status.sh`, `upgrade.sh` and `rollback.sh` are controlled local operator helpers. They do not expose shell/process control through the web console.

## Operator entry points

| Script | Purpose |
| --- | --- |
| `preflight.sh [--gpu]` | Host CPU/RAM/disk and, in GPU mode, device + driver + `nvidia` container-runtime checks. Fails loudly instead of silently degrading to CPU. |
| `install.sh` | Clean-host install: preflight -> build -> start -> wait for checkpoint readiness -> typed-decision smoke -> record the pinned release. |
| `status.sh` | Process state plus checkpoint readiness. A running container with `model_loaded=false` reports **not ready**, never classifier-ready. |
| `smoke.sh` | Authenticated `POST /v1/systemone` against the pinned contract. |
| `upgrade.sh <version>` | Reviewed upgrade; readiness/smoke failure keeps the previous version serving. |
| `rollback.sh` | Return to the recorded previous release and re-verify readiness. |

Preload and offline weights are configured through Compose:

- `LAYA_MODELS` - the preload set warmed on start (empty uses the runtime default).
- `LAYA_PRELOAD=1` - warm the preload set before reporting readiness.
- `HF_HUB_OFFLINE=1` - require the pinned checkpoint in the persistent `hf_models` cache; a missing weight must fail readiness instead of triggering an unpinned download.
- `LAYA_DEVICE=cpu|cuda` - CPU is the default; `compose.gpu.yml` adds the NVIDIA device reservation.

The sidecar is never published on a public port: it is only reachable on the private Compose network, and the internal caller must present `LAYA_API_KEY`.

## Held-out evaluation gate

Semantic classification is **off by default** (`LAYA_ENABLED=false`). It may only be enabled after `scripts/laya_holdout.py` reports an accepted run against the fixed Chinese holdout set, with thresholds, holdout digest and report recorded:

```sh
# Score a recorded run (reproducible without the GPU host).
python3 scripts/laya_holdout.py <holdout.jsonl> \
  --predictions /tmp/predictions.jsonl \
  --min-macro-f1 <recorded> --min-per-class-recall <recorded> \
  --max-p95-latency-ms <recorded> --max-ece <recorded> \
  --output docs/reports/laya-holdout.json

# Or classify live against the private sidecar.
python3 scripts/laya_holdout.py <holdout.jsonl> --base-url http://127.0.0.1:8000 \
  --api-key "$(cat deploy/secrets/laya_api_key.txt)" --min-macro-f1 <recorded> ...
```

The script prints per-class recall, macro-F1, accuracy, ECE, p95 latency and the holdout SHA-256. Without recorded thresholds it exits non-zero: the launch threshold is a measurement result, not an assumption written into code.
