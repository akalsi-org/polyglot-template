# Polyglot Template Architecture

## Scope And Status

This document describes the implemented bootstrap slice in this repository.
It is a pinned, offline-after-bootstrap workspace for C++, Python, Go,
TypeScript, and a React static site. Fleet, deployment, schema-generation,
benchmark, and third-party-adapter designs in adjacent documents are proposals;
they are not part of the current `repo.sh` interface.

## Repository Shape

```text
repo/
|- AGENTS.md -> .agents/md/overview.md
|- repo.sh                         # public command surface
|- tools.lock.toml                 # exact bootstrap artifacts
|- package.toml                    # package declarations
|- runtime-resolution.lock.toml    # runtime closure lock
|- moon.yml                        # aggregate task graph
|- toolchain/                      # bootstrap, target, lock, doctor helpers
|- cpp/{lib,app,test}/             # C++ graph manifests and sources
|- python/{lib,app,test}/          # Python and native extension sources
|- go/{lib,app,test}/              # pure-Go lane
|- ts/{lib,app,test}/              # Deno TypeScript lane
|- tsweb/{lib,app,test}/           # React/Vite static application
|- tools/                          # graph, package, lint, and smoke helpers
|- test/                           # repository-contract tests
|- .local/                         # ignored tools and caches
|- build/                          # ignored build output
`- dist/                           # ignored package output
```

Each language owns `lib/<name>/`, `app/<name>/`, and `test/`. Use
`./repo.sh` rather than calling the installed language tools directly.

## Public Commands

Run `./repo.sh help` for the canonical, current list. The primary commands are:

```text
bootstrap [--offline] [--dry-run]
doctor [--deep]
target
lint
build [dbg|opt]
test
compile-commands [dbg|opt]
cpp-configure [dbg|opt] | cpp-build [dbg|opt] | cpp-run [dbg|opt] | cpp-test
python [args...] | python-build | python-test
deno [args...] | ts-build | ts-test | tsweb-build | tsweb-test
go [args...] | go-build | go-test
cpp-reflection-probe
package-validate | package-resolve <name> [out]
package <name> [dbg|opt] | package-smoke <archive> <name>
release-check <name> <tag> | release-notes <tag>
ci
```

`infra-lint`, the lane-specific lint commands, `infra-test`, and
`_job-budget` are internal task entrypoints used by Moon or repository tests;
they are intentionally omitted from the public help surface.

## Toolchain And Native Policy

`tools.lock.toml` records immutable URLs, SHA-256 values, archive layouts, and
expected binaries for every supported target. `./repo.sh bootstrap` is the only
command allowed to fetch. It installs verified artifacts below
`.local/toolchain/<target>/`; normal build, lint, test, package, and release
commands use that local installation and do not fall back to host compilers.

The current host CPU selects the target: x86-64 hosts produce
`x86_64-linux-musl`, and ARM64 hosts produce `aarch64-linux-musl`. Native C++
and Python-extension work uses the pinned GCC+musl toolchain. The Python
interpreter and C++ executables run through that toolchain's matching musl
loader on glibc hosts. Go runs with `CGO_ENABLED=0`, a repo-local Go toolchain,
isolated caches, and the currently global `GOEXPERIMENT=jsonv2` setting. Deno,
Ninja, Moon, and doctest are likewise pinned.

## Build And Test Graph

`repo.sh` delegates aggregate `lint`, `build`, `test`, and `ci` work to the
pinned Moon binary. It derives a bounded coarse/inner job budget from
`POLYGLOT_JOBS`; each language retains its native build semantics below that
coarse scheduler.

The C++ graph is defined by `cpp/cpp.toml` plus component-local `build.toml`
files. `tools/cpp_graph.py` generates Ninja, a C++ compile database fragment,
and the test inventory. `tools/python_build.py` records the native-extension
compile actions in a Python fragment. `./repo.sh compile-commands` merges the
available fragments into the root `compile_commands.json`.

Direct language test commands are self-contained. Aggregate Moon test tasks
depend on their corresponding build tasks and defer compilation-database merging
until the dedicated merge task, avoiding concurrent root-file rewrites.

## Packages And Releases

`package.toml` declares package identity, version, targets, executables, and
runtime requirements. `runtime-resolution.lock.toml` fixes the exact runtime
closure. `./repo.sh package-validate` validates these contracts before package
assembly.

`./repo.sh package <name> [dbg|opt]` creates a deterministic gzip tar archive
at `dist/<name>-<version>-<target>.tar.gz`. For `polyglot-demo`, it first builds
the C++, Python, Go, and web outputs needed by its staging plan. Package smoke
extracts an archive into a clean temporary location and runs the declared
consumer checks. `release-check` verifies a package name and tag against the
root `CHANGELOG.md`; `release-notes` prints that tag's changelog section.

The GitHub workflow runs the normal quality gates on native x64 and ARM64
runners. A `packages/<name>/v<version>` tag additionally packages that named
entry, runs the release check, uploads target-specific `.tar.gz` and `.sha256`
files, and creates a GitHub Release from the root changelog section. See
[CI-RELEASE.md](CI-RELEASE.md) for the exact workflow contract.

## Editor And Agent Guidance

`.vscode/settings.json` points clangd at the generated root compilation
database, configures Deno for `ts/` and `tsweb/`, exposes Python library and app
roots, and routes Go through the repository wrapper. Repository operating
guidance is tracked in `.agents/md/overview.md` and exposed through the
relative `AGENTS.md` symlink. Generated agent state, credentials, downloads,
caches, build products, and releases remain ignored.

## Deliberately Unimplemented

This checkout does not currently provide toolchain-update commands, schema
generation, benchmark/coverage commands, third-party dependency adapters,
fleet management, deployment, certificate automation, Zstandard packaging, or
attestation generation. Documents describing those capabilities retain their
proposal or research status and must not be treated as executable contracts.
