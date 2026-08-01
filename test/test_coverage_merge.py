"""Regression tests for coverage source-path normalization."""
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))

import coverage_merge  # noqa: E402


class CxxSourceNormalizationTest(unittest.TestCase):
  def test_declared_include_tree_header_maps_to_repo_source(self) -> None:
    entry = {"headers": ["cpp/lib/pgt/mpsc/spsc_ring.hh"]}
    header_map = coverage_merge._cxx_header_map(entry)
    staged = (
      "buck-out/v2/art/root/hash/cpp/lib/pgt/mpsc/__mpsc__/"
      "mpsc__include_tree__/pgt/mpsc/spsc_ring.hh"
    )

    self.assertEqual(
      coverage_merge._normalize_cxx_source(staged, header_map),
      "cpp/lib/pgt/mpsc/spsc_ring.hh",
    )

  def test_undeclared_generated_header_stays_excluded(self) -> None:
    staged = "buck-out/v2/art/root/hash/pkg/__dep__/dep__include_tree__/third_party.hh"

    self.assertIsNone(coverage_merge._normalize_cxx_source(staged, {}))

  def test_repo_relative_spelling_is_canonicalized(self) -> None:
    path = "cpp/lib/pgt/mpsc/../mpsc/spsc_ring.hh"

    self.assertEqual(
      coverage_merge._normalize_cxx_source(path, {}),
      "cpp/lib/pgt/mpsc/spsc_ring.hh",
    )


if __name__ == "__main__":
  unittest.main()
