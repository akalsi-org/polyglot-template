#!/usr/bin/env python3
"""Normalizes every lane's raw coverage artifact into one merged lcov file
plus a small per-file % summary.txt.

Invoked by rules/coverage.bzl's coverage_report rule (//:coverage) as
`python3 coverage_merge.py <manifest.json> <out.lcov> <out summary.txt>`.
The manifest is a JSON list of per-test-target entries; see
rules/coverage.bzl's CoverageInfo provider / _entry_for() for the exact
shape each `kind` carries:

  cxx_gcov:     primary = dir of raw .gcda counters (GCOV_PREFIX output),
                tool = pinned gcov binary, gcnos = list of .gcno paths
                sitting next to the objects linked into that test binary.
                gcov -j decodes each (gcno, gcda) pair to JSON; see
                https://gcc.gnu.org/onlinedocs/gcc/Invoking-Gcov.html.
  go_profile:   primary = a `go test -coverprofile` text file (the "simple"
                format: "mode: ...\\n" then "file:startLine.col,endLine.col
                numStmt count" lines) - parsed directly, no converter.
  deno:         primary = the dir `deno test --coverage=<dir>` wrote,
                containing an already-lcov lcov.info with ABSOLUTE SF: paths
                rooted at that action's own ephemeral staging directory
                (named "proj" - see rules/deno.bzl's module docstring); each
                SF: path is rewritten to whatever follows its last "/proj/"
                segment, which is exactly the repo-relative path since
                deno_test always stages sources at their real repo-relative
                paths underneath "proj".
  python_lcov:  primary = an already-lcov file (tools/py_cover.py's output,
                repo-relative paths already, no rewriting needed).

Every lane's raw format ultimately reduces to one shared in-memory shape:
counts[repo_relative_path][line_number] = hit_count, built up across every
manifest entry (so a source file exercised by more than one test - e.g.
cpp/lib/example/example.cc via both example_test and example_edge_test -
gets its coverage merged, not just overwritten). Counts from independent
runs are merged with max() rather than summed: this repo only ever reports
lines-hit / lines-found percentages (never raw hit magnitudes), and max()
avoids implying a false total from adding independent processes' counts.
"""
import gzip
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile


def _merge_counts(counts, file, line, hit):
  file_counts = counts.setdefault(file, {})
  file_counts[line] = max(file_counts.get(line, 0), hit)


def _process_cxx_gcov(entry, counts, scratch_root):
  gcda_dir = entry["primary"]
  gcov_bin = os.path.abspath(entry["tool"])
  for gcno in entry["gcnos"]:
    if not gcno.endswith(".gcno"):
      continue
    gcda_rel = gcno[:-len(".gcno")] + ".gcda"
    gcda_path = os.path.join(gcda_dir, gcda_rel)
    if not os.path.exists(gcda_path):
      # The object was compiled+linked into this test binary but the
      # binary was never actually executed as part of collecting this
      # entry (shouldn't happen given rules/cxx.bzl always runs the test
      # binary to produce gcda_dir - guarded defensively anyway).
      continue
    work = tempfile.mkdtemp(dir = scratch_root)
    try:
      base = os.path.basename(gcno)[:-len(".gcno")]
      shutil.copy(gcno, os.path.join(work, base + ".gcno"))
      shutil.copy(gcda_path, os.path.join(work, base + ".gcda"))
      # gcov resolves the .gcda location from the path recorded inside the
      # .gcno at compile time, ignoring any argument path we pass here -
      # co-locating a same-basename .gcno/.gcda pair in one scratch dir and
      # invoking gcov there (empirically validated) is what makes it find
      # the actual counters instead of reporting "not executed".
      # gcov's stderr is captured rather than discarded: a pinned-toolchain
      # version skew ("version 'B16 ' prefers 'A16 '") is reported there, and
      # discarding it surfaced only as a bare CalledProcessError with no clue
      # which pair failed or why.
      result = subprocess.run(
        [gcov_bin, "-j", base + ".gcda"],
        cwd = work,
        check = False,
        stdout = subprocess.DEVNULL,
        stderr = subprocess.PIPE,
        text = True,
      )
      if result.returncode != 0:
        raise SystemExit(
          "coverage merge: gcov failed (exit %d) for %s in %s:\n%s"
          % (result.returncode, base, gcda_dir, result.stderr.strip())
        )
      for name in os.listdir(work):
        if not name.endswith(".gcov.json.gz"):
          continue
        with gzip.open(os.path.join(work, name), "rt") as f:
          data = json.load(f)
        for file_entry in data["files"]:
          path = file_entry["file"]
          if path.startswith("/") or path.startswith("buck-out/"):
            # Toolchain/system headers get dropped two ways: gcc's own
            # implicit search dirs (libstdc++, ...) yield ABSOLUTE paths,
            # while the vendored doctest.h under the extracted toolchain
            # (compiled via a relative -I into buck-out - see
            # rules/cxx.bzl's cxx_test/_doctest_include) yields a
            # buck-out-RELATIVE path that is technically "repo-relative"
            # text but not this repo's own source and not stable (it
            # embeds a config-hash-bearing buck-out path). Neither belongs
            # in "every path in the merged lcov is repo source" (see this
            # module's docstring), so both prefixes are dropped here. This
            # repo's own sources are always compiled with a plain
            # repo-relative src arg (e.g. "cpp/lib/example/example.cc"),
            # which starts with neither prefix.
            continue
          for line in file_entry["lines"]:
            _merge_counts(counts, path, line["line_number"], line.get("count", 0))
    finally:
      shutil.rmtree(work, ignore_errors = True)


