# VS Code

The committed `.vscode` files are an editor projection of the repository
contracts; `./repo.sh` remains the build and runtime entry point.

Start a new checkout with **Workspace: bootstrap pinned tools**, then run
**Workspace: verify editor prerequisites** and reload the VS Code window. The
Deno extension uses the repo-local frozen cache; clangd reads the root
`compile_commands.json`; Go uses the repo-local wrapper; and Pylance resolves
the Python source and typed native-extension stub roots.

Pylance intentionally does not search `build/python/<target>/lib`: that
directory contains a CPU-specific compiled extension, and permanently listing
both supported architectures can make an old foreign artifact win import
resolution. Instead, every native extension keeps its `.pyi` stub and `py.typed`
marker in a checked-in Python source root; Pylance reads those architecture-neutral
paths. `./repo.sh build` stages every declared native extension for the current
host target. Use **Python: build native extension** as the focused shortcut,
then **Python: run active file with native extension** whenever execution must
load a compiled module. That task invokes `./repo.sh python`, which selects the
current host target and places the staged extensions first on `PYTHONPATH`.

Use **C++: refresh compile commands** after C/C++ graph changes. The remaining
lane tasks use the same pinned command surface as CI; they are quick entry
points, not replacements for the full `./repo.sh lint` or `./repo.sh test`
handoff checks.
