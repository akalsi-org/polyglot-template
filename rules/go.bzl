"""First-party go_binary / go_test / go_lint rules for the go lane.

Mirrors repo.sh's go/go-build/go-test/go-lint semantics: builds, tests, and
lints with the pinned in-graph Go 1.26.5 toolchain (GOROOT from the
extracted toolchain, GOTOOLCHAIN=local, CGO_ENABLED=0, GOEXPERIMENT=jsonv2,
-trimpath), module github.com/akalsi-org/polyglot-template. Go binaries
built with CGO_ENABLED=0 are static, so unlike rules/cxx.bzl's cxx_binary
there is no musl-loader launcher to wrap them in: RunInfo points straight at
`go build`'s output.

Determinism / caching note: `go build`/`go test`/`go vet` each keep their
own content-addressed build cache under GOCACHE (see
https://go.dev/cmd/go/#hdr-Build_and_test_caching). Left at its default
location that cache is process-global and would leak state across buck2
actions (and across repo.sh's own `go`/`go-build`/`go-test` passthroughs'
GOCACHE under .local/cache/go) if reused here - but unlike GOPATH/GOMODCACHE
below, GOCACHE is *safe* to share across concurrent buck2 actions: it is
itself content-addressed (keyed by action ID, a hash of inputs+toolchain
version), so two actions writing the same key write the same bytes, and two
actions writing different keys never collide. Every action below therefore
points GOCACHE at a stable, shared directory under
buck-out/go-build-cache/<target-triple> (see _GOCACHE_DIR below) -
deliberately a sibling of buck-out/v2, not a path inside it. buck2 owns
buck-out/v2 end-to-end (content-addressed gen/ outputs, its own tmp/
scratch dirs) and materializes/tracks exactly what it put there; a path
buck2 never wrote to and never lists as a declared output or hidden input
is invisible to it, so buck-out/go-build-cache survives untouched across
buck2's own action-cache bookkeeping while still being a `buck-out` path
that `buck2 clean` and repo cleanup conventions expect to be safe to
delete. This is the same doctrine rules/deno.bzl uses for its sqlite
scratch: the directory is DELIBERATELY outside buck2's input/output
tracking, which is sound here because GOCACHE only ever affects how fast a
`go build`/`go test`/`go vet` invocation runs (cache hit vs. miss), never
what it *produces* - buck2's own declared outputs (the binary, the test
result, the coverage profile) are unaffected by GOCACHE's contents, so
buck2 not tracking this directory cannot make a build produce stale or
incorrect artifacts, only a slower one on a cold cache. GOPATH/GOMODCACHE
(module downloads/extraction - this repo has no external modules today,
but the mechanism is generic) and Go's $WORK compile/vet intermediates
(via TMPDIR) remain per-action `mktemp -d` scratch as before: those are
small, and unlike GOCACHE are not documented by `go help cache` as safe for
concurrent multi-process sharing, so they keep the private/EXIT-trap
lifecycle. ($BUCK_SCRATCH_PATH, buck2's own per-build-action scratch dir,
would give the same exclusivity guarantee for go_binary's ctx.actions.run,
but go_test/go_lint run their command directly as
ExternalRunnerTestInfo/RunInfo outside that action context, where
$BUCK_SCRATCH_PATH is unset; mktemp -d works identically in both, so it is
used everywhere for consistency.) buck2 then layers its own action-level
cache on top, keyed on the declared inputs (the go/ source files passed as
hidden cmd_args below); unchanged inputs mean buck2 skips re-invoking
`go build`/`go test`/`gofmt`/`go vet` entirely on a cache hit, and a change
to any declared input invalidates only the actions that read it - GOCACHE
sharing only matters on the remaining case, an actual buck2 cache miss,
where it turns a cold Go recompile into a warm one.

go.mod lives at the repo root, outside go/'s own package; it is tracked here
via the root //BUCK's `export_file(name = "go.mod")` (see _TOOLCHAIN_ATTRS'
`_gomod`) rather than a plain `attrs.source("//go.mod")` string, since
buck2 requires a target - not just a path - to reference a source file
across a package boundary. Threading it into every action's hidden inputs
alongside the go/ sources means a go.mod-only edit invalidates buck2's
action cache for these targets exactly as it should.

Toolchain artifact tracking (fixed bug): every script below needs the path
to the extracted go toolchain (tools.goroot), embedded as literal text so
the emitted shell reads naturally. That text-only reference is not enough
on its own for buck2 to treat the toolchain directory as a real input:
standalone `buck2 build //go/app/hello:hello` worked, but reached transitively
through another target with a different configuration (e.g.
//packages:polyglot-demo, which builds go:hello under a target platform
instead of go:hello's own default), the toolchain extraction for that
OTHER configuration was a cache hit buck2 had no reason to materialize -
"go: not found" even though the file exists elsewhere in buck-out under a
different config hash. Every call site below now also passes tools.go_dir
(the real Artifact) into its action's `hidden` list directly, which is
what actually makes buck2 track and materialize it per-configuration.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//config:flags.bzl", "coverage_enabled_flag")
load("//rules:coverage.bzl", "CoverageInfo")
load("//rules:pkg.bzl", "PACKAGE_LABELS_ATTR", "PackageEntry", "check_pkg_name", "package_info")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors rules/cxx.bzl's _native_target() / toolchains/defs.bzl's: this
  # repo only ever builds+runs the host's own musl output triplet.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()

# Persistent, shared GOCACHE - see module docstring's "Determinism / caching
# note" for why this is sound outside buck2's own input/output tracking.
# Segmented per _NATIVE_TARGET so a hypothetical future multi-configuration
# build (see fail_if_cross_arch's cross-arch guard) never has two different
# target triples' object code landing in the same GOCACHE.
_GOCACHE_DIR = "buck-out/go-build-cache/" + _NATIVE_TARGET

_GO = TOOLCHAINS["go"][_NATIVE_TARGET]
_GO_BIN_DIR = _GO["expected"].rsplit("/", 1)[0]  # "go/bin"
_GOROOT_REL = _GO_BIN_DIR.rsplit("/", 1)[0]  # "go"

# GoSourceSet: a transitive_set of Artifact lists, mirroring rules/deno.bzl's
# DenoSourceSet / rules/pkg.bzl's PackageEntrySet convention repo-wide - see
# their own doc comments for the general rule this repeats. GoInfo.srcs used
# to be a plain flat list, rebuilt by simple `+=` concatenation
# (_merge_dep_srcs) at every level of the dep graph; that duplicates a
# diamond-shared dep's sources once per path that reaches it (e.g. two
# targets both depending on the same go_library each re-flatten and
# re-concatenate its sources into their own GoInfo.srcs, and a third target
# depending on both would then see that shared library's sources twice).
# Using a tset instead makes buck2's own DAG dedupe the work: two tsets that
# share a descendant node point at the SAME node rather than copying its
# contents, so a single traverse() over a merged node visits it exactly once
# no matter how many parents reach it.
GoSourceSet = transitive_set()
GoInfo = provider(fields = ["srcs"])  # `srcs`: a GoSourceSet transitive_set

def _go_srcs(ctx: AnalysisContext, own_srcs: list, deps: list):
  """Builds this target's own GoSourceSet tset (own_srcs as this node's own
  value, each dep's GoInfo.srcs tset as a child) and flattens it via a
  SINGLE traverse() into an order-stable, deduped Artifact list - diamond-
  safe, since traverse() visits each DAG node exactly once regardless of how
  many parents reach it. Returns (flattened_list, tset): the flattened list
  is for this target's own local use (hidden inputs, gofmt's file list); the
  tset is what this target's own GoInfo(srcs=...) should carry forward, so a
  further consumer's own traversal stays a single pass too instead of
  re-flattening an already-flat list at every level of the graph."""
  tset = ctx.actions.tset(GoSourceSet, value = list(own_srcs), children = [d[GoInfo].srcs for d in deps])
  seen = {}
  flattened = []
  for value in tset.traverse():
    for src in value:
      key = str(src)
      if key not in seen:
        seen[key] = True
        flattened.append(src)
  return flattened, tset

_TOOLCHAIN_ATTRS = {
  "_go": attrs.dep(default = "//toolchains:go-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_gomod": attrs.source(default = "//:go.mod"),
  "_target_arch": target_arch_attr(),
}

def _toolchain_tools(ctx):
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  go_dir = ctx.attrs._go[DefaultInfo].default_outputs[0]
  # go_dir is kept alongside goroot (a .project() of it) so callers can pass
  # the real artifact into `hidden` directly - see _write_go_script's
  # comment on why embedding it as script text via cmd_args() is not
  # sufficient on its own for buck2 to track/materialize it correctly.
  return struct(go_dir = go_dir, goroot = go_dir.project(_GOROOT_REL))

# Common preamble shared by build/test/lint scripts: point GOCACHE at the
# persistent shared cache and GOPATH/GOMODCACHE/TMPDIR at this action's
# exclusive scratch dir (see module docstring), pin the same env repo.sh's
# `go`/go-build/go-test set, and put the pinned go/gofmt on PATH.
# `tail_lines` is the mode-specific command(s) appended after the preamble.
#
# tools.goroot is embedded here as literal path TEXT (via cmd_args(...,
# delimiter = "")), not as a standalone cmd_args element, purely so the
# emitted shell script reads naturally (`GOROOT="$(pwd)/<path>"`). That
# text-only reference does NOT reliably register the underlying toolchain
# artifact as a tracked input: allow_args=True's "written" return is meant
# for exactly this, but concatenating an artifact inline with literal
# prefix/suffix strings turned out not to survive into a hidden-worthy
# reference in practice (this broke under a different build configuration -
# see module docstring). Callers MUST additionally pass tools.go_dir
# (the real Artifact, not a path-string derivative of it) into their own
# `hidden` list; `written` is still returned/used here as an extra safety
# net for the ctx.attrs._gomod-style hidden inputs, but is not load-bearing
# for the toolchain artifact itself anymore.
def _write_go_script(ctx, name, tools, tail_lines):
  lines = [
    "#!/bin/sh",
    "set -eu",
    # Both GOROOT and GOPATH must be absolute (`go` rejects relative
    # GOPATH outright, and a relative GOROOT breaks go build's own
    # internal invocation of GOROOT-relative tools like
    # pkg/tool/*/asm once it changes into its own work dir). Actions
    # run with cwd at the project root (see AnalysisActions.run docs),
    # so anchor every buck2-relative path with $(pwd) rather than
    # assuming the artifact path buck2 hands us is already absolute.
    cmd_args("GOROOT=\"$(pwd)/", tools.goroot, "\"", delimiter = ""),
    "export GOROOT",
    # $BUCK_SCRATCH_PATH is only populated for ctx.actions.run build
    # actions, not for the plain RunInfo/ExternalRunnerTestInfo command
    # `buck2 test`/`buck2 run` execute outside that action context - so
    # go_test and go_lint would see it unset. A repo-local mktemp -d
    # works identically in both contexts and gives the same
    # one-action/one-run exclusivity guarantee; trap removes it so
    # nothing survives past this invocation, mirroring what
    # $BUCK_SCRATCH_PATH would have done. Deliberately anchored under
    # buck-out rather than bare `mktemp -d` (which defaults to /tmp): CI
    # and dev boxes alike often back /tmp with a small tmpfs shared by
    # every concurrent build/test on the machine, and Go's own build
    # cache is not small.
    "SCRATCH_BASE=\"$(pwd)/buck-out/v2/tmp/go-rules\"",
    "mkdir -p \"$SCRATCH_BASE\"",
    "SCRATCH=$(mktemp -d \"$SCRATCH_BASE/tmp.XXXXXX\")",
    "trap 'rm -rf \"$SCRATCH\"' EXIT",
    # GOCACHE is deliberately NOT under $SCRATCH: it is a stable, shared
    # directory outside buck2's own tracking (buck-out/v2), reused across
    # every invocation and every concurrent action - see module docstring's
    # "Determinism / caching note" for why sharing it is safe.
    "export GOCACHE=\"$(pwd)/" + _GOCACHE_DIR + "\"",
    "export GOPATH=\"$SCRATCH/gopath\"",
    "export GOMODCACHE=\"$SCRATCH/gomodcache\"",
    # `go`'s own $WORK build directory (compile/vet intermediates) is
    # independent of GOCACHE and defaults to TMPDIR/tmp - redirect it
    # too, for the same shared-/tmp reason as SCRATCH_BASE above.
    "export TMPDIR=\"$SCRATCH/tmp\"",
    "mkdir -p \"$GOCACHE\" \"$GOPATH\" \"$GOMODCACHE\" \"$TMPDIR\"",
    "export CGO_ENABLED=0 GOTOOLCHAIN=local GOEXPERIMENT=jsonv2 GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off",
    "export PATH=\"$GOROOT/bin:$PATH\"",
  ] + tail_lines
  return ctx.actions.write(name + ".sh", lines, is_executable = True, allow_args = True)

# --- go_library: source-set-only target so go_binary/go_test/go_lint can
# depend on a shared package (go/lib/greeting) and pick up its sources as
# hidden inputs for correct cache invalidation. `go build`/`go test`/`go
# vet` resolve the actual import graph themselves by walking the module on
# disk (they are handed a package path, not a file list), so this provider
# exists purely for buck2's own input tracking, not for the Go toolchain.

def _go_library_impl(ctx: AnalysisContext) -> list[Provider]:
  _flattened, tset = _go_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  # No emission of its own (mirrors rules/cxx.bzl's cxx_library) - still
  # folds deps' PackageInfo transitively for uniformity.
  return [
    DefaultInfo(default_outputs = list(ctx.attrs.srcs)),
    GoInfo(srcs = tset),
    package_info(ctx, deps = ctx.attrs.deps),
  ]

go_library = rule(
  impl = _go_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "srcs": attrs.list(attrs.source()),
  } | PACKAGE_LABELS_ATTR,
)

# --- go_binary: `go build -trimpath` of one package, mirroring repo.sh's
# go-build (`go build -trimpath -o build/go/$target/hello ./go/app/hello`).

def _go_binary_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  own_srcs, srcs_tset = _go_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  all_srcs = own_srcs + [ctx.attrs._gomod]
  binary = ctx.actions.declare_output(ctx.attrs.name)
  tail = [
    "PKG=" + ctx.attrs.package,
    # NOT `exec`: exec replaces the shell, so the EXIT trap that removes
    # $SCRATCH would never fire and every action would leak its scratch
    # dir (observed: ~63M per invocation accumulating under
    # buck-out/v2/tmp/go-rules). Run as a child; the script's exit status
    # is still the go command's status.
    "\"$GOROOT/bin/go\" build -trimpath -o \"$1\" \"$PKG\"",
  ]
  script, written = _write_go_script(ctx, ctx.attrs.name + "-build", tools, tail)
  ctx.actions.run(
    cmd_args(["/bin/sh", script, binary.as_output()], hidden = all_srcs + written + [tools.go_dir]),
    category = "go_build",
    identifier = ctx.attrs.name,
  )
  # Packaging: go build with CGO_ENABLED=0 is static, so unlike cxx_binary
  # this stages straight to bin/<pkg_name> with no loader-wrapping needed -
  # "static-bin" is plain staging in rules/package.bzl's kind dispatch.
  pkg_name = check_pkg_name(ctx, ctx.attrs.pkg_name or ctx.attrs.name)
  info = package_info(
    ctx,
    entries = [PackageEntry(dest = "bin/" + pkg_name, artifact = binary, kind = "static-bin", owner = str(ctx.label.raw_target()))],
    deps = ctx.attrs.deps,
  )
  return [
    DefaultInfo(default_output = binary),
    RunInfo(args = cmd_args(binary)),
    # GoInfo, same shape go_library emits: lets a lane-wide lint target (e.g.
    # go/BUCK's gofmt_check/go_vet) reach this binary's own sources plus its
    # transitive deps' sources via `deps=` alone, now that the per-component
    # split means glob(["**/*.go"]) from go/BUCK's own package can no longer
    # see files that moved into go/app/*/BUCK's own package boundary.
    # Deliberately excludes ctx.attrs._gomod (go.mod is not a .go file;
    # go_lint's fmt mode would otherwise hand it to `gofmt -l`). Carries the
    # tset forward (not the flattened own_srcs list) so a further consumer's
    # own traversal stays diamond-safe - see _go_srcs' doc comment.
    GoInfo(srcs = srcs_tset),
    info,
  ]

_go_binary_rule = rule(
  impl = _go_binary_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "package": attrs.string(),
    "pkg_name": attrs.option(attrs.string(), default = None),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS | PACKAGE_LABELS_ATTR,
)

# --- go_test: `go test -trimpath` over a package set, mirroring repo.sh's
# go-test (`go test ./go/...`), exposed as an ExternalRunnerTestInfo target
# so `buck2 test` can run it directly instead of going through go_binary +
# a separate runner.

# Coverage collection runs `go test` a SECOND time, with -coverprofile
# pointed at a declared output, as its own ctx.actions.run() build action -
# separate from the ExternalRunnerTestInfo/RunInfo path `buck2 test` uses
# for pass/fail reporting - so the profile is a real cacheable build
# artifact //:coverage can depend on (mirrors rules/cxx.bzl's
# _coverage_collect_action; see rules/coverage.bzl's module docstring for
# why every lane does it this way). -coverpkg is set to the same package
# set under test so coverage of an imported-but-not-directly-tested package
# (e.g. go/lib/greeting, exercised only via go/test's external test
# package) is still attributed - plain `go test -coverprofile` on its own
# only instruments the package(s) actually under test.
def _go_test_coverage_action(ctx, tools, all_srcs):
  cov_file = ctx.actions.declare_output(ctx.attrs.name + ".cov")
  coverpkg = ",".join(ctx.attrs.packages)
  tail = [
    # No `exec` - see go_binary's tail comment (EXIT trap must fire).
    "\"$GOROOT/bin/go\" test -trimpath -coverpkg=" + coverpkg +
    " -coverprofile=\"$1\" " + " ".join(ctx.attrs.packages),
  ]
  script, written = _write_go_script(ctx, ctx.attrs.name + "-cov", tools, tail)
  ctx.actions.run(
    cmd_args(["/bin/sh", script, cov_file.as_output()], hidden = all_srcs + written + [tools.go_dir]),
    category = "go_test_coverage",
    identifier = ctx.attrs.name,
  )
  return cov_file

def _go_test_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  own_srcs, srcs_tset = _go_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  all_srcs = own_srcs + [ctx.attrs._gomod]
  tail = [
    # No `exec` - see go_binary's tail comment (EXIT trap must fire).
    "\"$GOROOT/bin/go\" test -trimpath " + " ".join(ctx.attrs.packages),
  ]
  script, written = _write_go_script(ctx, ctx.attrs.name, tools, tail)
  command = cmd_args(script, hidden = all_srcs + written + [tools.go_dir])
  providers = [
    DefaultInfo(default_output = script),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "go_test",
      command = [command],
      run_from_project_root = True,
    ),
    # See go_binary's identical GoInfo addition above for why (tset, not the
    # flattened list - diamond-safe forwarding).
    GoInfo(srcs = srcs_tset),
  ]
  if ctx.attrs._coverage_enabled:
    cov_file = _go_test_coverage_action(ctx, tools, all_srcs)
    providers.append(CoverageInfo(
      kind = "go_profile",
      primary = cov_file,
      tool = None,
      gcnos = None,
      toolchain_dir = None,
    ))
  return providers

_go_test_rule = rule(
  impl = _go_test_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "packages": attrs.list(attrs.string(), default = ["./go/..."]),
    "srcs": attrs.list(attrs.source(), default = []),
    "_coverage_enabled": attrs.bool(default = coverage_enabled_flag()),
  } | _TOOLCHAIN_ATTRS,
)

# Every go_* rule except go_library now carries a select()-driven attr
# default (_TOOLCHAIN_ATTRS' _target_arch, resolved via config/defs.bzl's
# target_arch_attr() - see MAJOR 3's fail_if_cross_arch()/module docstring;
# go_test additionally has its own _coverage_enabled). select() only
# resolves when the target has a concrete configuration, which - absent a
# prelude - requires `default_target_platform` to be set explicitly (see
# rules/cxx.bzl's identical _DEFAULT_PLATFORM/macro pattern, used here for
# the same reason). go_library has no select()-driven attrs (no
# _TOOLCHAIN_ATTRS at all) and so is exported as a bare rule() below instead
# of needing a macro.
_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

def go_binary(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _go_binary_rule(**kwargs)

def go_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  # See rules/cxx.bzl's cxx_test macro for why: //:coverage now depends on
  # go_test targets directly, which need to be reachable from the root
  # package without editing every existing go/BUCK call site.
  kwargs.setdefault("visibility", ["PUBLIC"])
  _go_test_rule(**kwargs)

# --- go_lint: two flavors selected by `mode`, mirroring repo.sh's go-lint
# lane semantics (gofmt-canonical formatting + `go vet`), each its own
# ExternalRunnerTestInfo target so lint has the same per-lane test/lint
# parity as the cxx and other lanes instead of being deferred.

def _go_lint_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  # gofmt itself doesn't read go.mod, but go vet does, and keeping the
  # hidden-input set uniform across both modes is simpler than splitting it.
  # go_lint is a leaf consumer (nothing depends on ITS GoInfo), so only the
  # flattened list from _go_srcs is needed here - the tset it also returns
  # is discarded.
  own_srcs, _tset = _go_srcs(ctx, ctx.attrs.srcs, ctx.attrs.deps)
  all_srcs = own_srcs + [ctx.attrs._gomod]
  if ctx.attrs.mode == "fmt":
    # own_srcs (not ctx.attrs.srcs alone): after the per-component split,
    # go/BUCK's own package has no .go files directly in it any more - every
    # file gofmt needs to see arrives via `deps=`' GoInfo (go_library's own
    # shape, now also emitted by go_binary/go_test - see their own doc
    # comments above) instead of a whole-tree glob() that package boundaries
    # would otherwise silently truncate.
    fmt_check = cmd_args(["OUT=$(", "\"$GOROOT/bin/gofmt\"", "-l"] + own_srcs + [")"], delimiter = " ")
    tail = [
      fmt_check,
      "if [ -n \"$OUT\" ]; then printf '%s\\n' \"$OUT\" >&2; " +
      "echo 'gofmt: files not canonically formatted (run: go fmt ./go/...)' >&2; exit 1; fi",
    ]
  elif ctx.attrs.mode == "vet":
    tail = [
      # No `exec` - see go_binary's tail comment (EXIT trap must fire).
      "\"$GOROOT/bin/go\" vet " + " ".join(ctx.attrs.packages),
    ]
  else:
    fail("go_lint: unknown mode '{}' (want 'fmt' or 'vet')".format(ctx.attrs.mode))
  script, written = _write_go_script(ctx, ctx.attrs.name, tools, tail)
  command = cmd_args(script, hidden = all_srcs + written + [tools.go_dir])
  return [
    DefaultInfo(default_output = script),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "go_lint_" + ctx.attrs.mode,
      command = [command],
      run_from_project_root = True,
      labels = ["lint"],
    ),
  ]

_go_lint_rule = rule(
  impl = _go_lint_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "mode": attrs.string(),
    "packages": attrs.list(attrs.string(), default = ["./go/..."]),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)

def go_lint(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _go_lint_rule(**kwargs)
