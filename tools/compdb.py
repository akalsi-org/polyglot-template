#!/usr/bin/env python3
"""Merge language-owned C/C++ actions into the root compilation database."""

from __future__ import annotations

import argparse
import fcntl
import json
import os
from pathlib import Path
from typing import Any


def load(path: Path) -> list[dict[str, Any]]:
  if not path.exists():
    return []
  value = json.loads(path.read_text(encoding="utf-8"))
  if not isinstance(value, list) or not all(isinstance(row, dict) for row in value):
    raise SystemExit(f"invalid compilation database: {path}")
  return value


def atomic_write(path: Path, value: Any) -> None:
  path.parent.mkdir(parents=True, exist_ok=True)
  temporary = path.with_name(f".{path.name}.tmp.{os.getpid()}")
  temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
  os.replace(temporary, path)


def merge(root: Path, fragments: list[Path]) -> list[dict[str, Any]]:
  output = root / "compile_commands.json"
  fragment_rows = [load(fragment) for fragment in fragments]
  if any(not rows for rows in fragment_rows):
    raise SystemExit("compilation database fragments must be non-empty")
  rows = [row for entries in fragment_rows for row in entries]
  files = [str(row["file"]) for row in rows]
  if len(files) != len(set(files)):
    raise SystemExit("each translation unit must have exactly one root compile action")
  rows.sort(key=lambda row: (str(row["file"]), str(row.get("output", ""))))
  lock = root / ".local/locks/compile-commands.lock"
  lock.parent.mkdir(parents=True, exist_ok=True)
  with lock.open("w") as stream:
    fcntl.flock(stream, fcntl.LOCK_EX)
    if output.is_symlink():
      output.unlink()
    atomic_write(output, rows)
  return rows


def main() -> None:
  parser = argparse.ArgumentParser()
  parser.add_argument("--root", type=Path, required=True)
  parser.add_argument("--fragment", type=Path, action="append", required=True)
  args = parser.parse_args()
  merge(args.root.resolve(), [fragment.resolve() for fragment in args.fragment])


if __name__ == "__main__":
  main()
