#!/usr/bin/env python3
"""Check or repair repository-owned whitespace shared by every source lane."""

from __future__ import annotations

import argparse
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
IGNORED_PARTS = {
  ".git",
  ".agents",
  ".claude",
  ".codex",
  ".grok",
  ".local",
  ".omc",
  ".omx",
  ".headroom",
  "build",
  "buck-out",
  "dist",
  "node_modules",
  "__pycache__",
}
SUFFIXES = {
  ".bzl",
  ".bxl",
  ".c",
  ".cc",
  ".cpp",
  ".cxx",
  ".h",
  ".hh",
  ".hpp",
  ".json",
  ".py",
  ".sh",
  ".toml",
  ".yaml",
  ".yml",
}
NAMES = {"BUCK", "repo.sh"}


def source_files() -> list[Path]:
  paths = []
  for path in ROOT.rglob("*"):
    if not path.is_file() or IGNORED_PARTS.intersection(path.relative_to(ROOT).parts):
      continue
    if path.suffix in SUFFIXES or path.name in NAMES:
      paths.append(path)
  return sorted(paths)


def whitespace_errors(path: Path, source: str) -> list[str]:
  errors = []
  relative = path.relative_to(ROOT)
  if "\r" in source:
    errors.append(f"{relative}: CRLF is not permitted")
  if source and not source.endswith("\n"):
    errors.append(f"{relative}: missing final newline")
  for number, line in enumerate(source.splitlines(), start=1):
    if line.rstrip(" \t") != line:
      errors.append(f"{relative}:{number}: trailing whitespace")
    indentation = line[: len(line) - len(line.lstrip(" \t"))]
    if "\t" in indentation:
      errors.append(f"{relative}:{number}: indentation must use spaces")
  return errors


def normalized(source: str) -> str:
  lines = source.splitlines()
  result = "\n".join(line.rstrip(" \t") for line in lines)
  return result + "\n" if source else source


def main() -> int:
  parser = argparse.ArgumentParser()
  parser.add_argument("--check", action="store_true", help="fail instead of repairing whitespace")
  args = parser.parse_args()

  errors = []
  changed = []
  for path in source_files():
    source = path.read_text(encoding="utf-8")
    fixed = normalized(source)
    if not args.check and fixed != source:
      path.write_text(fixed, encoding="utf-8")
      source = fixed
      changed.append(path.relative_to(ROOT))
    errors.extend(whitespace_errors(path, source))

  if errors:
    print("\n".join(errors))
    print("run: ./repo.sh format")
    return 1
  if changed:
    print("normalized whitespace:")
    print("\n".join(str(path) for path in changed))
  else:
    print("two-space whitespace policy: ok")
  return 0


if __name__ == "__main__":
  raise SystemExit(main())
