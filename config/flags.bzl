"""Mirrors cpp/cpp.toml's [profiles.dbg] / [profiles.opt] flag sets so
rules/cxx.bzl does not need to parse TOML at analysis time.

cpp/cpp.toml (consumed by tools/cpp_graph.py) stays authoritative for the
moon-driven `./repo.sh cpp-build` lane until that lane is retired; this file
must be kept in sync with it by hand until then.
"""

STANDARD = "gnu++26"

FORBIDDEN_FLAGS = ["-fhardened", "-ftrivial-auto-var-init=zero"]

DBG_COMPILE_FLAGS = [
  "-O0",
  "-g3",
  "-Wall",
  "-Wextra",
  "-Wpedantic",
  "-Werror=uninitialized",
  "-Werror=maybe-uninitialized",
  "-fno-omit-frame-pointer",
]

OPT_COMPILE_FLAGS = [
  "-O3",
  "-DNDEBUG",
  "-Wall",
  "-Wextra",
  "-Wpedantic",
  "-Werror=uninitialized",
  "-Werror=maybe-uninitialized",
]

# Same for both profiles today; cpp.toml declares it per-profile so this
# stays a function of profile too, in case that changes.
LINK_FLAGS = ["-fuse-ld=mold"]

def profile_compile_flags():
  return select({
    "//config:opt": OPT_COMPILE_FLAGS,
    "DEFAULT": DBG_COMPILE_FLAGS,
  })

def profile_link_flags():
  return select({
    "//config:opt": LINK_FLAGS,
    "DEFAULT": LINK_FLAGS,
  })
