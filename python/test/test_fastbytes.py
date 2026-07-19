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
