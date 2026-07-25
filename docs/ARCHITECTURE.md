# Polyglot Template Architecture

## Scope And Status

This document describes the implemented bootstrap slice in this repository.
It is a Linux-only, CPU-native, offline-after-bootstrap workspace for C++,
Python, Go, TypeScript, and a React static site. Linux x86-64 hosts build
`x86_64-linux-musl`; Linux ARM64 hosts build `aarch64-linux-musl`. Fleet, schema-generation, benchmark, and third-party-adapter designs in adjacent
documents are proposals; they are not part of the current `repo.sh` interface.
Deployment has only the read-only Buck plan/observation contract documented in
[CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md); it has no deploy command or
remote mutation path.

## Repository Shape

```text
repo/
|- AGENTS.md -> .agents/md/overview.md
|- repo.sh                         # public command surface
|- tools.lock.toml                 # exact bootstrap artifacts
|- packages/catalog.bzl            # canonical package and runtime catalog
|- BUCK, rules/, config/, platforms/, toolchains/  # Buck2 build graph
|- bxl/                            # BXL queries (compdb.bxl, coverage.bxl)
|- toolchain/                      # bootstrap, target, lock, doctor helpers
|- cpp/{lib,app,test}/             # C++ sources (BUCK-owned targets)
|- python/{lib,app,test}/          # Python and native extension sources
|- go/{lib,app,test}/              # pure-Go lane
|- ts/{lib,app,test}/              # Deno TypeScript lane
|- tsweb/{lib,app,test}/           # React/Vite static application
|- infra/deploy/                   # read-only deployment plan/observe contract
|- tools/                          # package, lint, and smoke helpers
|- test/                           # repository-contract tests
|- .local/                         # ignored tools and caches
|- build/                          # ignored build output
`- dist/                           # ignored package output
```

Each language owns `lib/<name>/`, `app/<name>/`, and `test/`. Use
`./repo.sh` rather than calling the installed language tools directly.

## Public Commands

Run `./repo.sh help` for the canonical, current list, which `repo.sh`'s own
`usage()` owns. Reproduced here:

```text
shell
exec <command> [args...]
help
target
bootstrap [--offline] [--dry-run] [--repair]
doctor [--deep]
toolchain-lock [--check] | toolchain-qualify [--offline]
buck2 [args...]
infra-lint
format [--check]
lint
build [dbg|opt]
coverage
test [dbg|opt]
compile-commands [dbg|opt]
cpp-build [dbg|opt] | cpp-run [dbg|opt] | cpp-test
python [args...] | python-build | python-test
deno [args...] | ts-build | ts-test | tsweb-build | tsweb-test
go [args...] | go-build | go-test
init-project <name> [org]
infra-test
package-list | package-explain <name>
package-validate | package-resolve <name> [out]
package-target-check <name>
package <name> [dbg|opt] | package-smoke <archive> <name>
release-check <name> <tag> | release-notes <tag>
ci
```

`format [--check]` applies, or checks, repository formatting for every lane;
`lint` includes the non-mutating `format --check`, so `./repo.sh format --check`
is the cheapest pre-handoff gate. `test` takes an optional `[dbg|opt]` profile
and defaults to `dbg`. `init-project <name> [org]` renames this template into a
new project (see the README). `package-list` and `package-explain <name>` are
read-only discovery commands over `packages/catalog.bzl`, and
`package-target-check <name>` is the release-side requirement that a
`//packages:<name>` Buck target exists.

`infra-lint` and `infra-test` are internal entrypoints used by `lint` and by
repository CI/tests respectively. Every build/test/lint lane command above is a
thin wrapper around a Buck2 target invocation - see the Build And Test Graph
section below.

## Toolchain And Native Policy

`tools.lock.toml` records immutable URLs, SHA-256 values, archive layouts, and
expected binaries for every supported target. `./repo.sh bootstrap` is the only
command allowed to fetch. It installs verified artifacts below
`.local/toolchain/<target>/`; normal build, lint, test, package, and release
commands use that local installation and do not fall back to host compilers.
`toolchain-lock` regenerates or checks the derived Buck2 projection, while
`toolchain-qualify` reinstalls and probes every locked artifact before a deep
doctor check. See [TOOLCHAIN-LIFECYCLE.md](TOOLCHAIN-LIFECYCLE.md) for the
reviewed update, recovery, and rollback workflow.

The current host CPU selects the target: x86-64 hosts produce
`x86_64-linux-musl`, and ARM64 hosts produce `aarch64-linux-musl`. Native C++
and Python-extension work uses the pinned GCC+musl toolchain. The Python
interpreter and C++ executables run through that toolchain's matching musl
loader on glibc hosts. Go runs with `CGO_ENABLED=0`, a repo-local Go toolchain,
isolated caches, and the currently global `GOEXPERIMENT=jsonv2` setting. Deno,
Buck2, and doctest are likewise pinned; every non-Buck2 tool above is
also wired into the Buck2 graph as an in-graph toolchain (see
`toolchains/lock.bzl`, generated from `tools.lock.toml`), so buck2-driven
builds never depend on a host-installed compiler, interpreter, or runtime.
Graph actions never fetch: in-graph toolchains extract from the
sha256-verified archives bootstrap retains under `.local/downloads/`, and
deno resolution seeds from bootstrap's `.local/cache/deno` - a cold
`buck-out` replays the entire graph offline (CI proves this by wiping
buck-out and rebuilding+testing inside a no-network namespace).

## Build And Test Graph

