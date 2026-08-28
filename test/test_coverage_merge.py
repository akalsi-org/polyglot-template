"""Regression tests for retained coverage normalization."""
import os
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))

import coverage_merge  # noqa: E402


class CoverageNormalizationTest(unittest.TestCase):
  def test_go_module_prefix_is_removed(self) -> None:
    with tempfile.TemporaryDirectory() as tmp:
      profile = Path(tmp) / "coverage.out"
      module = next(
        line.removeprefix("module ").strip()
        for line in (ROOT / "go.mod").read_text().splitlines()
        if line.startswith("module ")
      )
      profile.write_text(
        "mode: set\n"
        f"{module}/go/lib/mpsc/queue.go:10.1,12.2 3 1\n"
      )
      counts: dict[str, dict[int, int]] = {}
      old_cwd = os.getcwd()
      try:
        os.chdir(ROOT)
        coverage_merge._go_module_prefix_cache[0] = None
        coverage_merge._process_go_profile({"primary": str(profile)}, counts)
      finally:
        os.chdir(old_cwd)

    self.assertEqual({10: 1, 11: 1, 12: 1}, counts["go/lib/mpsc/queue.go"])

  def test_lcov_counts_merge_by_maximum(self) -> None:
    with tempfile.TemporaryDirectory() as tmp:
      lcov = Path(tmp) / "coverage.lcov"
      lcov.write_text("SF:python/lib/example/example.py\nDA:4,2\nend_of_record\n")
      counts = {"python/lib/example/example.py": {4: 1}}
      coverage_merge._merge_lcov_file(str(lcov), counts, rewrite_proj_prefix=False)

    self.assertEqual(2, counts["python/lib/example/example.py"][4])


if __name__ == "__main__":
  unittest.main()
