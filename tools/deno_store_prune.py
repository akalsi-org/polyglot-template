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
import urllib.parse

# deno stores an npm package under npm/<registry-host>/<name>/<version>, and a
# lock entry names its host ONLY when it is not the default one: the @jsr/*
# packages here carry "tarball": "https://npm.jsr.io/...", every
# registry.npmjs.org entry omits the field. Deriving the host from the lock
# (rather than searching whatever host directories the seed happens to have)
# keeps this projection a pure function of deno.lock, which is the whole point
# of the file.
DEFAULT_REGISTRY = "registry.npmjs.org"


def registry_host(entry: dict) -> str:
  tarball = entry.get("tarball")
  if not tarball:
    return DEFAULT_REGISTRY
  host = urllib.parse.urlsplit(tarball).netloc
  if not host:
    raise SystemExit(
      f"deno store prune: lock entry has an unparseable tarball URL {tarball!r}"
    )
  return host


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
  seen = set()
  copied = 0
  skipped = 0
  hosts: dict[str, int] = {}
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
    host = registry_host(npm[key] or {})
    if (host, name, version) in seen:
      continue
    seen.add((host, name, version))
    src_pkg = os.path.join(src, "npm", host, name, version)
    if not os.path.isdir(src_pkg):
      # Not an error: the lock spans every platform (darwin/win32 native
      # binaries, wasm fallbacks like @emnapi/*), while deno materializes
      # only the current platform's subset. Resolution SUCCEEDED, so
      # everything actually needed is present; a locked-but-unmaterialized
      # package is by definition not needed here. The projection stays
      # deterministic per (lock, platform, deno): skipped set is deno's
      # own platform selection, not host state.
      #
      # That reasoning is ONLY sound because `host` comes from the lock. This
      # line used to assume registry.npmjs.org for every package, so every
      # npm.jsr.io package missed and was silently absorbed here as "other
      # platforms" - which dropped @jsr/deno__loader from the projected
      # DENO_DIR and made //python/test:pyright (--cached-only) fail in any
      # tree whose buck-out had not already cached a successful run.
      skipped += 1
      continue
    dst_pkg = os.path.join(dst, "npm", host, name, version)
    os.makedirs(os.path.dirname(dst_pkg), exist_ok = True)
    shutil.copytree(src_pkg, dst_pkg)
    hosts[host] = hosts.get(host, 0) + 1
    copied += 1

  # Per-host counts, so an entire registry going missing shows up in the log
  # instead of hiding inside the skipped tally the way the bug above did.
  breakdown = ", ".join(f"{host}: {count}" for host, count in sorted(hosts.items()))
  print(
    f"deno store prune: projected {copied} packages "
    f"({breakdown}; {skipped} locked-for-other-platforms skipped)",
    file = sys.stderr,
  )


if __name__ == "__main__":
  main()
