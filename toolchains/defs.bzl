"""Instantiates one toolchain target per locked tool x target triple.

BUCK files use a restricted dialect that forbids top-level `for`/`if`, so the
generation loop lives here and toolchains/BUCK just calls it.
"""

load("//rules:group.bzl", "group")
load("//rules:host.bzl", "native_target")
load("//rules:toolchain.bzl", "gcc_musl_toolchain", "python_toolchain", "version_probe_toolchain")
load(":lock.bzl", "TOOLCHAINS")

def define_toolchains():
  target = native_target()

  # The lock records artifacts for both supported host architectures, but a
  # checkout instantiates only the current host's five toolchain targets.
  # This makes `buck2 build //...` a valid native build instead of asking an
  # x86_64 checkout to stage aarch64-hosted archives (or vice versa).
  gcc = TOOLCHAINS["gcc-musl"][target]
  gcc_musl_toolchain(
    name = "gcc-musl-" + target,
    url = gcc["url"], sha256 = gcc["sha256"], archive = gcc["archive"],
    expected = gcc["expected"], mold = gcc["mold"], loader = gcc["loader"],
    target_triple = target,
    probe = True, visibility = ["PUBLIC"],
  )

  python = TOOLCHAINS["python"][target]
  python_toolchain(
    name = "python-" + target,
    url = python["url"], sha256 = python["sha256"], archive = python["archive"],
    expected = python["expected"], loader = gcc["loader"],
    gcc = ":gcc-musl-" + target,
    probe = True, visibility = ["PUBLIC"],
  )

  for tool, probe_arg in (("go", "version"), ("deno", "--version")):
    fields = TOOLCHAINS[tool][target]
    version_probe_toolchain(
      name = tool + "-" + target,
      url = fields["url"], sha256 = fields["sha256"], archive = fields["archive"],
      expected = fields["expected"], probe_arg = probe_arg,
      probe = True, visibility = ["PUBLIC"],
    )

  # The only instantiated toolchains are already native, so this group is a
  # stable public spelling for CI and targeted toolchain validation.
  group(
    name = "native",
    deps = [
      ":gcc-musl-" + target,
      ":python-" + target,
      ":go-" + target,
      ":deno-" + target,
    ],
    visibility = ["PUBLIC"],
  )
