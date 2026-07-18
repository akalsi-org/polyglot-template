"""Minimal first-party group() rule: aggregates the default outputs of
several targets behind one buildable label.

There is no prelude in this project, so buck2 has no built-in `alias()`,
`test_suite()`, or `filegroup()`-as-aggregator to reach for. This is the
smallest replacement: `buck2 build //:build` forwards DefaultInfo's
default_outputs from every dep, so building the group builds all of them.

This only aggregates DefaultInfo, and only DefaultInfo. It does NOT (and
cannot) aggregate ExternalRunnerTestInfo: buck2 lets a rule return at most
one ExternalRunnerTestInfo, so N deps' test-ness cannot be forwarded 1:1
onto a single group() target the way default_outputs can. A group() built
from test-rule deps is therefore buildable (its deps' scripts get built)
but not runnable as a test: `buck2 test //:that-group` reports the target
matched no tests, because the group rule itself never returns
ExternalRunnerTestInfo. There is no supported way to make one buck2 target
*run* many independent tests short of a custom test provider/executor
integration, which is out of scope here. Use `buck2 test` with a target
pattern instead (e.g. `buck2 test //...`); see the root BUCK file's
comments for the convention this repo uses.
"""

def _group_impl(ctx: AnalysisContext) -> list[Provider]:
  outputs = []
  for dep in ctx.attrs.deps:
    outputs += dep[DefaultInfo].default_outputs
  return [DefaultInfo(default_outputs = outputs)]

_group_rule = rule(
  impl = _group_impl,
  attrs = {
    "deps": attrs.list(attrs.dep()),
  },
)

def _native_target() -> str:
  # Mirrors rules/cxx.bzl's/rules/go.bzl's _native_target(): this repo only
  # ever builds the host's own musl output triplet.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

# group()'s deps typically cross into //config:opt-or-dbg-selecting lanes
# (cxx_binary, py_binary, polyglot_package, ...); a dependency edge (unlike a
# target given directly on the buck2 command line) has no configuration
# unless one is supplied, so without a default target platform here `buck2
# build //:build` fails resolving those deps' select()s under the
# <unspecified> platform. Mirrors rules/cxx.bzl's/rules/python.bzl's own
# `default_target_platform` macro pattern so plain `buck2 build //:build`
# works without requiring `-m`/`--target-platforms` on every invocation.
def group(**kwargs):
  kwargs.setdefault("default_target_platform", "//config:{}-dbg".format(_native_target()))
  _group_rule(**kwargs)
