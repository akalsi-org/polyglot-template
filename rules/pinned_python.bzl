"""Pinned CPython action helpers shared by first-party Buck rules.

The Python toolchain is a standalone musl build, so an action must invoke its
interpreter through the matching pinned musl loader.  These helpers make both
artifacts declared inputs and keep the loader/library-path invocation uniform.
"""

load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target():
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_PYTHON = TOOLCHAINS["python"][_NATIVE_TARGET]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]
_PY_ROOT_REL = _PYTHON["expected"].rsplit("/", 2)[0]
_PY_LIB_REL = _PY_ROOT_REL + "/lib"

# Add these attrs to every rule that executes Python.  The deliberately
# distinctive names avoid collisions with lane-specific toolchain attrs.
PINNED_PYTHON_ATTRS = {
  "_pinned_python": attrs.dep(default = "//toolchains:python-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_pinned_python_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
  "_pinned_python_target_arch": attrs.string(default = _NATIVE_TARGET),
}

def pinned_python_tools(ctx):
  if ctx.attrs._pinned_python_target_arch != _NATIVE_TARGET:
    fail("{}: target platform arch {!r} does not match native build arch {!r}".format(
      ctx.label.raw_target(),
      ctx.attrs._pinned_python_target_arch,
      _NATIVE_TARGET,
    ))
  py_dir = ctx.attrs._pinned_python[DefaultInfo].default_outputs[0]
  gcc_dir = ctx.attrs._pinned_python_gcc[DefaultInfo].default_outputs[0]
  return struct(
    python_dir = py_dir,
    python3 = py_dir.project(_PYTHON["expected"]),
    python_lib_dir = py_dir.project(_PY_LIB_REL),
    gcc_dir = gcc_dir,
    loader = gcc_dir.project(_GCC["loader"]),
    loader_dir = gcc_dir.project(_LOADER_DIR),
  )

def pinned_python_tools_from_dirs(python_dir, gcc_dir):
  return struct(
    python_dir = python_dir,
    python3 = python_dir.project(_PYTHON["expected"]),
    python_lib_dir = python_dir.project(_PY_LIB_REL),
    gcc_dir = gcc_dir,
    loader = gcc_dir.project(_GCC["loader"]),
    loader_dir = gcc_dir.project(_LOADER_DIR),
  )

def pinned_python_command_from_tools(tools, args):
  return cmd_args(
    [
      tools.loader,
      "--library-path",
      cmd_args(tools.loader_dir, tools.python_lib_dir, delimiter = ":"),
      tools.python3,
    ] + args,
    hidden = [tools.gcc_dir, tools.python_dir],
  )

def pinned_python_command(ctx, args):
  return pinned_python_command_from_tools(pinned_python_tools(ctx), args)

def pinned_python_script_args(ctx):
  tools = pinned_python_tools(ctx)
  return cmd_args(
    [tools.loader, tools.loader_dir, tools.python_lib_dir, tools.python3],
    hidden = [tools.gcc_dir, tools.python_dir],
  )
