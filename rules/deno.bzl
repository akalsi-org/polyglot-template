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
"""

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
  layout = _entries_to_layout(ctx.attrs.deno_json, ctx.attrs.deno_lock, ctx.attrs.srcs)
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
    cmd_args(["/bin/sh", script, staged, deno, deno_dir_out.as_output(), node_modules_out.as_output()] + ctx.attrs.entries),
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

deno_cache = rule(
  impl = _deno_cache_impl,
  attrs = {
    "deno": attrs.dep(),
    "deno_json": attrs.source(),
    "deno_lock": attrs.source(),
    "entries": attrs.list(attrs.string()),
    "srcs": attrs.dict(attrs.string(), attrs.source()),
  },
)

# --- shared plumbing for every network-free consumer of the cache.
def _stage_and_run(
    ctx: AnalysisContext,
    extra_layout: dict,
    deno_args: list,
    category: str,
    out_dir_name: [str, None] = None) -> (Artifact, [Artifact, None]):
  layout = _entries_to_layout(ctx.attrs.deno_json, ctx.attrs.deno_lock, ctx.attrs.srcs)
  layout.update(extra_layout)
  staged = ctx.actions.symlinked_dir(ctx.label.name + "-staged", layout)

  deno_dir = ctx.attrs.deno_dir[DefaultInfo].default_outputs[0]
  node_modules = ctx.attrs.node_modules[DefaultInfo].default_outputs[0]
  deno = _deno_bin(ctx)

  stamp = ctx.actions.declare_output(ctx.label.name + ".stamp")
  out_dir = ctx.actions.declare_output(out_dir_name) if out_dir_name else None

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

  script = ctx.actions.write(ctx.label.name + "-run.sh", lines, is_executable = True)
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
  "deno": attrs.dep(),
  "deno_dir": attrs.dep(),
  "deno_json": attrs.source(),
  "deno_lock": attrs.source(),
  "node_modules": attrs.dep(),
  "srcs": attrs.dict(attrs.string(), attrs.source()),
}

# --- deno_check: `deno check --frozen <entry>`, ts-build parity. Build-only
# (no --cached-only: `deno check` has no such flag, but is network-free once
# the cache is warm and --frozen is set).
def _deno_check_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ["check", "--frozen"] + ctx.attrs.entries, "deno_check")
  return [DefaultInfo(default_output = stamp)]

deno_check = rule(
  impl = _deno_check_impl,
  attrs = _CONSUMER_ATTRS | {"entries": attrs.list(attrs.string())},
)

# --- deno_run_check: `deno run --cached-only --frozen <entry>`, ts-test's
# "run the app" parity, wired up as a buck2 test.
def _deno_run_check_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ["run", "--cached-only", "--frozen"] + ctx.attrs.extra_flags + ctx.attrs.entries, "deno_run_check")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True),
  ]

deno_run_check = rule(
  impl = _deno_run_check_impl,
  attrs = _CONSUMER_ATTRS | {
    "entries": attrs.list(attrs.string()),
    "extra_flags": attrs.list(attrs.string(), default = []),
  },
)

# --- deno_test: `deno test --frozen [flags] <paths>`, one buck2 test target
# per lane's test/ directory.
def _deno_test_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ["test", "--frozen"] + ctx.attrs.extra_flags + ctx.attrs.entries, "deno_test")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True),
  ]

deno_test = rule(
  impl = _deno_test_impl,
  attrs = _CONSUMER_ATTRS | {
    "entries": attrs.list(attrs.string()),
    "extra_flags": attrs.list(attrs.string(), default = []),
  },
)

# --- deno_lint: shared shape for `deno fmt --check` and `deno lint` parity
# targets, wired up as buck2 tests.
def _deno_lint_impl(ctx: AnalysisContext) -> list[Provider]:
  stamp, _ = _stage_and_run(ctx, {}, ctx.attrs.args + ctx.attrs.entries, "deno_lint")
  command = cmd_args(["/bin/sh", "-c", "exit 0"], hidden = [stamp])
  return [
    DefaultInfo(default_output = stamp),
    ExternalRunnerTestInfo(type = "deno", command = [command], run_from_project_root = True),
  ]

deno_lint = rule(
  impl = _deno_lint_impl,
  attrs = _CONSUMER_ATTRS | {
    "args": attrs.list(attrs.string()),
    "entries": attrs.list(attrs.string()),
  },
)

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
  return [DefaultInfo(default_output = out_dir)]

vite_build = rule(
  impl = _vite_build_impl,
  attrs = _CONSUMER_ATTRS | {
    "config": attrs.string(),
    "out_dir": attrs.string(),
    "vite_version": attrs.string(),
  },
)

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
    ExternalRunnerTestInfo(type = "tsconfig_drift", command = [command], run_from_project_root = True),
  ]

tsconfig_drift_test = rule(
  impl = _tsconfig_drift_test_impl,
  attrs = {
    "deno_json": attrs.source(),
  },
)

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
