"""Deno rules for the ts/ and tsweb/ Buck2 lanes.

DENO_DIR design (validated by a local spike): a single population action
(deno_cache) is the ONLY network-permitted action here. It stages a copy of
the project config (deno.json, deno.lock, and the ts/ and tsweb/ source
trees) at a fresh working directory, runs `deno cache --frozen` against it,
then splits the result into two declared outputs:

  - a stripped DENO_DIR: the sqlite/gen scratch caches (check_cache_v2*,
    dep_analysis_cache_v2*, node_analysis_cache_v2*, v8_code_cache_v2*,
    fast_check_cache_v2*, including their -wal/-shm siblings), the gen/
    subtree (it embeds the staging action's absolute path and is therefore
    non-hermetic), and npm/registry.npmjs.org/*/registry.json (mtime/etag
    metadata that is not reproducible) are all deleted. What is left is
    npm/ package metadata.
  - the node_modules projection deno.json's `nodeModulesDir: "auto"`
    materializes next to deno.json. Despite living under node_modules/,
    this is NOT a set of symlinks into DENO_DIR: deno vendors real package
    copies under node_modules/.deno/<pkg>@<version>/... and node_modules/
    itself holds short *relative* symlinks into that .deno/ subtree. That
    makes node_modules independently relocatable from DENO_DIR, which is
    why it is safe to hand to every consumer as one read-only symlink
    rather than a private copy.

Every consuming action (deno_check, deno_test, deno_run_check, vite_build)
gets a PRIVATE WRITABLE copy of the small stripped DENO_DIR — deno still
writes sqlite scratch back even on --cached-only/frozen runs — plus a
read-only symlink of node_modules placed next to a staged copy of the
sources it needs. All consumers pass --frozen (and --cached-only where
supported) with DENO_NO_UPDATE_CHECK=1, so none of them touch the network.

Coverage (default-on under dbg - see rules/coverage.bzl's module docstring):
deno_test's coverage variant adds `--coverage=coverage_raw` to the same
`deno test --cached-only --frozen` invocation, as a SEPARATE ctx.actions.run() build
action from the ExternalRunnerTestInfo path `buck2 test` uses for pass/fail
reporting (mirrors rules/cxx.bzl's/rules/go.bzl's own coverage collection
actions - see rules/coverage.bzl for why). `deno test --coverage=<dir>`
auto-generates coverage_raw/lcov.info with NO extra `deno coverage --lcov`
step needed (validated empirically against the pinned deno 2.9.2: it always
writes an lcov + html report at the end unless
--coverage-raw-data-only is passed). Its SF: paths are ABSOLUTE, rooted at
this action's own ephemeral `$WORK/proj` staging directory (torn down by
this same script's EXIT trap, same as every other _stage_and_run consumer)
- tools/coverage_merge.py fixes this up by slicing each SF: path at its
last "/proj/" segment, which is safe because `_stage_and_run` always names
the staging directory exactly "proj" and stages sources at their real
repo-relative paths (e.g. "ts/lib/greeting/greeting.ts") underneath it.
"""

load("//config:flags.bzl", "coverage_enabled_flag")
load("//rules:coverage.bzl", "CoverageInfo")
load("//rules:pkg.bzl", "PACKAGE_LABELS_ATTR", "PackageEntry", "check_pkg_name", "package_info")

def _native_target() -> str:
  # Mirrors rules/{cxx,go,python}.bzl's own _native_target(): this repo only
  # ever builds+runs the host's own musl output triplet, so the deno
  # toolchain dep's default can be derived the same way instead of every
  # consumer target hand-writing "//toolchains:deno-x86_64-linux-musl".
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_DENO_TOOLCHAIN = "//toolchains:deno-" + _NATIVE_TARGET

# --- DenoSourcesInfo: a transitive set of (staged_path, artifact) pairs,
# threaded through deno_library/deno_app deps so every consumer
# (deno_check/deno_test/deno_run_check/deno_lint/vite_build/deno_graph_check)
# can fold a target's own sources PLUS its whole transitive dep graph's
# sources into one staged tree without any hand-written path dict (the old
# _SRCS dicts in ts/BUCK and tsweb/BUCK - see this file's "artifact-tracking
# law" doctrine in the module docstring, now generalized: staged_path is
# derived from ctx.label.package + the source's own short_path, exactly the
# convention _entries_to_layout's callers used to hand-maintain).
#
# Diamond-safe by construction, mirroring rules/pkg.bzl's
# flatten_merged_package_entries() doc comment: _flatten_deno_sources below
# builds ONE new tset node with every dep's `sources` tset as a child and
# traverses THAT once, so a source shared by two deps (e.g. two apps both
# depending on the same deno_library) surfaces exactly once.
DenoSourceSet = transitive_set()
DenoSourcesInfo = provider(fields = ["sources"])

# deno_app additionally carries its own entrypoint's staged path, so
# consumers (deno_cache's entries, deno_graph_check's entries) can derive the
# CLI-facing entry path from a deno_app dep instead of repeating it as a
# string.
DenoAppInfo = provider(fields = ["entry"])

def _staged_path(ctx: AnalysisContext, src: Artifact) -> str:
  return ctx.label.package + "/" + src.short_path

def _own_deno_sources(ctx: AnalysisContext, srcs: list) -> list:
  return [(_staged_path(ctx, src), src) for src in srcs]

