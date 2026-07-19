#!/usr/bin/env python3
"""Readable unittest-compatible runner with plain function support."""

from __future__ import annotations

import argparse
import asyncio
import inspect
import sys
import time
import unittest
from collections.abc import Callable
from typing import TextIO


class FunctionTest(unittest.TestCase):
  def __init__(self, function: Callable[[], object]):
    super().__init__("run_test")
    self._function = function

  def id(self) -> str:
    return f"{self._function.__module__}.{self._function.__qualname__}"

  def shortDescription(self) -> str | None:
    return inspect.getdoc(self._function)

  def run_test(self) -> None:
    result = self._function()
    if inspect.isawaitable(result):
      asyncio.run(result)


class Loader(unittest.TestLoader):
  def loadTestsFromModule(self, module, *, pattern: str | None = None):
    suite = super().loadTestsFromModule(module, pattern = pattern)
    functions = [
      FunctionTest(value)
      for name, value in vars(module).items()
      if name.startswith("test_") and inspect.isfunction(value)
    ]
    if functions:
      suite.addTests(functions)
    return suite


class Result(unittest.TextTestResult):
  def startTest(self, test) -> None:
    self._started_at = time.perf_counter()
    super().startTest(test)

  def _report(self, status: str, test) -> None:
    elapsed_ms = (time.perf_counter() - self._started_at) * 1000
    self.stream.writeln(f"{status:5} {test.id()} ({elapsed_ms:.1f} ms)")

  def addSuccess(self, test) -> None:
    super().addSuccess(test)
    self._report("PASS", test)

  def addFailure(self, test, err) -> None:
    super().addFailure(test, err)
    self._report("FAIL", test)

  def addError(self, test, err) -> None:
    super().addError(test, err)
    self._report("ERROR", test)

  def addSkip(self, test, reason: str) -> None:
    super().addSkip(test, reason)
    self._report("SKIP", test)


class Runner(unittest.TextTestRunner):
  resultclass = Result

  def __init__(self, stream: TextIO, *, failfast: bool = False):
    super().__init__(stream = stream, verbosity = 0, failfast = failfast)


def run_discovery(start: str, pattern: str, stream: TextIO, *, failfast: bool = False) -> unittest.TestResult:
  suite = Loader().discover(start_dir = start, pattern = pattern)
  return Runner(stream, failfast = failfast).run(suite)


def main(argv: list[str] | None = None) -> int:
  parser = argparse.ArgumentParser()
  subcommands = parser.add_subparsers(dest = "command", required = True)
  discover = subcommands.add_parser("discover")
  discover.add_argument("-s", "--start", default = "python/test")
  discover.add_argument("-p", "--pattern", default = "test_*.py")
  discover.add_argument("-f", "--failfast", action = "store_true")
  args = parser.parse_args(argv)
  result = run_discovery(args.start, args.pattern, stream = sys.stderr, failfast = args.failfast)
  return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
  raise SystemExit(main())
