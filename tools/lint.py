#!/usr/bin/env python3
"""Check repository-owned structured and Python source files without mutation."""

from __future__ import annotations

import ast
import io
import json
import sys
import tokenize
import tomllib
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parent))
import gen_toolchain_lock  # noqa: E402


IGNORED_PARTS = {".git", ".local", "build", "dist", "__pycache__", "buck-out"}


def source_files(root: Path, suffix: str):
  for path in sorted(root.rglob(f"*{suffix}")):
    if not IGNORED_PARTS.intersection(path.relative_to(root).parts):
      yield path


def starlark_files(root: Path):
  # BUCK files carry no file extension, so they need their own glob;
  # rglob("*BUCK") also happens to match them since "*" matches zero chars.
  for suffix in ("BUCK", ".bzl", ".bxl"):
    yield from source_files(root, suffix)


def check_python_indentation(source: str, path: Path) -> list[str]:
  failures: list[str] = []
  levels = [0]
  try:
    tokens = tokenize.generate_tokens(io.StringIO(source).readline)
    for token in tokens:
      if token.type == tokenize.INDENT:
        indentation = token.string
        width = len(indentation)
        if "\t" in indentation or width != levels[-1] + 2:
          failures.append(
            f"{path}: line {token.start[0]}: Python blocks must indent by two spaces"
          )
        levels.append(width)
      elif token.type == tokenize.DEDENT and len(levels) > 1:
        levels.pop()
  except (IndentationError, tokenize.TokenError) as error:
    failures.append(f"{path}: {error}")
  return failures


def main() -> int:
  root = Path(__file__).resolve().parent.parent
  failures: list[str] = []
  for path in source_files(root, ".py"):
    source = path.read_text(encoding="utf-8")
    try:
      ast.parse(source, filename=str(path))
    except (OSError, SyntaxError) as error:
      failures.append(f"{path.relative_to(root)}: {error}")
    failures.extend(check_python_indentation(source, path.relative_to(root)))
  for path in starlark_files(root):
    source = path.read_text(encoding="utf-8")
    try:
      ast.parse(source, filename=str(path))
    except (OSError, SyntaxError) as error:
      failures.append(f"{path.relative_to(root)}: {error}")
    failures.extend(check_python_indentation(source, path.relative_to(root)))
  for path in source_files(root, ".toml"):
    try:
      with path.open("rb") as stream:
        tomllib.load(stream)
    except (OSError, tomllib.TOMLDecodeError) as error:
      failures.append(f"{path.relative_to(root)}: {error}")
  for path in source_files(root, ".json"):
    try:
      json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
      failures.append(f"{path.relative_to(root)}: {error}")
  lock = root / "tools.lock.toml"
  generated = root / "toolchains/lock.bzl"
  expected = gen_toolchain_lock.render_starlark(
    gen_toolchain_lock.build_toolchains(gen_toolchain_lock.load_artifacts(lock))
  )
  actual = generated.read_text(encoding="utf-8") if generated.exists() else ""
  if actual != expected:
    failures.append(f"{generated.relative_to(root)}: stale; run tools/gen_toolchain_lock.py to refresh it")
  if failures:
    print("\n".join(failures), file=sys.stderr)
    return 1
  print("structured and Python lint: ok")
  return 0


if __name__ == "__main__":
  raise SystemExit(main())
