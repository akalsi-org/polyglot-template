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
import hashlib
import itertools
import json
import math
import os
import platform
import random
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

VARIANTS = ("mpsc", "mpsc-padded", "spsc")
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
    "--writers", nargs="+", type=int, default=list(WRITERS),
    help="writer counts for MPSC variants; SPSC still runs only at one writer",
  )
  parser.add_argument(
    "--counters", action="store_true",
    help="run one PMU-counter process per configuration after the throughput campaign",
  )
  parser.add_argument(
    "--telemetry",
    type=Path,
    help="optional JSONL file for one-second host samples synchronized to each benchmark process",
  )
  parser.add_argument(
    "--benchmark-cpus",
    help="optional CPU list passed to taskset for each benchmark process, e.g. 0-4",
  )
  parser.add_argument(
    "--telemetry-cpu",
    type=int,
    help="optional CPU for this Python sampler process; must not overlap --benchmark-cpus",
  )
  parser.add_argument(
    "--build-command",
    help="exact command used to build the benchmark binary; copied into metadata",
  )
  parser.add_argument("--compiler", help="compiler identity string; copied into metadata")
  parser.add_argument("--cxxflags", help="compiler flags string; copied into metadata")
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


def read_first_line(path: Path) -> str | None:
  try:
    return path.read_text().splitlines()[0].strip()
  except (FileNotFoundError, IndexError, OSError):
    return None


def allowed_cpus() -> list[int]:
  try:
    return sorted(os.sched_getaffinity(0))
  except AttributeError:
    return []


def parse_cpu_list(text: str | None) -> list[int]:
  if text is None:
    return []
  cpus: set[int] = set()
  for part in text.split(","):
    if "-" in part:
      lo_s, hi_s = part.split("-", 1)
      lo = int(lo_s)
      hi = int(hi_s)
      if hi < lo:
        raise ValueError(f"invalid CPU range: {part}")
      cpus.update(range(lo, hi + 1))
    elif part:
      cpus.add(int(part))
  return sorted(cpus)


def cpu_topology(cpus: list[int]) -> list[dict[str, Any]]:
  rows = []
  for cpu in cpus:
    root = Path(f"/sys/devices/system/cpu/cpu{cpu}/topology")
    freq = Path(f"/sys/devices/system/cpu/cpu{cpu}/cpufreq/scaling_cur_freq")
    rows.append({
      "cpu": cpu,
      "package": read_first_line(root / "physical_package_id"),
      "core": read_first_line(root / "core_id"),
      "thread_siblings": read_first_line(root / "thread_siblings_list"),
      "scaling_cur_freq_khz": read_first_line(freq),
    })
  return rows


def proc_pressure(kind: str) -> dict[str, str] | None:
  path = Path(f"/proc/pressure/{kind}")
  try:
    return {
      parts[0]: " ".join(parts[1:])
      for parts in (line.split() for line in path.read_text().splitlines())
      if parts
    }
  except OSError:
    return None


def proc_status_sample() -> dict[str, Any]:
  sample: dict[str, Any] = {
    "wall_time": time.time(),
    "monotonic_time": time.monotonic(),
  }
  try:
    sample["loadavg"] = Path("/proc/loadavg").read_text().strip()
  except OSError:
    sample["loadavg"] = None
  try:
    meminfo = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
      key, value = line.split(":", 1)
      if key in {"MemAvailable", "Dirty", "Writeback"}:
        meminfo[key] = value.strip()
    sample["meminfo"] = meminfo
  except OSError:
    sample["meminfo"] = None
  stat: dict[str, Any] = {}
  try:
    for line in Path("/proc/stat").read_text().splitlines():
      parts = line.split()
      if parts and parts[0] in {"cpu", "ctxt", "procs_running", "procs_blocked"}:
        stat[parts[0]] = parts[1:]
  except OSError:
    stat = {}
  sample["stat"] = stat
  sample["pressure"] = {"cpu": proc_pressure("cpu"), "memory": proc_pressure("memory")}
  return sample


def proc_cpu_lines(cpus: list[int]) -> dict[str, list[str]]:
  wanted = {f"cpu{cpu}" for cpu in cpus}
  rows: dict[str, list[str]] = {}
  if not wanted:
    return rows
  try:
    for line in Path("/proc/stat").read_text().splitlines():
      parts = line.split()
      if parts and parts[0] in wanted:
        rows[parts[0]] = parts[1:]
  except OSError:
    return {}
  return rows


class Telemetry:
  def __init__(self, path: Path | None):
    self.path = path
    self._lock = threading.Lock()

  def emit(self, row: dict[str, Any]) -> None:
    if self.path is None:
      return
    with self._lock:
      with self.path.open("a") as f:
        print(json.dumps(row, sort_keys=True), file=f, flush=True)

  def run_while(self, context: dict[str, Any], process: subprocess.Popen[str],
                cpus: list[int]) -> None:
    if self.path is None:
      return
    while process.poll() is None:
      row = proc_status_sample()
      row["per_cpu_stat"] = proc_cpu_lines(cpus)
      row.update({"mode": "host_telemetry", **context})
      self.emit(row)
      time.sleep(1.0)


def binary_sha256(binary: Path) -> str:
  h = hashlib.sha256()
  with binary.open("rb") as f:
    for chunk in iter(lambda: f.read(1024 * 1024), b""):
      h.update(chunk)
  return h.hexdigest()


