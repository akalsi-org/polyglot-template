"""First-party py_library / py_binary / py_test rules for the Python lane.

py_binary and py_test wrap the pinned musl loader around the pinned CPython interpreter (mirroring repo.sh's `python` case:
`"$gcc_install/$loader" --library-path "$loader_dir:$python_install/python/lib"
"$python_install/$python_expected" "$@"`), with PYTHONPATH assembled from
each target's `deps` in the order given (mirroring repo.sh's
`build/python/$target/lib:python/lib:python/app` ordering, which is caller
-controlled here via dep order).

There is no prelude in this project, so py_test builds its own
ExternalRunnerTestInfo rather than relying on a prelude-provided python_test,
following the same pattern as rules/go.bzl's go_test.

Toolchain artifact tracking (fixed bug, mirroring rules/go.bzl's own fix):
py_binary/py_test/py_compileall_check each write a
loader-wrapped launcher script via ctx.actions.write() that embeds the gcc
loader / python3 interpreter paths as literal TEXT (via _loader_exec_prefix
 cmd_args(..., delimiter=" ")), for a naturally readable emitted script.
That text-only reference, plus the `written` list allow_args=True returns,
is not reliably enough for buck2 to track/materialize the underlying
gcc-musl/python toolchain extraction once a target is reached transitively
under a different configuration (e.g. via //packages:polyglot-demo under
//config:x86_64-linux-musl-dbg instead of this rule's own default target
platform) - the extraction is a cache hit the materializer has no reason to
realize for that other configuration, and the script fails with a path that
exists elsewhere in buck-out under a different config hash. _gcc_tools() /
_python_tools() below therefore also return the raw extracted-toolchain
Artifacts (gcc_dir, py_dir); every consuming action's/command's `hidden`
list must include both directly, not just `written`.

"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//config:flags.bzl", "coverage_enabled_flag")
load("//rules:coverage.bzl", "CoverageInfo")
load("//rules:env.bzl", "action_env")
load("//rules:host.bzl", "native_target")
load("//rules:pkg.bzl", "PACKAGE_LABELS_ATTR", "PackageEntry", "check_pkg_name", "package_info")
load("//toolchains:lock.bzl", "TOOLCHAINS")

_NATIVE_TARGET = native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_PYTHON = TOOLCHAINS["python"][_NATIVE_TARGET]
_DENO_TOOLCHAIN = "//toolchains:deno-" + _NATIVE_TARGET
_GCC_BIN_DIR = _GCC["expected"].rsplit("/", 1)[0]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]

# _PYTHON["expected"] is "python/bin/python3"; the interpreter's own install
# root ("python/") is what sysconfig.get_path("include") / the loader
# --library-path python/lib argument in repo.sh are both relative to.
_PY_ROOT_REL = _PYTHON["expected"].rsplit("/", 2)[0]
_PY_LIB_REL = _PY_ROOT_REL + "/lib"

# PySourceSet: a transitive_set of Artifact lists. Every sibling lane
# already converted its own equivalent with written rationale (rules/go.bzl's
# GoSourceSet, rules/cxx.bzl's CxxArtifactSet, rules/pkg.bzl's
# PackageEntrySet, rules/deno_sources.bzl's DenoSourceSet); python was the
# one holdout, still doing a plain `srcs += info.srcs` with no dedupe at
# every level of the graph - so a diamond (python/test depends on both
# fastbytes and example, which will share a base library) re-concatenated
# the shared library's sources once per path that reached it, and every
# consuming action's hidden-input list grew with it.
#
# `roots` stays a plain deduped list: it is short (one entry per
# py_extension package dir plus the repo-relative library roots), order is
# load-bearing because it becomes PYTHONPATH, and it mixes strings with
# Artifacts - none of which a tset expresses better than the explicit
# seen-set already here.
PySourceSet = transitive_set()

# `srcs`: a PySourceSet transitive_set. `roots`: list[str | Artifact].
PyInfo = provider(fields = ["srcs", "roots"])

def _gcc_tools(ctx):
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  # gcc_dir is kept alongside its .project()ed paths so callers can pass the
  # real extracted-toolchain Artifact into `hidden` directly. Embedding
  # gxx/bin_dir/loader/loader_dir as literal path TEXT inside a
  # ctx.actions.write()-generated script (see py_binary/py_test/
  # py_compileall_check/py_lock_consistency_test below) is not enough on its
  # own for buck2 to track/materialize the toolchain extraction across
  # configurations - see rules/go.bzl's module docstring for the bug this
  # mirrors (a target reached transitively under a different configuration,
  # e.g. via //packages:polyglot-demo, got a cache-hit-but-unmaterialized
  # toolchain dir and failed with "file not found" despite standalone builds
  # working fine).
  return struct(
    gcc_dir = gcc_dir,
    loader = gcc_dir.project(_GCC["loader"]),
    loader_dir = gcc_dir.project(_LOADER_DIR),
  )

def _python_tools(ctx):
  py_dir = ctx.attrs._python[DefaultInfo].default_outputs[0]
  # py_dir is kept for the same reason as _gcc_tools' gcc_dir above: callers
  # must pass it into `hidden` directly wherever python3/include_dir/lib_dir
  # are embedded as script text.
  return struct(
    py_dir = py_dir,
    python3 = py_dir.project(_PYTHON["expected"]),
    lib_dir = py_dir.project(_PY_LIB_REL),
  )

_TOOLCHAIN_ATTRS = {
  "_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_python": attrs.dep(default = "//toolchains:python-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_target_arch": target_arch_attr(),
}

# select() is only resolved as an attrs default (see rules/cxx.bzl's
# _PROFILE_ATTRS for the full explanation); every profile-dependent flag
# list here is threaded through as an attr for the same reason.

def _py_srcs(ctx: AnalysisContext, own_srcs: list, deps: list):
  """Mirrors rules/go.bzl's _go_srcs: builds this target's own PySourceSet
  node (own_srcs as its value, each dep's PyInfo.srcs tset as a child) and
  flattens it with a SINGLE traverse() into an order-stable, deduped list.
  Returns (flattened, tset) - the flattened list is this target's own hidden
  inputs; the tset is what its own PyInfo should carry forward so a further
  consumer's traversal stays one pass instead of re-flattening."""
  tset = ctx.actions.tset(PySourceSet, value = list(own_srcs), children = [d[PyInfo].srcs for d in deps])
  seen = {}
  flattened = []
  for value in tset.traverse():
    for src in value:
      key = str(src)
      if key not in seen:
        seen[key] = True
        flattened.append(src)
  return flattened, tset

def _merge_roots(deps):
  roots = []
  seen_roots = {}
  for dep in deps:
    for root in dep[PyInfo].roots:
      key = str(root)
      if key not in seen_roots:
        seen_roots[key] = True
        roots.append(root)
  return roots

def _pythonpath_env_lines(roots):
  if not roots:
    return []
  return [
    cmd_args("PYTHONPATH=", cmd_args(roots, delimiter = ":"), "${PYTHONPATH:+:$PYTHONPATH}", delimiter = ""),
    "export PYTHONPATH",
  ]

def _loader_exec_prefix(gcc, py):
  return [gcc.loader, "--library-path", cmd_args(gcc.loader_dir, py.lib_dir, delimiter = ":"), py.python3]

# --- py_library: source-tree grouping, mirroring go_library: no build step,
# just a PYTHONPATH root (a plain repo-relative directory string, since
# these sources live in-tree unmodified) plus the source artifacts for
# buck2's own input tracking (the pinned interpreter resolves imports off
# the real on-disk PYTHONPATH itself, not from a buck2-provided file list).

def _py_library_impl(ctx: AnalysisContext) -> list[Provider]:
  _srcs, srcs_tset = _py_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  dep_roots = _merge_roots(ctx.attrs.deps)
  roots = [ctx.attrs.root] + [r for r in dep_roots if str(r) != ctx.attrs.root]
  # Packaging: one tree entry per OWN source file (not dep_srcs - deps'
  # entries are already folded in transitively via package_info(deps=...)
  # below), staged under this target's own buck2 package path + the src's
  # own package-relative short_path - e.g. a py_library declared in
  # python/lib/example/BUCK (ctx.label.package == "python/lib/example")
  # with srcs = ["example.py"] (short_path == "example.py") stages at
  # python/lib/example/example.py - the package root mirrors the repo's own
  # top-level layout (python/, ts/, web/, ... as siblings of bin/lib/
  # libexec/runtime/share). Deliberately keyed on ctx.label.package rather
  # than short_path alone (which by itself would stage at
  # python/lib/example.py, losing the "example/" nesting
  # `import example.example`'s namespace-package resolution depends on) -
  # this is the same staged_path convention rules/deno.bzl's DenoSourcesInfo
  # uses (ctx.label.package + short_path), so the destination stays correct
  # regardless of which BUCK file a py_library is declared in.
  info = package_info(
    ctx,
    entries = [PackageEntry(dest = ctx.label.package + "/" + src.short_path, artifact = src, kind = "tree", owner = str(ctx.label.raw_target())) for src in ctx.attrs.srcs],
    deps = ctx.attrs.deps,
  )
  return [
    DefaultInfo(default_outputs = list(ctx.attrs.srcs)),
    PyInfo(srcs = srcs_tset, roots = roots),
    info,
  ]

_py_library_rule = rule(
  impl = _py_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "root": attrs.string(),
    "srcs": attrs.list(attrs.source()),
  } | PACKAGE_LABELS_ATTR,
)