def _deno_sources_children(deps: list) -> list:
  return [d[DenoSourcesInfo].sources for d in deps if DenoSourcesInfo in d]

# Makes DenoAppInfo load-bearing: every consumer below resolves its effective
# `deno` CLI entry-arg list as explicit `entries` (still needed for
# directory-scoped cases like deno_test's "ts/test/" or deno_lint's "ts/")
# PLUS the DenoAppInfo.entry of every dep that is a deno_app - deduped,
# order-stable (explicit entries first, then deps in dep-list order) - so an
# app's own entrypoint path is declared exactly once, at the deno_app itself,
# never re-typed as a string at every consumer that already depends on it.
def _resolve_entries(ctx: AnalysisContext) -> list:
  seen = {}
  out = []
  for entry in list(ctx.attrs.entries) + [d[DenoAppInfo].entry for d in ctx.attrs.deps if DenoAppInfo in d]:
    if entry not in seen:
      seen[entry] = True
      out.append(entry)
  return out

def _flatten_deno_sources(ctx: AnalysisContext, deps: list) -> dict:
  """Folds every `deps` DenoSourcesInfo target's transitive sources into one
  staged_path -> Artifact layout dict, diamond-safe (single traverse() over
  one merged tset node - see DenoSourceSet's doc comment above)."""
  children = _deno_sources_children(deps)
  if not children:
    return {}
  merged = ctx.actions.tset(DenoSourceSet, children = children)
  layout = {}
  for value in merged.traverse():
    for staged_path, artifact in value:
      layout[staged_path] = artifact
  return layout

# --- deno_library: a source-set-only target (mirrors go_library/py_library's
# shape) usable for both lib/ and app/ directories - deno_app (below) is the
# app-flavored sibling that additionally records an entrypoint. Deliberately
# no layering restriction between the two: an app is a valid dep of a test
# (this repo runs apps via deno_run_check/deno_test), so both emit the same
# DenoSourcesInfo provider and visibility alone governs who may depend on
# whom.
def _deno_library_impl(ctx: AnalysisContext) -> list[Provider]:
  own = _own_deno_sources(ctx, ctx.attrs.srcs)
  tset = ctx.actions.tset(DenoSourceSet, value = own, children = _deno_sources_children(ctx.attrs.deps))
  return [
    DefaultInfo(default_outputs = list(ctx.attrs.srcs)),
    DenoSourcesInfo(sources = tset),
  ]

_deno_library_rule = rule(
  impl = _deno_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [DenoSourcesInfo]), default = []),
    "srcs": attrs.list(attrs.source()),
  },
)

def deno_library(**kwargs):
  _deno_library_rule(**kwargs)

# --- deno_app: like deno_library, but `main` names this app's own entrypoint
# (must also be listed in `srcs`) - recorded via DenoAppInfo for
# check/run/deno_cache/deno_graph_check targets to reference without
# repeating the path as a string.
def _deno_app_impl(ctx: AnalysisContext) -> list[Provider]:
  if ctx.attrs.main not in ctx.attrs.srcs:
    fail("{}: `main` ({}) must also be listed in `srcs`".format(ctx.label.raw_target(), ctx.attrs.main))
  own = _own_deno_sources(ctx, ctx.attrs.srcs)
  tset = ctx.actions.tset(DenoSourceSet, value = own, children = _deno_sources_children(ctx.attrs.deps))

  # Packaging: stage this app's own transitive DenoSourcesInfo closure - own
  # `srcs` plus every deno_library/deno_app dep's sources, diamond-safe via
  # the same shared-tset-merge _flatten_deno_sources already uses for every
  # other consumer in this file - one "tree" PackageEntry per source file, at
  # its own staged_path directly (e.g. ts/app/server/main.ts,
  # ts/lib/greeting/greeting.ts) - the package root mirrors the repo's own
  # top-level layout, so a deno_app's ts/ closure lands as a root-level
  # sibling of bin/lib/libexec/runtime/share, not nested under an "app/"
  # prefix - plus a "deno-app-launcher" marker entry (no artifact of its
  # own) at bin/<pkg_name> whose `meta` records this app's own entrypoint
  # dest so rules/package.bzl's kind handler can pair the launcher back to
  # the right file even though several of this app's own staged entries
  # share the same owner. needs "deno-runtime" so package() also stages the
  # pinned deno binary.
  closure = dict(own)
  closure.update(_flatten_deno_sources(ctx, ctx.attrs.deps))
  pkg_name = check_pkg_name(ctx, ctx.attrs.pkg_name or ctx.attrs.name)
  owner = str(ctx.label.raw_target())
  main_dest = _staged_path(ctx, ctx.attrs.main)
  pkg_entries = [
    PackageEntry(dest = staged_path, artifact = artifact, kind = "tree", owner = owner)
    for staged_path, artifact in closure.items()
  ]
  pkg_entries.append(PackageEntry(dest = "bin/" + pkg_name, artifact = None, kind = "deno-app-launcher", owner = owner, meta = main_dest))
  info = package_info(ctx, entries = pkg_entries, needs = ["deno-runtime"], deps = ctx.attrs.deps)

  return [
    DefaultInfo(default_outputs = list(ctx.attrs.srcs)),
    DenoSourcesInfo(sources = tset),
    DenoAppInfo(entry = _staged_path(ctx, ctx.attrs.main)),
    info,
  ]

