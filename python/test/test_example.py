import unittest

from example.example import greeting


class ExampleTest(unittest.TestCase):
  def test_greeting(self) -> None:
    self.assertEqual(greeting("world"), "hello, world")


if __name__ == "__main__":
  unittest.main()