# --- py_binary: loader-wrapped launcher script running the pinned
# interpreter directly on `main`, mirroring repo.sh's `python` case.

def _py_binary_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  srcs, srcs_tset = _py_srcs(ctx, [ctx.attrs.main], ctx.attrs.deps)
  roots = _merge_roots(ctx.attrs.deps)
  lines = ["#!/bin/sh", "set -eu"] + _pythonpath_env_lines(roots) + [
    cmd_args(["exec"] + _loader_exec_prefix(gcc, py) + [ctx.attrs.main, "\"$@\""], delimiter = " "),
  ]
  launcher, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(launcher, hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir])
  # Packaging: this rule's own source (`main`) stages under this target's own
  # buck2 package path + main's own package-relative short_path (e.g. a
  # py_binary declared in python/app/hello/BUCK with main = "main.py" stages
  # at python/app/hello/main.py) - see py_library's identical dest formula
  # above for why ctx.label.package (not short_path alone) is load-bearing
  # here now that a py_binary may be declared several directories below
  # python/app/ (or anywhere else - a py_binary is not required to live
  # under python/app/ at all) - plus a "py-app-launcher" marker entry (no
  # artifact of its own) at bin/<pkg_name> whose `meta` records this app's
  # own entrypoint dest directly, the same PackageEntry.meta mechanism
  # rules/deno.bzl's "deno-app-launcher" marker uses for its own entrypoint
  # (see rules/pkg.bzl's PackageEntry.meta doc comment for why pairing a
  # marker back to its source by dest-prefix/owner convention alone isn't
  # reliable - it previously assumed every py_binary's own source entry
  # started with "python/app/", which broke for any py_binary declared
  # elsewhere). rules/package.bzl's kind handler generates bin/<pkg_name> +
  # bin/python + bin/python3 from it, with PYTHONPATH assembled from every
  # folded python/* tree entry (this target's own deps' py_library/
  # py_extension entries, folded in transitively below).
  pkg_name = check_pkg_name(ctx, ctx.attrs.pkg_name or ctx.attrs.name)
  main_dest = ctx.label.package + "/" + ctx.attrs.main.short_path
  info = package_info(
    ctx,
    entries = [
      PackageEntry(dest = main_dest, artifact = ctx.attrs.main, kind = "tree", owner = str(ctx.label.raw_target())),
      PackageEntry(dest = "bin/" + pkg_name, artifact = None, kind = "py-app-launcher", owner = str(ctx.label.raw_target()), meta = main_dest),
    ],
    # bin/python + bin/python3 (the loader-wrapped interpreter launchers
    # rules/package.bzl's "py-app-launcher" kind handler also writes) need
    # the musl loader present too, not just the python runtime tree.
    needs = ["python-runtime", "musl-loader"],
    deps = ctx.attrs.deps,
  )
  return [
    DefaultInfo(default_output = launcher, other_outputs = written),
    RunInfo(args = command),
    PyInfo(srcs = srcs_tset, roots = []),
    info,
  ]

