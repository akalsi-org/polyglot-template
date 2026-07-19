"""Instantiates one toolchain target per locked tool x target triple.

BUCK files use a restricted dialect that forbids top-level `for`/`if`, so the
generation loop lives here and toolchains/BUCK just calls it.
"""

load("//rules:group.bzl", "group")
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

  # The lock records artifacts for both supported host architectures, but a
  # checkout instantiates only the current host's five toolchain targets.
  # This makes `buck2 build //...` a valid native build instead of asking an
  # x86_64 checkout to stage aarch64-hosted archives (or vice versa).
  gcc = TOOLCHAINS["gcc-musl"][native_target]
  gcc_musl_toolchain(
    name = "gcc-musl-" + native_target,
    url = gcc["url"], sha256 = gcc["sha256"], archive = gcc["archive"],
    expected = gcc["expected"], mold = gcc["mold"], loader = gcc["loader"],
    target_triple = native_target, reflection_src = "//cpp/test:reflection.cc",
    probe = True, visibility = ["PUBLIC"],
  )

  python = TOOLCHAINS["python"][native_target]
  python_toolchain(
    name = "python-" + native_target,
    url = python["url"], sha256 = python["sha256"], archive = python["archive"],
    expected = python["expected"], loader = gcc["loader"],
    gcc = ":gcc-musl-" + native_target,
    probe = True, visibility = ["PUBLIC"],
  )

  for tool, probe_arg in (("go", "version"), ("deno", "--version")):
    fields = TOOLCHAINS[tool][native_target]
    version_probe_toolchain(
      name = tool + "-" + native_target,
      url = fields["url"], sha256 = fields["sha256"], archive = fields["archive"],
      expected = fields["expected"], probe_arg = probe_arg,
      probe = True, visibility = ["PUBLIC"],
    )

  doctest = TOOLCHAINS["doctest"][native_target]
  header_probe_toolchain(
    name = "doctest-" + native_target,
    url = doctest["url"], sha256 = doctest["sha256"], archive = doctest["archive"],
    expected = doctest["expected"], probe_pattern = "^#define DOCTEST_VERSION_MAJOR 2$",
    visibility = ["PUBLIC"],
  )

  # The only instantiated toolchains are already native, so this group is a
  # stable public spelling for CI and targeted toolchain validation.
  group(
    name = "native",
    deps = [
      ":gcc-musl-" + native_target,
      ":python-" + native_target,
      ":go-" + native_target,
      ":deno-" + native_target,
      ":doctest-" + native_target,
    ],
    visibility = ["PUBLIC"],
  )
