# Repository Operating Guide

This repository is a pinned, offline-after-bootstrap polyglot workspace. Use `./repo.sh` for all documented operations; language tools and generated files are implementation details behind that interface.

## Invariants

- The current CPU architecture selects the build architecture. Native artifacts use the pinned GCC+musl ABI and never cross CPU architectures.
- `tools.lock.toml` owns tool provenance. Missing or placeholder digests fail closed.
- `package.toml` and `package-lock.toml` own package declarations and exact runtime closure resolution.
- `cpp/cpp.toml` owns C++ targets and profiles. Ninja and `compile_commands.json` are generated projections.
- Each language owns `lib/<name>/`, `app/<name>/`, and `test/`; its `lib/` directory is an import/include root.
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

`cpp-configure`, `cpp-build`, and `cpp-reflection-probe` require the pinned compiler installation and never use host `g++`. `python-check`, `ts-check`, and `tsweb-check` similarly use only pinned runtimes. CI performs real cached bootstrap, offline replay, deep doctor, and language-lane verification on x64 and ARM64.

## Agent Skills

Repository-owned skills belong under `.agents/skills/`. Load only the smallest skill set needed for a task. The initial implementation expects skills covering polyglot orchestration, hermetic C/C++, Python/native packaging, schema generation, linting, and benchmark regression policy.

Never commit authentication state, sessions, caches, downloaded tools, build output, or generated agent-runtime databases.