`repo.sh` delegates aggregate `lint`, `build`, `test`, and `ci` work to the
pinned Buck2 binary; every lane command (`cpp-build`, `python-test`,
`go-build`, `ts-test`, `tsweb-build`, ...) is a thin wrapper that invokes the
corresponding Buck2 target. There is one scheduler: Buck2 owns caching,
incrementality, and cross-lane concurrency directly, rather than a coarse
task runner layered over a separate jobserver.

- `build [dbg|opt]` discovers every lane's primary build output by rule
  kind (`buck2 uquery` over the binary/check/site rule kinds - no
  hand-listed //:build group exists) and builds the result (opt via
  `--target-platforms //config:<target>-opt`; `-m`/`--modifier` does not
  override a rule's own default target platform on the pinned buck2 - see
  `config/defs.bzl`).
- `test [dbg|opt]` runs `buck2 test //...` in the selected profile (default
  `dbg`), which builds and runs every lane's tests including its lint-as-test
  targets.
- `lint` runs `buck2 test //... --labels lint` (every lane's
  formatting/static-policy targets, selected by label) plus infra checks
  (`bash -n` and pinned ShellCheck over the shell scripts — ShellCheck is
  a locked artifact like every other tool, so the gate is mandatory rather
  than dependent on what the host happens to provide — plus
  `tools/lint.py`) that have no buck2
  target because they check files outside the buck2 graph.
- `coverage` is default-on-for-`dbg`: every instrumented lane's test
  collects coverage as a normal build output under `dbg`, and
  `./repo.sh coverage` (bxl/coverage.bxl) discovers every test by rule
  kind and merges the collected data into one repo-relative lcov report
  plus a per-file summary. `opt` builds stay uninstrumented. The same
  command then renders `buck-out/coverage-report/coverage.html`, a
  self-contained browsable report (`tools/coverage_html.py`) with
  annotated sources. That renderer is deliberately out-of-graph: it is a
  view of the merged lcov plus the working tree, nothing depends on it,
  and making it a Buck action would mean declaring every repository
  source as an input to the merge just to annotate them.

Every first-party Buck2 rule (there is no prelude in this repository) lives
under `rules/`: `rules/cxx.bzl`, `rules/go.bzl`, `rules/python.bzl`,
`rules/deno.bzl`, `rules/package.bzl`, `rules/coverage.bzl`,
`rules/toolchain.bzl`, plus small `constraint_setting`/`constraint_value`/
`group`/`export_file` replacements for prelude rules this repository does not
have. `./repo.sh compile-commands` materializes the root
`compile_commands.json` from `bxl/compdb.bxl`'s BXL compilation-database
query over the buck2-built cpp/python actions.

## Packages And Releases

`packages/catalog.bzl` declares package identity, version, targets, executables,
runtime requirements, and the exact runtime
closure. `./repo.sh package-validate` validates these contracts before package
assembly.

`./repo.sh package <name> [dbg|opt]` creates a deterministic gzip tar archive
at `dist/<name>-<version>-<target>.tar.gz`, but only when the catalog entry has
its own `//packages:<name>` Buck2 target (`rules/package.bzl`'s `package()`
rule - currently `polyglot-demo` and `polyglot-server`). `repo.sh` requires
that target and builds it entirely in-graph, folding every dep's staged
`PackageEntry` list into one deterministic tar.gz before copying Buck2's output
under the release naming convention. Catalog-only declarations (`gateway`,
`schema-cli`) fail closed: there is no raw-build fallback assembler. Package
smoke extracts an archive into a clean temporary location and runs the declared
consumer checks; the in-graph packages' own per-package checks live as
`package_smoke` Buck2 targets alongside their `package()` target. Current Deno
application packaging is limited to a zero-npm-dependency closure.
`release-check` verifies a package name and tag against the root
`CHANGELOG.md`; `release-notes` prints that tag's changelog section.

The GitHub workflow runs the normal quality gates on native Linux x64 and ARM64
runners. A `packages/<name>/v<version>` tag must name an in-graph package; CI
packages it, runs the release check, uploads target-specific `.tar.gz` and
`.sha256` files, generates a GitHub artifact attestation for each archive, and
creates a GitHub Release from the root changelog section. See
[CI-RELEASE.md](CI-RELEASE.md) for the exact workflow contract.

## Editor And Agent Guidance

`.vscode/settings.json` points clangd at the generated root compilation
database, configures Deno for `ts/` and `tsweb/`, exposes Python library and app
roots, and routes Go through the repository wrapper. Repository operating
guidance is tracked in `.agents/md/overview.md` and exposed through the
relative `AGENTS.md` symlink. Generated agent state, credentials, downloads,
caches, build products, and releases remain ignored.

## Deliberately Unimplemented

This checkout does not currently provide schema generation, benchmark commands,
third-party dependency adapters, fleet management, a deploy command or remote
mutation path, certificate automation, cross-compilation, macOS/Windows
support, sanitizers, or Zstandard packaging. Sanitizers remain explicitly
deferred to preserve the hermetic Linux-musl closure; they require a separately
evaluated host-debug toolchain. Tagged package archives receive GitHub artifact
attestations, and `tools/release_evidence.py` writes the deterministic
`.sbom.json` and `.provenance.json` sidecars that the package-release workflow
publishes alongside each archive and the release manifest. What remains
incomplete is release-tag signature verification: CI enforces an annotated tag
but cannot hold the public-key trust root, so signature policy lives outside the
repository (see [CI-RELEASE.md](CI-RELEASE.md)). Documents describing
unimplemented capabilities retain their proposal or research status and must
not be treated as executable contracts. See
[CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md) for the readiness matrix.