_py_binary_rule = rule(
  impl = _py_binary_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "main": attrs.source(),
    "pkg_name": attrs.option(attrs.string(), default = None),
  } | _TOOLCHAIN_ATTRS | PACKAGE_LABELS_ATTR,
)

# --- py_test: repo-local discovery over `start` (python/test by default),
# supporting both unittest.TestCase methods and top-level `test_*` functions
# (including async functions), exposed as ExternalRunnerTestInfo so `buck2
# test` runs it.

# Coverage (default-on under dbg - see rules/coverage.bzl's module
# docstring): runs the SAME repo-local discovery a second time, in-process
# under tools/py_cover.py's PEP 669 (sys.monitoring) line collector instead
# of a plain subprocess, as its own ctx.actions.run() build action
# separate from the ExternalRunnerTestInfo path `buck2 test` uses for
# pass/fail reporting (mirrors rules/cxx.bzl's/rules/go.bzl's/
# rules/deno.bzl's own coverage collection actions - see rules/coverage.bzl
# for why). Only python/lib and python/app are measured (python/test itself
# is excluded) - see tools/py_cover.py's own docstring. py_cover.py emits
# lcov directly, so no further conversion is needed at merge time.
def _py_test_coverage_action(ctx, gcc, py, srcs, roots, start):
  lcov = ctx.actions.declare_output(ctx.attrs.name + ".lcov")
  rewrite_args = []
  for root in roots:
    if type(root) == "Artifact":
      rewrite_args += ["--rewrite", cmd_args(root, "=python/lib", delimiter = "")]
  lines = ["#!/bin/sh", "set -eu"] + _pythonpath_env_lines(roots) + [
    "OUT=\"$1\"",
    "shift",
    cmd_args(
      ["exec"] + _loader_exec_prefix(gcc, py) + [
        ctx.attrs._py_cover,
        "--root",
        "python/lib",
        "--root",
        "python/app",
        "--exclude",
        "python/test",
      ] + rewrite_args + [
        "--out",
        "\"$OUT\"",
        "--runner",
        ctx.attrs._py_test_runner,
        "--",
        "discover",
        "-s",
        start,
        "-p",
        ctx.attrs.pattern,
      ],
      delimiter = " ",
    ),
  ]
  script, written = ctx.actions.write(ctx.attrs.name + "-cov.sh", lines, is_executable = True, allow_args = True)
  ctx.actions.run(
    cmd_args(["/bin/sh", script, lcov.as_output()], hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir, ctx.attrs._py_cover, ctx.attrs._py_test_runner]),
    category = "py_test_coverage",
    identifier = ctx.attrs.name,
    env = action_env(),
  )
  return lcov

