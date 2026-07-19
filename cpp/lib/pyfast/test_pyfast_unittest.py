"""Run the pyfast regression script through unittest discovery."""

import runpy
import unittest


class PyfastRegressionTest(unittest.TestCase):
  def test_regression_script(self) -> None:
    runpy.run_path("cpp/lib/pyfast/test_pyfast.py", run_name="__main__")
