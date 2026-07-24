"""Lightweight Python test helpers: parametrization, skips, mocks, and smart TestContext."""

from __future__ import annotations

import contextlib
import inspect
import tempfile
import unittest
import unittest.mock as mock
from unittest.mock import (
    ANY,
    DEFAULT,
    AsyncMock,
    MagicMock,
    Mock,
    PropertyMock,
    call,
    patch,
)
from pathlib import Path
from typing import Any, Callable, Sequence, TypeVar

T = TypeVar("T")

# Re-export unittest SkipTest
SkipTest = unittest.SkipTest


class TestContext(contextlib.ExitStack):
  """Execution context and resource stack for test functions.

  Provides explicit, zero-magic resource management and mocking hooks.
  All entered contexts, mocks, temporary directories, and cleanup callbacks
  are automatically torn down in reverse order when the test finishes.
  """

  def add_cleanup(self, item: Any, *args: Any, **kwargs: Any) -> Any:
    """Smart cleanup and resource registration.

    - If item has __enter__ and __exit__ (context manager), automatically calls
      __enter__() and registers __exit__() for automatic teardown.
    - If item is a callable, registers it to be called on test teardown.
    - If item has a close() method, registers item.close() for teardown.
    """
    if hasattr(item, "__enter__") and hasattr(item, "__exit__"):
      return self.enter_context(item)
    if callable(item):
      return self.callback(item, *args, **kwargs)
    if hasattr(item, "close") and callable(getattr(item, "close")):
      return self.callback(getattr(item, "close"))
    raise TypeError(f"Cannot register cleanup for object of type {type(item).__name__}")

  def enter(self, item: Any, *args: Any, **kwargs: Any) -> Any:
    """Alias for add_cleanup."""
    return self.add_cleanup(item, *args, **kwargs)

  def mock(self, target: str, new: Any = DEFAULT, **kwargs: Any) -> Any:
    """Patch a target string and return the mock object.

    Automatically unpatches when the test completes.
    """
    return self.enter_context(patch(target, new=new, **kwargs))

  def mock_object(self, target: Any, attribute: str, new: Any = DEFAULT, **kwargs: Any) -> Any:
    """Patch an object attribute and return the mock object.

    Automatically unpatches when the test completes.
    """
    return self.enter_context(patch.object(target, attribute, new=new, **kwargs))

  def mock_dict(self, in_dict: Any, values: Any = (), clear: bool = False) -> Any:
    """Patch a dictionary and restore original values on test teardown."""
    return self.enter_context(patch.dict(in_dict, values=values, clear=clear))

  def temp_dir(self) -> Path:
    """Create a temporary directory that is automatically deleted on test teardown."""
    tmp = tempfile.TemporaryDirectory()
    self.enter_context(tmp)
    return Path(tmp.name)


def parametrize(cases: Sequence[Any], names: Sequence[str] | None = None) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
  """Decorator to run a test function across multiple parameter sets.

  Usage:
      @parametrize([
          (1, 2, 3),
          (4, 5, 9),
      ])
      def test_add(a, b, expected):
          assert a + b == expected
  """
  def decorator(fn: Callable[..., Any]) -> Callable[..., Any]:
    setattr(fn, "__parametrize_cases__", (cases, names))
    return fn
  return decorator


def xfail(reason: str) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
  """Mark a test as expected to fail for the supplied reason.

  A failing xfail test is reported as XFAIL and does not fail the suite. If it
  succeeds instead, it is reported as XPASS and fails the suite.
  """
  def decorator(fn: Callable[..., Any]) -> Callable[..., Any]:
    setattr(fn, "__unittest_expecting_failure__", True)
    setattr(fn, "__xfail_reason__", reason)
    return fn
  return decorator


def skip(reason: str) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
  """Unconditionally skip a test function."""
  def decorator(fn: Callable[..., Any]) -> Callable[..., Any]:
    setattr(fn, "__unittest_skip__", True)
    setattr(fn, "__unittest_skip_why__", reason)
    return fn
  return decorator


def skip_if(condition: bool, reason: str) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
  """Skip a test function if condition evaluates to True."""
  def decorator(fn: Callable[..., Any]) -> Callable[..., Any]:
    if condition:
      setattr(fn, "__unittest_skip__", True)
      setattr(fn, "__unittest_skip_why__", reason)
    return fn
  return decorator


__all__ = [
    "ANY",
    "DEFAULT",
    "AsyncMock",
    "MagicMock",
    "Mock",
    "PropertyMock",
    "SkipTest",
    "TestContext",
    "call",
    "mock",
    "patch",
    "parametrize",
    "skip",
    "xfail",
    "skip_if",
]
