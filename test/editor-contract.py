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
    deno_config = json.loads((ROOT / "deno.json").read_text())
    assert deno_config["imports"] == {
        "@/greeting/": "./ts/lib/greeting/",
        "@/title/": "./tsweb/lib/title/",
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
        "golang.go",
    }
    assert required.issubset(extensions["recommendations"])

    repo_sh = (ROOT / "repo.sh").read_text()
    assert 'PYTHONPATH="$build_python:$ROOT/python/lib:$ROOT/python/app' in repo_sh
    assert (ROOT / ".vscode" / "go").stat().st_mode & 0o111

    print("editor contract: ok")


if __name__ == "__main__":
    main()
