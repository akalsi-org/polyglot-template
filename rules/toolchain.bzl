"""First-party toolchain rules: consume the pinned archive that
toolchain/bootstrap.sh already fetched and retained at
.local/downloads/<sha256>-<archive>, extract it with the host's tar/unzip,
then run the same capability probe toolchain/bootstrap.sh and
toolchain/doctor.sh already run, producing a stamp artifact that proves
the extracted tool is actually usable (not just present).

OFFLINE-AFTER-BOOTSTRAP: normal graph actions never fetch. Bootstrap owns
every download; the graph consumes its retained, sha-named archives. The
archive file deliberately lives OUTSIDE buck2's tracked inputs (.local/ is
not a buck2 package) - correctness is keyed on the sha256 attr from
toolchains/lock.bzl (itself drift-guarded against tools.lock.toml by
tools/lint.py): a pin change re-runs the staging action, and the action
verifies the file's actual bytes against the pinned sha before use, so a
stale or corrupt local file can never silently feed the graph. A missing
archive is an actionable "run ./repo.sh bootstrap" failure, never a
fallback download - a non-native triple's toolchain targets therefore only
build on a host whose bootstrap fetched that triple's archives, which
nothing in the normal graph requires.

Every rule takes a `probe` bool. Extraction always runs (a cross-arch archive
is still worth downloading and unpacking in-graph), but the capability probe
actually executes the extracted binary, which only works for the host's own
architecture — exactly like toolchain/bootstrap.sh and toolchain/doctor.sh,
which only ever probe the single native $target. Non-native targets get a
stamp recording that the probe was skipped instead of an exec-format-error.

EXECUTABLE BIT: restored exactly ONCE, here, by each rule's own
`post_extract` lines (see _executable_bits() and its call sites). It used to
be re-applied by every consumer instead - seven `chmod +x` sites across
rules/toolchain.bzl, rules/deno.bzl and rules/python.bzl - each of them
chmod'ing an artifact that was an INPUT to the very action doing the chmod.
rules/deno.bzl's _stage_and_run copy ran in every deno consumer, so N
concurrent deno actions raced to chmod the same inode of a shared,
already-materialized output. A produced artifact's mode is the producing
rule's responsibility; a consumer that has to fix up its own inputs is a
consumer mutating the build graph underneath itself.
"""

load("//rules:env.bzl", "action_env")
load("//rules:hosttools.bzl", "require_host_tools", "sha256sum_guard")

def _executable_bits(paths: list[str]) -> list[str]:
  # $1 is the extraction directory in every post_extract context below.
  return ["chmod +x \"$1/%s\"" % path for path in paths]

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

def _extract_tool(kind: str) -> str:
  return "unzip" if kind == "zip" else "tar"

def _download_and_extract(ctx: AnalysisContext, name: str, url: str, sha256: str, archive_name: str, post_extract: list[str] = []) -> Artifact:
  # `url` is deliberately unused here (kept for lock parity/diagnostics):
  # bootstrap owns fetching. See the module docstring's
  # OFFLINE-AFTER-BOOTSTRAP block for the correctness story.
  _ = url
  archive = ctx.actions.declare_output(archive_name)
  kind = _archive_kind(archive_name)
  stage_archive_script = ctx.actions.write(
    name + "-local-archive.sh",
    [
      "#!/bin/sh",
      "set -eu",
    ] + sha256sum_guard() + [
      "src=\".local/downloads/%s-%s\"" % (sha256, archive_name),
      "if [ ! -f \"$src\" ]; then",
      "  echo \"error: $src is missing - run ./repo.sh bootstrap (graph actions never fetch; bootstrap owns downloads)\" >&2",
      "  exit 1",
      "fi",
      "actual=$(sha256sum \"$src\" | cut -d ' ' -f1)",
      "if [ \"$actual\" != \"%s\" ]; then" % sha256,
      "  echo \"error: $src sha256 $actual does not match pinned %s - re-run ./repo.sh bootstrap\" >&2" % sha256,
      "  exit 1",
      "fi",
      "cp -- \"$src\" \"$1\"",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", stage_archive_script, archive.as_output()]),
    category = "stage_toolchain_archive",
    identifier = name,
    env = action_env(),
    # Reads the untracked .local/downloads path, which only exists on the
    # bootstrapped host - never eligible for remote execution.
    local_only = True,
  )
  out_dir = ctx.actions.declare_output(name + "-extracted", dir = True)
  extract_script = ctx.actions.write(
    name + "-extract.sh",
    [
      "#!/bin/sh",
      "set -eu",
    ] + require_host_tools([_extract_tool(kind)]) + [
      "mkdir -p \"$1\"",
      _extract_cmd(kind),
    ] + post_extract,
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", extract_script, out_dir.as_output(), archive]),
    category = "extract_toolchain",
    identifier = name,
    env = action_env(),
  )
  return out_dir

def _skipped_stamp(ctx: AnalysisContext, name: str) -> Artifact:
  return ctx.actions.write(name + ".stamp", "skipped: non-native execution platform\n")

# go, deno: extract, then a version probe (go uses the `version`
# subcommand rather than a `--version` flag; deno accepts `--version`).
def _version_probe_toolchain_impl(ctx: AnalysisContext) -> list[Provider]:
  name = ctx.label.name
  # The extracted binary's exec bit is restored here, in the producing
  # action, so neither this rule's own probe nor any downstream consumer
  # (rules/deno.bzl's every deno invocation used to) has to chmod an input.
  out_dir = _download_and_extract(
    ctx,
    name,
    ctx.attrs.url,
    ctx.attrs.sha256,
    ctx.attrs.archive,
    _executable_bits([ctx.attrs.expected]),
  )
  if not ctx.attrs.probe:
    return [DefaultInfo(default_outputs = [out_dir, _skipped_stamp(ctx, name)])]
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "BIN=\"$1/%s\"" % ctx.attrs.expected,
      "\"$BIN\" %s >\"$2\" 2>&1 || { cat \"$2\" >&2; exit 1; }" % ctx.attrs.probe_arg,
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
    env = action_env(),
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

# gcc-musl supplies the musl loader required by the pinned Python runtime.
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
  out_dir = _download_and_extract(
    ctx,
    name,
    ctx.attrs.url,
    ctx.attrs.sha256,
    ctx.attrs.archive,
    normalize_loader + _executable_bits([ctx.attrs.loader]),
  )
  if not ctx.attrs.probe:
    return [DefaultInfo(default_outputs = [out_dir, _skipped_stamp(ctx, name)])]
  stamp = ctx.actions.declare_output(name + ".stamp")
  probe_script = ctx.actions.write(
    name + "-probe.sh",
    ["#!/bin/sh", "set -eu", "test -x \"$1/%s\"" % ctx.attrs.loader, "printf ok >\"$2\""],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
    env = action_env(),
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
  out_dir = _download_and_extract(
    ctx,
    name,
    ctx.attrs.url,
    ctx.attrs.sha256,
    ctx.attrs.archive,
    # The loader's own exec bit is the gcc-musl rule's responsibility (this
    # rule only borrows it), so only the interpreter is restored here.
    _executable_bits([ctx.attrs.expected]),
  )
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
      "\"$LOADER\" --library-path \"$LOADER_DIR:$PYROOT/python/lib\" \"$PY\" -I -c " +
      "'import ctypes, hashlib, sqlite3, ssl, sys, zlib; print(sys.version)' >\"$OUT\" 2>&1 || { cat \"$OUT\" >&2; exit 1; }",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", probe_script, out_dir, gcc_out, stamp.as_output()]),
    category = "probe_toolchain",
    identifier = name,
    env = action_env(),
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
