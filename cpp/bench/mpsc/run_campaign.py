#!/usr/bin/env python3
"""Interleave independent MPSC coherence measurements.

Each configuration is one benchmark process.  A round contains every
variant/writer-count pair in a randomized order; repeating 11 rounds prevents a
single thermal or background-load phase from being assigned to one variant.
Results are emitted as JSON Lines so they can be analysed without scraping the
human-oriented benchmark output.
"""

from __future__ import annotations

import argparse
import itertools
import json
import math
import random
import subprocess
import sys
from pathlib import Path
from typing import Any

VARIANTS = ("two-plane", "two-plane-padded", "spsc")
WRITERS = (1, 2, 4, 8)


def parse_args() -> argparse.Namespace:
  parser = argparse.ArgumentParser(description=__doc__)
  parser.add_argument("benchmark", type=Path, help="bench_queue binary built with -DNDEBUG")
  parser.add_argument("--seconds", type=float, default=2.0, help="timed seconds per process")
  parser.add_argument("--runs", type=int, default=11, help="interleaved process runs per configuration")
  parser.add_argument("--seed", type=int, default=20260726, help="shuffle seed recorded in every result")
  parser.add_argument(
    "--variants", nargs="+", choices=VARIANTS, default=list(VARIANTS),
    help="selectors to run (default: every selector)",
  )
  parser.add_argument(
    "--counters", action="store_true",
    help="run one PMU-counter process per configuration after the throughput campaign",
  )
  return parser.parse_args()


def require_number(result: dict[str, Any], field: str) -> None:
  value = result.get(field)
  if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
    raise RuntimeError(f"benchmark result has invalid {field}: {result!r}")


def validate_result(result: object, mode: str, variant: str, writers: int) -> dict[str, Any]:
  if not isinstance(result, dict):
    raise RuntimeError(f"benchmark result is not an object: {result!r}")
  expected_mode = "throughput" if mode == "throughput" else "counters"
  if result.get("mode") != expected_mode or result.get("variant") != variant:
    raise RuntimeError(f"benchmark returned wrong selector: {result!r}")
  if result.get("writers") != writers:
    raise RuntimeError(f"benchmark returned wrong writer count: {result!r}")
  fields = ("seconds", "records", "mrec_s") if expected_mode == "throughput" else (
    "seconds", "records", "mrec_s", "misses", "misses_per_record",
    "cycles_per_record", "insns_per_record", "ipc", "fairness_min_max",
  )
  for field in fields:
    require_number(result, field)
  if result["seconds"] <= 0 or result["records"] < 0:
    raise RuntimeError(f"benchmark returned invalid measurement: {result!r}")
  return result


def invoke(binary: Path, mode: str, variant: str, writers: int, seconds: float) -> dict[str, Any]:
  completed = subprocess.run(
    [str(binary), mode, variant, str(writers), str(seconds)],
    check=False,
    capture_output=True,
    text=True,
  )
  if completed.returncode:
    raise RuntimeError(
      f"{mode} {variant} w={writers} exited {completed.returncode}: {completed.stderr.strip()}"
    )
  lines = [line for line in completed.stdout.splitlines() if line.strip()]
  if len(lines) != 1:
    raise RuntimeError(f"expected one JSON line, got {len(lines)}: {completed.stdout!r}")
  try:
    result = json.loads(lines[0])
  except json.JSONDecodeError as exc:
    raise RuntimeError(f"invalid benchmark JSON: {lines[0]!r}") from exc
  return validate_result(result, mode, variant, writers)


def emit(result: dict[str, Any], seed: int, repetition: int, ordinal: int) -> None:
  result.update({"repetition": repetition, "ordinal": ordinal, "seed": seed})
  print(json.dumps(result, sort_keys=True), flush=True)


def main() -> int:
  args = parse_args()
  # subprocess with a bare "bench_queue" searches PATH rather than the current
  # directory. Resolve here so the documented `./bench_queue` reliably names
  # this exact binary for every child invocation.
  benchmark = args.benchmark.resolve()
  if not benchmark.is_file():
    raise SystemExit(f"benchmark is not a file: {benchmark}")
  if args.seconds <= 0 or args.runs <= 0:
    raise SystemExit("--seconds and --runs must be positive")
  if len(set(args.variants)) != len(args.variants):
    raise SystemExit("--variants must not repeat a selector")

  rng = random.Random(args.seed)
  # spsc has exactly one writer by construction, so pairing it with the
  # multi-writer counts would just be a usage error repeated N times.
  configs = [(v, w) for v, w in itertools.product(args.variants, WRITERS)
       if v != "spsc" or w == 1]
  for repetition in range(args.runs):
    order = configs.copy()
    rng.shuffle(order)
    for ordinal, (variant, writers) in enumerate(order, start=1):
      result = invoke(benchmark, "throughput", variant, writers, args.seconds)
      emit(result, args.seed, repetition + 1, ordinal)

  if args.counters:
    # Counters run after the throughput campaign and are explanatory rather
    # than a substitute for it. They were measured as non-perturbing (the
    # PMU is enabled once around the timed window, never per operation), but
    # keeping them a separate pass means a throughput number is never read
    # off a differently-configured run. Order is shuffled and recorded for
    # the same thermal/background-load protection.
    order = configs.copy()
    rng.shuffle(order)
    for ordinal, (variant, writers) in enumerate(order, start=1):
      result = invoke(benchmark, "counters", variant, writers, args.seconds)
      emit(result, args.seed, 1, ordinal)
  return 0


if __name__ == "__main__":
  try:
    raise SystemExit(main())
  except RuntimeError as exc:
    print(f"campaign failed: {exc}", file=sys.stderr)
    raise SystemExit(2)