_deno_app_rule = rule(
  impl = _deno_app_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [DenoSourcesInfo]), default = []),
    "main": attrs.source(),
    "pkg_name": attrs.option(attrs.string(), default = None),
    "srcs": attrs.list(attrs.source()),
  } | PACKAGE_LABELS_ATTR,
)

def deno_app(**kwargs):
  _deno_app_rule(**kwargs)

def _entries_to_layout(deno_json, deno_lock, srcs):
  # `srcs` maps the path the staged layout needs (e.g. "ts/app/hello/main.ts",
  # matching how deno.json's scopes and the deno CLI entry args reference
  # it) to the source Artifact. A plain attrs.list(attrs.source()) would
  # only expose each source's package-relative short_path (e.g.
  # "app/hello/main.ts" with no "ts/" prefix), which breaks staging for
  # any rule instance that draws sources from more than one package.
  layout = {"deno.json": deno_json, "deno.lock": deno_lock}
  layout.update(srcs)
  return layout

def _deno_bin(ctx: AnalysisContext) -> Artifact:
  return ctx.attrs.deno[DefaultInfo].default_outputs[0]

# --- deno_cache: the one population action, network-permitted, keyed on
# deno.json + deno.lock (and every source file `deno cache` walks into).
def _deno_cache_impl(ctx: AnalysisContext) -> list[Provider]:
  srcs = _flatten_deno_sources(ctx, ctx.attrs.deps)
  srcs.update(ctx.attrs.srcs)
  layout = _entries_to_layout(ctx.attrs.deno_json, ctx.attrs.deno_lock, srcs)
  staged = ctx.actions.symlinked_dir(ctx.label.name + "-staged", layout)

  deno_dir_out = ctx.actions.declare_output(ctx.label.name + "-deno-dir", dir = True)
  node_modules_out = ctx.actions.declare_output(ctx.label.name + "-node-modules", dir = True)
  deno = _deno_bin(ctx)

  script = ctx.actions.write(
    ctx.label.name + "-cache.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(pwd)",
      "STAGED=\"$ROOT/$1\"",
      "DENO=\"$ROOT/$2/deno\"",
      "OUT_DENO_DIR=\"$ROOT/$3\"",
      "OUT_NODE_MODULES=\"$ROOT/$4\"",
      "shift 4",
      "chmod +x \"$DENO\"",
      "WORK=$(mktemp -d)",
      "trap 'rm -rf \"$WORK\"' EXIT",
      "cp -RL \"$STAGED\"/. \"$WORK\"/",
      "chmod -R u+w \"$WORK\"",
      "mkdir -p \"$WORK/.denodir\"",
      "export DENO_DIR=\"$WORK/.denodir\"",
      "export DENO_NO_UPDATE_CHECK=1",
      "cd \"$WORK\"",
      "\"$DENO\" cache --frozen \"$@\"",
      "rm -rf \"$DENO_DIR/check_cache_v2\" \"$DENO_DIR\"/check_cache_v2-* \\",
      "  \"$DENO_DIR/dep_analysis_cache_v2\" \"$DENO_DIR\"/dep_analysis_cache_v2-* \\",
      "  \"$DENO_DIR/node_analysis_cache_v2\" \"$DENO_DIR\"/node_analysis_cache_v2-* \\",
      "  \"$DENO_DIR/v8_code_cache_v2\" \"$DENO_DIR\"/v8_code_cache_v2-* \\",
      "  \"$DENO_DIR/fast_check_cache_v2\" \"$DENO_DIR\"/fast_check_cache_v2-* \\",
      "  \"$DENO_DIR/gen\"",
      "find \"$DENO_DIR/npm\" -path '*/registry.npmjs.org/*/registry.json' -delete 2>/dev/null || true",
      "mkdir -p \"$OUT_DENO_DIR\"",
      "cp -R \"$DENO_DIR\"/. \"$OUT_DENO_DIR\"/",
      "mkdir -p \"$OUT_NODE_MODULES\"",
      "[ -d \"$WORK/node_modules\" ] && cp -R \"$WORK/node_modules\"/. \"$OUT_NODE_MODULES\"/ || true",
    ],
    is_executable = True,
  )

  ctx.actions.run(
    cmd_args(["/bin/sh", script, staged, deno, deno_dir_out.as_output(), node_modules_out.as_output()] + _resolve_entries(ctx)),
    category = "deno_cache",
    identifier = ctx.label.name,
    local_only = True,
  )
  return [DefaultInfo(
    default_outputs = [deno_dir_out, node_modules_out],
    sub_targets = {
      "deno-dir": [DefaultInfo(default_output = deno_dir_out)],
      "node-modules": [DefaultInfo(default_output = node_modules_out)],
    },
  )]

_deno_cache_rule = rule(
  impl = _deno_cache_impl,
  attrs = {
    "deno": attrs.dep(default = _DENO_TOOLCHAIN),
    "deno_json": attrs.source(default = "//:deno.json"),
    "deno_lock": attrs.source(default = "//:deno.lock"),
    # deps of DenoSourcesInfo targets (deno_library/deno_app) contribute the
    # bulk of the staged tree without a hand-written path dict; `srcs` stays
    # available for the rare direct addition/override.
    "deps": attrs.list(attrs.dep(providers = [DenoSourcesInfo]), default = []),
    # Optional (default []): every deno_app dep's own DenoAppInfo.entry is
    # folded in automatically (see _resolve_entries) - `entries` here is only
    # for cache-warming a path that isn't any dep's own recorded entrypoint
    # (e.g. tsweb/vite.config.ts, a plain deno_library source, not an app).
    "entries": attrs.list(attrs.string(), default = []),
    "srcs": attrs.dict(attrs.string(), attrs.source(), default = {}),
  },
)

