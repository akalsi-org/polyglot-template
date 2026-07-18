"""First-party polyglot_package / package_smoke / package_manifest_test rules
for the packages/ lane.

`polyglot_package` stages the polyglot-demo release layout in-graph, mirroring
tools/package_release.py's assemble_polyglot_demo() + write_launcher() +
write_python_runtime_launcher() byte-for-byte (same relative launcher paths,
same "#!/bin/sh\nset -eu\n" preamble, same loader --library-path wiring), then
produces a deterministic tar.gz with the same normalization as
tools/package_release.py's make_archive()/reset_tarinfo(): entries sorted by
full relative path, uid=gid=0, uname=gname="root", mtime=0, dirs 0755,
executables 0755, everything else 0644, and a gzip wrapper written with
mtime=0 so the compressed bytes (not just the tar bytes) are reproducible.

Every piece is drawn from the already-ported lane providers (READ-ONLY
consumption of rules/{cxx,go,python,deno,toolchain}.bzl - never edited here):
  - //cpp/app/hello:hello's DefaultInfo.default_outputs[0] is the raw
    (non-loader-wrapped) cxx_binary output, exactly what
    assemble_polyglot_demo copies to libexec/cpp-hello before writing its own
    bin/cpp-hello launcher.
  - //go:hello's DefaultInfo.default_outputs[0] is the static (CGO_ENABLED=0)
    go_binary output, copied straight to bin/go-hello.
  - //python/lib:example and //python/lib:fastbytes are consumed via
    DefaultInfo (a single source file / a pre-staged `fastbytes/` package
    dir respectively - see rules/python.bzl's py_library/py_extension).
  - //python/app:hello (py_binary) has no DefaultInfo output for its own
    `main` source (its default_output is the loader-wrapped launcher script,
    which this rule does not want - it builds its own). main.py is instead
    recovered from the py_binary's PyInfo.srcs, which rules/python.bzl always
    appends `main` to last; `_pick_short_path` locates it by its known
    package-relative short_path ("hello/main.py") rather than by position,
    so this stays correct even if rules/python.bzl's dep-merge order changes.
  - //toolchains:python-<target>'s extracted tree is REUSED as-is for
    runtime/python instead of downloading the cpython archive a second time:
    package.toml's runtime-python artifact entries and tools.lock.toml's
    python toolchain artifact entries pin the identical filename+sha256 for
    both targets (verified by hand against both toml files), so this project()
    just points at the same "python/" subdirectory rules/python.bzl already
    projects for py_binary/py_test's own interpreter path.
  - //toolchains:gcc-musl-<target>'s extracted tree supplies the musl loader
    + libc.so staged under lib/, the same closure rules/cxx.bzl's cxx_binary
    already loader-wraps its own binaries with.

package.json/closure.json/runtime-ref.json (which tools/package_release.py's
assemble() also writes into the staged tree) ARE produced here, via a small
embedded metadata.py action script that imports the now-root-exported
//:package_model.py as a real module (`sys.path.insert(0,
<package_model.py's dirname>)`) and calls its packages()/resolve() directly -
the same functions tools/package_release.py itself calls - so the closure/
runtime-ref computation and package.json's shape
(schema_version/package/version/target/profile/executables/closure_sha256)
are byte-for-byte identical to assemble()'s, including the exact
`json.dumps(value, sort_keys=True, indent=2) + "\n"` formatting
package_model.py's own write_json() and package_release.py's stable_json()
both use.

package_smoke deliberately touches no toolchain target (no gcc/python/go
deps): it only extracts the already-built archive and runs the
already-self-contained (loader-wrapped or static) binaries inside it, mirror-
ing tools/package_release.py's smoke()'s "packaged executable" checks without
any host toolchain state.

package_manifest_test runs tools/package_model.py validate exactly like
repo.sh's package-validate case, taking //:package_model.py itself as a real
tracked `attrs.source()` input (not a project-root-relative string) alongside
the root-exported manifest/lock/tools-lock files, so an edit to any of the
four invalidates this test's buck2 cache entry.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//rules:python.bzl", "PyInfo")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors rules/{cxx,go,python}.bzl's _native_target(): every already-
  # ported lane only ever builds the host's own musl output triplet, so
  # polyglot_package inherits that same native-only constraint rather than
  # introducing per-instance cross-target packaging ahead of the lanes it
  # stages.
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

def _pick_short_path(srcs, short_path):
  matches = [a for a in srcs if a.short_path == short_path]
  if len(matches) != 1:
    fail("polyglot_package: expected exactly one source at {!r} (got {})".format(short_path, len(matches)))
  return matches[0]

# --- polyglot_package: stage the layout, then archive it deterministically.

def _polyglot_package_impl(ctx: AnalysisContext) -> list[Provider]:
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  loader = gcc_dir.project(_GCC["loader"])
  libc = gcc_dir.project(_LOADER_DIR + "/libc.so")

  python_runtime_dir = ctx.attrs._python_runtime[DefaultInfo].default_outputs[0].project(_PY_ROOT_REL)

  cpp_bin = ctx.attrs.cpp_hello[DefaultInfo].default_outputs[0]
  go_bin = ctx.attrs.go_hello[DefaultInfo].default_outputs[0]
  example_src = ctx.attrs.python_lib_example[DefaultInfo].default_outputs[0]
  fastbytes_pkg = ctx.attrs.python_lib_fastbytes[DefaultInfo].default_outputs[0]
  main_src = _pick_short_path(ctx.attrs.python_app[PyInfo].srcs, "hello/main.py")
  web_dir = ctx.attrs.web_site[DefaultInfo].default_outputs[0]

  stage = ctx.actions.declare_output(ctx.attrs.name + "-stage", dir = True)
  metadata_script = ctx.actions.write(ctx.attrs.name + "-metadata.py", _METADATA_PY)
  stage_lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("OUT=\"$(pwd)/", stage.as_output(), "\"", delimiter = ""),
    cmd_args("CPP=\"$(pwd)/", cpp_bin, "\"", delimiter = ""),
    cmd_args("GO=\"$(pwd)/", go_bin, "\"", delimiter = ""),
    cmd_args("PYRT=\"$(pwd)/", python_runtime_dir, "\"", delimiter = ""),
    cmd_args("PYEX=\"$(pwd)/", example_src, "\"", delimiter = ""),
    cmd_args("PYFB=\"$(pwd)/", fastbytes_pkg, "\"", delimiter = ""),
    cmd_args("PYMAIN=\"$(pwd)/", main_src, "\"", delimiter = ""),
    cmd_args("WEB=\"$(pwd)/", web_dir, "\"", delimiter = ""),
    cmd_args("LOADER=\"$(pwd)/", loader, "\"", delimiter = ""),
    cmd_args("LIBC=\"$(pwd)/", libc, "\"", delimiter = ""),
    "mkdir -p \"$OUT/bin\" \"$OUT/lib\" \"$OUT/libexec\" \"$OUT/runtime\" \"$OUT/app/python/lib/example\" \"$OUT/app/python/app/hello\" \"$OUT/app/web\"",
    "cp \"$CPP\" \"$OUT/libexec/cpp-hello\"",
    "chmod 0755 \"$OUT/libexec/cpp-hello\"",
    "cp \"$GO\" \"$OUT/bin/go-hello\"",
    "chmod 0755 \"$OUT/bin/go-hello\"",
    "cp -R \"$PYRT\"/. \"$OUT/runtime/python\"/",
    # Match tools/package_release.py's copy_tree(..., ignore=("__pycache__",
    # "*.pyc")): the reused toolchain tree may carry probe-generated
    # bytecode cache files (rules/toolchain.bzl's python_toolchain probe
    # imports stdlib modules against this same extracted tree), which are
    # not part of the deterministic package contract.
    "find \"$OUT/runtime/python\" -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true",
    "find \"$OUT/runtime/python\" -name '*.pyc' -delete 2>/dev/null || true",
    "cp \"$PYEX\" \"$OUT/app/python/lib/example/example.py\"",
    "cp -R \"$PYFB\"/. \"$OUT/app/python/lib\"/",
    "cp \"$PYMAIN\" \"$OUT/app/python/app/hello/main.py\"",
    "cp -R \"$WEB\"/. \"$OUT/app/web\"/",
    # Same __pycache__/*.pyc exclusion as tools/package_release.py's
    # copy_tree(..., ignore=(...)) for python/lib and python/app: the
    # fastbytes py_extension stage / interpreter probe actions may have
    # left compiled bytecode behind (see the runtime/python cleanup above).
    "find \"$OUT/app/python\" -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true",
    "find \"$OUT/app/python\" -name '*.pyc' -delete 2>/dev/null || true",
    "cp \"$LOADER\" \"$OUT/lib/%s\"" % _LOADER_NAME,
    "chmod 0755 \"$OUT/lib/%s\"" % _LOADER_NAME,
    "cp \"$LIBC\" \"$OUT/lib/libc.so\"",
    "chmod 0755 \"$OUT/lib/libc.so\"",
    # bin/python + bin/python3: byte-for-byte
    # write_python_runtime_launcher() output (relative()-anchored library
    # path "lib:runtime/python/lib", loader "lib/<loader_name>", python
    # "runtime/python/bin/python3" - fixed given the stage layout above).
    "cat > \"$OUT/bin/python\" <<'PYEOF'",
    "#!/bin/sh",
    "set -eu",
    "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
    "exec \"$ROOT/lib/%s\" --library-path \"$ROOT/lib:$ROOT/runtime/python/lib\" \"$ROOT/runtime/python/bin/python3\" \"$@\"" % _LOADER_NAME,
    "PYEOF",
    "chmod 0755 \"$OUT/bin/python\"",
    "cat > \"$OUT/bin/python3\" <<'PYEOF'",
    "#!/bin/sh",
    "set -eu",
    "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
    "exec \"$ROOT/lib/%s\" --library-path \"$ROOT/lib:$ROOT/runtime/python/lib\" \"$ROOT/runtime/python/bin/python3\" \"$@\"" % _LOADER_NAME,
    "PYEOF",
    "chmod 0755 \"$OUT/bin/python3\"",
    # bin/cpp-hello: write_launcher() output, loader-wrapping libexec/cpp-hello.
    "cat > \"$OUT/bin/cpp-hello\" <<'CPPEOF'",
    "#!/bin/sh",
    "set -eu",
    "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
    "exec \"$ROOT/lib/%s\" --library-path \"$ROOT/lib\" \"$ROOT/libexec/cpp-hello\" \"$@\"" % _LOADER_NAME,
    "CPPEOF",
    "chmod 0755 \"$OUT/bin/cpp-hello\"",
    # bin/python-hello: write_launcher() output.
    "cat > \"$OUT/bin/python-hello\" <<'PYHEOF'",
    "#!/bin/sh",
    "set -eu",
    "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
    "export PYTHONPATH=\"$ROOT/app/python/lib:$ROOT/app/python/app\"",
    "exec \"$ROOT/bin/python\" \"$ROOT/app/python/app/hello/main.py\" \"$@\"",
    "PYHEOF",
    "chmod 0755 \"$OUT/bin/python-hello\"",
    # package.json/closure.json/runtime-ref.json: same metadata.py step,
    # inside this one staging action (a second action can't keep writing
    # into an already-declared output directory), invoking the real
    # root-exported //:package_model.py module - see _METADATA_PY below.
    cmd_args("METADATA_SCRIPT=\"$(pwd)/", metadata_script, "\"", delimiter = ""),
    cmd_args("PACKAGE_MODEL=\"$(pwd)/", ctx.attrs._package_model, "\"", delimiter = ""),
    cmd_args("MANIFEST=\"$(pwd)/", ctx.attrs.manifest, "\"", delimiter = ""),
    cmd_args("LOCK=\"$(pwd)/", ctx.attrs.lock, "\"", delimiter = ""),
    cmd_args("TOOLS_LOCK=\"$(pwd)/", ctx.attrs.tools_lock, "\"", delimiter = ""),
    "python3 -B \"$METADATA_SCRIPT\" \"$PACKAGE_MODEL\" \"$MANIFEST\" \"$LOCK\" \"$TOOLS_LOCK\" %s %s %s \"$OUT\"" % (ctx.attrs.package_name, _NATIVE_TARGET, ctx.attrs.profile),
  ]
  metadata_inputs = [ctx.attrs._package_model, ctx.attrs.manifest, ctx.attrs.lock, ctx.attrs.tools_lock]
  stage_inputs = [cpp_bin, go_bin, python_runtime_dir, example_src, fastbytes_pkg, main_src, web_dir, loader, libc, metadata_script] + metadata_inputs
  stage_script, stage_written = ctx.actions.write(ctx.attrs.name + "-stage.sh", stage_lines, is_executable = True, allow_args = True)
  ctx.actions.run(
    cmd_args(["/bin/sh", stage_script, stage.as_output()], hidden = stage_inputs + stage_written),
    category = "polyglot_package_stage",
    identifier = ctx.attrs.name,
  )

  archive = ctx.actions.declare_output(ctx.attrs.name + ".tar.gz")
  archive_script = ctx.actions.write(ctx.attrs.name + "-archive.py", _ARCHIVE_PY)
  ctx.actions.run(
    cmd_args(["python3", archive_script, stage, archive.as_output()]),
    category = "polyglot_package_archive",
    identifier = ctx.attrs.name,
  )

  return [DefaultInfo(
    default_outputs = [archive],
    sub_targets = {"stage": [DefaultInfo(default_output = stage)]},
  )]

# Byte-for-byte port of tools/package_release.py's reset_tarinfo() +
# make_archive(): sorted(stage.rglob("*")) entry order, uid=gid=0,
# uname=gname="root", mtime=0 on every tar member, dirs 0755, any executable
# bit -> 0755, everything else -> 0644, and the gzip wrapper itself written
# with mtime=0 (gzip.GzipFile(..., mtime=0)) so the compressed bytes are
# reproducible too, not just the uncompressed tar bytes.
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
# packages()/resolve() (imported as a real module, not re-implemented) so
# this is byte-for-byte the same computation tools/package_release.py's
# assemble() performs, and package_model.py's write_json() /
# package_release.py's stable_json() both format as
# `json.dumps(value, sort_keys=True, indent=2) + "\n"` - matched here too.
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

_polyglot_package_rule = rule(
  impl = _polyglot_package_impl,
  attrs = {
    "cpp_hello": attrs.dep(providers = [DefaultInfo]),
    "go_hello": attrs.dep(providers = [DefaultInfo]),
    "lock": attrs.source(default = "//:runtime-resolution.lock.toml"),
    "manifest": attrs.source(default = "//:package.toml"),
    "package_name": attrs.string(default = "polyglot-demo"),
    "profile": attrs.string(default = select({"//config:opt": "opt", "DEFAULT": "dbg"})),
    "python_app": attrs.dep(providers = [PyInfo]),
    "python_lib_example": attrs.dep(providers = [DefaultInfo]),
    "python_lib_fastbytes": attrs.dep(providers = [DefaultInfo]),
    "tools_lock": attrs.source(default = "//:tools.lock.toml"),
    "web_site": attrs.dep(providers = [DefaultInfo]),
    "_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_package_model": attrs.source(default = "//:package_model.py"),
    "_python_runtime": attrs.dep(default = "//toolchains:python-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_target_arch": target_arch_attr(),
  },
)

def polyglot_package(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _polyglot_package_rule(**kwargs)

# --- package_smoke: extract the already-built archive and exercise every
# bin/ executable plus the packaged web assets. No toolchain deps at all -
# every binary in the archive is either static (go-hello) or already
# loader-wrapped (cpp-hello, python, python3, python-hello), exactly like
# tools/package_release.py's smoke()'s --execute path, but without needing
# host toolchain state to run it.

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
# this test's buck2 cache entry.

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
