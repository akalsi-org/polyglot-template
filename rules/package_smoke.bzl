"""Validation and quoting helpers for package smoke command declarations."""

def shell_quote(value):
  return "'" + value.replace("'", "'\"'\"'") + "'"

def check_smoke_program(ctx, program):
  if program == "" or program.startswith("/"):
    fail("package_smoke({}): command program {!r} must be a non-empty package-relative path".format(ctx.label.raw_target(), program))
  for segment in program.split("/"):
    if segment in ("", ".", ".."):
      fail("package_smoke({}): command program {!r} must not contain empty, '.' or '..' path segments".format(ctx.label.raw_target(), program))
