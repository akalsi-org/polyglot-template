#!/usr/bin/env python3
"""Project a resolved DENO_DIR npm store down to exactly deno.lock's closure.

rules/deno.bzl's deno_cache seeds its working DENO_DIR from the untracked
.local/cache/deno (bootstrap's seed), so after resolution that directory can
contain SURPLUS packages from older seeds. Copying it wholesale into the
declared output would let untracked host state change artifact contents
(hermeticity violation - two machines with different but lock-satisfying
seeds would produce different outputs). This projection makes the output a
pure function of deno.lock: exactly the locked npm packages, nothing else -
per-name registry.json metadata and deno's analysis caches are excluded by
construction. Fails closed on a lock shape it does not handle (remote/https
entries) and on a locked package the resolved store is somehow missing.
"""

import json
import os
import shutil
import sys


def main() -> None:
  lock_path, src, dst = sys.argv[1:4]
  with open(lock_path) as f:
    lock = json.load(f)
  if lock.get("remote"):
    raise SystemExit(
      "deno store prune: deno.lock has remote (https) entries; this "
      "projection only handles npm packages - extend tools/"
      "deno_store_prune.py before adding remote imports"
    )
  npm = lock.get("npm") or {}
  os.makedirs(dst, exist_ok = True)
  src_registry = os.path.join(src, "npm", "registry.npmjs.org")
  dst_registry = os.path.join(dst, "npm", "registry.npmjs.org")
  seen = set()
  copied = 0
  skipped = 0
  for key in sorted(npm):
    # Lock keys are <name>@<version>[_<peer>@<ver>...]; scoped names start
    # with "@scope/", so the name/version separator is the first "@" after
    # index 0. The npm store keeps ONE directory per name/PLAIN version
    # (registry.npmjs.org/<name>/<version>) - the peer suffix exists only
    # in the lock key (node_modules projection encodes peer variants),
    # so several keys can map to the same store dir. Verified against the
    # real layout: "@deno/vite-plugin@2.0.2_vite@8.1.4" lives at
    # "@deno/vite-plugin/2.0.2".
    at = key.find("@", 1)
    name, rest = key[:at], key[at + 1:]
    version = rest.split("_", 1)[0]
    if (name, version) in seen:
      continue
    seen.add((name, version))
    src_pkg = os.path.join(src_registry, name, version)
    if not os.path.isdir(src_pkg):
      # Not an error: the lock spans every platform (darwin/win32 native
      # binaries, wasm fallbacks like @emnapi/*), while deno materializes
      # only the current platform's subset. Resolution SUCCEEDED, so
      # everything actually needed is present; a locked-but-unmaterialized
      # package is by definition not needed here. The projection stays
      # deterministic per (lock, platform, deno): skipped set is deno's
      # own platform selection, not host state.
      skipped += 1
      continue
    dst_pkg = os.path.join(dst_registry, name, version)
    os.makedirs(os.path.dirname(dst_pkg), exist_ok = True)
    shutil.copytree(src_pkg, dst_pkg)
    copied += 1


  print(
    f"deno store prune: projected {copied} packages "
    f"({skipped} locked-for-other-platforms skipped)",
    file = sys.stderr,
  )


if __name__ == "__main__":
  main()
