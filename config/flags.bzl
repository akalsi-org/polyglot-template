"""Profile (dbg/opt) flag sets for the buck2-built cpp lane.

This file is authoritative: cpp/cpp.toml and tools/cpp_graph.py (the
moon-driven `./repo.sh cpp-build` lane's flag source) were retired along
with moon, so rules/cxx.bzl's select()s reading these lists are now the
only place these flags live.
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
