#!/usr/bin/env python3

import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
import lint  # noqa: E402


class PythonIndentationTest(unittest.TestCase):
  def test_accepts_two_space_blocks(self) -> None:
    self.assertEqual(lint.check_python_indentation("if True:\n  pass\n", Path("ok.py")), [])

  def test_rejects_four_space_blocks(self) -> None:
    self.assertTrue(
      lint.check_python_indentation("if True:\n    pass\n", Path("bad.py"))
    )


if __name__ == "__main__":
  unittest.main()
