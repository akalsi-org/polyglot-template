import unittest

from fastbytes import xor_bytes


class FastBytesTest(unittest.TestCase):
  def test_round_trip(self) -> None:
    value = bytes(range(256))
    self.assertEqual(xor_bytes(xor_bytes(value, 0xA5), 0xA5), value)

  def test_rejects_large_key(self) -> None:
    with self.assertRaises(ValueError):
      xor_bytes(b"value", 256)


if __name__ == "__main__":
  unittest.main()