_GO_LINE_RE = re.compile(r"^(\S+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$")

_go_module_prefix_cache = [None]  # single-element list used as a lazy-init cell


def _go_module_prefix():
  # go test -coverprofile paths are Go IMPORT paths ("<module>/go/lib/
  # greeting/greeting.go"), not filesystem paths - go.mod's own module line
  # is this repo's single source of truth for that prefix (go/BUCK's
  # go_binary/go_test/go_library targets never restate it - see
  # rules/go.bzl), so it is read from go.mod here rather than hardcoded, to
  # track go.mod if it ever changes. This script always runs with cwd =
  # project root (see this module's docstring on every action here doing
  # so), so a plain relative "go.mod" read is repo-relative by construction.
  if _go_module_prefix_cache[0] is None:
    prefix = ""
    try:
      with open("go.mod") as f:
        for line in f:
          line = line.strip()
          if line.startswith("module "):
            prefix = line[len("module "):].strip() + "/"
            break
    except OSError:
      pass
    _go_module_prefix_cache[0] = prefix
  return _go_module_prefix_cache[0]


def _process_go_profile(entry, counts):
  prefix = _go_module_prefix()
  with open(entry["primary"]) as f:
    lines = f.read().splitlines()
  for line in lines[1:]:  # lines[0] is the "mode: ..." header
    match = _GO_LINE_RE.match(line)
    if not match:
      continue
    file, start_line, _start_col, end_line, _end_col, _num_stmt, count = match.groups()
    if prefix and file.startswith(prefix):
      file = file[len(prefix):]
    count = int(count)
    # go's profile is block-ranged, not per-line; approximating every line
    # in [start_line, end_line] as hit iff the block was hit is the same
    # tradeoff repo.sh's own tooling accepts elsewhere for "simple, direct
    # parsing" - see rules/go.bzl's module docstring on this rule.
    for line_no in range(int(start_line), int(end_line) + 1):
      _merge_counts(counts, file, line_no, count)


def _merge_lcov_file(path, counts, rewrite_proj_prefix):
  current_file = None
  with open(path) as f:
    for raw_line in f:
      line = raw_line.rstrip("\n")
      if line.startswith("SF:"):
        current_file = line[len("SF:"):]
        if rewrite_proj_prefix:
          # FIRST match, not rfind: the staged layout is
          # <mktemp-dir>/proj/<repo-relative-path>, and mktemp's dir name
          # never contains "/proj/" - but the repo-relative tail could
          # (a source dir literally named proj/), and slicing at the LAST
          # marker would then attribute coverage to a nonexistent file.
          marker = "/proj/"
          idx = current_file.find(marker)
          if idx != -1:
            current_file = current_file[idx + len(marker):]
      elif line.startswith("DA:") and current_file:
        line_no_str, count_str = line[len("DA:"):].split(",")[:2]
        _merge_counts(counts, current_file, int(line_no_str), int(count_str))
      elif line == "end_of_record":
        current_file = None


def _process_deno(entry, counts):
  # Fail CLOSED on a missing lcov.info: the deno lane relies on `deno test
  # --coverage=<dir>` auto-writing it (see rules/deno.bzl); if a future
  # deno bump stops doing that, silently skipping here would zero out the
  # whole lane's coverage with every report still "green".
  lcov_path = os.path.join(entry["primary"], "lcov.info")
  if not os.path.exists(lcov_path):
    raise SystemExit(
      f"coverage merge: {entry['name']}: expected {lcov_path} to exist - "
      "deno stopped emitting lcov.info; update the deno coverage collection"
    )
  _merge_lcov_file(lcov_path, counts, rewrite_proj_prefix = True)


def _process_python_lcov(entry, counts):
  _merge_lcov_file(entry["primary"], counts, rewrite_proj_prefix = False)


def _write_lcov(counts, out_path):
  lines = []
  for file in sorted(counts):
    file_hits = counts[file]
    lines.append("SF:%s" % file)
    found = 0
    hit = 0
    for line_no in sorted(file_hits):
      count = file_hits[line_no]
      lines.append("DA:%d,%d" % (line_no, count))
      found += 1
      if count > 0:
        hit += 1
    lines.append("LF:%d" % found)
    lines.append("LH:%d" % hit)
    lines.append("end_of_record")
  with open(out_path, "w") as f:
    f.write("\n".join(lines))
    if lines:
      f.write("\n")


