"""Explicit action environment for every ctx.actions.run() in this project.

WHY THIS EXISTS: buck2's local executor does NOT scrub the daemon's own
environment - an action inherits every variable the `buck2` daemon was
started with (verified empirically on the pinned buck2 by dumping `env`
from inside a ctx.actions.run: CC, CXX, GOROOT, GOFLAGS, GOEXPERIMENT,
CGO_ENABLED, GOCACHE and DENO_DIR all arrived from the repo.sh shell that
happened to start the daemon). That is a correctness hole, not just a
hygiene one, because NONE of those variables are part of any action's cache
key: a host CPATH / C_INCLUDE_PATH / CPLUS_INCLUDE_PATH folds glibc headers
into a compile against the pinned musl toolchain, LIBRARY_PATH does the same
at link time, and GOOS / GOARCH / GOAMD64 change Go codegen outright - all
producing a poisoned .o or binary that buck2 then caches and re-serves as if
it were clean, and that a `buck2 clean` on a differently-configured shell
cannot reproduce.

HOW: buck2's `env` kwarg MERGES into (rather than replaces) the inherited
environment - also verified empirically - so scrubbing has to be explicit:
every name below is pinned to a value of this rule set's own choosing.
Setting a variable to "" is a real scrub, not a half-measure: GCC treats an
empty CPATH/C_INCLUDE_PATH/... exactly as an absent one (verified: an empty
CPATH does NOT put the working directory on the search path the way a
literal "." element would), and Go's cmd/go reads its own configuration
through a getenv that treats "" as unset.

USAGE: every ctx.actions.run() in rules/ passes `env = action_env()`, or
`env = action_env({...})` when that action needs its own additions on top.
Actions that assemble their environment inside a generated shell script
(rules/go.bzl's _write_go_script, rules/deno.bzl's _stage_and_run) still do
so - those exports run AFTER this env is applied and deliberately win, since
they are the action's own pinned, in-graph values.
"""

# A fixed, minimal PATH. Nothing in this graph resolves a build tool through
# PATH (every compiler/interpreter/CLI is passed as a real Artifact path),
# so this exists only for the handful of POSIX utilities the generated
# scripts call - sh, cp, mkdir, tar, find, mktemp, unshare - and pinning it
# keeps a developer's ~/.local/bin shim out of the graph.
# /usr/sbin:/sbin are included because `ip` (rules/deno.bzl's run_offline
# brings loopback up inside its network namespace) lives there on several
# distributions, even though it is /usr/bin/ip on this one.
ACTION_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"

