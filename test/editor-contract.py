#!/usr/bin/env python3
"""Keep editor discovery aligned with repository command paths."""

from __future__ import annotations

import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def main() -> None:
  settings = json.loads((ROOT / ".vscode" / "settings.json").read_text())
  extensions = json.loads((ROOT / ".vscode" / "extensions.json").read_text())
  editorconfig = (ROOT / ".editorconfig").read_text()

  assert "[*]\n" in editorconfig
  assert "indent_style = space\nindent_size = 2\ntab_width = 2\n" in editorconfig
  assert "[*.go]\nindent_style = tab\nindent_size = tab\ntab_width = 2\n" in editorconfig
  clang_format = (ROOT / ".clang-format").read_text()
  assert "IndentWidth: 2\n" in clang_format
  assert "ContinuationIndentWidth: 2\n" in clang_format
  assert settings["editor.detectIndentation"] is False
  assert settings["editor.insertSpaces"] is True
  assert settings["editor.tabSize"] == 2
  assert settings["[go]"] == {
    "editor.detectIndentation": False,
    "editor.insertSpaces": False,
    "editor.tabSize": 2,
  }

  assert settings["clangd.arguments"] == ["--compile-commands-dir=${workspaceFolder}"]
  assert settings["deno.enable"] is True
  assert settings["deno.enablePaths"] == ["./ts", "./tsweb"]
  assert settings["deno.config"] == "./deno.json"
  assert settings["deno.cache"] == "./.local/cache/deno"
  deno_config = json.loads((ROOT / "deno.json").read_text())
  assert deno_config["fmt"]["indentWidth"] == 2
  assert deno_config["fmt"]["useTabs"] is False
  assert deno_config["nodeModulesDir"] == "auto"
  assert deno_config["imports"] == {
    "@deno/vite-plugin": "npm:@deno/vite-plugin@2.0.2",
    "@vitejs/plugin-react": "npm:@vitejs/plugin-react@6.0.3",
    "react": "npm:react@19.2.7",
    "react-dom/client": "npm:react-dom@19.2.7/client",
    "vite": "npm:vite@8.1.4",
  }
  assert deno_config["scopes"] == {
    "./ts/": {"@/": "./ts/lib/"},
    "./tsweb/": {"#/": "./tsweb/lib/"},
  }
  assert settings["go.alternateTools"] == {"go": "${workspaceFolder}/.vscode/go"}
  assert settings["go.toolsEnvVars"] == {
    "CGO_ENABLED": "0",
    "GOEXPERIMENT": "jsonv2",
    "GOTOOLCHAIN": "local",
  }
  assert settings["python.analysis.extraPaths"] == [
    "${workspaceFolder}/python/lib",
    "${workspaceFolder}/python/app",
    "${workspaceFolder}/python/test",
  ]
  assert (ROOT / "python/test/pyfast_test_ext/__init__.pyi").is_file()
  assert (ROOT / "python/test/pyfast_test_ext/py.typed").is_file()
  assert settings["python.defaultInterpreterPath"] == "${workspaceFolder}/.local/bin/python"
  assert settings["python.envFile"] == "${workspaceFolder}/.vscode/python.env"
  assert settings["python.terminal.useEnvFile"] is True
  assert (ROOT / ".vscode" / "python.env").read_text() == "PYTHONPATH=python/lib:python/app\n"
  assert settings["python.testing.unittestArgs"] == [
    "-s",
    "python/test",
    "-p",
    "test_*.py",
  ]

  required = {
    "denoland.vscode-deno",
    "llvm-vs-code-extensions.vscode-clangd",
    "ms-python.python",
    "ms-python.vscode-pylance",
    "golang.go",
  }
  assert required.issubset(extensions["recommendations"])

  repo_sh = (ROOT / "repo.sh").read_text()
  assert 'format [--check]' in repo_sh
  assert 'PYTHONPATH="$build_python:$ROOT/python/lib:$ROOT/python/app' in repo_sh
  assert (ROOT / ".vscode" / "go").stat().st_mode & 0o111
  assert settings["deno.path"] == "./.local/bin/deno"
  assert settings["buck2-lsp-adapter.buck2Path"] == "${workspaceFolder}/.local/bin/buck2"

  tasks = json.loads((ROOT / ".vscode" / "tasks.json").read_text())
  task_commands = {task["label"]: task["command"] for task in tasks["tasks"]}
  assert task_commands.pop("Python: run active file with native extension") == './repo.sh python "${file}"'
  assert task_commands == {
    "Workspace: bootstrap pinned tools": "./repo.sh bootstrap",
    "Workspace: verify editor prerequisites": "./repo.sh doctor",
    "C++: refresh compile commands": "./repo.sh compile-commands",
    "Python: build native extension": "./repo.sh python-build",
    "Deno: type check": "./repo.sh ts-build",
    "React: build site": "./repo.sh tsweb-build",
    "Go: test": "./repo.sh go-test",
  }
  launch = json.loads((ROOT / ".vscode" / "launch.json").read_text())
  assert launch["configurations"] == [{
    "name": "Deno: debug active TypeScript file",
    "type": "deno",
    "request": "launch",
    "program": "${file}",
  }]
  editor_guide = (ROOT / "docs" / "EDITOR.md").read_text()
  assert "Pylance intentionally does not search `build/python/<target>/lib`" in editor_guide
  assert "Python: run" in editor_guide
  assert "active file with native extension" in editor_guide

  print("editor contract: ok")


if __name__ == "__main__":
  main()
