"""Shared Deno source-closure and collision-safe staging helpers.

The public Deno rules remain in ``deno.bzl``.  This module owns only the
provider and pure source-layout operations so every consumer uses the same
diamond-safe, fail-closed merge.
"""

DenoSourceSet = transitive_set()
DenoSourcesInfo = provider(fields = ["sources"])
DenoAppInfo = provider(fields = ["entry"])

def staged_path(ctx: AnalysisContext, src: Artifact) -> str:
  return ctx.label.package + "/" + src.short_path

def own_deno_sources(ctx: AnalysisContext, srcs: list) -> list:
  return [(staged_path(ctx, src), src, str(ctx.label.raw_target())) for src in srcs]

def deno_sources_children(deps: list) -> list:
  return [d[DenoSourcesInfo].sources for d in deps if DenoSourcesInfo in d]

def add_deno_source(layout: dict, owners: dict, path: str, artifact: Artifact, owner: str):
  previous = layout.get(path)
  if previous != None and str(previous) != str(artifact):
    fail("deno source collision at {!r}: {} provides {} but {} provides {}".format(
      path,
      owners[path],
      previous,
      owner,
      artifact,
    ))
  layout[path] = artifact
  owners[path] = owner

def flatten_deno_sources(ctx: AnalysisContext, deps: list) -> dict:
  """Merge dependency source closures once; reject ambiguous staged paths."""
  children = deno_sources_children(deps)
  if not children:
    return {}
  merged = ctx.actions.tset(DenoSourceSet, children = children)
  layout = {}
  owners = {}
  for value in merged.traverse():
    for path, artifact, owner in value:
      add_deno_source(layout, owners, path, artifact, owner)
  return layout

def merge_direct_deno_sources(ctx: AnalysisContext, layout: dict, srcs: dict) -> dict:
  """Add explicitly mapped sources with the same collision contract."""
  owners = {path: "transitive dependency" for path in layout.keys()}
  for path, artifact in srcs.items():
    add_deno_source(layout, owners, path, artifact, str(ctx.label.raw_target()))
  return layout
