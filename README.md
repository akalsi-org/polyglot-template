# Polyglot Template Bootstrap Slice

This repository is the executable vertical slice associated with the decision package. It establishes the public command surface, native target policy, language-owned source layout, locked bootstrap model, package/runtime closure schema, and C++ build-graph generation without implementing fleet deployment.

Editors use two-space indentation across the repository. Go source remains `gofmt`-canonical with tabs displayed at a width of two spaces.

```bash
./repo.sh help
./repo.sh
./repo.sh exec <command> [args...]
./repo.sh lint
./repo.sh build
./repo.sh test
```

Running `./repo.sh` starts an interactive shell with the exact pinned
toolchain, runtime, cache, and source-root environment. Use `./repo.sh exec`
to run one command in that same environment; `python`/`python3`, `go`,
`deno`, and the binutils `ar`/`ranlib`/`nm`/`strip`/`objcopy`/`ld` resolve to
repository wrappers, while `CC` and `CXX` name the pinned native compiler.

CPython 3.14.6, CPU-native GCC 16.1+musl, bundled mold 2.41, Deno 2.9.2, Go 1.26.5, and Buck2 (2026-07-15) are pinned for x64 and ARM64 using immutable upstream URLs and verified SHA-256 values. The Python artifacts and C++ outputs are dynamically linked musl programs, so repository commands invoke them through the exact pinned loader/libc closure on glibc hosts.

The userdocs compiler source is intentional rather than historical accident. Against cross-tools release `20260515`, userdocs release `2628` is about half the compressed download and 22–24% smaller unpacked. Its ARM archive also contains an ARM64-hosted compiler suitable for `ubuntu-24.04-arm`; the cross-tools ARM64-target archive inspected during selection contains an x86-64-hosted compiler. See the parent architecture document for the measured table.

The upstream userdocs toolchains are configured with `--disable-libsanitizer`, and this template deliberately leaves sanitizers outside its musl toolchain contract. The usable `dbg` profile provides symbols, assertions, low optimization, warnings, and frame pointers; `opt` remains the portable release profile. A future sanitizer lane should be an independently evaluated host-debug toolchain rather than a fork requirement for these release artifacts. C++ tests use pinned doctest 2.5.3: its implementation and `main` compile once per native profile, then the same runner object links into each independently runnable test target.

Source is organized under `cpp/`, `python/`, `ts/`, and `tsweb/`. Every language owns `lib/<name>/`, `app/<name>/`, and `test/`. Library roots are configured by the repository commands, so code uses logical names such as `example/example.hh`, `example.example`, `@/greeting/greeting.ts`, and `#/title/title.ts` instead of relative traversal. `@/` is the local TypeScript library root; `#/` is the browser-TypeScript library root.

The Python lane includes a real C++ extension under `python/lib/fastbytes`. `python-build` queries include paths and `EXT_SUFFIX` from the exact pinned interpreter, compiles with pinned GCC/musl/mold, stages the importable module under `build/python/<target>/lib`, and merges its exact action into the canonical root `compile_commands.json`. The Go lane uses one module at `github.com/akalsi-org/polyglot-template`, `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, isolated repository caches, Go 1.26's default Green Tea collector, and the currently global `jsonv2` experiment. Bootstrap keeps the repo-local Go installation recursively owner-writable so it can be replaced or deleted without permission repair.

The committed `.vscode/` configuration mirrors those command-line roots: clangd consumes the generated root compilation database; Deno owns `ts/` and `tsweb/` through `deno.json`; Pylance searches both `python/lib` and `python/app`; unittest discovers `python/test`; the Go extension invokes the pinned repository wrapper; and generated/toolchain directories are excluded from file watching and search.

`tsweb/app/site` is a static React 19 application built by pinned Vite through Deno's official Vite plugin. The plugin delegates application and import-map resolution to Deno, including the scoped `#/` library root. Bootstrap resolves the frozen npm graph into the repo-local Deno cache and Deno-managed ignored `node_modules` projection; CI caches both by native target and the exact tool/lock digests; normal `tsweb-build` is cached-only; and production smoke enforces referenced-asset integrity plus a 250 KB uncompressed JavaScript budget. The output under `build/tsweb/site` needs no Deno or JavaScript runtime when served.

The checked-in GitHub workflow performs a real native bootstrap on x64 and ARM64. It restores only exact target/lock/bootstrap-keyed caches, installs on a miss, proves the second bootstrap succeeds offline, runs deep capability checks, and verifies every language lane. A `packages/<name>/v<version>` tag assembles the named package independently on both native runners, executes clean-extraction consumer smoke, uploads separate target archives and checksums, and publishes one package-specific GitHub Release using its changelog section.

`polyglot-demo` is the complete consumer proof: one target archive contains the C++ executable and musl loader, static Go executable, Python application/native extension and exactly one CPython runtime, plus the React static bundle. Its package smoke runs every executable and validates every referenced web asset from an isolated extraction without host Python, Deno, Go, compiler, or source-tree state.

The command surface is uniform across lanes: `cpp-build`/`cpp-test`, `python-build`/`python-test`, `ts-build`/`ts-test`, `go-build`/`go-test`, and `tsweb-build`/`tsweb-test`; each is a thin wrapper around a Buck2 target. Aggregate `build` (every lane's primary outputs, discovered by rule kind) and `test` (`buck2 test //...`) run every lane's targets in one scheduler, in-graph, with Buck2 owning caching, incrementality, and cross-lane concurrency directly rather than through a coarse task runner plus a separate jobserver. `lint` runs `buck2 test //... --labels lint` (every lane's formatting/static-policy targets, selected by label) plus a small set of infra checks (`bash -n` over the shell scripts, `tools/lint.py`) that live outside the buck2 graph. Every first-party rule lives under `rules/`; `config/defs.bzl` defines the `dbg`/`opt` `profile` configuration, selected via `--target-platforms //config:<arch>-<profile>` (`-m`/`--modifier` does not override a rule's own default target platform on the pinned buck2). Coverage is default-on for `dbg` (`./repo.sh coverage` discovers every test by rule kind and merges every lane's coverage into one lcov report); `opt` stays uninstrumented. See [the historical parallel-build spike](docs/spikes/parallel-build.md), which predates the Buck2 migration.

For task-oriented source layout, command, and validation guidance by language,
see the [language guide](docs/LANGUAGE-GUIDE.md).
