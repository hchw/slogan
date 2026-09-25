#!/usr/bin/env python3
"""Capacity probe for a deployed gateway.

Measures end-to-end latency percentiles and success rate for one model against
the OpenAI-compatible endpoint. Thresholds are explicit CLI arguments with no
defaults: the deployment baseline is a measurement result, and the semantic
classification flag must stay off until the recorded baseline is met.

    GATEWAY_URL=http://127.0.0.1:8081 API_KEY=sk-... \
    python3 scripts/load_probe.py --model e2e-model --requests 200 --concurrency 8 \
      --max-p95-ms 3000 --max-error-rate 0.01 --output docs/reports/baseline.json
"""
from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request


def one_call(base_url: str, api_key: str, model: str, max_tokens: int, timeout: float) -> tuple[float, bool, str]:
    payload = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "请用一句话介绍你自己"}],
        "max_tokens": max_tokens,
    }).encode()
    request = urllib.request.Request(base_url.rstrip("/") + "/v1/chat/completions", data=payload, method="POST")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {api_key}")
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = json.loads(response.read().decode() or "{}")
        ok = bool(body.get("choices"))
        return (time.perf_counter() - started) * 1000, ok, ""
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode()[:200]
        return (time.perf_counter() - started) * 1000, False, f"HTTP {exc.code}: {detail}"
    except Exception as exc:  # noqa: BLE001 - probe records any transport failure
        return (time.perf_counter() - started) * 1000, False, str(exc)


def percentile(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, int(round(q * (len(ordered) - 1))))
    return ordered[index]


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", required=True)
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--max-tokens", type=int, default=32)
    parser.add_argument("--timeout", type=float, default=30.0)
    parser.add_argument("--base-url", default="")
    parser.add_argument("--api-key", default="")
    parser.add_argument("--output", default="")
    parser.add_argument("--max-p95-ms", type=float, help="recorded acceptance threshold; no default")
    parser.add_argument("--max-error-rate", type=float, help="recorded acceptance threshold; no default")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    base_url = args.base_url or os.environ.get("GATEWAY_URL", "http://127.0.0.1:8081")
    api_key = args.api_key or os.environ.get("API_KEY", "")
    if not api_key:
        print("API_KEY (or --api-key) is required", file=sys.stderr)
        return 2

    results: list[tuple[float, bool, str]] = []
    lock = threading.Lock()
    errors: list[str] = []

    def worker(count: int) -> None:
        for _ in range(count):
            outcome = one_call(base_url, api_key, args.model, args.max_tokens, args.timeout)
            with lock:
                results.append(outcome)
                if not outcome[1] and len(errors) < 5:
                    errors.append(outcome[2])

    threads = []
    per_thread = max(1, args.requests // args.concurrency)
    for _ in range(args.concurrency):
        thread = threading.Thread(target=worker, args=(per_thread,))
        threads.append(thread)
        thread.start()
    for thread in threads:
        thread.join()

    latencies = [r[0] for r in results if r[1]]
    failures = [r for r in results if not r[1]]
    total = len(results)
    error_rate = len(failures) / total if total else 1.0
    report = {
        "baseUrl": base_url,
        "model": args.model,
        "requests": total,
        "concurrency": args.concurrency,
        "maxTokens": args.max_tokens,
        "successRate": 1 - error_rate,
        "errorRate": error_rate,
        "latencyMs": {
            "min": min(latencies) if latencies else 0.0,
            "mean": statistics.fmean(latencies) if latencies else 0.0,
            "p50": percentile(latencies, 0.50),
            "p95": percentile(latencies, 0.95),
            "p99": percentile(latencies, 0.99),
            "max": max(latencies) if latencies else 0.0,
        },
        "sampleErrors": errors,
    }
    print(json.dumps(report, ensure_ascii=False, indent=2))
    if args.output:
        with open(args.output, "w", encoding="utf-8") as handle:
            json.dump(report, handle, ensure_ascii=False, indent=2)
            handle.write("\n")

    failures_to_report: list[str] = []
    if args.max_p95_ms is None or args.max_error_rate is None:
        failures_to_report.append("no recorded threshold: pass --max-p95-ms and --max-error-rate and record this run")
    if args.max_p95_ms is not None and report["latencyMs"]["p95"] > args.max_p95_ms:
        failures_to_report.append(f"p95 {report['latencyMs']['p95']:.1f}ms > {args.max_p95_ms}ms")
    if args.max_error_rate is not None and error_rate > args.max_error_rate:
        failures_to_report.append(f"error rate {error_rate:.4f} > {args.max_error_rate}")

    if failures_to_report:
        print("\nBASELINE NOT MET:", file=sys.stderr)
        for failure in failures_to_report:
            print(f"  - {failure}", file=sys.stderr)
        return 1
    print("\nBaseline met: record this report with the machine profile.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
