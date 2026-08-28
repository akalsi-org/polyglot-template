# VS Code

The committed `.vscode` files are an editor projection of the repository
contracts; `./repo.sh` remains the build and runtime entry point.

Start a new checkout with **Workspace: bootstrap pinned tools**, then run
**Workspace: verify editor prerequisites** and reload the VS Code window. The
Deno extension uses the repo-local frozen cache. Go uses the repo-local wrapper,
disables user Go configuration, uses `vendor/`, and disables VCS stamping.
Pylance resolves the pure Python source roots.

Pylance searches `python/lib` and `python/app`. Python remains pure Python, so
there is no target-specific extension path or generated typing stub root.

The lane tasks use the same pinned command surface as CI; they are quick entry
points, not replacements for the full `./repo.sh lint` or `./repo.sh test`
handoff checks.