def deno_cache(**kwargs):
  kwargs.setdefault("deno", _DENO_TOOLCHAIN)
  kwargs.setdefault("deno_json", "//:deno.json")
  kwargs.setdefault("deno_lock", "//:deno.lock")
  _deno_cache_rule(**kwargs)

# --- shared plumbing for every network-free consumer of the cache.
def _stage_and_run(
    ctx: AnalysisContext,
    extra_layout: dict,
    deno_args: list,
    category: str,
    out_dir_name: [str, None] = None,
    name_suffix: str = "") -> (Artifact, [Artifact, None]):
  # name_suffix disambiguates output paths when a single rule instance calls
  # _stage_and_run more than once (e.g. deno_test's plain run plus its
  # coverage-collection run - see _deno_test_impl) - every declared output
  # below is otherwise keyed only on ctx.label.name, which would collide.
  srcs = _flatten_deno_sources(ctx, ctx.attrs.deps)
  srcs.update(ctx.attrs.srcs)
  layout = _entries_to_layout(ctx.attrs.deno_json, ctx.attrs.deno_lock, srcs)
  layout.update(extra_layout)
  staged = ctx.actions.symlinked_dir(ctx.label.name + name_suffix + "-staged", layout)

  deno_dir = ctx.attrs.deno_dir[DefaultInfo].default_outputs[0]
  node_modules = ctx.attrs.node_modules[DefaultInfo].default_outputs[0]
  deno = _deno_bin(ctx)

  stamp = ctx.actions.declare_output(ctx.label.name + name_suffix + ".stamp")
  out_dir = ctx.actions.declare_output((out_dir_name + name_suffix) if out_dir_name else None) if out_dir_name else None

  lines = [
    "#!/bin/sh",
    "set -eu",
    "ROOT=$(pwd)",
    "STAGED=\"$ROOT/$1\"",
    "DENO_DIR_SRC=\"$ROOT/$2\"",
    "NODE_MODULES=\"$ROOT/$3\"",
    "DENO=\"$ROOT/$4/deno\"",
    "STAMP=\"$ROOT/$5\"",
    "OUT_DIR=\"$ROOT/$6\"",
    "shift 6",
    "chmod +x \"$DENO\"",
    "WORK=$(mktemp -d)",
    "trap 'rm -rf \"$WORK\"' EXIT",
    "mkdir -p \"$WORK/proj\" \"$WORK/denodir\"",
    "cp -RL \"$STAGED\"/. \"$WORK/proj\"/",
    "chmod -R u+w \"$WORK/proj\"",
    "cp -R \"$DENO_DIR_SRC\"/. \"$WORK/denodir\"/",
    "chmod -R u+w \"$WORK/denodir\"",
    "ln -s \"$NODE_MODULES\" \"$WORK/proj/node_modules\"",
    "export DENO_DIR=\"$WORK/denodir\"",
    "export DENO_NO_UPDATE_CHECK=1",
    "cd \"$WORK/proj\"",
    "\"$DENO\" \"$@\"",
  ]
  if out_dir_name:
    lines.append("mkdir -p \"$OUT_DIR\"")
    lines.append("cp -R \"$WORK/proj/%s\"/. \"$OUT_DIR\"/" % out_dir_name)
  lines.append("echo ok >\"$STAMP\"")

  script = ctx.actions.write(ctx.label.name + name_suffix + "-run.sh", lines, is_executable = True)
  run_args = [
    "/bin/sh",
    script,
    staged,
    deno_dir,
    node_modules,
    deno,
    stamp.as_output(),
    out_dir.as_output() if out_dir else "/dev/null",
  ] + deno_args
  ctx.actions.run(cmd_args(run_args), category = category, identifier = ctx.label.name)
  return stamp, out_dir

_CONSUMER_ATTRS = {
  "deno": attrs.dep(default = _DENO_TOOLCHAIN),
  "deno_dir": attrs.dep(default = "//:deno-cache[deno-dir]"),
  "deno_json": attrs.source(default = "//:deno.json"),
  "deno_lock": attrs.source(default = "//:deno.lock"),
  # deps of DenoSourcesInfo targets (deno_library/deno_app) - see
  # _flatten_deno_sources - fold transitive sources into the staged tree;
  # `srcs` stays available for the rare direct addition/override.
  "deps": attrs.list(attrs.dep(providers = [DenoSourcesInfo]), default = []),
  "node_modules": attrs.dep(default = "//:deno-cache[node-modules]"),
  "srcs": attrs.dict(attrs.string(), attrs.source(), default = {}),
}

# Lane plumbing every consumer macro below injects via kwargs.setdefault, so
# BUCK declarations shrink to name/entries/srcs/deps - explicit overrides
# (e.g. a future second deno_cache) still work since setdefault only fills in
# what the caller omitted. Kept as attrs-level defaults too (see
# _CONSUMER_ATTRS above) for consumers instantiated directly as rule() calls;
# the macros exist for the visibility/default_target_platform ergonomics the
# attrs layer can't express (see rules/cxx.bzl's identical macro doctrine).
def _consumer_defaults(kwargs):
  kwargs.setdefault("deno", _DENO_TOOLCHAIN)
  kwargs.setdefault("deno_dir", "//:deno-cache[deno-dir]")
  kwargs.setdefault("deno_json", "//:deno.json")
  kwargs.setdefault("deno_lock", "//:deno.lock")
  kwargs.setdefault("node_modules", "//:deno-cache[node-modules]")
  return kwargs

