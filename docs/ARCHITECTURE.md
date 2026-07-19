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
|- BUCK, rules/, config/, platforms/, toolchains/  # Buck2 build graph
|- toolchain/                      # bootstrap, target, lock, doctor helpers
|- cpp/{lib,app,test}/             # C++ sources (BUCK-owned targets)
|- python/{lib,app,test}/          # Python and native extension sources
|- go/{lib,app,test}/              # pure-Go lane
|- ts/{lib,app,test}/              # Deno TypeScript lane
|- tsweb/{lib,app,test}/           # React/Vite static application
|- tools/                          # package, lint, and smoke helpers
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
shell
exec <command> [args...]
bootstrap [--offline] [--dry-run]
doctor [--deep]
buck2 [args...]
target
lint
build [dbg|opt]
test
compile-commands [dbg|opt]
cpp-build [dbg|opt] | cpp-run [dbg|opt] | cpp-test
python [args...] | python-build | python-test
deno [args...] | ts-build | ts-test | tsweb-build | tsweb-test
go [args...] | go-build | go-test
package-validate | package-resolve <name> [out]
package <name> [dbg|opt] | package-smoke <archive> <name>
release-check <name> <tag> | release-notes <tag>
ci
```

`infra-lint` and `infra-test` are internal entrypoints used by `lint` and by
repository tests respectively; they are intentionally omitted from this list
but remain directly invocable. Every build/test/lint lane command above is a
thin wrapper around a Buck2 target invocation - see the Build And Test Graph
section below.

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
Ninja, Buck2, and doctest are likewise pinned; every non-Buck2 tool above is
also wired into the Buck2 graph as an in-graph toolchain (see
`toolchains/lock.bzl`, generated from `tools.lock.toml`), so buck2-driven
builds never depend on a host-installed compiler, interpreter, or runtime.

## Build And Test Graph

`repo.sh` delegates aggregate `lint`, `build`, `test`, and `ci` work to the
pinned Buck2 binary; every lane command (`cpp-build`, `python-test`,
`go-build`, `ts-test`, `tsweb-build`, ...) is a thin wrapper that invokes the
corresponding Buck2 target. There is one scheduler: Buck2 owns caching,
incrementality, and cross-lane concurrency directly, rather than a coarse
task runner layered over a separate jobserver.

- `build [dbg|opt]` runs `buck2 build //:build` (opt via
  `--target-platforms //config:<target>-opt`; `-m`/`--modifier` does not
  override a rule's own default target platform on the pinned buck2 - see
  `config/defs.bzl`).
- `test` runs `buck2 test //...`, which builds and runs every lane's tests
  including its lint-as-test targets.
- `lint` runs `buck2 test //... --labels lint` (every lane's
  formatting/static-policy targets, selected by label) plus infra checks
  (`bash -n` over the shell scripts, `tools/lint.py`) that have no buck2
  target because they check files outside the buck2 graph.
- `//:coverage` is a default-on-for-`dbg` merge target: every instrumented
  lane's test collects coverage as a normal build output under `dbg`, and
  `buck2 build //:coverage` merges it into one repo-relative lcov report plus
  a per-file summary. `opt` builds stay uninstrumented.

Every first-party Buck2 rule (there is no prelude in this repository) lives
under `rules/`: `rules/cxx.bzl`, `rules/go.bzl`, `rules/python.bzl`,
`rules/deno.bzl`, `rules/package.bzl`, `rules/coverage.bzl`,
`rules/toolchain.bzl`, plus small `constraint_setting`/`constraint_value`/
`group`/`export_file` replacements for prelude rules this repository does not
have. `./repo.sh compile-commands` materializes the root
`compile_commands.json` from `bxl/compdb.bxl`'s BXL compilation-database
query over the buck2-built cpp/python actions.

## Packages And Releases

`package.toml` declares package identity, version, targets, executables, and
runtime requirements. `runtime-resolution.lock.toml` fixes the exact runtime
closure. `./repo.sh package-validate` validates these contracts before package
assembly.

`./repo.sh package <name> [dbg|opt]` creates a deterministic gzip tar archive
at `dist/<name>-<version>-<target>.tar.gz`. Any package with its own
`//packages:<name>` Buck2 target (`rules/package.bzl`'s `package()` rule -
currently `polyglot-demo` and `polyglot-server`) is detected via `buck2
targets` and built entirely in-graph, folding every dep's staged
`PackageEntry` list into one deterministic tar.gz; `repo.sh` then copies
Buck2's output archive to `dist/` under the release naming convention. Every
other package (`gateway`, `schema-cli`: manifest-declared, no Buck2 target)
falls through to `tools/package_release.py`, which assembles from raw
build-directory executables. Package smoke extracts an archive into a clean
temporary location and runs the declared consumer checks - the in-graph
packages' own per-package checks live as `package_smoke` Buck2 targets
alongside their `package()` target. `release-check` verifies a package name
and tag against the root `CHANGELOG.md`; `release-notes` prints that tag's
changelog section.

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
