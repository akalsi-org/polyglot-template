# Polyglot Template Bootstrap Slice

This repository is the executable vertical slice associated with the decision package. It establishes the public command surface and CPU-native target policy. It defines the language-owned source layout, locked bootstrap model, and package runtime closure schema. It does not implement fleet deployment or remote mutation. See [current capabilities](docs/CURRENT-CAPABILITIES.md) for the verified readiness and compatibility boundary.

## Quick Start

The supported development hosts are Linux x86-64 and Linux ARM64. Builds are CPU-native only: an x86-64 host produces `x86_64-linux-musl` requiring the x86-64-v3 ISA baseline, and an ARM64 host produces `aarch64-linux-musl` requiring Armv8.2-A. These are deployment compatibility requirements, not host auto-detection; do not run an artifact on an older CPU. Bootstrap before invoking build, test, or language commands; it is the repository operation that installs the pinned toolchain and seeds the locked dependency cache.

```bash
./repo.sh bootstrap --dry-run
./repo.sh bootstrap
./repo.sh doctor --deep
./repo.sh test
```

After bootstrap, normal Buck-backed build, test, lint, package, and release commands run from the pinned local closure and do not fetch. `./repo.sh go` and `./repo.sh deno` are raw language-tool passthroughs: a command supplied to either may use the network according to that tool's own flags and cache state. Prefer the named `go-*`, `ts-*`, and `tsweb-*` commands for the repository's offline-after-bootstrap contract.

Editors use two-space indentation across the repository. Go source remains `gofmt`-canonical with tabs displayed at a width of two spaces.

```bash
./repo.sh help
./repo.sh
./repo.sh exec <command> [args...]
./repo.sh format --check
./repo.sh lint
./repo.sh build
./repo.sh test
```

### Instantiating A New Repository From This Template

This repository is a template. Clone it, then run `init-project` once, before
writing project code:

```bash
git clone ssh://git@github.com/akalsi-org/polyglot-template my-project
cd my-project
./repo.sh init-project my-project my-org
```

That single command renames, strips the template-only fat, and leaves you with
a fresh repository: one commit, no tags, no upstream. It never pushes - the
commands to attach your own remote are printed at the end.

`init-project` runs `tools/init_project.py` through the pinned interpreter. It
rewrites the repository namespace, the Go module path
(`github.com/<org>/<project>`), package catalog entries, and documentation
references away from `polyglot-template`/`akalsi-org`. The organization argument
is optional; omitting it keeps the current organization. `LICENSE` is left
alone on purpose: its copyright line names a holder, not a namespace, and
reassigning it is a claim only you can make.

Three renames are worth calling out, because none of them contains the string
`polyglot-template` and every one of them used to survive into forks:

- **Package identity.** `polyglot-demo` and `polyglot-server` become
  `<project>-demo` and `<project>-server` across `packages/catalog.bzl`,
  `packages/BUCK`, `infra/deploy/BUCK`, both CI workflows (including their
  cache-key prefixes), and the docs. The deployment plan/observation schema
  identifiers (`polyglot.deployment-plan/v1`) rename with them.
- **This section, and the changelog.** The README's title becomes the project
  name, these template-instantiation instructions are deleted, and
  `CHANGELOG.md` resets to an empty `# Changelog` rather than shipping this
  template's own release history.

It then walks you, one file at a time, through this template's own
self-referential test files (`test/docs-contract.sh`,
`test/workflow-contract.sh`, and others) - contracts that assert exact strings
from *this repository's* docs, CI YAML, or demo catalog, not your project.
Keeping them means they will start failing the moment you rewrite the README
or customize CI, which forking implies you will; each prompt explains why that
file is or isn't safe to remove and, if you remove one, also drops its
invocation from `repo.sh`'s `infra-test` gate. Answer non-interactively with
`--keep-all-tests` or `--strip-all-tests` (e.g. for scripted forking); the
command fails closed if stdin isn't a terminal and neither flag is given.

A second pass then strips the leaf demo code itself - the greeting/hello
example library and app in the Python and Go lanes, which exist only so a
fresh clone has something that builds, plus this template's own design history
(`docs/IMPROVEMENT-ROADMAP.md`, `docs/DESIGN-README.md`, `docs/proposals/`,
`docs/spikes/`), which records decisions your project did not make. Removing a
group also applies the edits that removal requires elsewhere (`python/test/BUCK`, `go/BUCK`, `packages/BUCK`,
`packages/catalog.bzl`, and the prose in `docs/` that would otherwise
be left pointing at deleted pages), so the graph stays buildable and the docs
stay link-clean rather than merely smaller; it then prints the
`repo.sh` lane verbs that now name a removed target and are yours to repoint. `--keep-all-demos` / `--strip-all-demos`
answer this pass non-interactively.

Reusable infrastructure is never a prune candidate. This includes
`python/lib/testlib.py`, its self-test, and `tools/py_test_runner.py`. Whole
lanes (`go/`, `ts/`, `tsweb/`) and the demo package entries are
deliberately not offered - removing those means editing `repo.sh`,
`//:deno-cache`, `go.mod`/`vendor/`, and CI, which is a fork's own job.