# --- deno_check: `deno check --frozen <entry>`, ts-build parity. Build-only
# (no --cached-only: `deno check` has no such flag, but is network-free once
# the cache is warm and --frozen is set).
def _deno_check_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ["check", "--frozen"] + _resolve_entries(ctx), "deno_check")
  return [DefaultInfo(default_output = stamp)]

_deno_check_rule = rule(
  impl = _deno_check_impl,
  # Optional (default []): every deno_app dep's DenoAppInfo.entry is folded
  # in automatically - see _resolve_entries. Only needed explicitly for an
  # entry that isn't a dep's own recorded entrypoint.
  attrs = _CONSUMER_ATTRS | {"entries": attrs.list(attrs.string(), default = [])},
)

def deno_check(**kwargs):
  _deno_check_rule(**_consumer_defaults(kwargs))

# --- deno_run_check: `deno run --cached-only --frozen <entry>`, ts-test's
# "run the app" parity, wired up as a buck2 test.
def _deno_run_check_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ["run", "--cached-only", "--frozen"] + ctx.attrs.extra_flags + _resolve_entries(ctx), "deno_run_check")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True),
  ]

_deno_run_check_rule = rule(
  impl = _deno_run_check_impl,
  attrs = _CONSUMER_ATTRS | {
    # Optional (default []) - see deno_check's identical comment above.
    "entries": attrs.list(attrs.string(), default = []),
    "extra_flags": attrs.list(attrs.string(), default = []),
  },
)

def deno_run_check(**kwargs):
  _deno_run_check_rule(**_consumer_defaults(kwargs))

# --- deno_test: `deno test --frozen [flags] <paths>`, one buck2 test target
# per lane's test/ directory.
def _deno_test_impl(ctx: AnalysisContext) -> list[Provider]:
  # Deliberately NOT _resolve_entries(ctx): deno_test's entries are always
  # directory-scoped ("ts/test/") - see _resolve_entries' doc comment -
  # unlike deno_check/deno_run_check/deno_graph_check/deno_cache, a
  # deno_app dep here exists purely for source availability (e.g. test code
  # importing an app's exported function), not to be run as an extra test
  # entry, so its DenoAppInfo.entry should NOT be folded in automatically.
  entries = ctx.attrs.entries
  stamp, _ = _stage_and_run(ctx, {}, ["test", "--cached-only", "--frozen"] + ctx.attrs.extra_flags + entries, "deno_test")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  providers = [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True),
  ]
  if ctx.attrs._coverage_enabled:
    # Second, separate `deno test` run (mirrors rules/cxx.bzl's/
    # rules/go.bzl's own coverage collection actions - see this file's
    # module docstring and rules/coverage.bzl for why) with
    # --coverage=coverage_raw added; deno auto-generates
    # coverage_raw/lcov.info, which _stage_and_run's out_dir_name copies out
    # whole as this action's declared output.
    _, out_dir = _stage_and_run(
      ctx,
      {},
      ["test", "--cached-only", "--frozen"] + ctx.attrs.extra_flags + ["--coverage=coverage_raw"] + entries,
      "deno_test_coverage",
      out_dir_name = "coverage_raw",
      name_suffix = "-cov",
    )
    providers.append(CoverageInfo(kind = "deno", primary = out_dir, tool = None, gcnos = None, toolchain_dir = None))
  return providers

_deno_test_rule = rule(
  impl = _deno_test_impl,
  attrs = _CONSUMER_ATTRS | {
    # Directory-scoped ("ts/test/") entries stay explicit - see
    # _resolve_entries' doc comment - deno_app deps still fold their own
    # entrypoint in additionally (harmless: `deno test` treats an extra file
    # arg with no Deno.test() calls as zero additional tests, not an error).
    "entries": attrs.list(attrs.string(), default = []),
    "extra_flags": attrs.list(attrs.string(), default = []),
    "_coverage_enabled": attrs.bool(default = coverage_enabled_flag()),
  },
)

# deno_test is the only deno_* rule with a select()-driven attr default
# (_coverage_enabled) - see rules/go.bzl's identical go_test macro for why
# that requires default_target_platform to be set explicitly. Derived from
# _NATIVE_TARGET (not hardcoded to x86_64-linux-musl) for the same reason
# _DENO_TOOLCHAIN is - this repo also builds/runs natively on aarch64-linux-
# musl hosts, and rules/{cxx,go,python}.bzl's own _DEFAULT_PLATFORM already
# follow this pattern.
_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

def deno_test(**kwargs):
  kwargs = _consumer_defaults(kwargs)
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  # See rules/cxx.bzl's cxx_test macro for why: the coverage bxl now depends on
  # deno_test targets directly, which need to be reachable from the root
  # package without editing ts/BUCK or tsweb/BUCK's existing call sites.
  kwargs.setdefault("visibility", ["PUBLIC"])
  _deno_test_rule(**kwargs)

# --- deno_lint: shared shape for `deno fmt --check` and `deno lint` parity
# targets, wired up as buck2 tests.
def _deno_lint_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ctx.attrs.args + ctx.attrs.entries, "deno_lint")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True, labels = ["lint"]),
  ]

