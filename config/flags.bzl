"""Mirrors cpp/cpp.toml's [profiles.dbg] / [profiles.opt] flag sets so
rules/cxx.bzl does not need to parse TOML at analysis time.

cpp/cpp.toml (consumed by tools/cpp_graph.py) stays authoritative for the
moon-driven `./repo.sh cpp-build` lane until that lane is retired; this file
must be kept in sync with it by hand until then.
"""

STANDARD = "gnu++26"

FORBIDDEN_FLAGS = ["-fhardened", "-ftrivial-auto-var-init=zero"]

# Coverage is default-on for the dbg profile (not a third profile - see
# rules/coverage.bzl's module docstring): dbg compile+link actions gain
# --coverage, which both instruments (compile) and links libgcov (link);
# opt builds stay clean of it entirely.
COVERAGE_FLAG = "--coverage"

DBG_COMPILE_FLAGS = [
  "-O0",
  "-g3",
  "-Wall",
  "-Wextra",
  "-Wpedantic",
  "-Werror=uninitialized",
  "-Werror=maybe-uninitialized",
  "-fno-omit-frame-pointer",
  COVERAGE_FLAG,
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

# Same for both profiles today except for COVERAGE_FLAG (dbg-only); cpp.toml
# declares it per-profile so this stays a function of profile too, in case
# that changes further.
LINK_FLAGS = ["-fuse-ld=mold"]

def profile_compile_flags():
  return select({
    "//config:opt": OPT_COMPILE_FLAGS,
    "DEFAULT": DBG_COMPILE_FLAGS,
  })

def profile_link_flags():
  return select({
    "//config:opt": LINK_FLAGS,
    "DEFAULT": LINK_FLAGS + [COVERAGE_FLAG],
  })

# Non-cxx lanes (go/deno/python) don't take a --coverage-style flag, but
# still need to know "is this the dbg profile" to decide whether to collect
# coverage at all - same select(), used the same way (only resolves as an
# attrs default; see rules/cxx.bzl's _PROFILE_ATTRS comment).
def coverage_enabled_flag():
  return select({
    "//config:opt": False,
    "DEFAULT": True,
  })
