#!/usr/bin/env python3
"""Chinese holdout evaluation for the local Laya intent classifier.

The holdout set is a fixed JSONL file (one object per line):

    {"text": "帮我用 Go 写一个并发安全的缓存", "intent": "coding"}

Two modes are supported:

* Live sidecar: point ``--base-url`` at the local Laya service and the script
  classifies every row through ``POST /v1/systemone``.
* Recorded run: pass ``--predictions`` (JSONL with ``{"text","intent","predicted",
  "confidence","latency_ms"}``) to score an already-recorded run, so the report
  is reproducible without the GPU host.

The report contains per-class recall, macro-F1, accuracy, confidence
calibration (ECE over 10 bins) and p95 latency. Semantic classification stays
disabled by default; the script only exits 0 when the operator-supplied
thresholds are recorded and met, which is the evidence required before the
feature flag may be enabled.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import statistics
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Iterable


@dataclass
class Row:
    text: str
    intent: str
    predicted: str = ""
    confidence: float | None = None
    latency_ms: float | None = None


@dataclass
class Report:
    total: int
    correct: int
    accuracy: float
    macro_f1: float
    per_class: dict[str, dict[str, float]] = field(default_factory=dict)
    ece: float = 0.0
    p95_latency_ms: float = 0.0
    unknown_predictions: int = 0

    def as_dict(self) -> dict:
        return {
            "total": self.total,
            "correct": self.correct,
            "accuracy": self.accuracy,
            "macroF1": self.macro_f1,
            "perClassRecall": {k: v["recall"] for k, v in self.per_class.items()},
            "perClass": self.per_class,
            "ece": self.ece,
            "p95LatencyMs": self.p95_latency_ms,
            "unknownPredictions": self.unknown_predictions,
        }


def load_rows(path: str, *, predictions: bool) -> list[Row]:
    rows: list[Row] = []
    with open(path, encoding="utf-8") as handle:
        for lineno, line in enumerate(handle, 1):
            line = line.strip()
            if not line:
                continue
            try:
                raw = json.loads(line)
            except json.JSONDecodeError as exc:
                raise SystemExit(f"{path}:{lineno}: invalid JSON: {exc}") from exc
            text = (raw.get("text") or "").strip()
            intent = (raw.get("intent") or raw.get("label") or "").strip()
            if not text:
                raise SystemExit(f"{path}:{lineno}: 'text' is required")
            if not predictions and not intent:
                raise SystemExit(f"{path}:{lineno}: 'intent' is required in holdout files")
            row = Row(text=text, intent=intent)
            if predictions:
                row.predicted = (raw.get("predicted") or "").strip()
                row.confidence = raw.get("confidence")
                row.latency_ms = raw.get("latency_ms")
                if not row.predicted:
                    raise SystemExit(f"{path}:{lineno}: 'predicted' is required in prediction files")
            rows.append(row)
    if not rows:
        raise SystemExit(f"{path}: no rows found")
    return rows


def classify_live(rows: Iterable[Row], base_url: str, api_key: str, timeout: float, min_confidence: float) -> None:
    endpoint = base_url.rstrip("/") + "/v1/systemone"
    for row in rows:
        payload = json.dumps({"input": row.text, "question": "intent"}).encode("utf-8")
        request = urllib.request.Request(endpoint, data=payload, method="POST")
        request.add_header("Content-Type", "application/json")
        if api_key:
            request.add_header("Authorization", f"Bearer {api_key}")
        started = time.perf_counter()
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                body = json.loads(response.read().decode("utf-8") or "{}")
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
            raise SystemExit(f"classifying {row.text!r} failed: {exc}") from exc
        row.latency_ms = (time.perf_counter() - started) * 1000
        label = body.get("choice") or body.get("label") or body.get("intent") or ""
        confidence = body.get("confidence", body.get("score"))
        row.confidence = float(confidence) if confidence is not None else None
        row.predicted = label if (row.confidence is None or row.confidence >= min_confidence) else "general"


def build_report(rows: list[Row]) -> Report:
    labels = sorted({row.intent for row in rows} | {row.predicted for row in rows if row.predicted})
    correct = sum(1 for row in rows if row.predicted == row.intent)
    per_class: dict[str, dict[str, float]] = {}
    f1_scores: list[float] = []
    for label in labels:
        tp = sum(1 for row in rows if row.intent == label and row.predicted == label)
        fp = sum(1 for row in rows if row.intent != label and row.predicted == label)
        fn = sum(1 for row in rows if row.intent == label and row.predicted != label)
        support = tp + fn
        recall = tp / support if support else 0.0
        precision = tp / (tp + fp) if (tp + fp) else 0.0
        f1 = 2 * precision * recall / (precision + recall) if (precision + recall) else 0.0
        per_class[label] = {
            "support": support, "precision": precision, "recall": recall, "f1": f1,
        }
        f1_scores.append(f1)

    # Expected calibration error over 10 equal-width confidence bins.
    bins = [[0, 0.0, 0.0] for _ in range(10)]  # count, confidence sum, accuracy sum
    for row in rows:
        if row.confidence is None:
            continue
        index = min(9, max(0, int(row.confidence * 10)))
        bins[index][0] += 1
        bins[index][1] += row.confidence
        bins[index][2] += 1.0 if row.predicted == row.intent else 0.0
    ece = 0.0
    total = len(rows)
    for count, conf_sum, acc_sum in bins:
        if count == 0:
            continue
        ece += (count / total) * abs(acc_sum / count - conf_sum / count)

    latencies = sorted(row.latency_ms for row in rows if row.latency_ms is not None)
    p95 = latencies[min(len(latencies) - 1, int(round(0.95 * (len(latencies) - 1))))] if latencies else 0.0
    unknowns = sum(1 for row in rows if row.predicted not in {r.intent for r in rows})
    return Report(
        total=total,
        correct=correct,
        accuracy=correct / total if total else 0.0,
        macro_f1=statistics.fmean(f1_scores) if f1_scores else 0.0,
        per_class=per_class,
        ece=ece,
        p95_latency_ms=p95,
        unknown_predictions=unknowns,
    )


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("holdout", help="fixed Chinese holdout JSONL file")
    parser.add_argument("--base-url", help="classify against a live Laya sidecar")
    parser.add_argument("--api-key", default="", help="bearer token for the sidecar")
    parser.add_argument("--timeout", type=float, default=0.5, help="per-request timeout in seconds (default 0.5)")
    parser.add_argument("--min-confidence", type=float, default=0.60, help="below this the run is recorded as general")
    parser.add_argument("--predictions", help="score a recorded JSONL run instead of calling the sidecar")
    parser.add_argument("--output", default="", help="write the JSON report to this path")
    parser.add_argument("--min-macro-f1", type=float, help="recorded acceptance threshold; no default")
    parser.add_argument("--min-per-class-recall", type=float, help="recorded acceptance threshold; no default")
    parser.add_argument("--max-p95-latency-ms", type=float, help="recorded acceptance threshold; no default")
    parser.add_argument("--max-ece", type=float, help="recorded acceptance threshold; no default")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    if bool(args.base_url) == bool(args.predictions):
        print("exactly one of --base-url or --predictions is required", file=sys.stderr)
        return 2
    rows = load_rows(args.predictions or args.holdout, predictions=bool(args.predictions))
    if args.predictions and args.predictions != args.holdout:
        # Keep the labelled holdout authoritative for ground truth.
        truth = {row.text: row.intent for row in load_rows(args.holdout, predictions=False)}
        for row in rows:
            if row.text not in truth:
                print(f"prediction row not present in the holdout set: {row.text!r}", file=sys.stderr)
                return 2
            row.intent = truth[row.text]
    if args.base_url:
        classify_live(rows, args.base_url, args.api_key, args.timeout, args.min_confidence)

    report = build_report(rows)
    payload = report.as_dict()
    # The holdout set must be fixed and versioned; record its digest with the result.
    with open(args.holdout, "rb") as handle:
        payload["holdoutPath"] = args.holdout
        payload["holdoutSha256"] = hashlib.sha256(handle.read()).hexdigest()
    payload["mode"] = "live" if args.base_url else "recorded"
    payload["minConfidence"] = args.min_confidence
    print(json.dumps(payload, ensure_ascii=False, indent=2))
    if args.output:
        with open(args.output, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            handle.write("\n")

    failures: list[str] = []
    if args.min_macro_f1 is None or args.min_per_class_recall is None:
        failures.append(
            "no recorded acceptance threshold: pass --min-macro-f1 and --min-per-class-recall "
            "(and optionally --max-p95-latency-ms/--max-ece) before enabling semantic classification"
        )
    if args.min_macro_f1 is not None and report.macro_f1 < args.min_macro_f1:
        failures.append(f"macro-F1 {report.macro_f1:.4f} < {args.min_macro_f1}")
    if args.min_per_class_recall is not None:
        for label, stats in report.per_class.items():
            if stats["support"] and stats["recall"] < args.min_per_class_recall:
                failures.append(f"recall[{label}] {stats['recall']:.4f} < {args.min_per_class_recall}")
    if args.max_p95_latency_ms is not None and report.p95_latency_ms > args.max_p95_latency_ms:
        failures.append(f"p95 latency {report.p95_latency_ms:.1f}ms > {args.max_p95_latency_ms}ms")
    if args.max_ece is not None and report.ece > args.max_ece:
        failures.append(f"ECE {report.ece:.4f} > {args.max_ece}")

    if failures:
        print("\nNOT ACCEPTED:", file=sys.stderr)
        for failure in failures:
            print(f"  - {failure}", file=sys.stderr)
        print("Semantic classification must stay disabled.", file=sys.stderr)
        return 1
    print("\nAccepted: record this report before enabling the semantic classification flag.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
