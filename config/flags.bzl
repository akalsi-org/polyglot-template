"""Profile selection shared by coverage-producing language rules."""


def coverage_enabled_flag():
  return select({
    "//config:opt": False,
    "DEFAULT": True,
  })
