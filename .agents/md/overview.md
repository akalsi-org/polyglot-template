# Repository Operating Guide

This repository is a pinned, offline-after-bootstrap polyglot workspace. Use `./repo.sh` for all documented operations; language tools and generated files are implementation details behind that interface.

## Invariants

- The current CPU architecture selects the build architecture. Native artifacts use the pinned GCC+musl ABI and never cross CPU architectures.
- `tools.lock.toml` owns tool provenance. Missing or placeholder digests fail closed.
- `package.toml` and `runtime-resolution.lock.toml` own package declarations and exact runtime closure resolution.
- `cpp/cpp.toml` owns C++ targets and profiles. Ninja and `compile_commands.json` are generated projections.
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

`cpp-configure`, `cpp-build`, and `cpp-reflection-probe` require the pinned compiler installation and never use host `g++`. `python-build`/`python-test`, `ts-build`/`ts-test`, and `tsweb-build`/`tsweb-test` similarly use only pinned runtimes. CI performs real cached bootstrap, offline replay, deep doctor, and language-lane verification on x64 and ARM64.

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
