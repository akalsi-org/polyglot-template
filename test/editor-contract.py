#!/usr/bin/env python3
"""Keep editor discovery aligned with repository command paths."""

from __future__ import annotations

import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def main() -> None:
    settings = json.loads((ROOT / ".vscode" / "settings.json").read_text())
    extensions = json.loads((ROOT / ".vscode" / "extensions.json").read_text())

    assert settings["clangd.arguments"] == ["--compile-commands-dir=${workspaceFolder}"]
    assert settings["deno.enable"] is True
    assert settings["deno.enablePaths"] == ["./ts", "./tsweb"]
    assert settings["deno.config"] == "${workspaceFolder}/deno.json"
    assert settings["python.analysis.extraPaths"] == [
        "${workspaceFolder}/python/lib",
        "${workspaceFolder}/python/app",
    ]
    assert settings["python.envFile"] == "${workspaceFolder}/.vscode/python.env"
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
    }
    assert required.issubset(extensions["recommendations"])

    repo_sh = (ROOT / "repo.sh").read_text()
    assert 'PYTHONPATH="$ROOT/python/lib:$ROOT/python/app' in repo_sh

    print("editor contract: ok")


if __name__ == "__main__":
    main()
