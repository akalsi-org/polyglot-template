"""Coverage merge rule and cross-lane CoverageInfo provider.

Design decision (final, not re-derived here): coverage is DEFAULT-ON FOR the
dbg profile, not a third profile. Under dbg (the default profile - see
config/flags.bzl), rules/cxx.bzl's cxx_test, rules/go.bzl's go_test,
rules/deno.bzl's deno_test, and rules/python.bzl's py_test each collect
their lane's coverage as a normal build output (via a dedicated
ctx.actions.run() that actually executes the test binary/runner, separate
from the ExternalRunnerTestInfo/RunInfo path `buck2 test` uses for pass/fail
reporting) and return a CoverageInfo provider describing it. Under
-m //config:opt none of that happens and no CoverageInfo is returned.

coverage_report (wired up at //:coverage in the root BUCK) walks its `deps`'
optional CoverageInfo providers (deps without one - e.g. lint-as-test
targets, or any target under -m //config:opt - are silently skipped, so
//:coverage's deps list can just be "every test target" without needing a
separate coverage-only allowlist), builds a manifest describing every
present entry, and hands it to tools/coverage_merge.py, which normalizes
everything - gcov's JSON intermediate format (cxx), a go coverage profile
(go), deno's own `deno coverage --lcov` output (ts/tsweb), and an
already-lcov file (python, see tools/py_cover.py) - into ONE merged lcov
file plus a small per-file % summary.txt, both declared as this rule's
outputs. Every path written into the merged lcov is repo-relative (the
underlying per-lane tools all emit repo-relative paths because every action
here runs with cwd = project root - see rules/go.bzl's module docstring for
the general pattern this repo uses), so the report is stable across
configurations/machines.

CRITICAL RULE-WRITING LAW (see rules/go.bzl / rules/python.bzl module
docstrings): the manifest below embeds every artifact's path as literal
TEXT (via ctx.actions.write_json). That text-only reference is not enough
on its own for buck2 to track the referenced artifact as a real input of
the merge action - every artifact appearing in the manifest (plus the
manifest file itself) is therefore also passed into the merge action's own
`hidden` list directly.
"""

CoverageInfo = provider(fields = [
  "kind",  # "cxx_gcov" | "go_profile" | "deno" | "python_lcov"

  # Main data artifact: a dir of raw .gcda counters (cxx_gcov), a go
  # coverage profile file (go_profile), a dir holding `deno test
  # --coverage=<dir>`'s auto-generated lcov.info (deno - deno itself
  # already normalizes to lcov, so no external tool call is needed at merge
  # time, just a path rewrite - see tools/coverage_merge.py), or an
  # already-lcov file (python_lcov, see tools/py_cover.py).
  "primary",

  # cxx_gcov only: Artifact, the pinned gcov binary the merge step must
  # invoke to decode `primary`'s raw .gcda counters.
  "tool",

  # cxx_gcov only: list[Artifact], the .gcno siblings of every object
  # linked into the test binary (gcov needs a .gcno next to each .gcda to
  # decode it - see tools/coverage_merge.py).
  "gcnos",

  # cxx_gcov only: Artifact, the extracted gcc-musl toolchain dir (kept
  # alongside `tool` for the same reason rules/cxx.bzl's _toolchain_tools
  # keeps `dir` alongside its .project()ed paths - a toolchain binary
  # invoked via `tool` needs its whole extracted dir tracked/materialized,
  # not just the one binary path).
  "toolchain_dir",
])

def _entry_for(name, info):
  entry = {"kind": info.kind, "name": name, "primary": info.primary}
  if info.tool != None:
    entry["tool"] = info.tool
  if info.gcnos != None:
    entry["gcnos"] = info.gcnos
  return entry

def _hidden_for(info):
  hidden = [info.primary]
  if info.tool != None:
    hidden.append(info.tool)
  if info.gcnos != None:
    hidden += info.gcnos
  if info.toolchain_dir != None:
    hidden.append(info.toolchain_dir)
  return hidden

def _coverage_report_impl(ctx: AnalysisContext) -> list[Provider]:
  entries = []
  hidden = []
  for dep in ctx.attrs.deps:
    info = dep.get(CoverageInfo)
    if info == None:
      # Not every test target carries coverage (lint-as-test targets never
      # do; nothing does under -m //config:opt) - skip rather than fail, so
      # //:coverage's deps list can just be "every dbg test target".
      continue
    entries.append(_entry_for(dep.label.name, info))
    hidden += _hidden_for(info)

  manifest = ctx.actions.write_json(ctx.label.name + "-manifest.json", entries, with_inputs = True)

  merged = ctx.actions.declare_output("merged.lcov")
  summary = ctx.actions.declare_output("summary.txt")
  ctx.actions.run(
    cmd_args(
      ["python3", ctx.attrs._merge_tool, manifest, merged.as_output(), summary.as_output()],
      # go.mod is read by tools/coverage_merge.py at run time (to strip the
      # Go module import prefix off go_profile entries' file paths) via a
      # plain relative "go.mod" open() - not passed as an argv path, so it
      # would otherwise be invisible to buck2's own dependency tracking;
      # listed here per this file's own CRITICAL RULE-WRITING LAW comment.
      hidden = hidden + [manifest, ctx.attrs._gomod],
    ),
    category = "coverage_merge",
    identifier = ctx.label.name,
  )
  return [
    DefaultInfo(
      default_outputs = [merged, summary],
      sub_targets = {
        "lcov": [DefaultInfo(default_output = merged)],
        "summary": [DefaultInfo(default_output = summary)],
      },
    ),
  ]

_coverage_report_rule = rule(
  impl = _coverage_report_impl,
  attrs = {
    "deps": attrs.list(attrs.dep()),
    "_gomod": attrs.source(default = "//:go.mod"),
    "_merge_tool": attrs.source(default = "//:coverage_merge.py"),
  },
)

def _native_target() -> str:
  # Mirrors rules/group.bzl's/rules/cxx.bzl's own _native_target(): this
  # repo only ever builds the host's own musl output triplet.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

# coverage_report's deps cross into //config:opt-or-dbg-selecting lane test
# rules (go_test, deno_test, py_test, cxx_test); a dependency edge has no
# configuration unless one is supplied, so without a default target
# platform here `buck2 build //:coverage` fails resolving those deps'
# select()s under the <unspecified> platform - see rules/group.bzl's
# identical default_target_platform macro (used by //:build) for the same
# reason.
def coverage_report(**kwargs):
  kwargs.setdefault("default_target_platform", "//config:{}-dbg".format(_native_target()))
  _coverage_report_rule(**kwargs)