_deno_lint_rule = rule(
  impl = _deno_lint_impl,
  attrs = _CONSUMER_ATTRS | {
    "args": attrs.list(attrs.string()),
    "entries": attrs.list(attrs.string()),
  },
)

def deno_lint(**kwargs):
  _deno_lint_rule(**_consumer_defaults(kwargs))

# --- vite_build: `deno run --cached-only --frozen -A npm:vite@<v> build
# --config <config>`, declaring the built site directory as the output.
def _vite_build_impl(ctx: AnalysisContext) -> list[Provider]:
  _, out_dir = _stage_and_run(
    ctx,
    {},
    ["run", "--cached-only", "--frozen", "-A", "npm:vite@" + ctx.attrs.vite_version, "build", "--config", ctx.attrs.config],
    "vite_build",
    out_dir_name = ctx.attrs.out_dir,
  )
  # Packaging: the whole built site directory stages as one tree entry at
  # web/<site> under the root-level "web" sibling of bin/lib/libexec/
  # runtime/share. The per-site subdirectory (site_name, defaulting to the
  # target name) is what lets one package carry several vite_build sites
  # without their trees colliding at a shared "web" root.
  site = ctx.attrs.site_name or ctx.attrs.name
  info = package_info(ctx, entries = [PackageEntry(dest = "web/" + site, artifact = out_dir, kind = "tree", owner = str(ctx.label.raw_target()))])
  return [DefaultInfo(default_output = out_dir), info]

_vite_build_rule = rule(
  impl = _vite_build_impl,
  attrs = _CONSUMER_ATTRS | {
    "config": attrs.string(),
    "out_dir": attrs.string(),
    "site_name": attrs.option(attrs.string(), default = None),
    "vite_version": attrs.string(),
  } | PACKAGE_LABELS_ATTR,
)

def vite_build(**kwargs):
  _vite_build_rule(**_consumer_defaults(kwargs))

# --- tsconfig_emit / tsconfig_drift_test: deno.json stays authoritative (it
# is what --frozen deno check/test/vite actually read); these targets derive
# editor-facing tsconfig.json/prettier config FROM it, as buck-out build
# artifacts, not files written into the repo tree. The generator is a small
# embedded Python script (Python is already a build-time dependency of this
# repo's other lanes) that reads deno.json's compilerOptions/fmt and writes:
#   tsconfig.base.json           - shared compilerOptions
#   ts/tsconfig.json             - extends the base, plain .ts
#   tsweb/tsconfig.json          - extends the base, adds jsx/jsxImportSource
#   .prettierrc.yml              - derived from deno.json's fmt block
# tsconfig_drift_test reruns the same derivation and asserts the emitted
# compilerOptions/fmt fields still equal deno.json's own values, so a future
# deno.json edit that the generator's mapping fails to track is caught.
_TSCONFIG_GEN_PY = [
  "import json, sys",
  "deno_json_path, out_dir, mode = sys.argv[1], sys.argv[2], sys.argv[3]",
  "with open(deno_json_path) as f:",
  "    dj = json.load(f)",
  "co = dj.get('compilerOptions', {})",
  "fmt = dj.get('fmt', {})",
  "base = {",
  "    'compilerOptions': {",
  "        'strict': co.get('strict', True),",
  "        'noUncheckedIndexedAccess': co.get('noUncheckedIndexedAccess', True),",
  "        'target': 'ESNext',",
  "        'module': 'ESNext',",
  "        'moduleResolution': 'Bundler',",
  "        'lib': [{'dom': 'DOM', 'dom.iterable': 'DOM.Iterable', 'esnext': 'ESNext'}.get(x, x) for x in co.get('lib', []) if x != 'deno.ns'],",
  "        'skipLibCheck': True,",
  "    },",
  "}",
  "ts_cfg = {'extends': '../tsconfig.base.json', 'include': ['**/*.ts']}",
  "tsweb_cfg = {",
  "    'extends': '../tsconfig.base.json',",
  "    'compilerOptions': {",
  "        'jsx': co.get('jsx', 'react-jsx'),",
  "        'jsxImportSource': co.get('jsxImportSource', 'react'),",
  "    },",
  "    'include': ['**/*.ts', '**/*.tsx'],",
  "}",
  "prettier = {",
  "    'tabWidth': fmt.get('indentWidth', 2),",
  "    'useTabs': fmt.get('useTabs', False),",
  "    'semi': True,",
  "    'singleQuote': False,",
  "}",
  "if mode == 'emit':",
  "    import os",
  "    os.makedirs(os.path.join(out_dir, 'ts'), exist_ok=True)",
  "    os.makedirs(os.path.join(out_dir, 'tsweb'), exist_ok=True)",
  "    with open(os.path.join(out_dir, 'tsconfig.base.json'), 'w') as f:",
  "        json.dump(base, f, indent=2)",
  "        f.write('\\n')",
  "    with open(os.path.join(out_dir, 'ts', 'tsconfig.json'), 'w') as f:",
  "        json.dump(ts_cfg, f, indent=2)",
  "        f.write('\\n')",
  "    with open(os.path.join(out_dir, 'tsweb', 'tsconfig.json'), 'w') as f:",
  "        json.dump(tsweb_cfg, f, indent=2)",
  "        f.write('\\n')",
  "    with open(os.path.join(out_dir, '.prettierrc.yml'), 'w') as f:",
  "        for k, v in prettier.items():",
  "            f.write('%s: %s\\n' % (k, json.dumps(v)))",
  "elif mode == 'check':",
  "    assert base['compilerOptions']['strict'] == co.get('strict', True), 'strict drifted from deno.json'",
  "    assert base['compilerOptions']['noUncheckedIndexedAccess'] == co.get('noUncheckedIndexedAccess', True), 'noUncheckedIndexedAccess drifted'",
  "    assert tsweb_cfg['compilerOptions']['jsx'] == co.get('jsx', 'react-jsx'), 'jsx drifted from deno.json'",
  "    assert tsweb_cfg['compilerOptions']['jsxImportSource'] == co.get('jsxImportSource', 'react'), 'jsxImportSource drifted'",
  "    assert prettier['tabWidth'] == fmt.get('indentWidth', 2), 'prettier tabWidth drifted from deno.json fmt.indentWidth'",
  "    assert prettier['useTabs'] == fmt.get('useTabs', False), 'prettier useTabs drifted from deno.json fmt.useTabs'",
  "    with open(out_dir, 'w') as f:",
  "        f.write('ok\\n')",
  "else:",
  "    sys.exit('unknown mode: %s' % mode)",
]

