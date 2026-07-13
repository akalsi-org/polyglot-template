#!/usr/bin/env python3
"""Merge language-owned C/C++ actions into the root compilation database."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def load(path: Path) -> list[dict[str, Any]]:
  if not path.exists():
    return []
  value = json.loads(path.read_text(encoding="utf-8"))
  if not isinstance(value, list) or not all(isinstance(row, dict) for row in value):
    raise SystemExit(f"invalid compilation database: {path}")
  return value


def owned(row: dict[str, Any], root: Path, prefix: str) -> bool:
  file = row.get("file")
  if not isinstance(file, str):
    raise SystemExit("compilation database entry requires a file string")
  path = Path(file)
  try:
    relative = (path if path.is_absolute() else root / path).resolve().relative_to(root)
  except ValueError:
    return False
  return relative.as_posix().startswith(prefix.rstrip("/") + "/")


def update(root: Path, fragment: Path, prefix: str) -> list[dict[str, Any]]:
  output = root / "compile_commands.json"
  retained = [row for row in load(output) if not owned(row, root, prefix)]
  replacement = load(fragment)
  if not replacement or not all(owned(row, root, prefix) for row in replacement):
    raise SystemExit(f"compilation database fragment does not exclusively own {prefix}")
  rows = retained + replacement
  files = [str(row["file"]) for row in rows]
  if len(files) != len(set(files)):
    raise SystemExit("each translation unit must have exactly one root compile action")
  rows.sort(key=lambda row: (str(row["file"]), str(row.get("output", ""))))
  if output.is_symlink():
    output.unlink()
  output.write_text(json.dumps(rows, indent=2, sort_keys=True) + "\n", encoding="utf-8")
  return rows


def main() -> None:
  parser = argparse.ArgumentParser()
  parser.add_argument("--root", type=Path, required=True)
  parser.add_argument("--fragment", type=Path, required=True)
  parser.add_argument("--replace-prefix", required=True)
  args = parser.parse_args()
  update(args.root.resolve(), args.fragment.resolve(), args.replace_prefix)


if __name__ == "__main__":
  main()