# Every name here is set to "" (i.e. scrubbed) unless a caller overrides it.
# Grouped by the mechanism each one poisons.
_SCRUBBED = [
  # --- C/C++ preprocessor + linker search paths. These prepend host
  # directories to the pinned musl toolchain's own search order, so a host
  # glibc header or library silently wins over the toolchain's.
  "CPATH",
  "C_INCLUDE_PATH",
  "CPLUS_INCLUDE_PATH",
  "OBJC_INCLUDE_PATH",
  "LIBRARY_PATH",
  "LD_LIBRARY_PATH",
  "LD_PRELOAD",
  "LD_RUN_PATH",
  # COMPILER_PATH redirects which as/ld binaries the driver execs.
  #
  # GCC_EXEC_PREFIX is DELIBERATELY ABSENT from this list, and that is not
  # an oversight: for this one variable an empty value is NOT equivalent to
  # an unset one. gcc's driver tests the pointer, not the string - a set-
  # but-empty GCC_EXEC_PREFIX replaces the standard exec prefix with "" and
  # then never falls back to the argv[0]-relative one, so the driver cannot
  # find cc1plus at all. Verified against the pinned toolchain: with
  # GCC_EXEC_PREFIX="" every compile fails with "cannot execute 'cc1plus'",
  # with or without -B. Leaving it inherited is the lesser evil precisely
  # because its failure mode is the loud one - a host GCC_EXEC_PREFIX points
  # at a different gcc installation whose cc1plus either does not exist
  # (immediate hard error) or is a different version (which the -dumpmachine
  # capability probe in rules/toolchain.bzl catches), never a silently
  # mis-resolved header the way CPATH is.
  "COMPILER_PATH",
  # DEPENDENCIES_OUTPUT / SUNPRO_DEPENDENCIES (which make the driver write
  # an undeclared .d file) are absent for the same pointer-not-string reason
  # as GCC_EXEC_PREFIX above - verified: DEPENDENCIES_OUTPUT="" makes every
  # compile fail with "opening dependency file : No such file or
  # directory". Their failure mode when inherited is also loud (an extra
  # file appears, or the named path is unwritable), never a mis-resolved
  # header.
  #
  # Ambient flag injection into any build system that honors them.
  "CC",
  "CXX",
  "AR",
  "LD",
  "CFLAGS",
  "CXXFLAGS",
  "CPPFLAGS",
  "LDFLAGS",

  # --- Go. GOOS/GOARCH/GOAMD64/GOARM change generated code; the rest
  # relocate caches and module resolution out from under rules/go.bzl's own
  # pinned exports (which run inside the script and therefore still win,
  # but only for the variables that script actually sets).
  "GOOS",
  "GOARCH",
  "GOAMD64",
  "GOARM",
  "GOROOT",
  "GOPATH",
  "GOBIN",
  "GOCACHE",
  "GOMODCACHE",
  "GOENV",
  "GOFLAGS",
  "GOEXPERIMENT",
  "GOTOOLCHAIN",
  "GOPROXY",
  "GOSUMDB",
  "GOPRIVATE",
  "GONOSUMDB",
  "GOWORK",
  "CGO_ENABLED",
  "CGO_CFLAGS",
  "CGO_CPPFLAGS",
  "CGO_CXXFLAGS",
  "CGO_LDFLAGS",

  # --- Deno / npm. DENO_DIR is the big one: repo.sh exports it at
  # .local/cache/deno, which is exactly the untracked bootstrap seed the
  # deno rules deliberately project away from (see rules/deno.bzl's
  # deno_cache doc comment). DENO_AUTH_TOKENS/DENO_CERT/NPM_CONFIG_REGISTRY
  # would additionally change what a fetch could reach.
  "DENO_DIR",
  "DENO_INSTALL_ROOT",
  "DENO_AUTH_TOKENS",
  "DENO_CERT",
  "DENO_TLS_CA_STORE",
  "NODE_OPTIONS",
  "NODE_PATH",
  "NPM_CONFIG_REGISTRY",

  # --- Python. PYTHONPATH/PYTHONHOME would let host site-packages shadow
  # the staged package roots rules/python.bzl assembles; the rest change
  # interpreter behavior (bytecode writes, warning-to-error promotion) in
  # ways no action asked for. Every rule that needs PYTHONPATH exports it
  # inside its own generated script, after this env applies.
  "PYTHONPATH",
  "PYTHONHOME",
  "PYTHONSTARTUP",
  "PYTHONOPTIMIZE",
  "PYTHONDONTWRITEBYTECODE",
  "PYTHONWARNINGS",
  "PYTHONEXECUTABLE",
]

def _base_env():
  env = {name: "" for name in _SCRUBBED}
  env["PATH"] = ACTION_PATH
  # LANG/LC_ALL are pinned rather than scrubbed: an unset locale makes some
  # tools fall back to ASCII and mangle non-ASCII diagnostics, while an
  # inherited one makes sort order and error text host-dependent.
  env["LANG"] = "C.UTF-8"
  env["LC_ALL"] = "C.UTF-8"
  return env

_BASE_ENV = _base_env()

def action_env(extra: dict = {}) -> dict:
  """The scrubbed environment every ctx.actions.run() in rules/ should pass.

  `extra` is merged last, so an action can re-add a variable it genuinely
  needs (e.g. a HOME for a tool that insists on one) without editing the
  shared scrub list."""
  return _BASE_ENV | extra
