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
