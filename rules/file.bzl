"""A minimal export_file() so other packages can depend on a plain source file.

There is no prelude in this project, so buck2's usual export_file() (normally
supplied by the prelude) does not exist; this is the smallest replacement.
"""

def _export_file_impl(ctx: AnalysisContext) -> list[Provider]:
  return [DefaultInfo(default_output = ctx.attrs.src)]

export_file = rule(
  impl = _export_file_impl,
  attrs = {
    "src": attrs.source(),
  },
)


def _export_files_impl(ctx: AnalysisContext) -> list[Provider]:
  return [DefaultInfo(default_outputs = ctx.attrs.srcs)]


# Export a complete source-file closure, such as a vendored dependency tree.
export_files = rule(
  impl = _export_files_impl,
  attrs = {
    "srcs": attrs.list(attrs.source()),
  },
)
