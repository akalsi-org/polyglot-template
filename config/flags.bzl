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
  "-Wno-pedantic",
  "-Werror",
  "-fno-omit-frame-pointer",
  COVERAGE_FLAG,
]

OPT_COMPILE_FLAGS = [
  "-O3",
  "-DNDEBUG",
  "-Wall",
  "-Wextra",
  "-Wno-pedantic",
  "-Werror",
]

# Deployment ISA baseline, not a host-tuning option. All native C/C++ targets,
# including Python extensions, use this through profile_compile_flags(). x86-64
# artifacts require v3 CPUs and request PREFETCHW; Arm artifacts require v8.2-A.
# The target platform is host-native by construction (config/defs.bzl rejects
# cross-arch requests), so a binary never silently carries the other ISA.
NATIVE_ISA_FLAGS = select({
  "//config:x86_64-linux-musl": ["-march=x86-64-v3", "-mtune=generic", "-mprfchw"],
  "//config:aarch64-linux-musl": ["-march=armv8.2-a"],
  "DEFAULT": [],
})

# Same for both profiles today except for COVERAGE_FLAG (dbg-only); cpp.toml
# declares it per-profile so this stays a function of profile too, in case
# that changes further.
LINK_FLAGS = ["-fuse-ld=mold"]

def profile_compile_flags():
  return select({
    "//config:opt": OPT_COMPILE_FLAGS,
    "DEFAULT": DBG_COMPILE_FLAGS,
  }) + NATIVE_ISA_FLAGS

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

def check_flags(flags):
  """Rejects flags outside the pinned musl toolchain's contract.

  Lives here rather than in rules/cxx.bzl because rules/python.bzl's
  py_extension compiles with the same toolchain and needs the identical
  check - it previously carried a verbatim copy, which is exactly the kind
  of duplicate that drifts the moment FORBIDDEN_FLAGS grows an entry."""
  for flag in flags:
    if flag in FORBIDDEN_FLAGS:
      fail("forbidden C++ flag: {}".format(flag))
    if flag.startswith("-fsanitize"):
      fail("sanitizers are outside the pinned musl toolchain contract: {}".format(flag))