def _py_test_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  srcs, _srcs_tset = _py_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  roots = _merge_roots(ctx.attrs.deps)
  start = ctx.attrs.start or ctx.label.package
  lines = ["#!/bin/sh", "set -eu"] + _pythonpath_env_lines(roots) + [
    cmd_args(
      ["exec"] + _loader_exec_prefix(gcc, py) + [ctx.attrs._py_test_runner, "discover", "-s", start, "-p", ctx.attrs.pattern],
      delimiter = " ",
    ),
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir, ctx.attrs._py_test_runner])
  providers = [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "python_test",
      command = [command],
      run_from_project_root = True,
    ),
  ]
  if ctx.attrs._coverage_enabled:
    lcov = _py_test_coverage_action(ctx, gcc, py, srcs, roots, start)
    providers.append(CoverageInfo(kind = "python_lcov", primary = lcov))
  return providers

_py_test_rule = rule(
  impl = _py_test_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "pattern": attrs.string(default = "test_*.py"),
    "srcs": attrs.list(attrs.source(), default = []),
    "start": attrs.option(attrs.string(), default = None),
    "_coverage_enabled": attrs.bool(default = coverage_enabled_flag()),
    "_py_cover": attrs.source(default = "//:py_cover.py"),
    "_py_test_runner": attrs.source(default = "//:py_test_runner.py"),
  } | _TOOLCHAIN_ATTRS,
)

