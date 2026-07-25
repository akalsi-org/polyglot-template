from fastbytes import xor_bytes


def test_round_trip() -> None:
  value = bytes(range(256))
  assert xor_bytes(xor_bytes(value, 0xA5), 0xA5) == value

def test_rejects_large_key() -> None:
  try:
    xor_bytes(b"value", 256)
  except ValueError:
    return
  raise AssertionError("xor_bytes accepted an out-of-range key")


def test_rejects_keys_outside_the_unsigned_int_range() -> None:
  # The "I" converter wrapped these to 0, 1 and 255 respectively, so the
  # one-byte guard passed and xor_bytes silently used the wrong key.
  for key in [2**32, 2**32 + 1, 2**64, -1, -256]:
    try:
      xor_bytes(b"abc", key)
    except (ValueError, OverflowError):
      continue
    raise AssertionError(f"xor_bytes accepted out-of-range key {key}")


def test_rejects_non_integer_keys() -> None:
  for key in [1.0, "1", None]:
    try:
      xor_bytes(b"abc", key)
    except TypeError:
      continue
    raise AssertionError(f"xor_bytes accepted non-integer key {key!r}")
