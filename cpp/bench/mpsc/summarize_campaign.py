#!/usr/bin/env python3
"""Summarize a run_campaign.py JSON Lines file as medians with IQRs.

The promotion rule this repository uses is deliberately conservative:
OVERLAPPING IQRs MEAN "NO MEASURED DIFFERENCE". This script prints the
interval for every cell and marks each comparison separated / overlapping so a
reader cannot quote a median without its precision.

  python3 cpp/bench/mpsc/summarize_campaign.py results.jsonl --baseline mpsc
"""

from __future__ import annotations

import argparse
import json
import statistics
from collections import defaultdict
from pathlib import Path


def quartiles(values: list[float]) -> tuple[float, float, float]:
  ordered = sorted(values)
  n = len(ordered)
  if n == 1:
    return ordered[0], ordered[0], ordered[0]
  lo = statistics.median(ordered[: n // 2])
  hi = statistics.median(ordered[(n + 1) // 2 :])
  return lo, statistics.median(ordered), hi


def main() -> int:
  parser = argparse.ArgumentParser(description=__doc__)
  parser.add_argument("results", type=Path)
  parser.add_argument("--baseline", default="mpsc", help="variant to compare against")
  args = parser.parse_args()

  rows = [json.loads(line) for line in args.results.read_text().splitlines() if line.strip()]
  rows = [r for r in rows if r.get("mode") == "throughput"]
  grouped: dict[tuple[str, int], list[float]] = defaultdict(list)
  for r in rows:
    grouped[(r["variant"], r["writers"])].append(r["mrec_s"])

  variants = sorted({v for v, _ in grouped})
  writers = sorted({w for _, w in grouped})
  stats = {k: quartiles(v) for k, v in grouped.items()}
  counts = {k: len(v) for k, v in grouped.items()}

  if not grouped:
    print("no throughput rows")
    return 0

  print("n per cell:")
  for (variant, writer_count), values in sorted(grouped.items()):
    print(f"  {variant} w={writer_count}: {len(values)}")
  header = f"{'writers':>8}" + "".join(f"  {v:>34}" for v in variants)
  print(header)
  for w in writers:
    cells = []
    for v in variants:
      if (v, w) not in stats:
        cells.append(f"  {'n/a':>10} [{'':8}, {'':8}]")
        continue
      lo, med, hi = stats[(v, w)]
      cells.append(f"  {med:>10.4f} [{lo:8.4f}, {hi:8.4f}]")
    print(f"{w:>8}" + "".join(cells))

  if args.baseline in variants:
    print()
    print(f"{'writers':>8}  {'variant':>18}  {'median change':>14}  verdict")
    for w in writers:
      if (args.baseline, w) not in stats:
        continue
      blo, bmed, bhi = stats[(args.baseline, w)]
      for v in variants:
        if v == args.baseline:
          continue
        if (v, w) not in stats:
          print(f"{w:>8}  {v:>18}  {'n/a':>14}  not run for this writer count")
          continue
        lo, med, hi = stats[(v, w)]
        change = (med / bmed - 1.0) * 100.0
        if min(counts[(args.baseline, w)], counts[(v, w)]) < 2:
          verdict = "single sample: smoke only, no interval"
        else:
          sep = lo > bhi or hi < blo
          verdict = "IQRs separated" if sep else "IQRs OVERLAP: no measured difference"
        print(f"{w:>8}  {v:>18}  {change:>+13.2f}%  {verdict}")
  return 0


if __name__ == "__main__":
  raise SystemExit(main())
