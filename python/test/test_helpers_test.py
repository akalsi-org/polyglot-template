import sys
from test_helpers import (
    MagicMock,
    TestContext,
    parametrize,
    skip,
    skip_if,
)


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


def test_context_mock_and_tempdir(ctx: TestContext) -> None:
  m = ctx.mock("sys.version", "3.14.6-custom-mock")
  assert sys.version == "3.14.6-custom-mock"

  tmp = ctx.temp_dir()
  assert tmp.exists()
  test_file = tmp / "sample.txt"
  test_file.write_text("polyglot-test")
  assert test_file.read_text() == "polyglot-test"

  cleaned = []
  ctx.add_cleanup(lambda: cleaned.append(True))
  assert not cleaned


@skip("Demonstrating unconditional skip feature")
def test_demonstrate_skip() -> None:
  assert False, "Should be skipped"


@skip_if(True, "Skipped by condition")
def test_demonstrate_skip_if() -> None:
  assert False, "Should be skipped by condition"


@skip_if(False, "Should not skip")
def test_demonstrate_skip_if_false() -> None:
  assert True
