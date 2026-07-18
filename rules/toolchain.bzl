"""First-party toolchain rules: download a pinned archive, extract it with the
host's tar/unzip, then run the same capability probe toolchain/bootstrap.sh
and toolchain/doctor.sh already run, producing a stamp artifact that proves
the extracted tool is actually usable (not just present).

There is no prelude in this project, so buck2's own sha256-verified
ctx.actions.download_file() is the integrity guard; extraction reuses the
host's tar/unzip the same way toolchain/bootstrap.sh does.

Every rule takes a `probe` bool. Extraction always runs (a cross-arch archive
is still worth downloading and unpacking in-graph), but the capability probe
actually executes the extracted binary, which only works for the host's own
architecture — exactly like toolchain/bootstrap.sh and toolchain/doctor.sh,
which only ever probe the single native $target. Non-native targets get a
stamp recording that the probe was skipped instead of an exec-format-error.
"""

def _archive_kind(archive_name: str) -> str:
  if archive_name.endswith(".zip"):
    return "zip"
  if archive_name.endswith(".tar.gz") or archive_name.endswith(".tgz"):
    return "tar.gz"
  if archive_name.endswith(".tar.xz"):
    return "tar.xz"
  fail("unsupported archive: {}".format(archive_name))

def _extract_cmd(kind: str) -> str:
  if kind == "zip":
    return "unzip -q \"$2\" -d \"$1\""
  if kind == "tar.gz":
    return "tar -xzf \"$2\" -C \"$1\" --no-same-owner --no-same-permissions"
  if kind == "tar.xz":
    return "tar -xJf \"$2\" -C \"$1\" --no-same-owner --no-same-permissions"
  fail("unsupported archive kind: {}".format(kind))

def _download_and_extract(ctx: AnalysisContext, name: str, url: str, sha256: str, archive_name: str, post_extract: list[str] = []) -> Artifact:
  archive = ctx.actions.download_file(archive_name, url, sha256 = sha256, is_executable = False)
  out_dir = ctx.actions.declare_output(name + "-extracted", dir = True)
  extract_script = ctx.actions.write(
    name + "-extract.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "mkdir -p \"$1\"",
      _extract_cmd(_archive_kind(archive_name)),
    ] + post_extract,
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", extract_script, out_dir.as_output(), archive]),
    category = "extract_toolchain",
    identifier = name,
  )
  return out_dir

def _skipped_stamp(ctx: AnalysisContext, name: str) -> Artifact:
  return ctx.actions.write(name + ".stamp", "skipped: non-native execution platform\n")

# go, deno, ninja: extract, then a version probe (go uses the `version`
# subcommand rather than a `--version` flag; ninja/deno accept `--version`).
def _version_probe_toolchain_impl(ctx: AnalysisContext) -> list[Provider]:
  name = ctx.label.name
  out_dir = _download_and_extract(ctx, name, ctx.attrs.url, ctx.attrs.sha256, ctx.attrs.archive)
  if not ctx.attrs.probe:
    return [DefaultInfo(default_outputs = [out_dir, _skipped_stamp(ctx, name)])]
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "BIN=\"$1/%s\"" % ctx.attrs.expected,
      "chmod +x \"$BIN\"",
      "\"$BIN\" %s >\"$2\" 2>&1 || { cat \"$2\" >&2; exit 1; }" % ctx.attrs.probe_arg,
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
  )
  return [DefaultInfo(default_outputs = [out_dir, stamp])]

version_probe_toolchain = rule(
  impl = _version_probe_toolchain_impl,
  attrs = {
    "archive": attrs.string(),
    "expected": attrs.string(),
    "probe": attrs.bool(default = True),
    "probe_arg": attrs.string(default = "--version"),
    "sha256": attrs.string(),
    "url": attrs.string(),
  },
)

# doctest: extract, then grep the pinned major-version header line. Header
# capability checks do not execute anything, so they always run.
def _header_probe_toolchain_impl(ctx: AnalysisContext) -> list[Provider]:
  name = ctx.label.name
  out_dir = _download_and_extract(ctx, name, ctx.attrs.url, ctx.attrs.sha256, ctx.attrs.archive)
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "HDR=\"$1/%s\"" % ctx.attrs.expected,
      "grep -q '%s' \"$HDR\" >\"$2\" 2>&1 || { cat \"$2\" >&2; exit 1; }" % ctx.attrs.probe_pattern,
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
  )
  return [DefaultInfo(default_outputs = [out_dir, stamp])]

header_probe_toolchain = rule(
  impl = _header_probe_toolchain_impl,
  attrs = {
    "archive": attrs.string(),
    "expected": attrs.string(),
    "probe_pattern": attrs.string(),
    "sha256": attrs.string(),
    "url": attrs.string(),
  },
)

