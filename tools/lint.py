#!/usr/bin/env python3
"""Check repository-owned structured and Python source files without mutation."""

from __future__ import annotations

import ast
import json
import sys
import tomllib
from pathlib import Path


IGNORED_PARTS = {".git", ".local", "build", "dist", "__pycache__"}


def source_files(root: Path, suffix: str):
    for path in sorted(root.rglob(f"*{suffix}")):
        if not IGNORED_PARTS.intersection(path.relative_to(root).parts):
            yield path


def main() -> int:
    root = Path(__file__).resolve().parent.parent
    failures: list[str] = []
    for path in source_files(root, ".py"):
        try:
            ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        except (OSError, SyntaxError) as error:
            failures.append(f"{path.relative_to(root)}: {error}")
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
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print("structured and Python lint: ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
