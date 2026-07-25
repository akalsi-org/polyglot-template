# Polyglot Template Bootstrap Slice

This repository is the executable vertical slice associated with the decision package. It establishes the public command surface, native target policy, language-owned source layout, locked bootstrap model, package/runtime closure schema, and C++ build-graph generation without implementing fleet deployment or remote mutation. See [current capabilities](docs/CURRENT-CAPABILITIES.md) for the verified readiness and compatibility boundary.

## Quick Start

The supported development hosts are Linux x86-64 and Linux ARM64. Builds are CPU-native only: an x86-64 host produces `x86_64-linux-musl`, and an ARM64 host produces `aarch64-linux-musl`. Bootstrap before invoking build, test, or language commands; it is the repository operation that installs the pinned toolchain and seeds the locked dependency cache.

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

A second pass then offers the leaf demo code itself - the greeting/hello
example library and app in the C++ and Python lanes, which exist only so a
fresh clone has something that builds. Removing a group also applies the edits
that removal requires elsewhere (`cpp/test/BUCK`, `python/test/BUCK`,
`packages/BUCK`, `packages/catalog.bzl`, `test/graph-compdb-contract.sh`), so
the graph stays buildable rather than merely smaller; it then prints the
`repo.sh` lane verbs (`cpp-run`, `python-build`, ...) that now name a removed
target and are yours to repoint. `--keep-all-demos` / `--strip-all-demos`
answer this pass non-interactively.

Reusable infrastructure is never a prune candidate, including some that does
not look like infrastructure: `cpp/lib/pyfast/` and its test fixture under
`python/test/` (`pyfast_test_ext.c`, `extension_init.py`,
`test_pyfast_extension.py` - the only thing that tests `pyfast.h`),
`python/lib/testlib.py` with its self-test and `tools/py_test_runner.py`,
`python/lib/fastbytes/` (the worked `py_extension` example), and
`cpp/test/reflection.cc` (`rules/toolchain.bzl`'s reflection capability
probe). Whole lanes (`go/`, `ts/`, `tsweb/`) and the demo package entries are
deliberately not offered - removing those means editing `repo.sh`,
`//:deno-cache`, `go.mod`/`vendor/`, and CI, which is a fork's own job.

Re-run bootstrap-backed commands afterward and verify with
`./repo.sh doctor --deep`, `./repo.sh test`, and `./repo.sh infra-test`.

Running `./repo.sh` starts an interactive shell with the exact pinned
toolchain, runtime, cache, and source-root environment. Use `./repo.sh exec`
to run one command in that same environment; `python`/`python3`, `go`,
`deno`, and the binutils `ar`/`ranlib`/`nm`/`strip`/`objcopy`/`ld` resolve to
repository wrappers, while `CC` and `CXX` name the pinned native compiler.

CPython 3.14.6, CPU-native GCC 16.1+musl, bundled mold 2.41.0, Deno 2.9.2, Go 1.26.5, and Buck2 (2026-07-15) are pinned for x64 and ARM64 using immutable upstream URLs and verified SHA-256 values. The Python artifacts and C++ outputs are dynamically linked musl programs, so repository commands invoke them through the exact pinned loader/libc closure on glibc hosts.

The userdocs compiler source is intentional rather than historical accident. Against cross-tools release `20260515`, userdocs release `2628` is about half the compressed download and 22–24% smaller unpacked. Its ARM archive also contains an ARM64-hosted compiler suitable for `ubuntu-24.04-arm`; the cross-tools ARM64-target archive inspected during selection contains an x86-64-hosted compiler. See the parent architecture document for the measured table.

The upstream userdocs toolchains are configured with `--disable-libsanitizer`, and this template deliberately leaves sanitizers outside its musl toolchain contract. The usable `dbg` profile provides symbols, assertions, low optimization, warnings, and frame pointers; `opt` remains the portable release profile. A future sanitizer lane should be an independently evaluated host-debug toolchain rather than a fork requirement for these release artifacts. C++ tests use pinned doctest 2.5.3: its implementation and `main` compile once per native profile, then the same runner object links into each independently runnable test target.

Source is organized under `cpp/`, `python/`, `ts/`, and `tsweb/`. Every language owns `lib/<name>/`, `app/<name>/`, and `test/`. Library roots are configured by the repository commands, so code uses logical names such as `example/example.hh`, `example.example`, `@/greeting/greeting.ts`, and `#/title/title.ts` instead of relative traversal. `@/` is the local TypeScript library root; `#/` is the browser-TypeScript library root.

