#!/usr/bin/env python3
"""Execute a frozen Deno cache operation from a graph-produced manifest.

Keeping argument expansion here avoids a second, shell-maintained list of
Deno entrypoints in bootstrap.  The manifest contains repository-relative
paths and is safe to use from both the Buck action staging directory and the
bootstrap checkout.
"""

import argparse
import json
import os
import subprocess
import sys


def main() -> int:
  parser = argparse.ArgumentParser()
  parser.add_argument("--deno", required=True)
  parser.add_argument("--manifest", required=True)
  parser.add_argument("--root", required=True)
  args = parser.parse_args()

  with open(args.manifest, encoding="utf-8") as manifest_file:
    entries = json.load(manifest_file)
  if not isinstance(entries, list) or not entries or not all(
      isinstance(entry, str) and entry and not os.path.isabs(entry)
      for entry in entries
  ):
    raise ValueError("Deno cache manifest must be a non-empty relative-path list")

  paths = [os.path.join(args.root, entry) for entry in entries]
  return subprocess.run([args.deno, "cache", "--frozen", *paths], check=False).returncode


if __name__ == "__main__":
  sys.exit(main())
