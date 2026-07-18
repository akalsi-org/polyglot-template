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
actions (and across the moon lane's own GOCACHE under .local/cache/go) if
reused here. Every action below instead points GOCACHE/GOPATH/GOMODCACHE at
a `mktemp -d` scratch directory private to that one invocation, torn down
via an EXIT trap - never the repo's shared .local/cache/go and never the
invoking user's ~/.cache. ($BUCK_SCRATCH_PATH, buck2's own per-build-action
scratch dir, would give the same exclusivity guarantee for go_binary's
ctx.actions.run, but go_test/go_lint run their command directly as
ExternalRunnerTestInfo/RunInfo outside that action context, where
$BUCK_SCRATCH_PATH is unset; mktemp -d works identically in both, so it is
used everywhere for consistency.) Go's own cache therefore never survives
past one invocation and never collides with a concurrent one. buck2 then
layers its own action-level cache on top, keyed on the declared inputs (the
go/ source files passed as hidden cmd_args below); unchanged inputs mean
buck2 skips re-invoking
`go build`/`go test`/`gofmt`/`go vet` entirely on a cache hit, and a change
to any declared input invalidates only the actions that read it.

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
standalone `buck2 build //go:hello` worked, but reached transitively
through another target with a different configuration (e.g.
//packages:polyglot-demo, which builds go:hello under a target platform
instead of go:hello's own default), the toolchain extraction for that
OTHER configuration was a cache hit buck2 had no reason to materialize -
"go: not found" even though the file exists elsewhere in buck-out under a
different config hash. Every call site below now also passes tools.go_dir
(the real Artifact) into its action's `hidden` list directly, which is
what actually makes buck2 track and materialize it per-configuration.
"""

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
_GO = TOOLCHAINS["go"][_NATIVE_TARGET]
_GO_BIN_DIR = _GO["expected"].rsplit("/", 1)[0]  # "go/bin"
_GOROOT_REL = _GO_BIN_DIR.rsplit("/", 1)[0]  # "go"

GoInfo = provider(fields = ["srcs"])

_TOOLCHAIN_ATTRS = {
  "_go": attrs.dep(default = "//toolchains:go-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_gomod": attrs.source(default = "//:go.mod"),
}

def _toolchain_tools(ctx):
  go_dir = ctx.attrs._go[DefaultInfo].default_outputs[0]
  # go_dir is kept alongside goroot (a .project() of it) so callers can pass
  # the real artifact into `hidden` directly - see _write_go_script's
  # comment on why embedding it as script text via cmd_args() is not
  # sufficient on its own for buck2 to track/materialize it correctly.
  return struct(go_dir = go_dir, goroot = go_dir.project(_GOROOT_REL))

def _merge_dep_srcs(deps):
  srcs = []
  for dep in deps:
    srcs += dep[GoInfo].srcs
  return srcs

# Common preamble shared by build/test/lint scripts: point Go's own caches
# at this action's exclusive scratch dir (see module docstring), pin the
# same env repo.sh's `go`/go-build/go-test set, and put the pinned go/gofmt
# on PATH. `tail_lines` is the mode-specific command(s) appended after the
# preamble.
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
    "export GOCACHE=\"$SCRATCH/gocache\"",
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
  srcs = list(ctx.attrs.srcs) + _merge_dep_srcs(ctx.attrs.deps)
  return [
    DefaultInfo(default_outputs = list(ctx.attrs.srcs)),
    GoInfo(srcs = srcs),
  ]

go_library = rule(
  impl = _go_library_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "srcs": attrs.list(attrs.source()),
  },
)

# --- go_binary: `go build -trimpath` of one package, mirroring repo.sh's
# go-build (`go build -trimpath -o build/go/$target/hello ./go/app/hello`).

def _go_binary_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  all_srcs = list(ctx.attrs.srcs) + _merge_dep_srcs(ctx.attrs.deps) + [ctx.attrs._gomod]
  binary = ctx.actions.declare_output(ctx.attrs.name)
  tail = [
    "PKG=" + ctx.attrs.package,
    "exec \"$GOROOT/bin/go\" build -trimpath -o \"$1\" \"$PKG\"",
  ]
  script, written = _write_go_script(ctx, ctx.attrs.name + "-build", tools, tail)
  ctx.actions.run(
    cmd_args(["/bin/sh", script, binary.as_output()], hidden = all_srcs + written + [tools.go_dir]),
    category = "go_build",
    identifier = ctx.attrs.name,
  )
  return [
    DefaultInfo(default_output = binary),
    RunInfo(args = cmd_args(binary)),
  ]

go_binary = rule(
  impl = _go_binary_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "package": attrs.string(),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)

# --- go_test: `go test -trimpath` over a package set, mirroring repo.sh's
# go-test (`go test ./go/...`), exposed as an ExternalRunnerTestInfo target
# so `buck2 test` can run it directly instead of going through go_binary +
# a separate runner.

def _go_test_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  all_srcs = list(ctx.attrs.srcs) + _merge_dep_srcs(ctx.attrs.deps) + [ctx.attrs._gomod]
  tail = [
    "exec \"$GOROOT/bin/go\" test -trimpath " + " ".join(ctx.attrs.packages),
  ]
  script, written = _write_go_script(ctx, ctx.attrs.name, tools, tail)
  command = cmd_args(script, hidden = all_srcs + written + [tools.go_dir])
  return [
    DefaultInfo(default_output = script),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "go_test",
      command = [command],
      run_from_project_root = True,
    ),
  ]

go_test = rule(
  impl = _go_test_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "packages": attrs.list(attrs.string(), default = ["./go/..."]),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)

# --- go_lint: two flavors selected by `mode`, mirroring repo.sh's go-lint
# lane semantics (gofmt-canonical formatting + `go vet`), each its own
# ExternalRunnerTestInfo target so lint has the same per-lane test/lint
# parity as the cxx and other lanes instead of being deferred.

def _go_lint_impl(ctx: AnalysisContext) -> list[Provider]:
  tools = _toolchain_tools(ctx)
  # gofmt itself doesn't read go.mod, but go vet does, and keeping the
  # hidden-input set uniform across both modes is simpler than splitting it.
  all_srcs = list(ctx.attrs.srcs) + _merge_dep_srcs(ctx.attrs.deps) + [ctx.attrs._gomod]
  if ctx.attrs.mode == "fmt":
    fmt_check = cmd_args(["OUT=$(", "\"$GOROOT/bin/gofmt\"", "-l"] + list(ctx.attrs.srcs) + [")"], delimiter = " ")
    tail = [
      fmt_check,
      "if [ -n \"$OUT\" ]; then printf '%s\\n' \"$OUT\" >&2; " +
      "echo 'gofmt: files not canonically formatted (run: go fmt ./go/...)' >&2; exit 1; fi",
    ]
  elif ctx.attrs.mode == "vet":
    tail = [
      "exec \"$GOROOT/bin/go\" vet " + " ".join(ctx.attrs.packages),
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
    ),
  ]

go_lint = rule(
  impl = _go_lint_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(providers = [GoInfo]), default = []),
    "mode": attrs.string(),
    "packages": attrs.list(attrs.string(), default = ["./go/..."]),
    "srcs": attrs.list(attrs.source(), default = []),
  } | _TOOLCHAIN_ATTRS,
)