def _tsconfig_emit_impl(ctx: AnalysisContext) -> list[Provider]:
  gen = ctx.actions.write(ctx.label.name + "-gen.py", _TSCONFIG_GEN_PY)
  out_dir = ctx.actions.declare_output(ctx.label.name, dir = True)
  ctx.actions.run(
    cmd_args(["python3", gen, ctx.attrs.deno_json, out_dir.as_output(), "emit"]),
    category = "tsconfig_emit",
    identifier = ctx.label.name,
  )
  return [DefaultInfo(default_output = out_dir)]

tsconfig_emit = rule(
  impl = _tsconfig_emit_impl,
  attrs = {
    "deno_json": attrs.source(),
  },
)

def _tsconfig_drift_test_impl(ctx: AnalysisContext) -> list[Provider]:
  gen = ctx.actions.write(ctx.label.name + "-gen.py", _TSCONFIG_GEN_PY)
  stamp = ctx.actions.declare_output(ctx.label.name + ".stamp")
  ctx.actions.run(
    cmd_args(["python3", gen, ctx.attrs.deno_json, stamp.as_output(), "check"]),
    category = "tsconfig_drift_check",
    identifier = ctx.label.name,
  )
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "tsconfig_drift", command = [command], run_from_project_root = True, labels = ["lint"]),
  ]

tsconfig_drift_test = rule(
  impl = _tsconfig_drift_test_impl,
  attrs = {
    "deno_json": attrs.source(),
  },
)

# --- deno_graph_check: drift check between the DECLARED source set (this
# target's own `srcs` plus its transitive `deps`' DenoSourcesInfo, exactly
# what every other consumer above stages) and the RESOLVED set `deno info
# --json --cached-only <entry>` actually walks into for each of `entries`.
# Catches the failure mode DenoSourcesInfo's diamond-safe folding cannot:
# a real import the graph author forgot to declare via `deps`/`srcs` at all,
# which `deno check`/`deno test` would only ever surface as a runtime
# "module not found" if the file happened to be missing from disk too - here
# it fails loudly at analysis-adjacent test time instead, before the missing
# dep ships. Runs cached-only/frozen/network-off like every other consumer
# (see module docstring); labeled "lint" so `buck2 test //... --labels lint`
# picks it up alongside deno_lint/tsconfig_drift_test.
#
# The checker script's marker-based path fixup ("/proj/" - see below) reuses
# the exact convention this file's module docstring documents for
# tools/coverage_merge.py's SF: path fixup: _stage_and_run (and this rule's
# own staging, which mirrors it) always names the staging directory exactly
# "proj", so slicing each resolved local path at its last "/proj/" segment
# recovers the same repo-relative staged path this file declares everywhere
# else.
_GRAPH_CHECK_PY = [
  "import json, sys, os",
  "from urllib.parse import urlsplit, unquote",
  "",
  "infos_dir, declared_path = sys.argv[1], sys.argv[2]",
  "with open(declared_path) as f:",
  "    declared = set(json.load(f))",
  "",
  "marker = os.sep + 'proj' + os.sep",
  "",
  "def to_rel(path):",
  "    idx = path.find(marker)",
  "    return path[idx + len(marker):] if idx != -1 else path",
  "",
  "missing = []",
  "seen_modules = 0",
  "for name in sorted(os.listdir(infos_dir)):",
  "    with open(os.path.join(infos_dir, name)) as f:",
  "        info = json.load(f)",
  "    for m in info.get('modules', []):",
  "        specifier = m.get('specifier', '')",
  "        if not specifier.startswith('file://'):",
  "            continue",
  "        local = m.get('local')",
  # `deno info --json` exits 0 even for a local import it cannot resolve as
  # a JS/TS module (e.g. a vite-only CSS/asset import, which staged
  # correctly but deno's own module graph refuses to classify) - it records
  # an "error" with local=null instead of failing outright, so `local`
  # alone cannot distinguish a real missing file from a real,
  # already-declared, non-JS asset. Derive the candidate repo-relative path
  # from `local` when present, or from the specifier URL otherwise, and
  # check declared membership either way - only a path that is in neither
  # state is drift.
  "        seen_modules += 1",
  "        rel = to_rel(local) if local else to_rel(unquote(urlsplit(specifier).path))",
  "        if rel not in declared and rel not in ('deno.json', 'deno.lock'):",
  "            missing.append(rel)",
  "",
  "if missing:",
  "    print('deno_graph_check: resolved local files missing from the declared srcs/deps graph:', file=sys.stderr)",
  "    for m in sorted(set(missing)):",
  "        print('  ' + m, file=sys.stderr)",
  "    print('add the missing file(s) to a deno_library/deno_app srcs= (and wire it into deps=) so the staged tree covers them.', file=sys.stderr)",
  "    sys.exit(1)",
  "print('deno_graph_check: ok (%d resolved local modules, %d declared)' % (seen_modules, len(declared)))",
]

