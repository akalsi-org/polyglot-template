import sys
import tempfile
from test_helpers import (
    MagicMock,
    TestContext,
    parametrize,
    skip,
    skip_if,
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


def test_smart_add_cleanup(ctx: TestContext) -> None:
  # 1. Smart add_cleanup with a ContextManager (__enter__ / __exit__)
  td = ctx.add_cleanup(tempfile.TemporaryDirectory())
  assert isinstance(td, str) and len(td) > 0

  # 2. Smart add_cleanup with a closeable object
  closeable = DummyCloseable()
  ctx.add_cleanup(closeable)
  assert not closeable.closed

  # 3. Smart add_cleanup with a callback function
  cleaned = []
  ctx.add_cleanup(lambda: cleaned.append(True))
  assert not cleaned


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