# --- py_compileall_check: `python -m compileall -q <dirs...>`, mirroring
# repo.sh's python-test lane's own compileall step, as its own test target
# for lint parity with the rest of the python lane.
#
# It ALSO enforces the declared-vs-resolved contract for the whole python
# lane, which is this rule's more important job. Every Python rule
# here resolves imports off the LIVE tree: PyInfo.roots are repo-relative
# directory STRINGS handed to the interpreter via PYTHONPATH, so the
# interpreter reads whatever is on disk under them while buck2 only tracks
# files that some target named in `srcs`. An undeclared module therefore
# imports and runs perfectly, but no action depends on it, so editing it
# invalidates nothing - reproduced before this check existed by appending
# invalid Python to python/lib/testlib.py, which left `./repo.sh
# coverage` reporting SUCCESS off a stale cached report.
#
# `dirs` is exactly the set of directories the lane claims to own, which
# makes it the right place to assert the converse of deno_graph_check's
# contract: every *.py that exists under those directories must be reachable
# from this target's own srcs+deps closure. The check runs BEFORE compileall
# so the actionable message wins over a confusing downstream one.
_COMPILEALL_DECLARED_PY = [
  "import json, os, sys",
  "",
  "declared_path = sys.argv[1]",
  "dirs = sys.argv[2:]",
  "with open(declared_path) as f:",
  "  declared = set(json.load(f))",
  "",
  "missing = []",
  "seen = 0",
  "for d in dirs:",
  "  for dirpath, dirnames, filenames in os.walk(d):",
  # __pycache__ holds generated bytecode, not source; pruning it from the
  # walk also keeps a previous compileall run from poisoning this one.
  "    dirnames[:] = [n for n in dirnames if n != '__pycache__']",
  "    for name in filenames:",
  "      if not name.endswith('.py'):",
  "        continue",
  "      seen += 1",
  "      rel = os.path.join(dirpath, name).replace(os.sep, '/')",
  "      if rel not in declared:",
  "        missing.append(rel)",
  "",
  "if missing:",
  "  print('py_compileall_check: .py files present under the checked dirs but missing from the declared srcs/deps closure:', file=sys.stderr)",
  "  for m in sorted(missing):",
  "    print('  ' + m, file=sys.stderr)",
  "  print('buck2 tracks only files some target names in srcs=; an undeclared module still imports at run time off PYTHONPATH, so nothing invalidates when you edit it. Add each to a py_library/py_binary srcs=, then list that target in the deps= of this check.', file=sys.stderr)",
  "  sys.exit(1)",
  "print('py_compileall_check: ok (%d .py files under %s, all declared)' % (seen, ', '.join(dirs)))",
]

def _py_compileall_check_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  srcs, _srcs_tset = _py_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  # write_json renders each source Artifact as its repo-relative path, the
  # same spelling os.walk produces below (every action runs with cwd =
  # project root).
  declared = ctx.actions.write_json(ctx.attrs.name + "-declared.json", srcs)
  checker = ctx.actions.write(ctx.attrs.name + "-declared.py", _COMPILEALL_DECLARED_PY)
  lines = ["#!/bin/sh", "set -eu", cmd_args(
    _loader_exec_prefix(gcc, py) + ["-I", checker, declared] + list(ctx.attrs.dirs),
    delimiter = " ",
  ), cmd_args(
    ["exec"] + _loader_exec_prefix(gcc, py) + ["-m", "compileall", "-q"] + list(ctx.attrs.dirs),
    delimiter = " ",
  )]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = srcs + written + [gcc.gcc_dir, py.py_dir, checker, declared])
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "python_compileall",
      command = [command],
      run_from_project_root = True,
      labels = ["lint"],
    ),
  ]

_py_compileall_check_rule = rule(
  impl = _py_compileall_check_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "dirs": attrs.list(attrs.string()),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)