def _deno_graph_check_impl(ctx: AnalysisContext) -> list[Provider]:
  srcs = _flatten_deno_sources(ctx, ctx.attrs.deps)
  srcs.update(ctx.attrs.srcs)
  layout = _entries_to_layout(ctx.attrs.deno_json, ctx.attrs.deno_lock, srcs)
  staged = ctx.actions.symlinked_dir(ctx.label.name + "-staged", layout)

  declared = ctx.actions.write_json(ctx.label.name + "-declared.json", sorted(layout.keys()))
  checker = ctx.actions.write(ctx.label.name + "-check.py", _GRAPH_CHECK_PY)

  deno_dir = ctx.attrs.deno_dir[DefaultInfo].default_outputs[0]
  node_modules = ctx.attrs.node_modules[DefaultInfo].default_outputs[0]
  deno = _deno_bin(ctx)
  stamp = ctx.actions.declare_output(ctx.label.name + ".stamp")

  script = ctx.actions.write(
    ctx.label.name + "-graph.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(pwd)",
      "STAGED=\"$ROOT/$1\"",
      "DENO_DIR_SRC=\"$ROOT/$2\"",
      "NODE_MODULES=\"$ROOT/$3\"",
      "DENO=\"$ROOT/$4/deno\"",
      "DECLARED=\"$ROOT/$5\"",
      "CHECKER=\"$ROOT/$6\"",
      "STAMP=\"$ROOT/$7\"",
      "shift 7",
      "chmod +x \"$DENO\"",
      "WORK=$(mktemp -d)",
      "trap 'rm -rf \"$WORK\"' EXIT",
      "mkdir -p \"$WORK/proj\" \"$WORK/denodir\" \"$WORK/infos\"",
      "cp -RL \"$STAGED\"/. \"$WORK/proj\"/",
      "chmod -R u+w \"$WORK/proj\"",
      "cp -R \"$DENO_DIR_SRC\"/. \"$WORK/denodir\"/",
      "chmod -R u+w \"$WORK/denodir\"",
      "ln -s \"$NODE_MODULES\" \"$WORK/proj/node_modules\"",
      "export DENO_DIR=\"$WORK/denodir\"",
      "export DENO_NO_UPDATE_CHECK=1",
      "cd \"$WORK/proj\"",
      "i=0",
      "for entry in \"$@\"; do",
      "  i=$((i + 1))",
      # `deno info` has no --cached-only/--frozen flag (unlike check/test/run
      # above) - it never touches the network on its own; DENO_NO_UPDATE_CHECK
      # plus this action having no network access is what keeps it hermetic.
      "  \"$DENO\" info --json \"$entry\" >\"$WORK/infos/$i.json\"",
      "done",
      "cd \"$ROOT\"",
      "python3 \"$CHECKER\" \"$WORK/infos\" \"$DECLARED\"",
      "echo ok >\"$STAMP\"",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", script, staged, deno_dir, node_modules, deno, declared, checker, stamp.as_output()] + _resolve_entries(ctx)),
    category = "deno_graph_check",
    identifier = ctx.label.name,
  )
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno_graph_check", command = [command], run_from_project_root = True, labels = ["lint"]),
  ]

_deno_graph_check_rule = rule(
  impl = _deno_graph_check_impl,
  # Optional (default []) - see deno_check's identical comment above.
  attrs = _CONSUMER_ATTRS | {"entries": attrs.list(attrs.string(), default = [])},
)

def deno_graph_check(**kwargs):
  _deno_graph_check_rule(**_consumer_defaults(kwargs))

# --- smoke_test: reuses an existing, read-only python3 validation script
# (e.g. tools/tsweb_smoke.py) against a built artifact directory.
def _smoke_test_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp = ctx.actions.declare_output(ctx.label.name + ".stamp")
  script = ctx.actions.write(
    ctx.label.name + "-smoke.sh",
    [
      "#!/bin/sh",
      "set -eu",
      "SCRIPT=$1",
      "ROOT=$2",
      "STAMP=$3",
      "shift 3",
      "python3 \"$SCRIPT\" --root \"$ROOT\" \"$@\"",
      "echo ok >\"$STAMP\"",
    ],
    is_executable = True,
  )
  ctx.actions.run(
    cmd_args(["/bin/sh", script, ctx.attrs.script, ctx.attrs.root, stamp.as_output()] + ctx.attrs.extra_args),
    category = "smoke_test",
    identifier = ctx.label.name,
  )
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "smoke", command = [command], run_from_project_root = True),
  ]

smoke_test = rule(
  impl = _smoke_test_impl,
  attrs = {
    "extra_args": attrs.list(attrs.string(), default = []),
    "root": attrs.source(),
    "script": attrs.source(),
  },
)
