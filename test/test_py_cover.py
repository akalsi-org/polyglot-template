#!/usr/bin/env python3

import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
import py_cover  # noqa: E402


class PythonCoverageTest(unittest.TestCase):
  def test_uncalled_branch_is_reported_uncovered(self) -> None:
    with tempfile.TemporaryDirectory() as directory:
      root = Path(directory)
      (root / "test_branch.py").write_text(
        "def classify(value):\n"
        "  if value:\n"
        "    return 'yes'\n"
        "  return 'no'\n"
        "\n"
        "def test_classify():\n"
        "  assert classify(True) == 'yes'\n"
      )
      report = self._collect_coverage(root)

    self.assertIn("DA:4,0", report)
    self.assertIn("LF:6", report)
    self.assertIn("LH:5", report)

  def test_try_except_collects_without_none_line_start(self) -> None:
    with tempfile.TemporaryDirectory() as directory:
      root = Path(directory)
      (root / "test_try_except.py").write_text(
        "def test_try_except():\n"
        "  try:\n"
        "    raise ValueError('expected')\n"
        "  except ValueError:\n"
        "    pass\n"
      )
      report = self._collect_coverage(root)

    self.assertIn("SF:", report)
    self.assertNotIn("DA:None", report)

  def _collect_coverage(self, root: Path) -> str:
    in_scope = py_cover._make_scope_check([str(root)], [])
    hits, executable = py_cover._run_with_monitoring(
      str(ROOT / "tools/py_test_runner.py"),
      ["discover", "-s", str(root), "-p", "test_*.py"],
      in_scope,
    )
    output = root / "coverage.lcov"
    py_cover._write_lcov(hits, executable, [], str(output))
    return output.read_text()


if __name__ == "__main__":
  unittest.main()
