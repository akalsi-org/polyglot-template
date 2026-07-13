# Language Guide

Use this guide to enter a language lane without rediscovering its build and
runtime contract. `./repo.sh` is the only supported command surface: it selects
the pinned toolchain, runtime, caches, and target ABI. Do not call host
compilers, interpreters, package managers, or generated build files directly.

Before making a change, inspect the owning `lib/`, `app/`, and `test/` paths.
Afterward, run the lane's test command. Run `./repo.sh lint` when changing more
than a narrowly isolated lane or before handing off a multi-file change.

## C++

| Item | Contract |
| --- | --- |
| Source layout | `cpp/lib/<name>/`, `cpp/app/<name>/`, `cpp/test/` |
| Public includes | Use logical paths rooted at `cpp/lib`, for example `#include "pgt/core/types.hh"`. |
| Build metadata | `cpp/cpp.toml` owns global toolchain/profile policy. Nearby `build.toml` files own executable and test targets. |
| Build | `./repo.sh cpp-build [dbg|opt]` |
| Test | `./repo.sh cpp-test` |
| Run | `./repo.sh cpp-run [dbg|opt]` |

The lane uses pinned GCC+musl, Ninja, mold, and `-std=gnu++26`. The build graph
and root compilation database are generated; edit source or component
`build.toml` files, never `build/`. Add each new C++ test executable to the
nearest `build.toml`; doctest's runner is shared automatically. Use the
existing two-space indentation and project naming conventions.

## Python

| Item | Contract |
| --- | --- |
| Source layout | `python/lib/<name>/`, `python/app/<name>/`, `python/test/` |
| Imports | `python/lib` and `python/app` are available through `./repo.sh python`. |
| Native extensions | Build against the exact pinned CPython ABI with the pinned GCC+musl toolchain. |
| Build extension | `./repo.sh python-build` |
| Test | `./repo.sh python-test` |
| Run Python | `./repo.sh python <args...>` |

Do not use host `python`, `pip`, or a virtual environment for repository work.
`python-build` stages native extension outputs under `build/python/<target>/lib`
and updates the canonical compilation database. Keep pure Python tests under
`python/test/` using the existing `unittest` discovery pattern.

## TypeScript

| Item | Contract |
| --- | --- |
| Source layout | `ts/lib/<name>/`, `ts/app/<name>/`, `ts/test/` |
| Library imports | `@/` resolves to `ts/lib/`. |
| Runtime and graph | Pinned Deno and the frozen root `deno.lock`. |
| Build/type-check | `./repo.sh ts-build` |
| Test | `./repo.sh ts-test` |
| Run Deno | `./repo.sh deno <args...>` |

Keep TypeScript strict and use Deno's configured formatter and linter through
`./repo.sh lint`. Normal lane commands use the frozen dependency graph; do not
introduce an ad hoc npm or Node workflow.

## Browser TypeScript

| Item | Contract |
| --- | --- |
| Source layout | `tsweb/lib/<name>/`, `tsweb/app/<name>/`, `tsweb/test/` |
| Library imports | `#/` resolves to `tsweb/lib/`. |
| Application | React 19 static site at `tsweb/app/site/`; Vite config is `tsweb/vite.config.ts`. |
| Build | `./repo.sh tsweb-build` |
| Test and asset smoke | `./repo.sh tsweb-test` |

The pinned Deno Vite integration resolves the frozen npm graph. The production
bundle is emitted to `build/tsweb/site`; do not commit it. Tests must cover
component behavior, while `tsweb-test` also checks emitted asset integrity.

## Go

| Item | Contract |
| --- | --- |
| Module | Root `go.mod`: `github.com/akalsi-org/polyglot-template` |
| Source layout | `go/lib/<name>/`, `go/app/<name>/`, `go/test/` |
| Build | `./repo.sh go-build` |
| Test | `./repo.sh go-test` |
| Run Go | `./repo.sh go <args...>` |

The wrapper pins Go, isolates all Go caches under `.local/`, sets
`GOTOOLCHAIN=local`, and enforces `CGO_ENABLED=0`. Do not use the system `go`
command or add CGO dependencies without revisiting the native toolchain
contract. Keep Go formatting canonical with `gofmt`; `./repo.sh lint` runs it
and `go vet` through the pinned setup.

## Cross-Lane Work

Use `./repo.sh build` or `./repo.sh test` for changes that span multiple
languages. Those commands use pinned Moon to run independent lanes with a
bounded concurrency budget. Use `./repo.sh ci` before a release-oriented
handoff; it adds package validation to lint, build, and test.

Bootstrap is the only operation permitted to fetch dependencies:
`./repo.sh bootstrap`. For an already provisioned checkout, use
`./repo.sh bootstrap --offline` to prove that normal work does not require
network access. `./repo.sh doctor --deep` validates the installed toolchain and
target closure when environment problems are suspected.
