# Repository Operating Guide

This repository is a pinned, offline-after-bootstrap polyglot workspace. Use `./repo.sh` for all documented operations; language tools and generated files are implementation details behind that interface.

## Invariants

- The current CPU architecture selects the build architecture. Native artifacts use the pinned GCC+musl ABI and never cross CPU architectures.
- `tools.lock.toml` owns tool provenance. Missing or placeholder digests fail closed.
- `package.toml` and `package-lock.toml` own package declarations and exact runtime closure resolution.
- `native/native.toml` owns native targets and profiles. Ninja and `compile_commands.json` are generated projections.
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

`native-configure`, `native-build`, and `reflection-probe` require the pinned compiler installation and never use host `g++`. The checked-in release workflow is a preflight only while GCC/Ninja lock entries remain unresolved.

## Agent Skills

Repository-owned skills belong under `.agents/skills/`. Load only the smallest skill set needed for a task. The initial implementation expects skills covering polyglot orchestration, hermetic C/C++, Python/native packaging, schema generation, linting, and benchmark regression policy.

Never commit authentication state, sessions, caches, downloaded tools, build output, or generated agent-runtime databases.
