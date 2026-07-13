#!/usr/bin/env python3

import json
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
import compdb  # noqa: E402


class CompdbTest(unittest.TestCase):
  def test_language_fragments_replace_only_their_ownership(self) -> None:
    with tempfile.TemporaryDirectory() as temporary:
      root = Path(temporary)
      cpp = root / "cpp.json"
      python = root / "python.json"
      cpp.write_text(json.dumps([self.row(root, "cpp/app/main.cc")]))
      python.write_text(json.dumps([self.row(root, "python/lib/ext.cc")]))

      compdb.update(root, cpp, "cpp")
      rows = compdb.update(root, python, "python")

      self.assertEqual(
        [Path(row["file"]).relative_to(root).as_posix() for row in rows],
        ["cpp/app/main.cc", "python/lib/ext.cc"],
      )

  def test_replaces_a_legacy_root_symlink(self) -> None:
    with tempfile.TemporaryDirectory() as temporary:
      root = Path(temporary)
      fragment = root / "cpp.json"
      fragment.write_text(json.dumps([self.row(root, "cpp/app/main.cc")]))
      (root / "compile_commands.json").symlink_to(fragment.name)

      compdb.update(root, fragment, "cpp")

      self.assertFalse((root / "compile_commands.json").is_symlink())

  @staticmethod
  def row(root: Path, source: str) -> dict[str, object]:
    return {
      "arguments": ["c++", "-c", source],
      "directory": str(root),
      "file": str(root / source),
    }


if __name__ == "__main__":
  unittest.main()
