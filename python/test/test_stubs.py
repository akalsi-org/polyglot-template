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
import pyfast_test_ext


def _pkg_dir(package: object) -> pathlib.Path:
  init = getattr(package, "__file__")
  assert init is not None
  return pathlib.Path(init).parent


def _stub_names(stub: pathlib.Path) -> set[str]:
  tree = ast.parse(stub.read_text())
  return {
    node.name
    for node in tree.body
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))
  }


def test_fastbytes_native_stub_matches_built_module() -> None:
  stub = _pkg_dir(fastbytes) / "_native.pyi"
  assert stub.exists(), f"staged package is missing {stub}"
  module_names = {
    name for name in dir(fastbytes._native) if not name.startswith("_")
  }
  assert _stub_names(stub) == module_names, "_native.pyi and the built extension disagree on public names"


def test_pyfast_test_extension_stub_matches_built_module() -> None:
  stub = _pkg_dir(pyfast_test_ext) / "__init__.pyi"
  assert stub.exists(), f"staged package is missing {stub}"
  module_names = {
    name for name in dir(pyfast_test_ext) if not name.startswith("_")
  }
  assert _stub_names(stub) == module_names, "__init__.pyi and the built extension disagree on public names"


def test_py_typed_markers_staged() -> None:
  for package in [fastbytes, pyfast_test_ext]:
    marker = _pkg_dir(package) / "py.typed"
    assert marker.exists(), f"staged package is missing {marker}"
