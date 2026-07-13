#!/usr/bin/env python3
"""Build the pinned-CPython C++ extension with the pinned GCC/musl compiler."""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parent))
import compdb  # noqa: E402


def capture(command: list[str]) -> str:
  return subprocess.run(command, check=True, text=True, capture_output=True).stdout.strip()


def main() -> None:
  parser = argparse.ArgumentParser()
  parser.add_argument("--root", type=Path, required=True)
  parser.add_argument("--target", required=True)
  parser.add_argument("--cxx", required=True)
  args = parser.parse_args()

  root = args.root.resolve()
  metadata = json.loads(
    capture(
      [
        str(root / "repo.sh"),
        "python",
        "-I",
        "-c",
        "import json,sysconfig; print(json.dumps({'include': sysconfig.get_path('include'), 'ext_suffix': sysconfig.get_config_var('EXT_SUFFIX')}))",
      ]
    )
  )
  include = metadata["include"]
  suffix = metadata["ext_suffix"]
  if not isinstance(include, str) or not isinstance(suffix, str) or not suffix.endswith(".so"):
    raise SystemExit("invalid Python extension metadata")

  source = root / "python/lib/fastbytes/fastbytes.cc"
  package = root / f"build/python/{args.target}/lib/fastbytes"
  package.mkdir(parents=True, exist_ok=True)
  shutil.copy2(root / "python/lib/fastbytes/__init__.py", package / "__init__.py")
  output = package / f"_native{suffix}"
  command = [
    args.cxx,
    f"-B{Path(args.cxx).parent}",
    "-std=gnu++26",
    "-O2",
    "-fPIC",
    "-shared",
    "-Wall",
    "-Wextra",
    "-Wpedantic",
    f"-I{include}",
    str(source),
    "-o",
    str(output),
    "-fuse-ld=mold",
  ]
  subprocess.run(command, check=True, cwd=root)
  fragment = root / f"build/python/{args.target}/compile_commands.json"
  compdb.atomic_write(
    fragment,
    [
      {
        "arguments": command,
        "directory": str(root),
        "file": str(source),
        "output": str(output),
      }
    ],
  )
  print(output)


if __name__ == "__main__":
  main()
