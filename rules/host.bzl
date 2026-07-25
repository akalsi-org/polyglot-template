"""The one definition of this project's host-native output triplet.

Every rules/*.bzl file, toolchains/defs.bzl, config/defs.bzl and bxl/*.bxl
used to carry its own byte-identical `_native_target()` copy (nine of them),
plus one INCONSISTENT inline variant in bxl/coverage.bxl that read
`host_info().arch.is_x86_64` with no fail() branch, so an unsupported host
silently resolved the aarch64 toolchain label there while every other copy
failed loudly. One definition removes that whole class of drift.

This repo only ever builds+runs the host's own musl output triplet (see
config/defs.bzl's docstring on why cross-arch is a declared but unbuilt
dimension), so the answer is a pure function of the daemon host's CPU.
"""

def native_target() -> str:
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")
