"""First-party py_extension / py_library / py_binary / py_test rules for the
python lane.

Mirrors tools/python_build.py + repo.sh's python/python-build/python-test
semantics: py_extension compiles python/lib/fastbytes/*.cc with the in-graph
gcc-musl toolchain against the pinned interpreter's sysconfig include path
(snapshotted in tools.lock.toml / toolchains/lock.bzl rather than probed at
build time - see py_lock_consistency_test below, which makes drift between
the snapshot and the real interpreter a hard build/test failure instead of a
silent staleness), links a `_native<EXT_SUFFIX>` extension with mold, and
stages it next to a copy of `__init__.py` exactly like
tools/python_build.py's `package = root / f"build/python/{target}/lib/..."`
staging directory. py_binary/py_test wrap the pinned musl loader around the
pinned CPython interpreter (mirroring repo.sh's `python` case:
`"$gcc_install/$loader" --library-path "$loader_dir:$python_install/python/lib"
"$python_install/$python_expected" "$@"`), with PYTHONPATH assembled from
each target's `deps` in the order given (mirroring repo.sh's
`build/python/$target/lib:python/lib:python/app` ordering, which is caller
-controlled here via dep order).

There is no prelude in this project, so py_test builds its own
ExternalRunnerTestInfo rather than relying on a prelude-provided python_test,
following the same pattern as rules/cxx.bzl's cxx_test and rules/go.bzl's
go_test.

Toolchain artifact tracking (fixed bug, mirroring rules/go.bzl's own fix):
py_binary/py_test/py_compileall_check/py_lock_consistency_test each write a
loader-wrapped launcher script via ctx.actions.write() that embeds the gcc
loader / python3 interpreter paths as literal TEXT (via _loader_exec_prefix
+ cmd_args(..., delimiter=" ")), for a naturally readable emitted script.
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

The same law applies to `roots` (a py_extension's built `pkg_dir` package
directory, threaded through PyInfo.roots and embedded into the launcher
script's PYTHONPATH via _pythonpath_env_lines()): a py_extension's staged
package directory is itself a build output, not a repo source, so it is
exactly the same kind of "reached transitively under a different
configuration" hazard as gcc_dir/py_dir above. Every consuming action's/
command's `hidden` list that calls _pythonpath_env_lines(roots) must
therefore also include `roots` directly, alongside gcc_dir/py_dir.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//config:flags.bzl", "COVERAGE_FLAG", "FORBIDDEN_FLAGS", "STANDARD", "coverage_enabled_flag", "profile_compile_flags", "profile_link_flags")
load("//rules:coverage.bzl", "CoverageInfo")
load("//rules:pkg.bzl", "PACKAGE_LABELS_ATTR", "PackageEntry", "check_pkg_name", "package_info")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors rules/cxx.bzl's _native_target() / toolchain/target.sh: this
  # repo only ever builds+runs the host's own musl output triplet.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_PYTHON = TOOLCHAINS["python"][_NATIVE_TARGET]
_GCC_BIN_DIR = _GCC["expected"].rsplit("/", 1)[0]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]

# _PYTHON["expected"] is "python/bin/python3"; the interpreter's own install
# root ("python/") is what sysconfig.get_path("include") / the loader
# --library-path python/lib argument in repo.sh are both relative to.
_PY_ROOT_REL = _PYTHON["expected"].rsplit("/", 2)[0]
_PY_LIB_REL = _PY_ROOT_REL + "/lib"
_PY_INCLUDE_SUBPATH = _PYTHON["include_subpath"]
_PY_INCLUDE_REL = _PY_ROOT_REL + "/" + _PY_INCLUDE_SUBPATH
_EXT_SUFFIX = _PYTHON["ext_suffix"]

PyInfo = provider(fields = ["srcs", "roots"])

def _check_flags(flags):
  for flag in flags:
    if flag in FORBIDDEN_FLAGS:
      fail("forbidden C++ flag: {}".format(flag))
    if flag.startswith("-fsanitize"):
      fail("sanitizers are outside the pinned musl toolchain contract: {}".format(flag))

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
    gxx = gcc_dir.project(_GCC["expected"]),
    bin_dir = gcc_dir.project(_GCC_BIN_DIR),
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
    include_dir = py_dir.project(_PY_INCLUDE_REL),
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
_PROFILE_ATTRS = {
  "compile_flags": attrs.list(attrs.string(), default = profile_compile_flags()),
  "link_flags": attrs.list(attrs.string(), default = profile_link_flags()),
}

def _merge_pyinfo(deps):
  srcs = []
  roots = []
  seen_roots = {}
  for dep in deps:
    info = dep[PyInfo]
    srcs += info.srcs
    for root in info.roots:
      key = str(root)
      if key not in seen_roots:
        seen_roots[key] = True
        roots.append(root)
  return srcs, roots

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
  dep_srcs, dep_roots = _merge_pyinfo(ctx.attrs.deps)
  srcs = list(ctx.attrs.srcs) + dep_srcs
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
    PyInfo(srcs = srcs, roots = roots),
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

# --- py_extension: compiles python/lib/fastbytes/*.cc against the
# snapshotted sysconfig include path, links a `_native<EXT_SUFFIX>` shared
# object with mold, and stages it alongside a copy of `__init__.py` under a
# declared `<package>/` directory - exactly the shape
# tools/python_build.py builds under build/python/<target>/lib/fastbytes/,
# so the resulting root behaves as a drop-in replacement package directory
# on PYTHONPATH (a copy of __init__.py is required alongside the .so: a
# regular package with an __init__.py cannot span two PYTHONPATH roots the
# way an implicit namespace package could).

def _py_extension_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  # fastbytes' C++ side is deliberately out of scope for the coverage lane
  # (see rules/coverage.bzl's module docstring: only python/lib + python/app
  # .py files are measured, via tools/py_cover.py - not the compiled
  # extension). COVERAGE_FLAG still arrives here via config/flags.bzl's
  # shared profile_compile_flags()/profile_link_flags() (the same select()
  # cxx_test/cxx_binary use), so it has to be stripped explicitly: applying
  # --coverage to a -shared link pulls in the toolchain's non-PIC static
  # libgcov.a/libstdc++.a objects, which mold then refuses to link into a
  # shared object ("relocation ... can not be used; recompile with -fPIC") -
  # empirically confirmed against the pinned toolchain.
  compile_flags = [f for f in ctx.attrs.compile_flags if f != COVERAGE_FLAG]
  link_flags = [f for f in ctx.attrs.link_flags if f != COVERAGE_FLAG]
  _check_flags(compile_flags)
  _check_flags(link_flags)

  objects = []
  for src in ctx.attrs.srcs:
    obj = ctx.actions.declare_output("__objects__/{}/{}.o".format(ctx.attrs.name, src.short_path))
    args = [
      gcc.gxx,
      "-std={}".format(STANDARD),
      cmd_args(gcc.bin_dir, format = "-B{}"),
      cmd_args(py.include_dir, format = "-I{}"),
    ] + [cmd_args(d, format = "-I{}") for d in ctx.attrs.include_dirs]
    args += ["-fPIC"] + compile_flags
    args += ["-c", src, "-o", obj.as_output()]
    ctx.actions.run(
      cmd_args(args),
      category = "cxx_compile",
      identifier = "{}/{}".format(ctx.attrs.name, src.short_path),
    )
    objects.append(obj)

  native_name = "_native" + _EXT_SUFFIX
  shared_obj = ctx.actions.declare_output(ctx.attrs.name + "-" + native_name)
  link_args = [gcc.gxx] + objects + [cmd_args(gcc.bin_dir, format = "-B{}"), "-shared"] + link_flags
  link_args += ["-o", shared_obj.as_output()]
  ctx.actions.run(cmd_args(link_args), category = "cxx_link", identifier = ctx.attrs.name)

  pkg_dir = ctx.actions.declare_output(ctx.attrs.name + "-pkg", dir = True)
  stage_script = ctx.actions.write(
    ctx.attrs.name + "-stage.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "mkdir -p \"$1/%s\"" % ctx.attrs.package,
      "cp \"$2\" \"$1/%s/__init__.py\"" % ctx.attrs.package,
      "cp \"$3\" \"$1/%s/%s\"" % (ctx.attrs.package, native_name),
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", stage_script, pkg_dir.as_output(), ctx.attrs.init_src, shared_obj]),
    category = "py_extension_stage",
    identifier = ctx.attrs.name,
  )

  # Packaging: pkg_dir's OWN top level already contains a "<package>/"
  # subdirectory (this rule's own stage_script above: `mkdir -p
  # "$1/%s"`), so the entry's dest is the PARENT python/lib, not
  # python/lib/<package> - staging copies pkg_dir's contents (the
  # "<package>/" folder itself) straight into it. A py_extension always
  # needs the pinned interpreter present at runtime.
  info = package_info(
    ctx,
    entries = [PackageEntry(dest = "python/lib", artifact = pkg_dir, kind = "tree", owner = str(ctx.label.raw_target()))],
    needs = ["python-runtime"],
  )
  return [
    DefaultInfo(default_outputs = [pkg_dir]),
    PyInfo(srcs = list(ctx.attrs.srcs) + [ctx.attrs.init_src], roots = [pkg_dir]),
    info,
  ]

_py_extension_rule = rule(
  impl = _py_extension_impl,
  attrs = {
    "include_dirs": attrs.list(attrs.string(), default = []),
    "init_src": attrs.source(),
    "package": attrs.string(),
    "srcs": attrs.list(attrs.source()),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS | PACKAGE_LABELS_ATTR,
)

# --- py_binary: loader-wrapped launcher script running the pinned
# interpreter directly on `main`, mirroring repo.sh's `python` case.

def _py_binary_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  srcs, roots = _merge_pyinfo(ctx.attrs.deps)
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
    PyInfo(srcs = srcs + [ctx.attrs.main], roots = []),
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

# --- py_test: unittest discovery over `start` (python/test by default),
# mirroring repo.sh's `python -m unittest discover -s python/test -p
# 'test_*.py'`, exposed as ExternalRunnerTestInfo so `buck2 test` runs it.

# Coverage (default-on under dbg - see rules/coverage.bzl's module
# docstring): runs the SAME unittest discovery a second time, in-process
# under tools/py_cover.py's PEP 669 (sys.monitoring) line collector instead
# of a plain `python -m unittest`, as its own ctx.actions.run() build action
# separate from the ExternalRunnerTestInfo path `buck2 test` uses for
# pass/fail reporting (mirrors rules/cxx.bzl's/rules/go.bzl's/
# rules/deno.bzl's own coverage collection actions - see rules/coverage.bzl
# for why). Only python/lib and python/app are measured (python/test itself
# is excluded) - see tools/py_cover.py's own docstring. py_cover.py emits
# lcov directly, so no further conversion is needed at merge time.
def _py_test_coverage_action(ctx, gcc, py, srcs, roots):
  lcov = ctx.actions.declare_output(ctx.attrs.name + ".lcov")
  # Every py_extension dep (e.g. python/lib/fastbytes) contributes a build
  # -output package dir as its PYTHONPATH root (a COPY of __init__.py
  # staged next to the compiled extension - see rules/python.bzl's
  # py_extension rule) rather than the original source dir; at runtime
  # that's the file py_cover.py actually observes executing. Map each such
  # root back to "python/lib" (this repo's actual source dir for every
  # py_extension) via --rewrite so the emitted lcov still names the real
  # repo-relative source path - see tools/py_cover.py's docstring. Plain
  # py_library roots are already repo-relative strings (e.g. "python/lib"
  # itself) and need no rewriting.
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
        "--",
        "-m",
        "unittest",
        "discover",
        "-s",
        ctx.attrs.start,
        "-p",
        ctx.attrs.pattern,
      ],
      delimiter = " ",
    ),
  ]
  script, written = ctx.actions.write(ctx.attrs.name + "-cov.sh", lines, is_executable = True, allow_args = True)
  ctx.actions.run(
    cmd_args(["/bin/sh", script, lcov.as_output()], hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir, ctx.attrs._py_cover]),
    category = "py_test_coverage",
    identifier = ctx.attrs.name,
  )
  return lcov

def _py_test_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  dep_srcs, roots = _merge_pyinfo(ctx.attrs.deps)
  srcs = list(ctx.attrs.srcs) + dep_srcs
  lines = ["#!/bin/sh", "set -eu"] + _pythonpath_env_lines(roots) + [
    cmd_args(
      ["exec"] + _loader_exec_prefix(gcc, py) + ["-m", "unittest", "discover", "-s", ctx.attrs.start, "-p", ctx.attrs.pattern],
      delimiter = " ",
    ),
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = srcs + written + roots + [gcc.gcc_dir, py.py_dir])
  providers = [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "python_unittest",
      command = [command],
      run_from_project_root = True,
    ),
  ]
  if ctx.attrs._coverage_enabled:
    lcov = _py_test_coverage_action(ctx, gcc, py, srcs, roots)
    providers.append(CoverageInfo(kind = "python_lcov", primary = lcov, tool = None, gcnos = None, toolchain_dir = None))
  return providers

_py_test_rule = rule(
  impl = _py_test_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [PyInfo]), default = []),
    "pattern": attrs.string(default = "test_*.py"),
    "srcs": attrs.list(attrs.source(), default = []),
    "start": attrs.string(default = "python/test"),
    "_coverage_enabled": attrs.bool(default = coverage_enabled_flag()),
    "_py_cover": attrs.source(default = "//:py_cover.py"),
  } | _TOOLCHAIN_ATTRS,
)

# --- py_compileall_check: `python -m compileall -q <dirs...>`, mirroring
# repo.sh's python-test lane's own compileall step, as its own test target
# for lint parity with the rest of the python lane.

def _py_compileall_check_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  dep_srcs, _ = _merge_pyinfo(ctx.attrs.deps)
  srcs = list(ctx.attrs.srcs) + dep_srcs
  lines = ["#!/bin/sh", "set -eu", cmd_args(
    ["exec"] + _loader_exec_prefix(gcc, py) + ["-m", "compileall", "-q"] + list(ctx.attrs.dirs),
    delimiter = " ",
  )]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = srcs + written + [gcc.gcc_dir, py.py_dir])
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

# --- py_lock_consistency_test: runs the in-graph pinned interpreter and
# asserts sysconfig's real EXT_SUFFIX / include path still match the
# ext_suffix / include_subpath values snapshotted into tools.lock.toml (see
# rules/python.bzl's module docstring for why this is a snapshot rather than
# a build-time probe). Native-arch only, like every other capability probe
# in this project (rules/toolchain.bzl's `probe` gating): python.bzl only
# ever wires up the host's own _NATIVE_TARGET toolchain, so this is
# inherently native-only without any extra gating.

def _py_lock_consistency_test_impl(ctx: AnalysisContext) -> list[Provider]:
  gcc = _gcc_tools(ctx)
  py = _python_tools(ctx)
  checker = ctx.actions.write(
    ctx.attrs.name + "-check.py",
    [
      "import sys",
      "import sysconfig",
      "",
      "expected_ext_suffix = {!r}".format(_EXT_SUFFIX),
      "expected_include_subpath = {!r}".format(_PY_INCLUDE_SUBPATH),
      "",
      "actual_ext_suffix = sysconfig.get_config_var('EXT_SUFFIX')",
      "actual_include = sysconfig.get_path('include').replace('\\\\', '/')",
      "",
      "errors = []",
      "if actual_ext_suffix != expected_ext_suffix:",
      "    errors.append('EXT_SUFFIX drift: tools.lock.toml=%r sysconfig=%r' % (expected_ext_suffix, actual_ext_suffix))",
      "if not actual_include.endswith(expected_include_subpath):",
      "    errors.append('include path drift: tools.lock.toml subpath=%r sysconfig include=%r' % (expected_include_subpath, actual_include))",
      "",
      "if errors:",
      "    for error in errors:",
      "        print(error, file=sys.stderr)",
      "    print('run: update tools.lock.toml [[artifact]] python entries, then tools/gen_toolchain_lock.py', file=sys.stderr)",
      "    sys.exit(1)",
      "print('tools.lock.toml python ext_suffix/include_subpath snapshot matches sysconfig')",
    ],
  )
  lines = ["#!/bin/sh", "set -eu", cmd_args(
    ["exec"] + _loader_exec_prefix(gcc, py) + ["-I", checker],
    delimiter = " ",
  )]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = written + [checker, gcc.gcc_dir, py.py_dir])
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "python_lock_consistency",
      command = [command],
      run_from_project_root = True,
      labels = ["lint"],
    ),
  ]

_py_lock_consistency_test_rule = rule(
  impl = _py_lock_consistency_test_impl,
  attrs = _TOOLCHAIN_ATTRS,
)

# See rules/cxx.bzl's identical rationale for these macros: gives BUCK files
# a resolved (dbg, native arch) configuration for free and don't need to
# repeat `default_target_platform` on every target.
_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

def py_extension(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_extension_rule(**kwargs)

def py_library(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_library_rule(**kwargs)

def py_binary(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_binary_rule(**kwargs)

def py_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  # See rules/cxx.bzl's cxx_test macro for why: //:coverage now depends on
  # py_test targets directly, which need to be reachable from the root
  # package without editing every existing python/test/BUCK call site.
  kwargs.setdefault("visibility", ["PUBLIC"])
  _py_test_rule(**kwargs)

def py_compileall_check(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_compileall_check_rule(**kwargs)

def py_lock_consistency_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _py_lock_consistency_test_rule(**kwargs)
