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

bxl/coverage.bxl (the `./repo.sh coverage` entry point) discovers every
test by rule kind and walks their
optional CoverageInfo providers (targets without one - e.g. lint-as-test
targets, or anything under -m //config:opt - are silently skipped, so
no separate coverage-only allowlist exists anywhere), builds a manifest describing every
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

CONSEQUENCE OF "DEFAULT-ON FOR dbg", stated explicitly because it is
surprising and was previously undocumented: each lane's collector is a
ctx.actions.run() build action, so ANY command that builds a test target
under dbg also EXECUTES that test - a second time, in addition to whatever
`buck2 test` does. Concretely:

  - `buck2 build //...` runs every instrumented test.
  - a FAILING test therefore fails `buck2 build`, and fails it with a
    coverage-action error ("Action failed: ... (cxx_test_coverage foo)")
    rather than a test report naming the assertion.
  - anything depending on a test target inherits both effects.

`./repo.sh build` is deliberately NOT affected: it discovers buildables by
rule kind (cxx_binary/go_binary/py_binary/deno_check/vite_build - see the
uquery in repo.sh), so no test target, and therefore no collector, is in its
build set. `./repo.sh test` and `./repo.sh coverage` are the commands that
reach them.

This is left AS IS rather than gated behind a //config:coverage constraint,
and the choice is deliberate. Gating would mean a third configuration
dimension: a new constraint_setting, new platform() rows for every
arch x profile x coverage combination in config/defs.bzl, and every
`default_target_platform` in rules/ picking one - for a graph that has
exactly two profiles today and where the -m modifier demonstrably does not
override a rule's own default_target_platform (see config/defs.bzl's
docstring). It would also split coverage off from the dbg configuration
whose --coverage compile flags produce the .gcno files the cxx collector
reads, so cxx coverage would need dbg AND the new constraint to be
coherent - a constraint pair no invocation can express without also
teaching repo.sh, which is a bigger and more fragile change than the
behavior it fixes. The double execution costs wall-clock on a full build;
the failure-message confusion is real but rare, and is now written down
where someone hitting it will look.

CRITICAL RULE-WRITING LAW (see rules/go.bzl / rules/python.bzl module
docstrings): the manifest below embeds every artifact's path as literal
TEXT (via ctx.actions.write_json). That text-only reference is not enough
on its own for buck2 to track the referenced artifact as a real input of
the merge action - every artifact appearing in the manifest (plus the
manifest file itself) is therefore also passed into the merge action's own
`hidden` list directly.
"""

load("//rules:env.bzl", "action_env")
load("//rules:pinned_python.bzl", "pinned_python_command_from_tools", "pinned_python_tools_from_dirs")

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

# Shared by bxl/coverage.bxl (the discovery-driven entry point - there is
# no static coverage_report target anymore; tests are found by rule kind at
# invocation time so a new test participates by existing). `actions` is
# either an AnalysisContext.actions or a bxl_actions().actions - both
# expose the same write_json/declare_output/run surface used here.
def coverage_merge_actions(actions, merge_tool, gomod, infos, python_dir, gcc_dir):
  """infos: list of (name, CoverageInfo); returns (merged_lcov, summary)."""
  entries = []
  hidden = []
  for name, info in infos:
    entries.append(_entry_for(name, info))
    hidden += _hidden_for(info)

  manifest = actions.write_json("coverage-manifest.json", entries, with_inputs = True)

  merged = actions.declare_output("merged.lcov")
  summary = actions.declare_output("summary.txt")
  actions.run(
    cmd_args(
      pinned_python_command_from_tools(
        pinned_python_tools_from_dirs(python_dir, gcc_dir),
        [merge_tool, manifest, merged.as_output(), summary.as_output()],
      ),
      # go.mod is read by tools/coverage_merge.py at run time (to strip the
      # Go module import prefix off go_profile entries' file paths) via a
      # plain relative "go.mod" open() - not passed as an argv path, so it
      # would otherwise be invisible to buck2's own dependency tracking;
      # listed here per this file's own CRITICAL RULE-WRITING LAW comment.
      hidden = hidden + [manifest, gomod, merge_tool],
    ),
    category = "coverage_merge",
    identifier = "coverage",
    env = action_env(),
  )
  return merged, summary