The Python lane includes a real C++ extension under `python/lib/fastbytes`. `python-build` queries include paths and `EXT_SUFFIX` from the exact pinned interpreter, compiles with pinned GCC/musl/mold, stages the importable module under `build/python/<target>/lib`, and merges its exact action into the canonical root `compile_commands.json`. The Go lane uses one module at `github.com/akalsi-org/polyglot-template`, `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, isolated repository caches, Go 1.26's default Green Tea collector, and the currently global `jsonv2` experiment. Bootstrap keeps the repo-local Go installation recursively owner-writable so it can be replaced or deleted without permission repair.

The committed `.vscode/` configuration mirrors those command-line roots: clangd consumes the generated root compilation database; Deno owns `ts/` and `tsweb/` through `deno.json`; Pylance searches both `python/lib` and `python/app`; the repository Python runner discovers `python/test`; the Go extension invokes the pinned repository wrapper; and generated/toolchain directories are excluded from file watching and search.

`tsweb/app/site` is a static React 19 application built by pinned Vite through Deno's official Vite plugin. The plugin delegates application and import-map resolution to Deno, including the scoped `#/` library root. Bootstrap resolves the frozen npm graph into the repo-local Deno cache and Deno-managed ignored `node_modules` projection; CI caches both by native target and the exact tool/lock digests; normal `tsweb-build` is cached-only; and production smoke enforces referenced-asset integrity plus a 250 KB uncompressed JavaScript budget. The output under `build/tsweb/site` needs no Deno or JavaScript runtime when served.

The checked-in GitHub workflow performs a real native bootstrap on Linux x64 and ARM64 runners. It restores only exact target/lock/bootstrap-keyed caches, installs on a miss, proves the second bootstrap succeeds offline, runs deep capability checks, and verifies every language lane. A `packages/<name>/v<version>` tag must name an in-graph Buck package target; CI assembles it independently on both native runners, executes clean-extraction consumer smoke, uploads separate target archives and checksums, records a GitHub artifact attestation for each archive, and publishes one package-specific GitHub Release using its changelog section.

`./repo.sh package-list` prints every declared package's identity, Buck targets,
and executables; `./repo.sh package-explain <name>` prints one package's catalog
identity, runtime closure, and Buck target status, which is the direct way to see
why a catalog-only declaration cannot be packaged. `./repo.sh package-target-check <name>`
is the release-side gate that requires a `//packages:<name>` target.

`polyglot-demo` is the complete consumer proof: one target archive contains the C++ executable and musl loader, static Go executable, Python application/native extension and exactly one CPython runtime, plus the React static bundle. Its package smoke runs every executable and validates every referenced web asset from an isolated extraction without host Python, Deno, Go, compiler, or source-tree state.

The command surface is uniform across lanes: `cpp-build`/`cpp-test`, `python-build`/`python-test`, `ts-build`/`ts-test`, `go-build`/`go-test`, and `tsweb-build`/`tsweb-test`; each is a thin wrapper around a Buck2 target. `format` applies the pinned Deno and Go formatters and normalizes repository-owned C/C++, Python, Starlark, shell, and Buck source whitespace; `format --check` is its non-mutating counterpart. Python and Starlark blocks are structurally enforced at two spaces, C/C++ editors use the committed two-space clang-format policy, and Go preserves `gofmt` tabs rendered at width two. Aggregate `build` (every lane's primary outputs, discovered by rule kind) and `test` (`buck2 test //...`) run every lane's targets in one scheduler, in-graph, with Buck2 owning caching, incrementality, and cross-lane concurrency directly rather than through a coarse task runner plus a separate jobserver. `lint` runs `format --check`, `buck2 test //... --labels lint` (every lane's formatting/static-policy targets, selected by label), and a small set of infra checks (`bash -n` plus pinned ShellCheck over the shell scripts, `tools/lint.py`) that live outside the buck2 graph. Every first-party rule lives under `rules/`; `config/defs.bzl` defines the `dbg`/`opt` `profile` configuration, selected via `--target-platforms //config:<arch>-<profile>` (`-m`/`--modifier` does not override a rule's own default target platform on the pinned buck2). Coverage is default-on for `dbg` (`./repo.sh coverage` discovers every test by rule kind and merges every lane's coverage into one lcov report, then renders `buck-out/coverage-report/coverage.html`, a single self-contained page with annotated sources that CI also publishes as a job summary); `opt` stays uninstrumented. See [the historical parallel-build spike](docs/spikes/parallel-build.md), which predates the Buck2 migration.

For task-oriented source layout, command, and validation guidance by language,
see the [language guide](docs/LANGUAGE-GUIDE.md).
