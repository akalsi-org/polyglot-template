"""Instantiates one toolchain target per locked tool x target triple.

BUCK files use a restricted dialect that forbids top-level `for`/`if`, so the
generation loop lives here and toolchains/BUCK just calls it.
"""

load("//rules:toolchain.bzl", "gcc_musl_toolchain", "header_probe_toolchain", "python_toolchain", "version_probe_toolchain")
load(":lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors toolchain/target.sh: this repo only ever probes the host's own
  # musl output triplet, never a cross-compiled one.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

def define_toolchains():
  native_target = _native_target()

  for target, fields in TOOLCHAINS["gcc-musl"].items():
    gcc_musl_toolchain(
      name = "gcc-musl-" + target,
      url = fields["url"],
      sha256 = fields["sha256"],
      archive = fields["archive"],
      expected = fields["expected"],
      mold = fields["mold"],
      loader = fields["loader"],
      target_triple = target,
      reflection_src = "//cpp/test:reflection.cc",
      probe = target == native_target,
      visibility = ["PUBLIC"],
    )

  for target, fields in TOOLCHAINS["python"].items():
    python_toolchain(
      name = "python-" + target,
      url = fields["url"],
      sha256 = fields["sha256"],
      archive = fields["archive"],
      expected = fields["expected"],
      loader = TOOLCHAINS["gcc-musl"][target]["loader"],
      gcc = ":gcc-musl-" + target,
      probe = target == native_target,
      visibility = ["PUBLIC"],
    )

  for tool, probe_arg in (("go", "version"), ("deno", "--version"), ("ninja", "--version")):
    for target, fields in TOOLCHAINS[tool].items():
      version_probe_toolchain(
        name = tool + "-" + target,
        url = fields["url"],
        sha256 = fields["sha256"],
        archive = fields["archive"],
        expected = fields["expected"],
        probe_arg = probe_arg,
        probe = target == native_target,
        visibility = ["PUBLIC"],
      )

  for target, fields in TOOLCHAINS["doctest"].items():
    header_probe_toolchain(
      name = "doctest-" + target,
      url = fields["url"],
      sha256 = fields["sha256"],
      archive = fields["archive"],
      expected = fields["expected"],
      probe_pattern = "^#define DOCTEST_VERSION_MAJOR 2$",
      visibility = ["PUBLIC"],
    )