Re-run bootstrap-backed commands afterward and verify with
`./repo.sh doctor --deep`, `./repo.sh test`, and `./repo.sh infra-test`.

Running `./repo.sh` starts an interactive shell with the exact pinned
toolchain, runtime, cache, and source-root environment. Use `./repo.sh exec`
to run one command in that same environment; `python`/`python3`, `go`,
`deno`, and the binutils `ar`/`ranlib`/`nm`/`strip`/`objcopy`/`ld` resolve to
repository wrappers, while the musl loader remains available for packaged Python runtimes.

CPython 3.14.6, Deno 2.9.2, Go 1.27.0, and Buck2 (2026-07-15) are pinned for x64 and ARM64 using immutable upstream URLs and verified SHA-256 values. Packaged Python applications use the exact pinned musl loader and libc closure on glibc hosts.

Source is organized under `python/`, `go/`, `ts/`, and `tsweb/`. Every language owns `lib/<name>/`, `app/<name>/`, and `test/`. Library roots are configured by the repository commands, so code uses logical names such as `example.example`, `github.com/akalsi-org/polyglot-template/go/lib/example`, `@/greeting/greeting.ts`, and `#/title/title.ts` instead of relative traversal. `@/` is the local TypeScript library root; `#/` is the browser-TypeScript library root.

The Go lane uses one module at `github.com/akalsi-org/polyglot-template`, `CGO_ENABLED=0`, `GOENV=off`, `GOTOOLCHAIN=local`, isolated repository caches, the committed vendor closure, disabled VCS stamping, and Go 1.27's default runtime settings. The Go `go/lib/mpsc` package owns the Linux SPSC and MPSC format version 4 implementation. See [docs/mpsc-queue.md](docs/mpsc-queue.md) for its API, format, concurrency, recovery, ordering, and verification contracts. Its ordering tests and the `go/bench/mpsc` campaign replace the retired C++ queue and benchmark paths. Bootstrap keeps the repo-local Go installation recursively owner-writable so it can be replaced or deleted without permission repair.

The committed `.vscode/` configuration mirrors the command-line roots. Deno owns `ts/` and `tsweb/` through `deno.json`. Pylance searches `python/lib` and `python/app`. The repository Python runner discovers `python/test`. The Go extension invokes the pinned repository wrapper. VS Code excludes generated and toolchain directories from file watching and search.

`tsweb/app/site` is a static React 19 application built by pinned Vite through Deno's official Vite plugin. The plugin delegates application and import-map resolution to Deno, including the scoped `#/` library root. Bootstrap resolves the frozen npm graph into the repo-local Deno cache and Deno-managed ignored `node_modules` projection; CI caches both by native target and the exact tool/lock digests; normal `tsweb-build` is cached-only; and production smoke enforces referenced-asset integrity plus a 250 KB uncompressed JavaScript budget. The output under `build/tsweb/site` needs no Deno or JavaScript runtime when served.

The checked-in GitHub workflow performs a real native bootstrap on Linux x64 and ARM64 runners. It restores only exact target/lock/bootstrap-keyed caches, installs on a miss, proves the second bootstrap succeeds offline, runs deep capability checks, and verifies every language lane. A `packages/<name>/v<version>` tag must name an in-graph Buck package target; CI assembles it independently on both native runners, executes clean-extraction consumer smoke, uploads separate target archives and checksums, records a GitHub artifact attestation for each archive, and publishes one package-specific GitHub Release using its changelog section.

`./repo.sh package-list` prints every declared package's identity, Buck targets,
and executables; `./repo.sh package-explain <name>` prints one package's catalog
identity, runtime closure, and Buck target status, which is the direct way to see
why a catalog-only declaration cannot be packaged. `./repo.sh package-target-check <name>`
is the release-side gate that requires a `//packages:<name>` target.

`polyglot-demo` is the complete consumer proof: one target archive contains a static Go executable, a pure Python application with exactly one CPython runtime, and the React static bundle. Its package smoke runs every executable and validates every referenced web asset from an isolated extraction without host Python, Deno, Go, compiler, or source-tree state.

The command surface is uniform across lanes. Each lane command is a thin wrapper around a Buck2 target. `format` applies the pinned Deno and Go formatters. It also normalizes repository-owned Python, Starlark, shell, and Buck whitespace. `format --check` is the non-mutating counterpart. Python and Starlark blocks use two spaces. Go preserves `gofmt` tabs displayed at width two. Aggregate `build` discovers every lane's primary outputs by rule kind. Aggregate `test` runs `buck2 test //...`. Buck2 owns scheduling, caching, incrementality, and cross-lane concurrency. `lint` runs format checks, labeled Buck tests, and repository infrastructure checks. Every first-party rule lives under `rules/`. `config/defs.bzl` defines the `dbg` and `opt` profiles. Select a profile with `--target-platforms //config:<arch>-<profile>`. The pinned Buck2 ignores `-m` for rules with their own default target platform. Coverage uses the `dbg` profile. It discovers tests by rule kind and merges all lane profiles into one LCOV report. It renders `buck-out/coverage-report/coverage.html`. CI also publishes that report as a job summary. The `opt` profile stays uninstrumented.

For task-oriented source layout, command, and validation guidance by language,
see the [language guide](docs/LANGUAGE-GUIDE.md).
