"""Merge Go, Deno, and Python coverage into repository-relative LCOV.

Debug targets produce CoverageInfo artifacts. The BXL entry point discovers
those providers and passes every artifact as a tracked hidden input.
"""

load("//rules:env.bzl", "action_env")
load("//rules:pinned_python.bzl", "pinned_python_command_from_tools", "pinned_python_tools_from_dirs")

CoverageInfo = provider(fields = [
  "kind",
  "primary",
])

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
    entries.append({"kind": info.kind, "name": name, "primary": info.primary})
    hidden.append(info.primary)

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
