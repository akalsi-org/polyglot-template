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
  del result
  gc.collect()

  # The old guard here was `sys.getrefcount(result) <= 3`, which cannot
  # observe the leak it was written for: the intermediates leaked by a
  # VSTEAL-without-release loop hold no reference to the final result, so
  # the count is the same in a leaking and a fixed build. Measure the
  # live-block delta across repeated calls instead -- a per-iteration
  # leak scales with call count, a correct build does not.
  small = [Boxed(index) for index in range(50)]
  for _ in range(20):
    pyfast_test_ext.prefix_join(small, prefix="x=")
  gc.collect()
  before = sys.getallocatedblocks()
  for _ in range(200):
    pyfast_test_ext.prefix_join(small, prefix="x=")
  gc.collect()
  delta = sys.getallocatedblocks() - before
  # A leak of one intermediate str per item would be ~200 * 49 blocks.
  assert delta < 200, f"prefix_join leaked {delta} blocks over 200 calls"

  assert pyfast_test_ext.prefix_join([]) == ""
  try:
    pyfast_test_ext.prefix_join(())
  except TypeError as error:
    assert "argument 0: expected list, got tuple" in str(error)
  else:
    raise AssertionError("tuple was accepted as item list")


def test_prefix_join_survives_a_list_mutated_by_str() -> None:
  # __str__ runs arbitrary Python and may shrink the list mid-iteration.
  # A snapshotted count plus an unchecked PyList_GET_ITEM read freed
  # memory here (segfault); the item count must be re-read every step and
  # the borrowed item owned across the PyObject_Str call.
  items: list[object] = []

  class Evil:
    def __str__(self) -> str:
      del items[1:]
      return "E"

  items.extend([Evil()] + [object() for _ in range(200)])
  assert pyfast_test_ext.prefix_join(items) == "E"
  assert items == [items[0]]

  # Growing the list mid-iteration must not read past the vector either.
  grown: list[object] = []

  class Grower:
    def __str__(self) -> str:
      if len(grown) < 50:
        grown.append(1)
      return "g"

  grown.extend([Grower()])
  assert set(pyfast_test_ext.prefix_join(grown)) <= {"g", "1"}


def test_unknown_keyword_arguments_are_rejected() -> None:
  calls = [
    (lambda: pyfast_test_ext.greet(name="World", excite=True), "excite"),
    (lambda: pyfast_test_ext.xor_bytes(b"abc", key=1, keys=2), "keys"),
    (lambda: pyfast_test_ext.xor_kwb(data=b"abc", key=1, extra=0), "extra"),
    (lambda: pyfast_test_ext.xor_kwb_buf(data=b"abc", key=1, extra=0), "extra"),
    (lambda: pyfast_test_ext.xor_buffer(b"abc", key=1, extra=0), "extra"),
    (lambda: pyfast_test_ext.prefix_join([], prefixx=""), "prefixx"),
    (lambda: pyfast_test_ext.repeat(b"ab", time=2), "time"),
  ]
  for call, keyword in calls:
    try:
      call()
    except TypeError as error:
      assert f"unexpected keyword argument '{keyword}'" in str(error)
    else:
      raise AssertionError(f"unknown keyword {keyword!r} was accepted")


def test_greet_preserves_embedded_nul_and_rejects_long_names() -> None:
  assert pyfast_test_ext.greet(name="a\x00b") == "Hello, a\x00b."
  assert pyfast_test_ext.greet(name="a\x00b", excited=True) == "Hello, a\x00b!!"

  try:
    pyfast_test_ext.greet(name="x" * 512)
  except OverflowError as error:
    assert "too long" in str(error)
  else:
    raise AssertionError("an oversized name was accepted")


def test_missing_required_keyword_data() -> None:
  for function in [pyfast_test_ext.xor_kwb, pyfast_test_ext.xor_kwb_buf]:
    try:
      function(key=1)
    except TypeError as error:
      assert "missing required keyword argument 'data'" in str(error)
    else:
      raise AssertionError("missing data keyword was accepted")


def test_repeat_rejects_results_that_would_overflow() -> None:
  # Large enough to trip the len * times overflow guard, small enough to
  # still convert to a C long (10**100 exercises the conversion instead).
  try:
    pyfast_test_ext.repeat(b"ab", times=2**62)
  except OverflowError as error:
    assert "too large" in str(error)
  else:
    raise AssertionError("an overflowing repeat count was accepted")


def test_range_rejects_an_unbounded_stop() -> None:
  try:
    pyfast_test_ext.range(10**9)
  except ValueError as error:
    assert "1000000" in str(error)
  else:
    raise AssertionError("an unbounded range was accepted")


def test_point_is_not_a_method_descriptor() -> None:
  # Py_TPFLAGS_METHOD_DESCRIPTOR on a value type makes CPython treat a
  # Point held as a class attribute like an unbound method and prepend
  # the owner instance to the call.
  point = pyfast_test_ext.Point(1.0, 1.0)

  class Holder:
    attr = point

  holder = Holder()
  assert holder.attr is point
  translated = holder.attr(2.0, 3.0)
  assert isinstance(translated, pyfast_test_ext.Point)
  assert abs(translated.dist() - (3.0**2 + 4.0**2) ** 0.5) < 1e-9


def test_point_keyword_only_methods_reject_positional_arguments() -> None:
  point = pyfast_test_ext.Point(1.0, 2.0)
  for call in [lambda: point.move(100.0, 200.0), lambda: point.dist(1.0)]:
    try:
      call()
    except TypeError as error:
      assert "expected 0 arguments" in str(error)
    else:
      raise AssertionError("keyword-only Point method accepted positional arguments")
  # The discarded-positional bug made move() a silent no-op.
  assert abs(point.dist() - (5.0**0.5)) < 1e-9

  try:
    point.move(dx=1.0, dz=2.0)
  except TypeError as error:
    assert "unexpected keyword argument 'dz'" in str(error)
  else:
    raise AssertionError("Point.move accepted an unknown keyword")


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
