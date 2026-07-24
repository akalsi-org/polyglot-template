#!/usr/bin/env python3
"""Readable unittest-compatible runner with plain function, async, parametrization, and TestContext support."""

from __future__ import annotations

import argparse
import asyncio
import inspect
import sys
import time
import unittest
from collections.abc import Callable, Sequence
from typing import Any, TextIO

# Import TestContext from python.lib.test_helpers if available or define fallback
try:
  from test_helpers import TestContext
except ImportError:
  import contextlib

  class TestContext(contextlib.ExitStack):  # type: ignore[no-redef]
    pass


class FunctionTest(unittest.TestCase):
  def __init__(
      self,
      function: Callable[..., object],
      args: tuple[Any, ...] = (),
      kwargs: dict[str, Any] | None = None,
      name_suffix: str = "",
  ):
    self._function = function
    self._args = args
    self._kwargs = kwargs or {}
    self._name_suffix = name_suffix
    method = "_run_expected_failure" if getattr(function, "__unittest_expecting_failure__", False) else "run_test"
    super().__init__(method)

  def id(self) -> str:
    base = f"{self._function.__module__}.{self._function.__qualname__}"
    return f"{base}{self._name_suffix}"

  def shortDescription(self) -> str | None:
    return inspect.getdoc(self._function)

  def _run_function(self) -> None:
    if getattr(self._function, "__unittest_skip__", False):
      reason = getattr(self._function, "__unittest_skip_why__", "skipped")
      self.skipTest(reason)

    with TestContext() as ctx:
      sig = inspect.signature(self._function)
      kwargs = dict(self._kwargs)

      # Automatically inject TestContext if requested by name
      if "ctx" in sig.parameters and "ctx" not in kwargs:
        kwargs["ctx"] = ctx
      elif "stack" in sig.parameters and "stack" not in kwargs:
        kwargs["stack"] = ctx

      result = self._function(*self._args, **kwargs)
      if inspect.isawaitable(result):
        asyncio.run(result)

  def run_test(self) -> None:
    self._run_function()

  @unittest.expectedFailure
  def _run_expected_failure(self) -> None:
    self._run_function()


class Loader(unittest.TestLoader):
  def loadTestsFromModule(self, module: Any, *, pattern: str | None = None) -> unittest.TestSuite:
    suite = super().loadTestsFromModule(module, pattern=pattern)
    test_cases: list[unittest.TestCase] = []

    for name, value in vars(module).items():
      if name.startswith("test_") and (inspect.isfunction(value) or inspect.ismethod(value)):
        parametrize_info = getattr(value, "__parametrize_cases__", None)
        if parametrize_info:
          cases, custom_names = parametrize_info
          for idx, case in enumerate(cases):
            if isinstance(case, dict):
              args: tuple[Any, ...] = ()
              kwargs = case
              param_str = ", ".join(f"{k}={v!r}" for k, v in case.items())
              default_label = f"[{param_str}]"
            elif isinstance(case, tuple):
              args = case
              kwargs = {}
              param_str = ", ".join(repr(x) for x in case)
              default_label = f"[({param_str})]"
            elif isinstance(case, list):
              args = tuple(case)
              kwargs = {}
              param_str = ", ".join(repr(x) for x in case)
              default_label = f"[[{param_str}]]"
            else:
              args = (case,)
              kwargs = {}
              default_label = f"[{case!r}]"

            if custom_names and idx < len(custom_names):
              label = f"[{custom_names[idx]}]"
            else:
              label = default_label

            test_cases.append(FunctionTest(value, args=args, kwargs=kwargs, name_suffix=label))
        else:
          test_cases.append(FunctionTest(value))

    if test_cases:
      suite.addTests(test_cases)
    return suite


class Result(unittest.TextTestResult):
  def startTest(self, test: unittest.Testable) -> None:
    self._started_at = time.perf_counter()
    super().startTest(test)

  def _report(self, status: str, test: unittest.Testable, reason: str | None = None) -> None:
    elapsed_ms = (time.perf_counter() - self._started_at) * 1000
    detail = f" [{reason}]" if reason else ""
    self.stream.writeln(f"{status:5} {test.id()}{detail} ({elapsed_ms:.1f} ms)")

  @staticmethod
  def _xfail_reason(test: unittest.Testable) -> str | None:
    function = getattr(test, "_function", None)
    return getattr(function, "__xfail_reason__", None)

  def addSuccess(self, test: unittest.Testable) -> None:
    super().addSuccess(test)
    self._report("PASS", test)

  def addFailure(self, test: unittest.Testable, err: Any) -> None:
    super().addFailure(test, err)
    self._report("FAIL", test)

  def addError(self, test: unittest.Testable, err: Any) -> None:
    super().addError(test, err)
    self._report("ERROR", test)

  def addSkip(self, test: unittest.Testable, reason: str) -> None:
    super().addSkip(test, reason)
    self._report("SKIP", test)

  def addExpectedFailure(self, test: unittest.Testable, err: Any) -> None:
    super().addExpectedFailure(test, err)
    self._report("XFAIL", test, self._xfail_reason(test))

  def addUnexpectedSuccess(self, test: unittest.Testable) -> None:
    super().addUnexpectedSuccess(test)
    self._report("XPASS", test, self._xfail_reason(test))


class Runner(unittest.TextTestRunner):
  resultclass = Result

  def __init__(self, stream: TextIO, *, failfast: bool = False):
    super().__init__(stream=stream, verbosity=0, failfast=failfast)


def run_discovery(start: str, pattern: str, stream: TextIO, *, failfast: bool = False) -> unittest.TestResult:
  suite = Loader().discover(start_dir=start, pattern=pattern)
  return Runner(stream, failfast=failfast).run(suite)


def main(argv: list[str] | None = None) -> int:
  parser = argparse.ArgumentParser()
  subcommands = parser.add_subparsers(dest="command", required=True)
  discover = subcommands.add_parser("discover")
  discover.add_argument("-s", "--start", default="python/test")
  discover.add_argument("-p", "--pattern", default="test_*.py")
  discover.add_argument("-f", "--failfast", action="store_true")
  args = parser.parse_args(argv)
  result = run_discovery(args.start, args.pattern, stream=sys.stderr, failfast=args.failfast)
  return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
  raise SystemExit(main())