def invoke(binary: Path, mode: str, variant: str, writers: int, seconds: float,
           telemetry: Telemetry, context: dict[str, Any], benchmark_cpu_text: str | None,
           telemetry_cpus: list[int]) -> dict[str, Any]:
  command = [str(binary), mode, variant, str(writers), str(seconds)]
  if benchmark_cpu_text is not None:
    command = ["taskset", "-c", benchmark_cpu_text, *command]
  process = subprocess.Popen(
    command,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
  )
  sampler_cpus = sorted(set(context["benchmark_cpus"]) | set(telemetry_cpus))
  sampler = threading.Thread(
    target=telemetry.run_while, args=(context, process, sampler_cpus), daemon=True
  )
  sampler.start()
  stdout, stderr = process.communicate()
  sampler.join(timeout=2.0)
  completed = subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)
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


def emit(result: dict[str, Any], seed: int, repetition: int, ordinal: int,
         metadata: dict[str, Any]) -> None:
  result.update({
    "repetition": repetition,
    "ordinal": ordinal,
    "seed": seed,
    "binary_sha256": metadata["binary_sha256"],
    "binary": metadata["binary"],
    "affinity_cpus": metadata["affinity_cpus"],
    "benchmark_cpus": metadata["benchmark_cpus"],
    "telemetry_cpus": metadata["telemetry_cpus"],
    "kernel": metadata["kernel"],
    "host": metadata["host"],
    "requested_seconds": metadata["requested_seconds"],
    "build_command": metadata["build_command"],
    "compiler": metadata["compiler"],
    "cxxflags": metadata["cxxflags"],
  })
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
  if len(set(args.writers)) != len(args.writers) or any(w <= 0 for w in args.writers):
    raise SystemExit("--writers must be unique positive integers")
  original_allowed_cpus = allowed_cpus()
  try:
    benchmark_cpus = parse_cpu_list(args.benchmark_cpus)
  except ValueError as exc:
    raise SystemExit(str(exc)) from exc
  if benchmark_cpus and not set(benchmark_cpus).issubset(original_allowed_cpus):
    raise SystemExit("--benchmark-cpus must be a subset of the current process affinity")
  if args.telemetry_cpu is not None and args.telemetry_cpu < 0:
    raise SystemExit("--telemetry-cpu must be non-negative")
  telemetry_cpus = [args.telemetry_cpu] if args.telemetry_cpu is not None else []
  if telemetry_cpus and not set(telemetry_cpus).issubset(original_allowed_cpus):
    raise SystemExit("--telemetry-cpu must be in the current process affinity")
  if telemetry_cpus and not benchmark_cpus:
    raise SystemExit("--telemetry-cpu requires --benchmark-cpus so benchmark children stay pinned")
  if benchmark_cpus and telemetry_cpus and set(benchmark_cpus) & set(telemetry_cpus):
    raise SystemExit("--telemetry-cpu must not overlap --benchmark-cpus")
  if args.telemetry_cpu is not None:
    os.sched_setaffinity(0, {args.telemetry_cpu})

  if args.telemetry is not None:
    args.telemetry.parent.mkdir(parents=True, exist_ok=True)
    args.telemetry.write_text("")
  telemetry = Telemetry(args.telemetry)
  cpus = allowed_cpus()
  metadata = {
    "mode": "campaign_metadata",
    "binary": str(benchmark),
    "binary_sha256": binary_sha256(benchmark),
    "python": sys.version.split()[0],
    "kernel": platform.release(),
    "host": platform.node(),
    "platform": platform.platform(),
    "affinity_cpus": cpus,
    "original_allowed_cpus": original_allowed_cpus,
    "benchmark_cpus": benchmark_cpus,
    "telemetry_cpus": telemetry_cpus,
    "cpu_topology": cpu_topology(original_allowed_cpus),
    "requested_seconds": args.seconds,
    "runs": args.runs,
    "seed": args.seed,
    "variants": args.variants,
    "writer_counts": args.writers,
    "telemetry": str(args.telemetry) if args.telemetry else None,
    "build_command": args.build_command,
    "compiler": args.compiler,
    "cxxflags": args.cxxflags,
  }
  print(json.dumps(metadata, sort_keys=True), flush=True)
  telemetry.emit(metadata)

  rng = random.Random(args.seed)
  # spsc has exactly one writer by construction, so pairing it with the
  # multi-writer counts would just be a usage error repeated N times.
  configs = [(v, w) for v, w in itertools.product(args.variants, args.writers)
       if v != "spsc" or w == 1]
  for repetition in range(args.runs):
    order = configs.copy()
    rng.shuffle(order)
    for ordinal, (variant, writers) in enumerate(order, start=1):
      context = {
        "phase": "throughput",
        "variant": variant,
        "writers": writers,
        "repetition": repetition + 1,
        "ordinal": ordinal,
        "benchmark_cpus": benchmark_cpus,
        "telemetry_cpus": telemetry_cpus,
      }
      result = invoke(
        benchmark, "throughput", variant, writers, args.seconds, telemetry, context,
        args.benchmark_cpus, telemetry_cpus,
      )
      emit(result, args.seed, repetition + 1, ordinal, metadata)

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
      context = {
        "phase": "counters",
        "variant": variant,
        "writers": writers,
        "repetition": 1,
        "ordinal": ordinal,
        "benchmark_cpus": benchmark_cpus,
        "telemetry_cpus": telemetry_cpus,
      }
      result = invoke(
        benchmark, "counters", variant, writers, args.seconds, telemetry, context,
        args.benchmark_cpus, telemetry_cpus,
      )
      emit(result, args.seed, 1, ordinal, metadata)
  return 0


if __name__ == "__main__":
  try:
    raise SystemExit(main())
  except RuntimeError as exc:
    print(f"campaign failed: {exc}", file=sys.stderr)
    raise SystemExit(2)
