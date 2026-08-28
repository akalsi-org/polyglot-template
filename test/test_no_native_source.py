from __future__ import annotations

import re
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from tools.lint import starlark_files  # noqa: E402
NATIVE_SUFFIXES = {".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx"}
NATIVE_RULE_KINDS = ("cxx_binary", "cxx_library", "cxx_test", "py_extension")


class NoNativeSourceTest(unittest.TestCase):
  def test_tracked_native_source_is_vendor_only(self) -> None:
    tracked = subprocess.run(
      ["git", "ls-files", "-z"],
      cwd=ROOT,
      check=True,
      capture_output=True,
    ).stdout.decode().split("\0")
    violations = [
      path for path in tracked
      if path
      and (ROOT / path).exists()
      and not path.startswith("vendor/")
      and Path(path).suffix.lower() in NATIVE_SUFFIXES
    ]
    self.assertEqual([], violations)

  def test_graph_has_no_native_rule_kinds(self) -> None:
    violations: list[str] = []
    for path in starlark_files(ROOT):
      text = path.read_text()
      for kind in NATIVE_RULE_KINDS:
        if re.search(rf"(?:def|load|kind)?[^\n]*\b{kind}\s*\(", text):
          violations.append(f"{path.relative_to(ROOT)}: {kind}")
    self.assertEqual([], violations)


if __name__ == "__main__":
  unittest.main()
