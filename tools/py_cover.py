#!/usr/bin/env python3
"""Repo-local PEP 669 (sys.monitoring) line-coverage collector for py_test.

Runs the repo-local test runner in-process so sys.monitoring's LINE event
can observe every executed Python source line belonging to the roots under
measurement, then emits lcov directly. This repo's Python lane is
stdlib-only (no pip - see rules/python.bzl's module docstring), so this
stands in for coverage.py.

Only files under one of --root (or one of --rewrite's OLD sides - see
below) are recorded; files under --exclude (an absolute-or-relative prefix,
matched the same way as --root) are skipped even if nested under a --root.
Output paths in the emitted lcov are relative to the current working
directory (buck2 test actions run with cwd = project root, so this yields
repo-relative SF: paths) UNLESS a --rewrite OLD=NEW applies, in which case
the OLD prefix is swapped for NEW first.

--rewrite exists because rules/python.bzl's py_extension (e.g.
python/lib/fastbytes) stages a COPY of its package (__init__.py + the
compiled extension) under a build-output directory for use as a PYTHONPATH
root - so at runtime, the file sys.monitoring actually observes executing
is that build-output copy, not the original python/lib/fastbytes/__init__.py
source. rules/python.bzl passes one `--rewrite <pkg build dir>=python/lib`
per py_extension dep root so the emitted lcov still reports the real
repo-relative source path.

Usage:
  py_cover.py --root DIR [--root DIR ...] [--exclude DIR ...] \
      [--rewrite OLD=NEW ...] --out OUT.lcov --runner RUNNER \
      -- discover -s python/test -p 'test_*.py'
"""
import argparse
import dis
import importlib.util
import os
import sys


def _parse_args(argv):
  roots = []
  excludes = []
  rewrites = []
  out_path = None
  runner_path = None
  i = 0
  while i < len(argv):
    arg = argv[i]
    if arg == "--root":
      i += 1
      roots.append(os.path.abspath(argv[i]))
    elif arg == "--exclude":
      i += 1
      excludes.append(os.path.abspath(argv[i]))
    elif arg == "--rewrite":
      i += 1
      old, new = argv[i].split("=", 1)
      rewrites.append((os.path.abspath(old), new))
    elif arg == "--out":
      i += 1
      out_path = argv[i]
    elif arg == "--runner":
      i += 1
      runner_path = argv[i]
    elif arg == "--":
      i += 1
      break
    else:
      sys.exit("py_cover.py: unknown arg %r" % (arg,))
    i += 1
  if out_path is None:
    sys.exit("py_cover.py: --out is required")
  if runner_path is None:
    sys.exit("py_cover.py: --runner is required")
  # Every --rewrite OLD side is implicitly also a --root: it names a
  # directory whose files should be recorded, just under a different
  # reported path.
  roots = roots + [old for old, _new in rewrites]
  return roots, excludes, rewrites, out_path, runner_path, argv[i:]


def _make_scope_check(roots, excludes):
  cache = {}

  def in_scope(path):
    cached = cache.get(path)
    if cached is not None:
      return cached
    ap = os.path.abspath(path)
    included = any(ap == r or ap.startswith(r + os.sep) for r in roots)
    decision = included and not any(ap == e or ap.startswith(e + os.sep) for e in excludes)
    cache[path] = decision
    return decision

  return in_scope


def _load_runner(path):
  spec = importlib.util.spec_from_file_location("repo_py_test_runner", path)
  if spec is None or spec.loader is None:
    raise RuntimeError("could not load test runner %r" % (path,))
  module = importlib.util.module_from_spec(spec)
  spec.loader.exec_module(module)
  return module


def _run_with_monitoring(runner_path, runner_argv, in_scope):
  hits = {}
  executable = {}
  seen_codes = set()
  tool_id = sys.monitoring.COVERAGE_ID
  sys.monitoring.use_tool_id(tool_id, "py_cover")

  def remember_executable(code):
    # The module's constants contain function, class, and comprehension code
    # objects even when their bodies are never called, so walk them all rather
    # than treating only observed LINE callbacks as executable source lines.
    if id(code) in seen_codes:
      return
    seen_codes.add(id(code))
    path = code.co_filename
    if in_scope(path):
      ap = os.path.abspath(path)
      lines = executable.setdefault(ap, set())
      lines.update(
        line
        for _offset, line in dis.findlinestarts(code)
        if isinstance(line, int) and line > 0
      )
    for constant in code.co_consts:
      if isinstance(constant, type(code)):
        remember_executable(constant)

  def on_line(code, line_number):
    remember_executable(code)
    path = code.co_filename
    if not in_scope(path):
      return sys.monitoring.DISABLE
    ap = os.path.abspath(path)
    file_hits = hits.setdefault(ap, {})
    file_hits[line_number] = file_hits.get(line_number, 0) + 1
    return None

  sys.monitoring.register_callback(tool_id, sys.monitoring.events.LINE, on_line)
  sys.monitoring.set_events(tool_id, sys.monitoring.events.LINE)
  try:
    parser = argparse.ArgumentParser(add_help = False)
    parser.add_argument("command")
    parser.add_argument("-s", "--start", default = "python/test")
    parser.add_argument("-p", "--pattern", default = "test_*.py")
    parser.add_argument("-f", "--failfast", action = "store_true")
    args = parser.parse_args(runner_argv)
    if args.command != "discover":
      raise RuntimeError("py_cover.py only supports runner discovery")
    runner = _load_runner(runner_path)
    result = runner.run_discovery(args.start, args.pattern, stream = sys.stderr, failfast = args.failfast)
    if not result.wasSuccessful():
      raise RuntimeError("covered test run failed")
  finally:
    sys.monitoring.set_events(tool_id, 0)
    sys.monitoring.register_callback(tool_id, sys.monitoring.events.LINE, None)
    sys.monitoring.free_tool_id(tool_id)
  return hits, executable


def _rewrite_path(ap, rewrites, cwd):
  for old, new in rewrites:
    if ap == old or ap.startswith(old + os.sep):
      return new + ap[len(old):]
  return os.path.relpath(ap, cwd)


def _write_lcov(hits, executable, rewrites, out_path):
  cwd = os.getcwd()
  lines_out = []
  files = sorted(set(hits) | set(executable))
  for ap in files:
    rel = _rewrite_path(ap, rewrites, cwd)
    file_hits = hits.get(ap, {})
    lines_out.append("SF:%s" % rel)
    found = 0
    covered = 0
    for line in sorted(set(file_hits) | executable.get(ap, set())):
      count = file_hits.get(line, 0)
      lines_out.append("DA:%d,%d" % (line, count))
      found += 1
      if count > 0:
        covered += 1
    lines_out.append("LF:%d" % found)
    lines_out.append("LH:%d" % covered)
    lines_out.append("end_of_record")
  os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
  with open(out_path, "w") as f:
    f.write("\n".join(lines_out))
    if lines_out:
      f.write("\n")


def main(argv):
  roots, excludes, rewrites, out_path, runner_path, rest = _parse_args(argv)
  in_scope = _make_scope_check(roots, excludes)
  hits, executable = _run_with_monitoring(runner_path, rest, in_scope)
  _write_lcov(hits, executable, rewrites, out_path)


if __name__ == "__main__":
  main(sys.argv[1:])
