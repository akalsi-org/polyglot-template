# Language Guide

Use this guide to enter a language lane without rediscovering its build and
runtime contract. `./repo.sh` is the only supported command surface: it selects
the pinned toolchain, runtime, caches, and target ABI. Do not call host interpreters, package managers, or generated build files directly.

Run `./repo.sh` without arguments for an interactive shell with that
environment, or `./repo.sh exec <command> [args...]` for one command. In either
case, `python`/`python3`, `go`, `deno`, resolve to pinned wrappers.

Before making a change, inspect the owning `lib/`, `app/`, and `test/` paths.
Afterward, run the lane's test command. Run `./repo.sh lint` when changing more
than a narrowly isolated lane or before handing off a multi-file change.

## Python

| Item | Contract |
| --- | --- |
| Source layout | `python/lib/<name>/`, `python/app/<name>/`, `python/test/` |
| Imports | `python/lib` and `python/app` are available through `./repo.sh python`. |
| Build | `./repo.sh python-build` |
| Test | `./repo.sh python-test` |
| Run Python | `./repo.sh python <args...>` |

Do not use host `python`, `pip`, or a virtual environment for repository work.
The Python lane contains pure Python only. Keep tests under
`python/test/` as top-level `test_*` functions; `py_test()` discovers them
with the repository's readable stdlib-only runner. Use `@parametrize`, `@skip`,
`@skip_if`, and `@xfail` from `testlib` for function-test annotations.
`@xfail("reason")` reports a failing test as `XFAIL` with its reason; an xfail
test that passes is an `XPASS` with its reason and fails the suite, so remove
stale xfail annotations promptly.

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
`./repo.sh lint`. Normal Buck-backed lane commands use the frozen dependency
graph and do not fetch after bootstrap; do not introduce an ad hoc npm or Node
workflow. `./repo.sh deno <args...>` is a raw Deno passthrough, not an offline
sandbox: a caller can supply a network-capable Deno subcommand or flags. Use the
named `ts-*` and `tsweb-*` commands when the offline-after-bootstrap guarantee
matters.

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

The wrapper pins Go and isolates all Go caches under `.local/`. It sets
`GOENV=off`, `GOTOOLCHAIN=local`, and `CGO_ENABLED=0`. It also enforces
`-mod=vendor` and `-buildvcs=false`. User `go env -w` state and Git status cannot
change repository builds. Go dependencies are locked in `go.sum` and committed
under `vendor/`; update both with the pinned Go command before changing an
external import. Named `go-build` and `go-test` commands use the vendored closure
with module services disabled, so they remain offline after bootstrap. Do not
use the system `go` command or add CGO
dependencies without revisiting the native toolchain contract. `./repo.sh go
<args...>` intentionally passes its arguments to Go, so commands such as module
download or installation can use the network; it is not part of the offline
guarantee. Keep Go formatting canonical with `gofmt`; `./repo.sh lint` runs it
and `go vet` through the pinned setup.

### Shared-memory queues and benchmark campaign

`go/lib/mpsc` is the current Linux shared-memory queue implementation. It
provides SPSC and MPSC queues on amd64 and arm64 with CGO disabled. The package
creates and attaches shared-memory format version 4 only. See
[mpsc-queue.md](mpsc-queue.md) for its API, format, ownership, recovery,
ordering, benchmark, and verification contracts.

Run the queue tests and ordering checks through Go targets:

```bash
./repo.sh buck2 test //go/lib/mpsc:mpsc_test
./repo.sh exec go/lib/mpsc/ordering_mutants.sh
```

The Go benchmark campaign lives under `go/bench/mpsc`. Buck targets build and
test `bench_queue`, `run_campaign`, and `summarize_campaign`. Run the campaign
through these targets. Do not use the retired C++ benchmark commands or paths.

## Cross-Lane Work

Use `./repo.sh build` or `./repo.sh test` for changes that span multiple
languages. Those commands run a rule-kind-discovered `buck2 build` / `buck2 test //...`,
which build and test every lane's targets in one graph. Use `./repo.sh ci`
before a release-oriented handoff; it adds package validation to lint, build,
and test.

`./repo.sh bootstrap` is the repository operation that installs toolchain
artifacts and seeds the locked Deno dependency cache. For an already provisioned
checkout, use `./repo.sh bootstrap --offline` to prove that normal Buck-backed
work does not require network access. The raw `go` and `deno` passthroughs remain
caller-controlled and may use the network; they do not weaken the named lane
commands' contract. `./repo.sh doctor --deep` validates the installed toolchain
and target closure when environment problems are suspected.
