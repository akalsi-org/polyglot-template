"""Stub-sync guard: the committed typed surface must match the built
extension. fastbytes/_native.pyi is hand-authored (there is no in-repo
stub generator dependency), so this test is what keeps it honest - a
public symbol added to or removed from fastbytes.cc without a matching
stub edit fails here, and the staged package must carry both the stub
and the PEP 561 py.typed marker so editors/type checkers see them."""

import ast
import pathlib

import fastbytes
import fastbytes._native


def _pkg_dir() -> pathlib.Path:
  init = fastbytes.__file__
  assert init is not None
  return pathlib.Path(init).parent


def test_native_stub_matches_built_module() -> None:
  stub = _pkg_dir() / "_native.pyi"
  assert stub.exists(), f"staged package is missing {stub}"
  tree = ast.parse(stub.read_text())
  stub_names = {
    node.name
    for node in tree.body
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))
  }
  module_names = {
    name for name in dir(fastbytes._native) if not name.startswith("_")
  }
  assert stub_names == module_names, "_native.pyi and the built extension disagree on public names"


def test_py_typed_marker_staged() -> None:
  marker = _pkg_dir() / "py.typed"
  assert marker.exists(), f"staged package is missing {marker}"
