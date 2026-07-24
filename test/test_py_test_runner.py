#!/usr/bin/env python3

import io
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "python/lib"))
sys.path.insert(0, str(ROOT / "tools"))
import py_test_runner  # noqa: E402


class PythonTestRunnerTest(unittest.TestCase):
  def _discover(self, source: str) -> tuple[unittest.TestResult, str]:
    try:
      with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        (root / "test_xfail.py").write_text(source)
        stream = io.StringIO()
        result = py_test_runner.run_discovery(str(root), "test_*.py", stream)
        return result, stream.getvalue()
    finally:
      sys.modules.pop("test_xfail", None)

  def test_xfail_is_successful_and_reports_status(self) -> None:
    result, output = self._discover(
      "from test_helpers import xfail\n"
      "@xfail('known failure')\n"
      "def test_known_failure():\n"
      "  assert False\n"
    )

    self.assertTrue(result.wasSuccessful())
    self.assertEqual(len(result.expectedFailures), 1)
    self.assertIn("XFAIL test_xfail.test_known_failure [known failure]", output)

  def test_xpass_fails_and_reports_status(self) -> None:
    result, output = self._discover(
      "from test_helpers import xfail\n"
      "@xfail('fixed unexpectedly')\n"
      "def test_fixed():\n"
      "  assert True\n"
    )

    self.assertFalse(result.wasSuccessful())
    self.assertEqual(len(result.unexpectedSuccesses), 1)
    self.assertIn("XPASS test_xfail.test_fixed [fixed unexpectedly]", output)

  def test_skip_takes_precedence_over_xfail(self) -> None:
    result, output = self._discover(
      "from test_helpers import skip, xfail\n"
      "@xfail('known failure')\n"
      "@skip('not applicable')\n"
      "def test_skipped():\n"
      "  assert False\n"
    )

    self.assertTrue(result.wasSuccessful())
    self.assertEqual(len(result.skipped), 1)
    self.assertEqual(len(result.expectedFailures), 0)
    self.assertIn("SKIP  test_xfail.test_skipped", output)

  def test_parametrized_xfail_keeps_case_labels(self) -> None:
    result, output = self._discover(
      "from test_helpers import parametrize, xfail\n"
      "@xfail('known failure')\n"
      "@parametrize([(1,), (2,)])\n"
      "def test_cases(value):\n"
      "  assert False\n"
    )

    self.assertTrue(result.wasSuccessful())
    self.assertEqual(len(result.expectedFailures), 2)
    self.assertIn("test_xfail.test_cases[(1)]", output)
    self.assertIn("test_xfail.test_cases[(2)]", output)


if __name__ == "__main__":
  unittest.main()
