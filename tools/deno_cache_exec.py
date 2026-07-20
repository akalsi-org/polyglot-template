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
      isinstance(entry, str)
      and entry
      and (entry.startswith("npm:") or (not os.path.isabs(entry) and ".." not in entry.split("/")))
      for entry in entries
  ):
    raise ValueError("Deno cache manifest must contain npm specifiers or safe relative paths")

  paths = [entry if entry.startswith("npm:") else os.path.join(args.root, entry) for entry in entries]
  # First populate the global DENO_DIR cache without a local node_modules
  # projection. The second, frozen pass creates the projection exclusively
  # from that lock-keyed cache, so untracked host node_modules content cannot
  # influence Buck's declared output.
  if subprocess.run(
    [args.deno, "cache", "--frozen", "--node-modules-dir=none", *paths],
    check=False,
  ).returncode:
    return 1
  return subprocess.run(
    [args.deno, "cache", "--frozen", "--node-modules-dir=auto", *paths],
    check=False,
  ).returncode


if __name__ == "__main__":
  sys.exit(main())
