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

load("//config:flags.bzl", "FORBIDDEN_FLAGS", "STANDARD", "profile_compile_flags", "profile_link_flags")
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

CxxInfo = provider(fields = ["include_dirs", "hdrs", "objects"])

def _check_flags(flags):
  for flag in flags:
    if flag in FORBIDDEN_FLAGS:
      fail("forbidden C++ flag: {}".format(flag))
    if flag.startswith("-fsanitize"):
      fail("sanitizers are outside the pinned musl toolchain contract: {}".format(flag))

def _toolchain_tools(ctx):
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  return struct(
    dir = gcc_dir,
    gxx = gcc_dir.project(_GCC["expected"]),
    ar = gcc_dir.project(_GCC["ar"]),
    bin_dir = gcc_dir.project(_GCC_BIN_DIR),
    loader = gcc_dir.project(_GCC["loader"]),
    loader_dir = gcc_dir.project(_LOADER_DIR),
  )

def _doctest_include(ctx):
  doctest_dir = ctx.attrs._doctest[DefaultInfo].default_outputs[0]
  return doctest_dir.project(_DOCTEST_INCLUDE_DIR)

def _merge_deps(deps):
  include_dirs = []
  hdrs = []
  objects = []
  for dep in deps:
    info = dep[CxxInfo]
    include_dirs += info.include_dirs
    hdrs += info.hdrs
    objects += info.objects
  return include_dirs, hdrs, objects

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
  ctx.actions.run(
    cmd_args(args, hidden = hdrs),
    category = "cxx_compile",
    identifier = identifier,
  )
  return obj

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
  dep_include_dirs, dep_hdrs, dep_objects = _merge_deps(ctx.attrs.deps)
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  own_objects = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], False, src.short_path)
    for src in ctx.attrs.srcs
  ]
  objects = own_objects + dep_objects
  outputs = own_objects if own_objects else hdrs
  return [
    DefaultInfo(default_outputs = outputs),
    CxxInfo(include_dirs = include_dirs, hdrs = hdrs, objects = objects),
  ]

_cxx_library_rule = rule(
  impl = _cxx_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "include_dirs": attrs.list(attrs.string(), default = []),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS,
)

# --- cxx_adapter: one precompiled object, mirroring tools/cpp_graph.py's
# load_adapter() for a single native-dependency adapter action (this repo's
# fragments currently declare exactly one action each; cpp/adapters/BUCK
# hand-declares one cxx_adapter per fragment action, since Starlark cannot
# parse the JSON fragment at analysis time the way cpp_graph.py does).

def _cxx_adapter_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  include_dirs, hdrs, _ = _merge_deps(ctx.attrs.deps)
  hdrs = list(ctx.attrs.hdrs) + hdrs
  obj = _compile_one(ctx, tools, ctx.attrs.src, include_dirs, hdrs, ctx.attrs.compile_flags, ctx.attrs.extra_args, ctx.attrs.doctest, ctx.attrs.name)
  return [
    DefaultInfo(default_outputs = [obj]),
    CxxInfo(include_dirs = [], hdrs = hdrs, objects = [obj]),
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
  dep_include_dirs, dep_hdrs, dep_objects = _merge_deps(ctx.attrs.deps)
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  own_objects = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], False, src.short_path)
    for src in ctx.attrs.srcs
  ]
  objects = _dedupe_artifacts(own_objects + dep_objects)
  binary = _link(ctx, tools, objects, ctx.attrs.link_flags, ctx.attrs.name)
  launcher, written = _loader_launcher(ctx, tools, binary)
  return [
    DefaultInfo(default_output = binary, other_outputs = [launcher] + written),
    RunInfo(args = cmd_args(launcher, hidden = [binary, tools.dir] + written)),
  ]

_cxx_binary_rule = rule(
  impl = _cxx_binary_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [CxxInfo]), default = []),
    "hdrs": attrs.list(attrs.source(), default = []),
    "include_dirs": attrs.list(attrs.string(), default = []),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS | _PROFILE_ATTRS,
)

# --- cxx_test: like cxx_binary, but links the shared per-profile doctest
# runner object (built once per configuration and reused across every test
# that depends on it, since buck2 shares identical configured subgraphs) and
# provides ExternalRunnerTestInfo so `buck2 test` can run it.

def _cxx_test_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  dep_include_dirs, dep_hdrs, dep_objects = _merge_deps(ctx.attrs.deps)
  runner_objects = ctx.attrs._runner[CxxInfo].objects
  include_dirs = list(ctx.attrs.include_dirs) + dep_include_dirs
  hdrs = list(ctx.attrs.hdrs) + dep_hdrs
  own_objects = [
    _compile_one(ctx, tools, src, include_dirs, hdrs, ctx.attrs.compile_flags, [], True, src.short_path)
    for src in ctx.attrs.srcs
  ]
  objects = _dedupe_artifacts(runner_objects + own_objects + dep_objects)
  binary = _link(ctx, tools, objects, ctx.attrs.link_flags, ctx.attrs.name)
  launcher, written = _loader_launcher(ctx, tools, binary)
  command = cmd_args(launcher, hidden = [binary, tools.dir] + written)
  return [
    DefaultInfo(default_output = binary, other_outputs = [launcher] + written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "doctest",
      command = [command],
      run_from_project_root = True,
    ),
  ]

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
  _cxx_test_rule(**kwargs)