# gcc-musl: extract, then the mold + binutils + dumpmachine + C++26 reflection
# capability probe mirrored from toolchain/bootstrap.sh's probe_gcc().
def _gcc_musl_toolchain_impl(ctx: AnalysisContext) -> list[Provider]:
  name = ctx.label.name

  # The upstream archive ships the musl dynamic loader as a symlink to the
  # absolute path /lib/libc.so, which does not exist outside the archive's
  # own build environment. toolchain/bootstrap.sh's normalize_gcc_loader()
  # rewrites that to a same-directory relative symlink; mirror it here so
  # the loader is actually usable once extracted.
  normalize_loader = [
    "LOADER=\"$1/%s\"" % ctx.attrs.loader,
    "if [ -L \"$LOADER\" ] && [ \"$(readlink -- \"$LOADER\")\" = /lib/libc.so ]; then",
    "  rm -- \"$LOADER\"",
    "  ln -s libc.so \"$LOADER\"",
    "fi",
  ]
  out_dir = _download_and_extract(ctx, name, ctx.attrs.url, ctx.attrs.sha256, ctx.attrs.archive, normalize_loader)
  if not ctx.attrs.probe:
    return [DefaultInfo(default_outputs = [out_dir, _skipped_stamp(ctx, name)])]
  reflection_src = ctx.attrs.reflection_src[DefaultInfo].default_outputs[0]
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "ROOT=\"$1\"",
      "SRC=\"$2\"",
      "OUT=\"$3\"",
      "GXX=\"$ROOT/%s\"" % ctx.attrs.expected,
      "MOLD=\"$ROOT/%s\"" % ctx.attrs.mold,
      "chmod +x \"$GXX\" \"$MOLD\"",
      "[ \"$(\"$GXX\" -dumpmachine)\" = \"%s\" ] || { echo 'compiler target mismatch' >&2; exit 1; }" % ctx.attrs.target_triple,
      "\"$MOLD\" --version | grep -q '^mold 2\\.41\\.0' || { echo 'mold capability probe failed' >&2; exit 1; }",
      "\"$GXX\" -std=gnu++26 -freflection -fsyntax-only \"$SRC\" >\"$OUT\" 2>&1 || { cat \"$OUT\" >&2; exit 1; }",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, reflection_src, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
  )
  return [DefaultInfo(default_outputs = [out_dir, stamp])]

gcc_musl_toolchain = rule(
  impl = _gcc_musl_toolchain_impl,
  attrs = {
    "archive": attrs.string(),
    "expected": attrs.string(),
    "loader": attrs.string(),
    "mold": attrs.string(),
    "probe": attrs.bool(default = True),
    "reflection_src": attrs.dep(providers = [DefaultInfo]),
    "sha256": attrs.string(),
    "target_triple": attrs.string(),
    "url": attrs.string(),
  },
)

# python: extract, then the musl-loader capability probe mirrored from
# toolchain/bootstrap.sh's probe_python(). The dependency on a gcc-musl
# toolchain target is real: this rule reads the loader out of gcc's own
# built output, so buck2 must build gcc-musl first.
def _python_toolchain_impl(ctx: AnalysisContext) -> list[Provider]:
  name = ctx.label.name
  out_dir = _download_and_extract(ctx, name, ctx.attrs.url, ctx.attrs.sha256, ctx.attrs.archive)
  if not ctx.attrs.probe:
    return [DefaultInfo(default_outputs = [out_dir, _skipped_stamp(ctx, name)])]
  gcc_out = ctx.attrs.gcc[DefaultInfo].default_outputs[0]
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "PYROOT=\"$1\"",
      "GCCROOT=\"$2\"",
      "OUT=\"$3\"",
      "PY=\"$PYROOT/%s\"" % ctx.attrs.expected,
      "LOADER=\"$GCCROOT/%s\"" % ctx.attrs.loader,
      "LOADER_DIR=$(dirname \"$LOADER\")",
      "chmod +x \"$PY\" \"$LOADER\"",
      "\"$LOADER\" --library-path \"$LOADER_DIR:$PYROOT/python/lib\" \"$PY\" -I -c " +
      "'import ctypes, hashlib, sqlite3, ssl, sys, zlib; print(sys.version)' >\"$OUT\" 2>&1 || { cat \"$OUT\" >&2; exit 1; }",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, gcc_out, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
  )
  return [DefaultInfo(default_outputs = [out_dir, stamp])]

python_toolchain = rule(
  impl = _python_toolchain_impl,
  attrs = {
    "archive": attrs.string(),
    "expected": attrs.string(),
    "gcc": attrs.dep(providers = [DefaultInfo]),
    "loader": attrs.string(),
    "probe": attrs.bool(default = True),
    "sha256": attrs.string(),
    "url": attrs.string(),
  },
)