# --- pyright_check: runs the Deno-locked npm pyright CLI from the same
# bootstrap-seeded cache used by the TypeScript lanes. No host pip installation
# or network access is permitted after bootstrap.
def _pyright_check_impl(ctx: AnalysisContext) -> list[Provider]:
  srcs, _srcs_tset = _py_srcs(ctx, [], ctx.attrs.deps)
  roots = _merge_roots(ctx.attrs.deps)
  deno_dir = ctx.attrs._deno_dir[DefaultInfo].default_outputs[0]
  deno = ctx.attrs._deno[DefaultInfo].default_outputs[0]
  stamp = ctx.actions.declare_output(ctx.attrs.name + ".stamp")
  script = ctx.actions.write(
    ctx.attrs.name + ".sh",
    [
      "#!/bin/sh",
      "set -eu",
      "DENO_DIR_SRC=$1",
      "DENO_ROOT=$2",
      "CONFIG=$3",
      "STAMP=$4",
      "DENO=\"$DENO_ROOT/deno\"",
      # Anchored under buck-out rather than a bare `mktemp -d` (host /tmp):
      # /tmp is routinely a small tmpfs shared by every concurrent build on
      # the machine, and this copies a whole DENO_DIR into it. Same
      # rationale, and same shape, as rules/go.bzl's SCRATCH_BASE.
      "SCRATCH_BASE=\"$(pwd)/buck-out/v2/tmp/pyright-check\"",
      "mkdir -p \"$SCRATCH_BASE\"",
      "WORK=$(mktemp -d \"$SCRATCH_BASE/tmp.XXXXXX\")",
      "trap 'rm -rf \"$WORK\"' EXIT",
      "mkdir -p \"$WORK/denodir\"",
      "cp -R \"$DENO_DIR_SRC\"/. \"$WORK/denodir\"/",
      "chmod -R u+w \"$WORK/denodir\"",
      "export DENO_DIR=\"$WORK/denodir\"",
      "export DENO_NO_UPDATE_CHECK=1",
      "\"$DENO\" run --cached-only --frozen -A npm:pyright@1.1.407 --project \"$CONFIG\"",
      "echo ok >\"$STAMP\"",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", script, deno_dir, deno, ctx.attrs._config, stamp.as_output()], hidden = srcs + roots),
    category = "pyright_check",
    identifier = ctx.attrs.name,
    env = action_env(),
  )
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    # RunInfo for parity with every other test rule in this file (py_test,
    # py_compileall_check, py_lock_consistency_test) - `buck2 run` on this
    # target used to fail with "target does not have RunInfo" purely because
    # this one rule forgot it.
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "pyright",
      command = [command],
      run_from_project_root = True,
      labels = ["lint"],
    ),
  ]

_pyright_check_rule = rule(
  impl = _pyright_check_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "_config": attrs.source(default = "//:pyrightconfig.json"),
    "_deno": attrs.dep(default = _DENO_TOOLCHAIN, providers = [DefaultInfo]),
    "_deno_dir": attrs.dep(default = "//:deno-cache[deno-dir]", providers = [DefaultInfo]),
  },
)

# --- py_script_test: runs ONE repo-local Python script under the pinned
# interpreter, with PYTHONPATH assembled from `deps` exactly the way py_test
# does, and reports pass/fail straight from its exit status.
#
# py_test can only express unittest DISCOVERY over a directory, which does
# not fit a standalone check script whose whole body is module-level
# (cpp/lib/pyfast/leak_check.py: import the extension, hammer it, print the
# allocated-block delta). Running it through discovery would work only by
# accident - the import side effect - and would report "0 tests, OK" whether
# it ran or not. This rule makes the script itself the test, so a crash, an
# import failure or a non-zero exit is a red test rather than nothing at all.

def _py_script_test_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  srcs, _srcs_tset = _py_srcs(ctx, [ctx.attrs.main] + list(ctx.attrs.srcs), ctx.attrs.deps)
  roots = _merge_roots(ctx.attrs.deps)
  lines = ["#!/bin/sh", "set -eu"] + _pythonpath_env_lines(roots) + [
    cmd_args(
      ["exec"] + _loader_exec_prefix(gcc, py) + [ctx.attrs.main] + list(ctx.attrs.args),
      delimiter = " ",
    ),
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir])
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "python_script",
      command = [command],
      run_from_project_root = True,
    ),
  ]

_py_script_test_rule = rule(
  impl = _py_script_test_impl,
  attrs = {
    "args": attrs.list(attrs.string(), default = []),
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "main": attrs.source(),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)

# Give BUCK files a resolved debug platform without repeated declarations.
_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

def py_library(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_library_rule(**kwargs)

def py_binary(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_binary_rule(**kwargs)

def py_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  kwargs.setdefault("visibility", ["PUBLIC"])
  _py_test_rule(**kwargs)

def py_script_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  kwargs.setdefault("visibility", ["PUBLIC"])
  _py_script_test_rule(**kwargs)

def py_compileall_check(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_compileall_check_rule(**kwargs)

def pyright_check(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _pyright_check_rule(**kwargs)
