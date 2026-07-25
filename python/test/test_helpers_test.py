import sys
import tempfile
from pathlib import Path
from test_helpers import (
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


@parametrize([
    (2, 3, 5),
    (10, 20, 30),
    (-1, 1, 0),
])
def test_parametrize_tuples(a: int, b: int, expected: int) -> None:
  assert a + b == expected


@parametrize([
    {"name": "Alice", "greeting": "hello, Alice"},
    {"name": "Bob", "greeting": "hello, Bob"},
])
def test_parametrize_dicts(name: str, greeting: str) -> None:
  assert f"hello, {name}" == greeting


class CallableCloseable:
  """Both callable and closeable: pins which branch add_cleanup picks."""

  def __init__(self) -> None:
    self.closed = False
    self.called = False

  def __call__(self) -> None:
    self.called = True

  def close(self) -> None:
    self.closed = True


def test_smart_add_cleanup() -> None:
  # Teardown is what this is about, so run the registrations inside their
  # own context and assert the effects after it exits. Asserting before
  # teardown (the previous shape of this test) only ever observed
  # "nothing has happened yet", which is equally true of a TestContext
  # that runs no cleanups at all.
  cleaned: list[bool] = []
  closeable = DummyCloseable()
  both = CallableCloseable()

  with TestContext() as ctx:
    # 1. Context manager: entered now, exited on teardown.
    td = ctx.add_cleanup(tempfile.TemporaryDirectory())
    assert isinstance(td, str) and Path(td).is_dir()

    # 2. Closeable object: close() registered, item returned.
    assert ctx.add_cleanup(closeable) is closeable

    # 3. Plain callable: invoked on teardown.
    assert not cleaned
    ctx.add_cleanup(lambda: cleaned.append(True))

    # 4. Callable *and* closeable resolves to close(), not the call.
    assert ctx.add_cleanup(both) is both

  assert not Path(td).exists(), "TemporaryDirectory was not exited"
  assert closeable.closed, "close() was never called"
  assert cleaned == [True], "callback was never called"
  assert both.closed and not both.called, "closeable branch must outrank callable"


def test_add_cleanup_returns_mocks_unchanged() -> None:
  # A MagicMock answers hasattr() for every name, so an instance-level
  # __enter__/__exit__ probe used to route it into enter_context and
  # return mock.__enter__() -- an unrelated child mock -- instead of the
  # mock under test.
  mocked = MagicMock()
  with TestContext() as ctx:
    assert ctx.add_cleanup(mocked) is mocked
    mocked.__enter__.assert_not_called()


def test_add_cleanup_rejects_unsupported_objects() -> None:
  with TestContext() as ctx:
    try:
      ctx.add_cleanup(object())
    except TypeError as error:
      assert "Cannot register cleanup" in str(error)
    else:
      raise AssertionError("add_cleanup accepted a non-resource object")


def test_context_mock_and_tempdir(ctx: TestContext) -> None:
  m = ctx.mock("sys.version", "3.14.6-custom-mock")
  assert sys.version == "3.14.6-custom-mock"

  tmp = ctx.temp_dir()
  assert tmp.exists()
  test_file = tmp / "sample.txt"
  test_file.write_text("polyglot-test")
  assert test_file.read_text() == "polyglot-test"


@skip("Demonstrating unconditional skip feature")
def test_demonstrate_skip() -> None:
  assert False, "Should be skipped"


@skip_if(True, "Skipped by condition")
def test_demonstrate_skip_if() -> None:
  assert False, "Should be skipped by condition"


@skip_if(False, "Should not skip")
def test_demonstrate_skip_if_false() -> None:
  assert True


@xfail("Demonstrating an expected failure")
def test_demonstrate_xfail() -> None:
  assert False, "Expected failures are reported without failing the suite"
