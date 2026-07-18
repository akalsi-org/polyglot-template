"""First-party cxx_library / cxx_binary / cxx_test rules for the cpp lane.

Mirrors tools/cpp_graph.py's semantics: a flat compile of each translation
unit with the pinned in-graph gcc-musl toolchain (-std=gnu++26, profile
flags from config/flags.bzl which mirrors cpp/cpp.toml), archives via the
toolchain's gcc-ar for cxx_library, links with -fuse-ld=mold via -B<gcc's own
bin dir> (mold lives next to g++ in the same extracted archive), and wraps
binaries/tests with the pinned musl loader since these are musl binaries
running on a (possibly glibc) host.

There is no prelude in this project, so cxx_test builds its own
ExternalRunnerTestInfo rather than relying on a prelude-provided cxx_test.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//config:flags.bzl", "COVERAGE_FLAG", "FORBIDDEN_FLAGS", "STANDARD", "profile_compile_flags", "profile_link_flags")
load("//rules:coverage.bzl", "CoverageInfo")
load("//rules:pkg.bzl", "PACKAGE_LABELS_ATTR", "PackageEntry", "package_info")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors toolchains/defs.bzl's _native_target() / toolchain/target.sh:
  # this repo only ever builds+runs the host's own musl output triplet.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_DOCTEST = TOOLCHAINS["doctest"][_NATIVE_TARGET]
_GCC_BIN_DIR = _GCC["expected"].rsplit("/", 1)[0]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]
_DOCTEST_INCLUDE_DIR = _DOCTEST["expected"].rsplit("/", 2)[0]

CxxInfo = provider(fields = ["include_dirs", "hdrs", "objects", "gcnos"])

def _check_flags(flags):
  for flag in flags:
    if flag in FORBIDDEN_FLAGS:
      fail("forbidden C++ flag: {}".format(flag))
    if flag.startswith("-fsanitize"):
      fail("sanitizers are outside the pinned musl toolchain contract: {}".format(flag))

def _toolchain_tools(ctx):
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  return struct(
    dir = gcc_dir,
    gxx = gcc_dir.project(_GCC["expected"]),
    ar = gcc_dir.project(_GCC["ar"]),
    bin_dir = gcc_dir.project(_GCC_BIN_DIR),
    loader = gcc_dir.project(_GCC["loader"]),
    loader_dir = gcc_dir.project(_LOADER_DIR),
    # Same bin dir as g++ (userdocs' qbt-musl-cross-make layout installs
    # gcov as a sibling of g++), named "<target>-gcov" - not in
    # toolchains/lock.bzl's TOOLCHAINS dict (only g++/ar/ld/... are, since
    # only those were needed before the coverage lane existed), so derived
    # here the same way _GCC_BIN_DIR itself is derived from _GCC["expected"].
    gcov = gcc_dir.project(_GCC_BIN_DIR + "/" + _NATIVE_TARGET + "-gcov"),
  )

def _doctest_include(ctx):
  doctest_dir = ctx.attrs._doctest[DefaultInfo].default_outputs[0]
  return doctest_dir.project(_DOCTEST_INCLUDE_DIR)

def _merge_deps(deps):
  include_dirs = []
  hdrs = []
  objects = []
  gcnos = []
  for dep in deps:
    info = dep[CxxInfo]
    include_dirs += info.include_dirs
    hdrs += info.hdrs
    objects += info.objects
    gcnos += info.gcnos
  return include_dirs, hdrs, objects, gcnos

def _dedupe_artifacts(artifacts):
  seen = {}
  ordered = []
  for artifact in artifacts:
    key = str(artifact)
    if key not in seen:
      seen[key] = True
      ordered.append(artifact)
  return ordered

def _compile_one(ctx, tools, src, include_dirs, hdrs, compile_flags, extra_args, doctest, identifier):
  _check_flags(compile_flags)
  obj = ctx.actions.declare_output("__objects__/{}/{}.o".format(ctx.attrs.name, identifier))
  args = [
    tools.gxx,
    "-std={}".format(STANDARD),
    cmd_args(tools.bin_dir, format = "-B{}"),
  ]
  if doctest:
    args.append(cmd_args(_doctest_include(ctx), format = "-I{}"))
  args += [cmd_args(d, format = "-I{}") for d in include_dirs]
  args += extra_args
  args += compile_flags
  args += ["-c", src, "-o", obj.as_output()]
  # --coverage makes gcc write a .gcno sidecar next to -o's path (same
  # basename, swapped extension) at compile time; the matching .gcda is
  # only written at test-run time (see cxx_test's coverage collection
  # action below). Declare it as a second output of this same action
  # whenever coverage is on, so downstream CxxInfo can carry it forward.
  gcno = None
  if COVERAGE_FLAG in compile_flags:
    gcno = ctx.actions.declare_output("__objects__/{}/{}.gcno".format(ctx.attrs.name, identifier))
  ctx.actions.run(
    cmd_args(args, hidden = hdrs + ([gcno.as_output()] if gcno else [])),
    category = "cxx_compile",
    identifier = identifier,
  )
  return obj, gcno

def _link(ctx, tools, objects, link_flags, identifier):
  _check_flags(link_flags)
  binary = ctx.actions.declare_output(ctx.attrs.name)
  args = [tools.gxx] + objects + [cmd_args(tools.bin_dir, format = "-B{}")] + link_flags + ["-o", binary.as_output()]
  ctx.actions.run(cmd_args(args), category = "cxx_link", identifier = identifier)
  return binary

def _loader_launcher(ctx, tools, binary):
  # The `written` list from allow_args=True is not sufficient on its own to
  # track the loader as an input (see RULE-WRITING LAW: it doesn't reliably
  # capture artifacts referenced only via inline cmd_args concatenation
  # inside the written content, and that gap only surfaces once this target
  # is reached under a non-default configuration whose toolchain extraction
  # is a cache hit the materializer never had a reason to materialize).
  # Every consumer of the launcher must additionally put tools.dir (the
  # whole extracted toolchain directory the loader lives under) in its own
  # `hidden` list.
  launcher, written = ctx.actions.write(
    ctx.attrs.name + ".sh",
    [
      "#!/bin/sh",
      cmd_args("exec", tools.loader, "--library-path", tools.loader_dir, binary, "\"$@\"", delimiter = " "),
    ],
    is_executable = True,
    allow_args = True,
  )
  return launcher, written

_TOOLCHAIN_ATTRS = {
  "_doctest": attrs.dep(default = "//toolchains:doctest-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_target_arch": target_arch_attr(),
}

# select() is only resolved by buck2 when used as an attrs default (the
# configuration phase runs between BUCK-file evaluation and rule impl); a
# bare profile_compile_flags() call inside an impl body returns an
# unresolved Select. So every profile-dependent flag list is threaded
# through as an attr instead of being read directly from config/flags.bzl
# inside the impls below.
_PROFILE_ATTRS = {
  "compile_flags": attrs.list(attrs.string(), default = profile_compile_flags()),
  "link_flags": attrs.list(attrs.string(), default = profile_link_flags()),
}

# --- cxx_library: headers (+ optional sources) reusable across targets/tests.

def _cxx_library_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  dep_include_dirs, dep_hdrs, dep_objects, dep_gcnos = _merge_deps(ctx.attrs.deps)
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  compiled = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], False, src.short_path)
    for src in ctx.attrs.srcs
  ]
  own_objects = [obj for obj, _gcno in compiled]
  own_gcnos = [gcno for _obj, gcno in compiled if gcno != None]
  objects = own_objects + dep_objects
  gcnos = own_gcnos + dep_gcnos
  outputs = own_objects if own_objects else hdrs
  # cxx_library stages nothing of its own (a compiled .o is not something the
  # package layout wants directly), but still calls package_info() with empty
  # entries/needs so PackageInfo flows through a library-only dependency
  # chain uniformly - see rules/pkg.bzl's module docstring.
  return [
    DefaultInfo(default_outputs = outputs),
    CxxInfo(include_dirs = include_dirs, hdrs = hdrs, objects = objects, gcnos = gcnos),
    package_info(ctx, deps = ctx.attrs.deps),
  ]

_cxx_library_rule = rule(
  impl = _cxx_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "include_dirs": attrs.list(attrs.string(), default = []),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS | PACKAGE_LABELS_ATTR,
)

# --- cxx_adapter: one precompiled object, mirroring tools/cpp_graph.py's
# load_adapter() for a single native-dependency adapter action (this repo's
# fragments currently declare exactly one action each; cpp/adapters/BUCK
# hand-declares one cxx_adapter per fragment action, since Starlark cannot
# parse the JSON fragment at analysis time the way cpp_graph.py does).

def _cxx_adapter_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  include_dirs, hdrs, _objects, _gcnos = _merge_deps(ctx.attrs.deps)
  hdrs = list(ctx.attrs.hdrs) + hdrs
  obj, gcno = _compile_one(ctx, tools, ctx.attrs.src, include_dirs, hdrs, ctx.attrs.compile_flags, ctx.attrs.extra_args, ctx.attrs.doctest, ctx.attrs.name)
  return [
    DefaultInfo(default_outputs = [obj]),
    CxxInfo(include_dirs = [], hdrs = hdrs, objects = [obj], gcnos = [gcno] if gcno != None else []),
  ]

# Also used for the shared doctest runner object (cpp/test/BUCK,
# doctest = True): a single precompiled object is the common shape shared by
# tools/cpp_graph.py's adapter actions and its test-framework runner action.
_cxx_adapter_rule = rule(
  impl = _cxx_adapter_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "doctest": attrs.bool(default = False),
    "extra_args": attrs.list(attrs.string(), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "src": attrs.source(),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS,
)

# --- cxx_binary: executable + loader-wrapped RunInfo launcher.

def _cxx_binary_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  dep_include_dirs, dep_hdrs, dep_objects, _dep_gcnos = _merge_deps(ctx.attrs.deps)
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  compiled = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], False, src.short_path)
    for src in ctx.attrs.srcs
  ]
  own_objects = [obj for obj, _gcno in compiled]
  objects = _dedupe_artifacts(own_objects + dep_objects)
  binary = _link(ctx, tools, objects, ctx.attrs.link_flags, ctx.attrs.name)
  launcher, written = _loader_launcher(ctx, tools, binary)
  # cxx_binary is deliberately left uninstrumented-output-free even under
  # dbg's --coverage compile/link flags: `buck2 run` executes RunInfo
  # directly (not through a declared-output-producing action), so any
  # runtime counter writes here would go nowhere declared - only cxx_test's
  # coverage collection action below actually feeds the report.
  #
  # Packaging: stages the RAW (non-loader-wrapped) binary under
  # libexec/<pkg_name> - rules/package.bzl's "loader-bin" kind handler
  # generates its own bin/<pkg_name> launcher from this entry, so this is
  # deliberately NOT `launcher` (this rule's own loader-wrapped RunInfo
  # script, whose paths are relative to THIS target's buck-out location, not
  # a packaged layout's lib/<loader>).
  pkg_name = ctx.attrs.pkg_name or ctx.attrs.name
  info = package_info(
    ctx,
    entries = [PackageEntry(dest = "libexec/" + pkg_name, artifact = binary, kind = "loader-bin", owner = str(ctx.label.raw_target()))],
    needs = ["musl-loader"],
    deps = ctx.attrs.deps,
  )
  return [
    DefaultInfo(default_output = binary, other_outputs = [launcher] + written),
    RunInfo(args = cmd_args(launcher, hidden = [binary, tools.dir] + written)),
    info,
  ]

_cxx_binary_rule = rule(
  impl = _cxx_binary_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "include_dirs": attrs.list(attrs.string(), default = []),
    "pkg_name": attrs.option(attrs.string(), default = None),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS | PACKAGE_LABELS_ATTR,
)

# --- cxx_test: like cxx_binary, but links the shared per-profile doctest
# runner object (built once per configuration and reused across every test
# that depends on it, since buck2 shares identical configured subgraphs) and
# provides ExternalRunnerTestInfo so `buck2 test` can run it.

def _coverage_collect_action(ctx, tools, binary, gcnos):
  # Runs the test binary a second time (separately from the
  # ExternalRunnerTestInfo/RunInfo path `buck2 test` uses for pass/fail
  # reporting) as a normal ctx.actions.run() build action, so its coverage
  # counters land in a real declared output buck2 can cache and //:coverage
  # can depend on - see rules/coverage.bzl's module docstring for why this
  # is a second, separate run rather than reusing the `buck2 test` one.
  #
  # GCOV_PREFIX/GCOV_PREFIX_STRIP route the runtime's .gcda writes (which
  # gcc always locates at an ABSOLUTE path derived from the object file's
  # path at compile time - here that's "$(pwd)/<declared .o path>", since
  # every action in this project runs with cwd = project root) into this
  # action's own declared output dir instead of wherever that absolute path
  # would otherwise land. GCOV_PREFIX_STRIP must strip exactly the number of
  # leading path components in the compile-time (== here, run-time) project
  # root absolute path, which is why it's computed at run time via `pwd`
  # rather than hardcoded - the project root's absolute path length is not
  # something this rule can know at analysis time (and differs machine to
  # machine / CI to CI). Empirically validated locally: this reproduces a
  # .gcda tree that mirrors the repo-relative .o path exactly, matching
  # where the .gcno siblings declared by _compile_one already live.
  gcov_dir = ctx.actions.declare_output(ctx.attrs.name + ".gcov_data", dir = True)
  script, written = ctx.actions.write(
    ctx.attrs.name + "-cov.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "OUT=\"$1\"",
      "shift",
      "mkdir -p \"$OUT\"",
      "STRIP=$(pwd | awk -F/ '{print NF-1}')",
      cmd_args("GCOV_PREFIX=\"$(pwd)/$OUT\"", delimiter = ""),
      "export GCOV_PREFIX",
      "export GCOV_PREFIX_STRIP=\"$STRIP\"",
      cmd_args("exec", tools.loader, "--library-path", tools.loader_dir, binary, delimiter = " "),
    ],
    is_executable = True,
    allow_args = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", script, gcov_dir.as_output()], hidden = [binary, tools.dir] + written),
    category = "cxx_test_coverage",
    identifier = ctx.attrs.name,
  )
  return gcov_dir

def _cxx_test_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  dep_include_dirs, dep_hdrs, dep_objects, dep_gcnos = _merge_deps(ctx.attrs.deps)
  runner_objects = ctx.attrs._runner[CxxInfo].objects
  runner_gcnos = ctx.attrs._runner[CxxInfo].gcnos
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  compiled = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], True, src.short_path)
    for src in ctx.attrs.srcs
  ]
  own_objects = [obj for obj, _gcno in compiled]
  own_gcnos = [gcno for _obj, gcno in compiled if gcno != None]
  objects = _dedupe_artifacts(runner_objects + own_objects + dep_objects)
  binary = _link(ctx, tools, objects, ctx.attrs.link_flags, ctx.attrs.name)
  launcher, written = _loader_launcher(ctx, tools, binary)
  command = cmd_args(launcher, hidden = [binary, tools.dir] + written)
  providers = [
    DefaultInfo(default_output = binary, other_outputs = [launcher] + written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "doctest",
      command = [command],
      run_from_project_root = True,
    ),
  ]
  if COVERAGE_FLAG in ctx.attrs.compile_flags:
    gcnos = _dedupe_artifacts(runner_gcnos + own_gcnos + dep_gcnos)
    gcov_dir = _coverage_collect_action(ctx, tools, binary, gcnos)
    providers.append(CoverageInfo(
      kind = "cxx_gcov",
      primary = gcov_dir,
      tool = tools.gcov,
      gcnos = gcnos,
      toolchain_dir = tools.dir,
    ))
  return providers

_cxx_test_rule = rule(
  impl = _cxx_test_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "include_dirs": attrs.list(attrs.string(), default = []),
    "srcs": attrs.list(attrs.source(), default = []),
    "_runner": attrs.dep(default = "//cpp/test:doctest_runner_obj", providers = [CxxInfo]),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS,
)

# `default_target_platform` is an implicit attribute every buck2 rule()
# target supports, but there is no way for a rule() to give it a schema
# default; select() (used by _PROFILE_ATTRS above) also errors outside a
# resolved configuration. These macros are the public cxx_* API so BUCK
# files get a resolved (dbg, native arch) configuration for free and don't
# need to repeat `default_target_platform` on every target; pass it
# explicitly (or `-m`/`--target-platforms` on the command line) to override.
_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

def cxx_library(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _cxx_library_rule(**kwargs)

def cxx_adapter(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _cxx_adapter_rule(**kwargs)

def cxx_binary(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _cxx_binary_rule(**kwargs)

def cxx_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  # Every test target this rule produces is now also a coverage source (see
  # rules/coverage.bzl's module docstring): //:coverage depends on cpp/test's
  # cxx_test targets directly, which - absent a repo-wide PACKAGE file
  # setting a default - would otherwise stay package-private (buck2's
  # unstated default) and be unreachable from the root package. Default to
  # PUBLIC here rather than editing every existing cpp/test/BUCK call site
  # (out of this change's file ownership); callers that already pass their
  # own `visibility` are unaffected by setdefault().
  kwargs.setdefault("visibility", ["PUBLIC"])
  _cxx_test_rule(**kwargs)
