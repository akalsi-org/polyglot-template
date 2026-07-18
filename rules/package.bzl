"""Generic `package()` / `package_smoke` / `package_manifest_test` rules for
the packages/ lane.

`package()` replaces the old hand-shaped `polyglot_package` rule (which knew
the exact set of polyglot-demo's five deps by name). It instead folds
PackageInfo (see rules/pkg.bzl) off an arbitrary `deps` list - each dep is any
packaging-aware target (cxx_binary, go_binary, py_library/py_extension/
py_binary, vite_build, or a cxx_library/go_library/py_library sitting
transitively underneath one) - and:

  1. Flattens the folded transitive_set of PackageEntry records (see
     rules/pkg.bzl's module docstring for what calls package_info() and what
     each lane emits) and fails loudly on any two entries claiming the same
     `dest`, naming both entries' owning targets.
  2. Flattens + dedupes the folded symbolic runtime `needs` (e.g.
     "musl-loader", "python-runtime") and resolves each ONCE against this
     project's pinned toolchain targets (see _NEED_RESOLVERS below) -
     version-aware in the sense that there is exactly one resolution per need
     per native target, so two deps requesting the same need always get the
     identical resolved artifact; this is asserted, not just assumed (see
     _resolve_needs).
  3. Runs one "kind handler" per entry kind ("loader-bin", "static-bin",
     "tree", "py-app-launcher" - see _stage_lines below) to stage the layout,
     the same shape tools/package_release.py's assemble_polyglot_demo() +
     write_launcher() + write_python_runtime_launcher() built by hand for the
     one polyglot-demo package (same relative launcher paths, same
     "#!/bin/sh\\nset -eu\\n" preamble, same loader --library-path wiring).
  4. Reuses the EXISTING deterministic archive + metadata machinery this file
     already had (sorted entries, uid/gid 0, mtime 0, mode normalization,
     gzip mtime 0; package.json/closure.json/runtime-ref.json via the
     embedded package_model metadata step) unchanged - see _ARCHIVE_PY /
     _METADATA_PY below, both byte-for-byte the same as before this rewrite.

package_smoke and package_manifest_test are unchanged from before this
rewrite (package_smoke only ever consumed `package()`'s DefaultInfo output
artifact, never its internal shape).

CRITICAL ARTIFACT-TRACKING LAW (see rules/pkg.bzl's module docstring, which
repeats rules/go.bzl's / rules/python.bzl's own copy of this law): every
PackageEntry.artifact embedded as literal path TEXT inside the staging script
below is also passed into that action's `hidden` list directly, and so is
every toolchain directory a resolved-need artifact was `.project()`ed from.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//rules:pkg.bzl", "PackageEntry", "PackageInfo", "flatten_package_entries", "flatten_package_needs")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors rules/{cxx,go,python}.bzl's _native_target(): every already-
  # ported lane only ever builds the host's own musl output triplet, so
  # package() inherits that same native-only constraint.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]
_LOADER_NAME = "ld-musl-x86_64.so.1" if _NATIVE_TARGET.startswith("x86_64") else "ld-musl-aarch64.so.1"

_PYTHON = TOOLCHAINS["python"][_NATIVE_TARGET]
_PY_ROOT_REL = _PYTHON["expected"].rsplit("/", 2)[0]  # "python", matches rules/python.bzl's _PY_ROOT_REL

_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

_DIGITS = "0123456789"

def _is_digits(s):
  if s == "":
    return False
  for c in s.elems():
    if c not in _DIGITS:
      return False
  return True

def _check_version(ctx, version):
  # Hand-rolled ^[0-9]+\.[0-9]+\.[0-9]+$ check: Starlark (buck2's dialect)
  # has no `re` module, so this walks the "major.minor.patch" shape
  # explicitly rather than pattern-matching.
  parts = version.split(".")
  ok = len(parts) == 3 and _is_digits(parts[0]) and _is_digits(parts[1]) and _is_digits(parts[2])
  if not ok:
    fail("package({}): version {!r} must match ^[0-9]+\\.[0-9]+\\.[0-9]+$ (major.minor.patch)".format(ctx.attrs.name, version))

# --- need resolution: exactly one resolver per symbolic need, keyed off this
# project's pinned toolchain targets. Because each need has exactly one
# static resolver (there is no per-instance choice of "which musl-loader"),
# two deps requesting the same need can never actually disagree - but the
# fold below still asserts it explicitly rather than silently assuming it,
# so a future second resolver path (e.g. a versioned musl-loader choice)
# can't silently produce two different artifacts under one `dest`.

def _resolve_musl_loader(ctx):
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  loader = gcc_dir.project(_GCC["loader"])
  libc = gcc_dir.project(_LOADER_DIR + "/libc.so")
  owner = "//toolchains:gcc-musl-" + _NATIVE_TARGET
  return [
    PackageEntry(dest = "lib/" + _LOADER_NAME, artifact = loader, kind = "tree", owner = owner),
    PackageEntry(dest = "lib/libc.so", artifact = libc, kind = "tree", owner = owner),
  ]

def _resolve_python_runtime(ctx):
  python_runtime_dir = ctx.attrs._python_runtime[DefaultInfo].default_outputs[0].project(_PY_ROOT_REL)
  owner = "//toolchains:python-" + _NATIVE_TARGET
  return [PackageEntry(dest = "runtime/python", artifact = python_runtime_dir, kind = "tree", owner = owner)]

_NEED_RESOLVERS = {
  "musl-loader": _resolve_musl_loader,
  "python-runtime": _resolve_python_runtime,
}

def _resolve_needs(ctx, needs):
  resolved = {}  # need name -> list[PackageEntry], resolved exactly once
  extra_entries = []
  for need in needs:
    resolver = _NEED_RESOLVERS.get(need)
    if resolver == None:
      fail("package({}): unknown runtime need {!r} (no resolver registered in rules/package.bzl's _NEED_RESOLVERS)".format(ctx.attrs.name, need))
    if need in resolved:
      # Unreachable today (each need has exactly one static resolver), but
      # kept as a real assertion rather than dead code: a future need with
      # more than one candidate resolver must still resolve identically
      # everywhere it's requested.
      fail("package({}): need {!r} resolved more than once".format(ctx.attrs.name, need))
    entries = resolver(ctx)
    resolved[need] = entries
    extra_entries += entries
  return extra_entries

def _check_collisions(ctx, entries):
  by_dest = {}
  for entry in entries:
    other = by_dest.get(entry.dest)
    if other != None:
      fail("package({}): dest collision at {!r} between {} and {}".format(ctx.attrs.name, entry.dest, other.owner, entry.owner))
    by_dest[entry.dest] = entry

# --- kind handlers: build the `stage.sh` script body that lays out every
# entry under $OUT, plus the two synthesized launcher families
# ("loader-bin" -> bin/<name>; "py-app-launcher" -> bin/<name> + bin/python +
# bin/python3).

def _dirname(path):
  return path.rsplit("/", 1)[0] if "/" in path else ""

def _stage_lines(ctx, entries, needs):
  lines = []
  hidden = []

  # One mkdir -p covering every entry's own parent dir, plus bin/ (every
  # loader-bin/py-app-launcher entry writes there) and lib/ (whenever the
  # musl loader is staged) up front, so later cp/cat lines never race a
  # missing parent directory.
  dirs = {"bin": True}
  if "musl-loader" in needs:
    dirs["lib"] = True
  for entry in entries:
    d = _dirname(entry.dest)
    if d:
      dirs[d] = True
  lines.append("mkdir -p " + " ".join(["\"$OUT/{}\"".format(d) for d in sorted(dirs.keys())]))

  # Stage every entry that carries a real artifact (kinds "tree",
  # "static-bin", "loader-bin" - a "py-app-launcher" entry's artifact is
  # always None, handled separately below). Whether the artifact is a
  # directory or a single file is only known at RUN time (Starlark doesn't
  # see through an Artifact to know which), so the staging line branches on
  # `-d` rather than needing separate kind-specific Starlark-side handling
  # for "tree" vs "static-bin" - this is the "plain staging" the contract's
  # kind-handler table means by grouping those two kinds together.
  idx = 0
  for entry in entries:
    if entry.artifact == None:
      continue
    idx += 1
    var = "ART{}".format(idx)
    lines.append(cmd_args("{}=\"$(pwd)/".format(var), entry.artifact, "\"", delimiter = ""))
    hidden.append(entry.artifact)
    dest = "\"$OUT/{}\"".format(entry.dest)
    lines.append(
      "if [ -d \"${var}\" ]; then mkdir -p {dest}; cp -R \"${var}\"/. {dest}/; else cp \"${var}\" {dest}; chmod 0755 {dest}; fi".format(
        var = var,
        dest = dest,
      ),
    )
    # Same __pycache__/*.pyc exclusion tools/package_release.py's
    # copy_tree(..., ignore=(...)) applies to runtime/python and
    # app/python/{lib,app}: any of these directories may carry
    # probe/build-time bytecode cache files that are not part of the
    # deterministic package contract.
    if entry.dest == "runtime/python" or entry.dest.startswith("app/python/"):
      lines.append("find {dest} -name __pycache__ -type d -prune -exec rm -rf {{}} + 2>/dev/null || true".format(dest = dest))
      lines.append("find {dest} -name '*.pyc' -delete 2>/dev/null || true".format(dest = dest))

  # "loader-bin": bin/<name> launcher exec'ing the staged libexec/<name>
  # binary through the staged lib/<loader>. Byte-for-byte
  # write_launcher()'s shape.
  for entry in entries:
    if entry.kind != "loader-bin":
      continue
    name = entry.dest.rsplit("/", 1)[-1]
    lines += [
      "cat > \"$OUT/bin/{name}\" <<'PKGEOF'".format(name = name),
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
      "exec \"$ROOT/lib/{loader}\" --library-path \"$ROOT/lib\" \"$ROOT/{dest}\" \"$@\"".format(loader = _LOADER_NAME, dest = entry.dest),
      "PKGEOF",
      "chmod 0755 \"$OUT/bin/{name}\"".format(name = name),
    ]

  # "py-app-launcher": bin/<name> (execing this same target's own
  # app/python/app/* source entry, paired by `owner`) plus the shared
  # bin/python + bin/python3 loader-wrapped interpreter launchers (written
  # once total, byte-for-byte write_python_runtime_launcher()'s shape).
  main_by_owner = {}
  for entry in entries:
    if entry.kind == "tree" and entry.dest.startswith("app/python/app/"):
      main_by_owner[entry.owner] = entry.dest

  wrote_python_launchers = False
  for entry in entries:
    if entry.kind != "py-app-launcher":
      continue
    name = entry.dest.rsplit("/", 1)[-1]
    main_dest = main_by_owner.get(entry.owner)
    if main_dest == None:
      fail("package({}): py-app-launcher entry for dest {!r} (target {}) has no matching app/python/app/* source entry from the same target".format(ctx.attrs.name, entry.dest, entry.owner))
    lines += [
      "cat > \"$OUT/bin/{name}\" <<'PKGEOF'".format(name = name),
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
      "export PYTHONPATH=\"$ROOT/app/python/lib:$ROOT/app/python/app\"",
      "exec \"$ROOT/bin/python\" \"$ROOT/{main_dest}\" \"$@\"".format(main_dest = main_dest),
      "PKGEOF",
      "chmod 0755 \"$OUT/bin/{name}\"".format(name = name),
    ]
    if not wrote_python_launchers:
      wrote_python_launchers = True
      for py in ("python", "python3"):
        lines += [
          "cat > \"$OUT/bin/{py}\" <<'PKGEOF'".format(py = py),
          "#!/bin/sh",
          "set -eu",
          "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
          "exec \"$ROOT/lib/{loader}\" --library-path \"$ROOT/lib:$ROOT/runtime/python/lib\" \"$ROOT/runtime/python/bin/python3\" \"$@\"".format(loader = _LOADER_NAME),
          "PKGEOF",
          "chmod 0755 \"$OUT/bin/{py}\"".format(py = py),
        ]

  return lines, hidden

# --- package(): fold deps' PackageInfo, resolve needs, stage, then archive.

def _package_impl(ctx: AnalysisContext) -> list[Provider]:
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  _check_version(ctx, ctx.attrs.version)

  infos = [d[PackageInfo] for d in ctx.attrs.deps if PackageInfo in d]
  entries = []
  for info in infos:
    entries += flatten_package_entries(info)
  needs = []
  seen_needs = {}
  for info in infos:
    for need in flatten_package_needs(info):
      if need not in seen_needs:
        seen_needs[need] = True
        needs.append(need)

  _check_collisions(ctx, entries)
  extra_entries = _resolve_needs(ctx, needs)
  all_entries = entries + extra_entries
  _check_collisions(ctx, all_entries)  # entries vs. toolchain-resolved extras

  stage_lines, stage_hidden = _stage_lines(ctx, all_entries, needs)

  stage = ctx.actions.declare_output(ctx.attrs.name + "-stage", dir = True)
  metadata_script = ctx.actions.write(ctx.attrs.name + "-metadata.py", _METADATA_PY)
  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("OUT=\"$(pwd)/", stage.as_output(), "\"", delimiter = ""),
  ] + stage_lines + [
    # package.json/closure.json/runtime-ref.json: same metadata.py step as
    # before this rewrite, inside this one staging action (a second action
    # can't keep writing into an already-declared output directory),
    # invoking the real root-exported //:package_model.py module - see
    # _METADATA_PY below.
    cmd_args("METADATA_SCRIPT=\"$(pwd)/", metadata_script, "\"", delimiter = ""),
    cmd_args("PACKAGE_MODEL=\"$(pwd)/", ctx.attrs._package_model, "\"", delimiter = ""),
    cmd_args("MANIFEST=\"$(pwd)/", ctx.attrs.manifest, "\"", delimiter = ""),
    cmd_args("LOCK=\"$(pwd)/", ctx.attrs.lock, "\"", delimiter = ""),
    cmd_args("TOOLS_LOCK=\"$(pwd)/", ctx.attrs.tools_lock, "\"", delimiter = ""),
    "python3 -B \"$METADATA_SCRIPT\" \"$PACKAGE_MODEL\" \"$MANIFEST\" \"$LOCK\" \"$TOOLS_LOCK\" %s %s %s \"$OUT\"" % (ctx.attrs.name, _NATIVE_TARGET, ctx.attrs.profile),
  ]
  metadata_inputs = [ctx.attrs._package_model, ctx.attrs.manifest, ctx.attrs.lock, ctx.attrs.tools_lock]
  stage_inputs = stage_hidden + [metadata_script] + metadata_inputs
  stage_script, stage_written = ctx.actions.write(ctx.attrs.name + "-stage.sh", lines, is_executable = True, allow_args = True)
  ctx.actions.run(
    cmd_args(["/bin/sh", stage_script, stage.as_output()], hidden = stage_inputs + stage_written),
    category = "package_stage",
    identifier = ctx.attrs.name,
  )

  archive = ctx.actions.declare_output(ctx.attrs.name + ".tar.gz")
  archive_script = ctx.actions.write(ctx.attrs.name + "-archive.py", _ARCHIVE_PY)
  ctx.actions.run(
    cmd_args(["python3", archive_script, stage, archive.as_output()]),
    category = "package_archive",
    identifier = ctx.attrs.name,
  )

  return [DefaultInfo(
    default_outputs = [archive],
    sub_targets = {"stage": [DefaultInfo(default_output = stage)]},
  )]

# Byte-for-byte port of tools/package_release.py's reset_tarinfo() +
# make_archive() (unchanged from before this rewrite): sorted(stage.rglob("*"))
# entry order, uid=gid=0, uname=gname="root", mtime=0 on every tar member,
# dirs 0755, any executable bit -> 0755, everything else -> 0644, and the
# gzip wrapper itself written with mtime=0 (gzip.GzipFile(..., mtime=0)) so
# the compressed bytes are reproducible too, not just the uncompressed tar
# bytes.
_ARCHIVE_PY = [
  "import gzip, sys, tarfile",
  "from pathlib import Path",
  "",
  "stage = Path(sys.argv[1])",
  "archive = Path(sys.argv[2])",
  "",
  "",
  "def reset(info):",
  "    info.uid = 0",
  "    info.gid = 0",
  "    info.uname = 'root'",
  "    info.gname = 'root'",
  "    info.mtime = 0",
  "    if info.isdir():",
  "        info.mode = 0o755",
  "    elif info.mode & 0o111:",
  "        info.mode = 0o755",
  "    else:",
  "        info.mode = 0o644",
  "    return info",
  "",
  "",
  "archive.parent.mkdir(parents=True, exist_ok=True)",
  "with archive.open('wb') as raw:",
  "    with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as zipped:",
  "        with tarfile.open(fileobj=zipped, mode='w') as tar:",
  "            for path in sorted(stage.rglob('*')):",
  "                tar.add(str(path), arcname=path.relative_to(stage).as_posix(), recursive=False, filter=reset)",
]

# package.json/closure.json/runtime-ref.json: reuses package_model.py's own
# packages()/resolve() (imported as a real module, not re-implemented),
# unchanged from before this rewrite - the package_name argument is now
# ctx.attrs.name (the package() target's own name, e.g. "polyglot-demo")
# rather than a separate `package_name` attr, since the generic rule has no
# reason to let those two diverge.
_METADATA_PY = [
  "import json, sys",
  "from pathlib import Path",
  "",
  # Do not write bytecode cache and do not resolve() the exported
  # //:package_model.py artifact: buck2's export_file() output is typically
  # a symlink back to the real tools/package_model.py, and resolve()-ing it
  # before sys.path insertion would make CPython treat that real source
  # directory as the import root, writing __pycache__ there from inside a
  # build action - a hermeticity/side-effect footgun this avoids entirely.
  "sys.dont_write_bytecode = True",
  "package_model_path = Path(sys.argv[1])",
  "sys.path.insert(0, str(package_model_path.parent))",
  "import package_model",
  "",
  "manifest = Path(sys.argv[2])",
  "lock = Path(sys.argv[3])",
  "tools_lock = Path(sys.argv[4])",
  "package_name = sys.argv[5]",
  "target = sys.argv[6]",
  "profile = sys.argv[7]",
  "stage = Path(sys.argv[8])",
  "",
  "model = package_model.load(manifest)",
  "catalog = package_model.packages(model)",
  "entry = catalog[package_name]",
  "closure, runtime_ref = package_model.resolve(model, package_model.load(lock), package_model.load(tools_lock), package_name, target)",
  "",
  "",
  "def write_json(path, value):",
  "    path.write_text(json.dumps(value, sort_keys=True, indent=2) + '\\n', encoding='utf-8')",
  "",
  "",
  "metadata = {",
  "    'schema_version': 1,",
  "    'package': package_name,",
  "    'version': entry['version'],",
  "    'target': target,",
  "    'profile': profile,",
  "    'executables': list(entry['executables']),",
  "    'closure_sha256': closure['closure_sha256'],",
  "}",
  "write_json(stage / 'package.json', metadata)",
  "write_json(stage / 'closure.json', closure)",
  "if runtime_ref:",
  "    write_json(stage / 'runtime-ref.json', runtime_ref)",
]

_package_rule = rule(
  impl = _package_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(), default = []),
    "lock": attrs.source(default = "//:runtime-resolution.lock.toml"),
    "manifest": attrs.source(default = "//:package.toml"),
    "profile": attrs.string(default = select({"//config:opt": "opt", "DEFAULT": "dbg"})),
    "tools_lock": attrs.source(default = "//:tools.lock.toml"),
    "version": attrs.string(),
    "_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_package_model": attrs.source(default = "//:package_model.py"),
    "_python_runtime": attrs.dep(default = "//toolchains:python-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_target_arch": target_arch_attr(),
  },
)

def package(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_rule(**kwargs)

# --- package_smoke: extract the already-built archive and exercise every
# bin/ executable plus the packaged web assets. No toolchain deps at all -
# every binary in the archive is either static (go-hello) or already
# loader-wrapped (cpp-hello, python, python3, python-hello), exactly like
# tools/package_release.py's smoke()'s --execute path, but without needing
# host toolchain state to run it. Unchanged from before this rewrite.

def _package_smoke_impl(ctx: AnalysisContext) -> list[Provider]:
  archive = ctx.attrs.package[DefaultInfo].default_outputs[0]
  smoke_script = ctx.attrs.smoke_script

  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("ARCHIVE=\"$(pwd)/", archive, "\"", delimiter = ""),
    cmd_args("SMOKE_SCRIPT=\"$(pwd)/", smoke_script, "\"", delimiter = ""),
    # Anchored under buck-out, not bare mktemp -d (which defaults to /tmp) -
    # host /tmp is a small tmpfs shared by every concurrent build/test on
    # this machine (see rules/go.bzl's _write_go_script for the identical
    # rationale).
    "SCRATCH_BASE=\"$(pwd)/buck-out/v2/tmp/package-smoke\"",
    "mkdir -p \"$SCRATCH_BASE\"",
    "WORK=$(mktemp -d \"$SCRATCH_BASE/tmp.XXXXXX\")",
    "trap 'rm -rf \"$WORK\"' EXIT",
    "tar -xzf \"$ARCHIVE\" -C \"$WORK\"",
    "\"$WORK/bin/cpp-hello\"",
    "\"$WORK/bin/go-hello\"",
    "\"$WORK/bin/python-hello\"",
    "\"$WORK/bin/python\" --version >/dev/null",
    "\"$WORK/bin/python3\" --version >/dev/null",
    "python3 \"$SMOKE_SCRIPT\" --root \"$WORK/app/web\"",
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = [archive, smoke_script] + written)
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "package_smoke",
      command = [command],
      run_from_project_root = True,
    ),
  ]

_package_smoke_rule = rule(
  impl = _package_smoke_impl,
  attrs = {
    "package": attrs.dep(providers = [DefaultInfo]),
    "smoke_script": attrs.source(),
  },
)

def package_smoke(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_smoke_rule(**kwargs)

# --- package_manifest_test: `python3 package_model.py validate`, repo.sh's
# package-validate lane, taking the root-exported //:package_model.py itself
# as a real tracked attrs.source() input alongside the root-exported
# manifest/lock/tools-lock files, so an edit to any of the four invalidates
# this test's buck2 cache entry. Unchanged from before this rewrite.

def _package_manifest_test_impl(ctx: AnalysisContext) -> list[Provider]:
  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("PACKAGE_MODEL=\"$(pwd)/", ctx.attrs.package_model, "\"", delimiter = ""),
    cmd_args("MANIFEST=\"$(pwd)/", ctx.attrs.manifest, "\"", delimiter = ""),
    cmd_args("LOCK=\"$(pwd)/", ctx.attrs.lock, "\"", delimiter = ""),
    cmd_args("TOOLS_LOCK=\"$(pwd)/", ctx.attrs.tools_lock, "\"", delimiter = ""),
    "exec python3 -B \"$PACKAGE_MODEL\" --manifest \"$MANIFEST\" --lock \"$LOCK\" --tools-lock \"$TOOLS_LOCK\" validate",
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = [ctx.attrs.package_model, ctx.attrs.manifest, ctx.attrs.lock, ctx.attrs.tools_lock] + written)
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "package_manifest_validation",
      command = [command],
      run_from_project_root = True,
    ),
  ]

_package_manifest_test_rule = rule(
  impl = _package_manifest_test_impl,
  attrs = {
    "lock": attrs.source(),
    "manifest": attrs.source(),
    "package_model": attrs.source(default = "//:package_model.py"),
    "tools_lock": attrs.source(),
  },
)

def package_manifest_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_manifest_test_rule(**kwargs)

