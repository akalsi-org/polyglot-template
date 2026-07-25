"""Tests for python/lib/testlib.py.

These are unittest.TestCase files on purpose, even though testlib exists to
support plain-function tests. The previous version of this file tested the
markers by WEARING them - a `@skip`-decorated function that asserted False,
an `@xfail` one that failed - so it demonstrated the features without
asserting anything about them, and a decorator that silently stopped setting
its attribute would still have produced a green run. What those cases really
exercised was tools/py_test_runner.py's handling of the markers, and that is
covered directly by test/test_py_test_runner.py, which runs discovery over a
generated module and inspects the resulting TestResult.

So this file asserts testlib's own contract: which attributes the decorators
attach, and what TestContext registers, returns, and tears down.
"""

import sys
import tempfile
import unittest
from pathlib import Path

from testlib import (
    MagicMock,
    TestContext,
    parametrize,
    skip,
    skip_if,
    xfail,
)


class DummyCloseable:
  def __init__(self) -> None:
    self.closed = False

  def close(self) -> None:
    self.closed = True


class CallableCloseable:
  """Both callable and closeable: pins which branch add_cleanup picks."""

  def __init__(self) -> None:
    self.closed = False
    self.called = False

  def __call__(self) -> None:
    self.called = True

  def close(self) -> None:
    self.closed = True


class AddCleanupTest(unittest.TestCase):
  def test_context_manager_is_entered_now_and_exited_on_teardown(self) -> None:
    # Teardown is the whole point, so assert AFTER the context exits.
    # Asserting inside it only ever observes "nothing has happened yet",
    # which is equally true of a TestContext that runs no cleanups at all.
    with TestContext() as ctx:
      directory = ctx.add_cleanup(tempfile.TemporaryDirectory())
      self.assertIsInstance(directory, str)
      self.assertTrue(Path(directory).is_dir())
    self.assertFalse(Path(directory).exists(), "TemporaryDirectory was not exited")

  def test_closeable_is_closed_and_returned_unchanged(self) -> None:
    closeable = DummyCloseable()
    with TestContext() as ctx:
      self.assertIs(ctx.add_cleanup(closeable), closeable)
      self.assertFalse(closeable.closed)
    self.assertTrue(closeable.closed, "close() was never called")

  def test_plain_callable_runs_on_teardown(self) -> None:
    calls: list[bool] = []
    with TestContext() as ctx:
      ctx.add_cleanup(lambda: calls.append(True))
      self.assertEqual(calls, [])
    self.assertEqual(calls, [True], "callback was never called")

  def test_callable_arguments_are_forwarded(self) -> None:
    calls: list[tuple[int, str]] = []
    with TestContext() as ctx:
      ctx.add_cleanup(lambda number, label: calls.append((number, label)), 7, label="x")
    self.assertEqual(calls, [(7, "x")])

  def test_closeable_branch_outranks_callable(self) -> None:
    both = CallableCloseable()
    with TestContext() as ctx:
      self.assertIs(ctx.add_cleanup(both), both)
    self.assertTrue(both.closed)
    self.assertFalse(both.called, "closeable branch must outrank callable")

  def test_mocks_are_returned_unchanged(self) -> None:
    # A MagicMock answers hasattr() for every name, so an instance-level
    # __enter__/__exit__ probe used to route it into enter_context and return
    # mock.__enter__() - an unrelated child mock - instead of the mock under
    # test.
    mocked = MagicMock()
    with TestContext() as ctx:
      self.assertIs(ctx.add_cleanup(mocked), mocked)
      mocked.__enter__.assert_not_called()

  def test_unsupported_object_raises_type_error(self) -> None:
    with TestContext() as ctx:
      with self.assertRaisesRegex(TypeError, "Cannot register cleanup"):
        ctx.add_cleanup(object())

  def test_enter_is_an_alias_for_add_cleanup(self) -> None:
    closeable = DummyCloseable()
    with TestContext() as ctx:
      self.assertIs(ctx.enter(closeable), closeable)
    self.assertTrue(closeable.closed)


class MockingTest(unittest.TestCase):
  def test_mock_patches_a_target_and_restores_it(self) -> None:
    original = sys.version
    with TestContext() as ctx:
      ctx.mock("sys.version", "3.14.6-custom-mock")
      self.assertEqual(sys.version, "3.14.6-custom-mock")
    self.assertEqual(sys.version, original, "patch outlived the TestContext")

  def test_mock_object_patches_an_attribute_and_restores_it(self) -> None:
    subject = DummyCloseable()
    with TestContext() as ctx:
      ctx.mock_object(subject, "closed", True)
      self.assertTrue(subject.closed)
    self.assertFalse(subject.closed)

  def test_mock_dict_restores_original_contents(self) -> None:
    mapping = {"keep": 1}
    with TestContext() as ctx:
      ctx.mock_dict(mapping, {"added": 2})
      self.assertEqual(mapping, {"keep": 1, "added": 2})
    self.assertEqual(mapping, {"keep": 1})

  def test_temp_dir_is_usable_and_removed_on_teardown(self) -> None:
    with TestContext() as ctx:
      temp = ctx.temp_dir()
      self.assertTrue(temp.exists())
      sample = temp / "sample.txt"
      sample.write_text("polyglot-test")
      self.assertEqual(sample.read_text(), "polyglot-test")
    self.assertFalse(temp.exists())


class MarkerTest(unittest.TestCase):
  """The attributes tools/py_test_runner.py reads.

  These names ARE the contract between the two files, so they are asserted
  literally: renaming one without the other would otherwise silently stop
  skipping tests, with no failure anywhere to say so.
  """

  def test_parametrize_records_tuple_cases(self) -> None:
    cases = [(2, 3, 5), (10, 20, 30)]

    @parametrize(cases)
    def subject(a: int, b: int, expected: int) -> None:
      pass

    self.assertEqual(subject.__parametrize_cases__, (cases, None))

  def test_parametrize_records_dict_cases_and_explicit_names(self) -> None:
    cases = [{"name": "Alice"}, {"name": "Bob"}]

    @parametrize(cases, names=["alice", "bob"])
    def subject(name: str) -> None:
      pass

    self.assertEqual(subject.__parametrize_cases__, (cases, ["alice", "bob"]))

  def test_skip_marks_unconditionally(self) -> None:
    @skip("not applicable")
    def subject() -> None:
      pass

    self.assertTrue(subject.__unittest_skip__)
    self.assertEqual(subject.__unittest_skip_why__, "not applicable")

  def test_skip_if_marks_only_when_the_condition_holds(self) -> None:
    @skip_if(True, "skipped by condition")
    def skipped() -> None:
      pass

    @skip_if(False, "should not skip")
    def kept() -> None:
      pass

    self.assertTrue(skipped.__unittest_skip__)
    self.assertEqual(skipped.__unittest_skip_why__, "skipped by condition")
    self.assertFalse(hasattr(kept, "__unittest_skip__"))

  def test_xfail_marks_expected_failure_with_a_reason(self) -> None:
    @xfail("known failure")
    def subject() -> None:
      pass

    self.assertTrue(subject.__unittest_expecting_failure__)
    self.assertEqual(subject.__xfail_reason__, "known failure")


if __name__ == "__main__":
  unittest.main()