_TOTAL_RE = re.compile(r"^TOTAL\s+(\d+)\s+(\d+)\s+([0-9.]+)%$")


def read_summary_total(path):
  """Returns the TOTAL percentage recorded in a summary.txt written here."""
  with open(path) as f:
    for line in f:
      match = _TOTAL_RE.match(line.strip())
      if match:
        return float(match.group(3))
  raise SystemExit("coverage merge: %s has no TOTAL line" % (path,))


def _enforce_min_total(total_pct, min_total, source):
  if total_pct + 1e-9 < min_total:
    raise SystemExit(
      "coverage merge: total line coverage %.1f%% is below the required "
      "minimum %.1f%% (%s)" % (total_pct, min_total, source)
    )
  print("coverage: total %.1f%% (minimum %.1f%%)" % (total_pct, min_total))


def _write_summary(counts, out_path):
  header = "%-64s %8s %8s %7s" % ("File", "Lines", "Hit", "Pct")
  rule = "-" * len(header)
  lines = [header, rule]
  total_found = 0
  total_hit = 0
  for file in sorted(counts):
    file_hits = counts[file]
    found = len(file_hits)
    hit = sum(1 for count in file_hits.values() if count > 0)
    pct = (100.0 * hit / found) if found else 0.0
    lines.append("%-64s %8d %8d %6.1f%%" % (file, found, hit, pct))
    total_found += found
    total_hit += hit
  lines.append(rule)
  total_pct = (100.0 * total_hit / total_found) if total_found else 0.0
  lines.append("%-64s %8d %8d %6.1f%%" % ("TOTAL", total_found, total_hit, total_pct))
  with open(out_path, "w") as f:
    f.write("\n".join(lines))
    f.write("\n")
  return total_pct


_USAGE = (
  "usage: coverage_merge.py <manifest.json> <out.lcov> <out summary.txt> "
  "[--min-total PCT]\n"
  "       coverage_merge.py --check-total <summary.txt> --min-total PCT"
)


def _parse_min_total(value):
  try:
    return float(value)
  except ValueError:
    raise SystemExit("coverage_merge.py: --min-total expects a percentage, got %r" % (value,))


def main(argv):
  positional = []
  min_total = None
  check_total = None
  index = 0
  while index < len(argv):
    arg = argv[index]
    if arg == "--min-total":
      index += 1
      if index >= len(argv):
        raise SystemExit("coverage_merge.py: --min-total requires a value\n" + _USAGE)
      min_total = _parse_min_total(argv[index])
    elif arg == "--check-total":
      index += 1
      if index >= len(argv):
        raise SystemExit("coverage_merge.py: --check-total requires a path\n" + _USAGE)
      check_total = argv[index]
    else:
      positional.append(arg)
    index += 1

  if check_total is not None:
    if positional:
      raise SystemExit("coverage_merge.py: --check-total takes no positional arguments\n" + _USAGE)
    if min_total is None:
      raise SystemExit("coverage_merge.py: --check-total requires --min-total\n" + _USAGE)
    _enforce_min_total(read_summary_total(check_total), min_total, check_total)
    return

  # Bare tuple unpacking raised an opaque "not enough values to unpack" here;
  # the caller (rules/coverage.bzl's merge action) deserves the real usage.
  if len(positional) != 3:
    raise SystemExit(
      "coverage_merge.py: expected 3 positional arguments, got %d\n%s"
      % (len(positional), _USAGE)
    )
  manifest_path, out_lcov, out_summary = positional
  with open(manifest_path) as f:
    entries = json.load(f)

  # Repo scratch policy: never /tmp (small tmpfs), always under buck-out.
  # $BUCK_SCRATCH_PATH is populated for ctx.actions.run() build actions
  # (this merge step is one - see rules/coverage.bzl) even though it is NOT
  # populated for ExternalRunnerTestInfo commands (see rules/go.bzl's
  # module docstring on that distinction).
  scratch_root = os.environ.get("BUCK_SCRATCH_PATH")
  if not scratch_root:
    scratch_root = os.path.join("buck-out", "v2", "tmp", "coverage-merge")
  os.makedirs(scratch_root, exist_ok = True)

  counts = {}
  for entry in entries:
    kind = entry["kind"]
    if kind == "cxx_gcov":
      _process_cxx_gcov(entry, counts, scratch_root)
    elif kind == "go_profile":
      _process_go_profile(entry, counts)
    elif kind == "deno":
      _process_deno(entry, counts)
    elif kind == "python_lcov":
      _process_python_lcov(entry, counts)
    else:
      sys.exit("coverage_merge.py: unknown kind %r" % (kind,))

  _write_lcov(counts, out_lcov)
  total_pct = _write_summary(counts, out_summary)
  if min_total is not None:
    _enforce_min_total(total_pct, min_total, out_summary)


if __name__ == "__main__":
  main(sys.argv[1:])
