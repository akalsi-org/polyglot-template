import gc
import sys

import pyfast_test_ext


def _xor(data: bytes, key: int) -> bytes:
  return bytes(value ^ key for value in data)


def test_add_and_range() -> None:
  assert pyfast_test_ext.add(20, 22) == 42
  assert pyfast_test_ext.range(5) == [0, 1, 2, 3, 4]
  assert pyfast_test_ext.range(-2) == []


def test_positional_arity_and_scalar_errors() -> None:
  for args in [(), (1,), (1, 2, 3)]:
    try:
      pyfast_test_ext.add(*args)
    except TypeError as error:
      assert "expected 2 arguments" in str(error)
    else:
      raise AssertionError("add accepted invalid arity")

  for args in [(), (1, 2)]:
    try:
      pyfast_test_ext.range(*args)
    except TypeError as error:
      assert "expected 1 argument" in str(error)
    else:
      raise AssertionError("range accepted invalid arity")

  try:
    pyfast_test_ext.add("left", 1)
  except TypeError as error:
    assert "argument 0: expected int, got str" in str(error)
  else:
    raise AssertionError("add accepted a string")


def test_keyword_bytes_and_buffer_variants() -> None:
  data = b"abc\x00"
  assert pyfast_test_ext.xor_bytes(data, key=1) == _xor(data, 1)
  assert pyfast_test_ext.xor_kwb(data=data, key=2) == _xor(data, 2)
  assert pyfast_test_ext.xor_kwb_buf(data=data, key=3) == _xor(data, 3)
  assert pyfast_test_ext.xor_buffer(data, key=4) == _xor(data, 4)
  assert pyfast_test_ext.xor_buffer(bytearray(data), key=5) == _xor(data, 5)
  assert pyfast_test_ext.xor_buffer(memoryview(data), key=6) == _xor(data, 6)

  try:
    pyfast_test_ext.xor_kwb(data=data)
  except TypeError as error:
    assert "missing required keyword argument 'key'" in str(error)
  else:
    raise AssertionError("missing keyword was accepted")

  try:
    pyfast_test_ext.xor_bytes("abc", key=1)
  except TypeError as error:
    assert "argument 0: expected bytes, got str" in str(error)
  else:
    raise AssertionError("str was accepted as bytes")

  try:
    pyfast_test_ext.xor_buffer(object(), key=1)
  except TypeError as error:
    assert "argument 0: expected bytes-like, got object" in str(error)
  else:
    raise AssertionError("object was accepted as a buffer")

  for function in [pyfast_test_ext.xor_bytes, pyfast_test_ext.xor_buffer]:
    try:
      function(data, key=256)
    except ValueError as error:
      assert "one byte" in str(error)
    else:
      raise AssertionError("out-of-range key was accepted")


def test_keyword_only_fixtures_reject_positional_arguments() -> None:
  calls = [
    lambda: pyfast_test_ext.xor_kwb(b"abc", key=1),
    lambda: pyfast_test_ext.xor_kwb_buf(b"abc", key=1),
    lambda: pyfast_test_ext.greet("World"),
  ]
  for call in calls:
    try:
      call()
    except TypeError as error:
      assert "expected 0 arguments" in str(error)
    else:
      raise AssertionError("keyword-only fixture accepted positional arguments")


def test_keyword_string_and_optional_arguments() -> None:
  assert pyfast_test_ext.greet(name="World") == "Hello, World."
  assert pyfast_test_ext.greet(name="World", excited=True) == "Hello, World!!"
  assert pyfast_test_ext.greet(name="World", excited=False) == "Hello, World."

  try:
    pyfast_test_ext.greet(name=42)
  except TypeError as error:
    assert "keyword 'name': expected str, got int" in str(error)
  else:
    raise AssertionError("integer name was accepted")

  assert pyfast_test_ext.repeat(b"ab") == b"ab"
  assert pyfast_test_ext.repeat(b"ab", times=3) == b"ababab"
  assert pyfast_test_ext.repeat(b"ab", times=0) == b""
  try:
    pyfast_test_ext.repeat(b"ab", times=-1)
  except ValueError as error:
    assert "non-negative" in str(error)
  else:
    raise AssertionError("negative repeat count was accepted")

  try:
    pyfast_test_ext.repeat(b"ab", times=10**100)
  except OverflowError:
    pass
  else:
    raise AssertionError("oversized repeat count was accepted")


def test_prefix_join_and_cleanup_replacement() -> None:
  class Boxed:
    def __init__(self, value: int) -> None:
      self.value = value

    def __str__(self) -> str:
      return str(self.value)

  items = [Boxed(index) for index in range(2000)]
  result = pyfast_test_ext.prefix_join(items, prefix="x=")
  assert result == "x=" + "".join(str(index) for index in range(2000))
  # The runner may retain a temporary reference while reporting a test;
  # assert the result is not pinned by one reference per loop iteration.
  assert sys.getrefcount(result) <= 3
  del result
  gc.collect()

  assert pyfast_test_ext.prefix_join([]) == ""
  try:
    pyfast_test_ext.prefix_join(())
  except TypeError as error:
    assert "argument 0: expected list, got tuple" in str(error)
  else:
    raise AssertionError("tuple was accepted as item list")


def test_point_methods_repr_and_instance_vectorcall() -> None:
  point = pyfast_test_ext.Point(1.0, 2.0)
  assert abs(point.dist() - (5.0**0.5)) < 1e-9
  point.scale(2.0)
  assert abs(point.dist() - 2 * (5.0**0.5)) < 1e-9
  point.move(dx=1.0, dy=-2.0)
  assert abs(point.dist() - (13.0**0.5)) < 1e-9
  point.move()
  assert abs(point.dist() - (13.0**0.5)) < 1e-9
  assert repr(point).startswith("Point(")
  assert str(point).startswith("(") and str(point).endswith(")")

  translated = pyfast_test_ext.Point(0.0, 0.0)(3.0, 4.0)
  assert isinstance(translated, pyfast_test_ext.Point)
  assert abs(translated.dist() - 5.0) < 1e-9

  for call in [lambda: point.scale(), lambda: point(1.0)]:
    try:
      call()
    except TypeError as error:
      assert "expected 1 argument" in str(error) or "expected 2 arguments" in str(error)
    else:
      raise AssertionError("Point accepted invalid arity")

  try:
    point(1.0, dy=2.0)
  except TypeError as error:
    assert "keyword" in str(error)
  else:
    raise AssertionError("Point vectorcall accepted keywords")
