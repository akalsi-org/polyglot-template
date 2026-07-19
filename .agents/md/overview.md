# Repository Operating Guide

This repository is a pinned, offline-after-bootstrap polyglot workspace. Use `./repo.sh` for all documented operations; language tools and generated files are implementation details behind that interface.

## Invariants

- The current CPU architecture selects the build architecture. Native artifacts use the pinned GCC+musl ABI and never cross CPU architectures.
- `tools.lock.toml` owns tool provenance. Missing or placeholder digests fail closed.
- `package.toml` and `runtime-resolution.lock.toml` own package declarations and exact runtime closure resolution.
- Buck2 is the sole scheduler; there is no separate task runner. `BUCK` files plus `config/flags.bzl` own C++ targets and profile (`dbg`/`opt`) flags — `cpp/cpp.toml` no longer exists. Every first-party rule lives under `rules/` (no prelude in this repo). Ninja stays a pinned in-graph tool, and `compile_commands.json` is materialized by `./repo.sh compile-commands` from a BXL compdb query (`bxl/compdb.bxl`) over the buck2 action graph, not hand-generated.
- Each language owns `lib/<name>/`, `app/<name>/`, and `test/`; its `lib/` directory is an import/include root.
- `.vscode/settings.json` mirrors repository discovery: clangd uses the root compdb, Deno owns `ts/` and `tsweb/`, and Python analysis includes `python/lib` plus `python/app`.
- Python native extensions build against the exact pinned interpreter ABI with pinned GCC/musl; pure Go uses the pinned repo-local toolchain with `CGO_ENABLED=0` and isolated caches.
- Normal build, test, lint, and package operations do not fetch.
- Build products live under `build/`, release products under `dist/`, and toolchains/caches under ignored `.local/`.

## Commands

Run `./repo.sh help` for the canonical command list. Start with:

```bash
./repo.sh target
./repo.sh bootstrap --dry-run
./repo.sh doctor
./repo.sh test
```

Lane commands are thin wrappers over Buck2 targets: `build [dbg|opt]` runs `buck2 build //:build`, `test` runs `buck2 test //...`, `lint` runs `buck2 test //... --labels lint` plus shell/Python infra checks (`infra-lint`). `cpp-build`/`cpp-run`/`cpp-test`, `python-build`/`python-test`, `ts-build`/`ts-test`, `tsweb-build`/`tsweb-test`, and `go-build`/`go-test` each drive their lane's Buck2 targets and only ever use the pinned toolchain, never host compilers or runtimes. `./repo.sh buck2 [args...]` runs the pinned Buck2 binary directly for anything not covered by a named lane command. CI performs real cached bootstrap, offline replay, deep doctor, and language-lane verification on x64 and ARM64.

## Packaging

`package.toml` declares package identity/version/executables/runtime; `package()` targets in `packages/BUCK` (`rules/package.bzl`) assemble packages with their own Buck2 target (currently `polyglot-demo`, `polyglot-server`) entirely in-graph from a generic `PackageInfo` contract each lane's rules emit, staging a flattened sibling layout (`bin/`, `lib/`, `runtime/`, plus per-lane roots like `python/`, `ts/`, `web/`) at the package root. `./repo.sh package <name> [dbg|opt]` dispatches generically: it detects a `//packages:<name>` Buck2 target via `buck2 targets` and builds it in-graph, or falls through to `tools/package_release.py` for manifest-only packages (`gateway`, `schema-cli`) with no Buck2 target. Each in-graph package's `package_smoke` target (`checks` + optional `smoke_script`) verifies the assembled archive.

## Agent Skills

Repository-owned skills, when added, belong under `.agents/skills/`. This checkout currently has no repository-owned skills; load only the smallest applicable installed skill set needed for a task.

## Collaboration Shorthand

`cnp` (also `c+p`) means commit the current task's intended changes and push the current branch. Exclude unrelated, generated, credential-bearing, and untracked state unless the user explicitly includes it.

Never commit authentication state, sessions, caches, downloaded tools, build output, or generated agent-runtime databases.


<!-- headroom:memory-instructions -->
## Memory

Use the `headroom_memory` MCP server for persistent cross-session knowledge.

**Before** answering questions about prior decisions, conventions, project context,
architecture, user preferences, org info, codenames, debugging history, or anything
from past sessions — call `memory_search` first.

**After** making durable decisions, discovering conventions, or learning important
facts — call `memory_save` to persist them for future sessions.

Memory is your first source of truth for anything not visible in the current conversation.

## Component agent guides

- `cpp/lib/pyfast/AGENTS.md` — pyfast: single-header CPython fastcall/vectorcall scaffolding (known defects, wiring gaps, test plan).
