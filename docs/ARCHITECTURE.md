# Polyglot Template Architecture

## Repository Shape

```text
repo/
├── AGENTS.md -> .agents/md/overview.md
├── repo.sh
├── tools.lock.toml
├── package.toml
├── .gitignore
├── .agents/
│   ├── md/
│   ├── skills/
│   ├── roles/
│   └── templates/
├── .moon/
│   ├── workspace.yml
│   └── tasks/
├── toolchain/
│   ├── bootstrap.sh
│   ├── fetch_binary.sh
│   └── doctor.sh
├── cpp/
│   ├── cpp.toml
│   ├── lib/<name>/
│   ├── app/<name>/
│   └── test/
├── third_party/
│   ├── patches/
│   ├── licenses/
│   └── vendor/
├── go/{lib/<name>,app/<name>,test}/
├── python/{lib/<name>,app/<name>,test}/
├── ts/{lib/<name>,app/<name>,test}/
├── tsweb/{lib/<name>,app/<name>,test}/
├── deno.json
├── deno.lock
├── schema/
├── tools/
├── config/
├── bench/
├── test/
├── .local/                  # ignored
├── build/                   # ignored
└── dist/                    # ignored
```

## Public Commands

```text
bootstrap [--offline]
doctor [--deep]
target
toolchain check-updates
toolchain update <tool>|--all
third-party list|graph|verify|check-updates|update
graph
generate [--check]
format [--check]
lint
typecheck [--affected|--all]
build [dbg|opt]
test
cpp-build [dbg|opt] | cpp-test
python-build | python-test
ts-build | ts-test
go-build | go-test
tsweb-build | tsweb-test
coverage
compile-commands [profile] [--check|--first-party-only]
bench env|smoke|all|compare
package --package <name> [--version ...] [--compact|--format ...]
package-smoke
release-check --package <name> --version <version> --tag <tag>
release-notes --package <name> --version <version> --output <path>
ci
clean [--outputs|--cache|--toolchain]
```

Packages are independently versioned. Packaging selects exactly one declared package and derives its target triplet from the current host. Final release files are published beneath `dist/release/<package>/<version>/<host-triplet>/`; CI and release automation consume only this declared directory. There is no mandatory repository-wide archive or global package version.

`repo.sh` resolves the repository, establishes repo-local environment variables, and dispatches to moon. Dependency logic stays in moon tasks and lane tools rather than shell branches.

## Toolchain Lock And Bootstrap

`tools.lock.toml` records exact versions, immutable platform assets, SHA-256 values, archive prefixes, expected binaries, and capability probes. Downloads land under `.local/downloads`; installs land under `.local/toolchain/<platform>`; caches remain under `.local/cache`.

The current host selects the CPU architecture: x86-64 hosts build x86-64 and ARM64 hosts build ARM64. C, C++, native Python extensions, and source-built Python components always use the pinned GCC+musl toolchain, so the repository output ABI is `linux-musl` even when the build host itself uses glibc. “Native build” means that the compiler and produced executables run on the same CPU architecture as the runner; the initial template never cross-compiles CPU architectures. The resolved output triplet is therefore `<native-arch>-linux-musl`, and any explicit non-native architecture is rejected.

Installation is transactional:

1. normalize the platform;
2. select the locked asset;
3. reuse a verified cached archive when present;
4. download to `.partial` with bounded retries;
5. verify SHA-256;
6. reject archive traversal and escaping symlinks;
7. extract into a same-filesystem temporary prefix;
8. run declared capability probes;
9. atomically replace the install prefix;
10. write a content-derived stamp last.

Normal build, test, lint, benchmark-build, and package commands never fetch. `check-updates` is read-only. `update` installs and tests a candidate before leaving a reviewable lock diff.

## Initial Native Pins

The research baseline on 2026-07-11 is:

- userdocs `qbt-musl-cross-make` release `2628`, commit `671891d`;
- GCC 16.1.0, binutils 2.46.1, pinned musl, mold 2.41.0;
- Ninja 1.13.1;
- ccache 4.13.6;
- separately pinned LLVM tools for clangd, clang-format, and clang-tidy.

The native compiler provider is deliberately userdocs rather than `cross-tools/musl-cross`. Measured release artifacts were:

| Provider/target | Compressed bytes | Extracted payload bytes | Entries |
| --- | ---: | ---: | ---: |
| userdocs x64-host/x64-target | 44,478,052 | 274,748,708 | 2,659 |
| cross-tools x64 target | 85,186,472 | 359,602,533 | 3,405 |
| userdocs ARM64-host/ARM64-target | 38,593,104 | 298,991,984 | 2,761 |
| cross-tools ARM64 target | 82,213,552 | 382,579,375 | 3,512 |

Userdocs is roughly half the download size and 22–24% smaller after extraction. More importantly, its names encode host and target and release 2628 supplies true `aarch64-aarch64-linux-musl`; inspection of the compared cross-tools `aarch64-unknown-linux-musl` compiler executable showed an x86-64 ELF host binary. That archive targets ARM64 but cannot execute on the native ARM64 runner. Sanitizers are intentionally outside this pinned musl toolchain contract, so their presence does not affect provider selection.

The pinned native lane uses `-std=gnu++26`. Doctor probes every required language, reflection, and libstdc++ facility rather than claiming full conformance from the compiler version alone. `dbg` uses symbols, assertions, low optimization, warnings, and frame pointers. `opt` enables optimization. Sanitizers are excluded from both profiles. Neither profile uses `-fhardened`; hardening flags are selected individually, and automatic-variable initialization mistakes are addressed with warnings and static analysis rather than unconditional zero-initialization cost.

## C++ Build Graph

```text
cpp/cpp.toml + cpp/**/build.toml
  -> parser + validator
  -> resolved compile-action IR
      -> build/cpp/<arch>/<profile>/build.ninja
      -> build/cpp/<arch>/<profile>/compile_commands.json
      -> root compile_commands.json for the active dev profile
      -> test inventory
      -> benchmark inventory
      -> package staging inventory

python/lib/*/*.{c,cc}
  -> exact pinned-interpreter extension compile actions
      -> build/python/<arch>/compile_commands.json
      -> merged root compile_commands.json
```

Every command that may compile C or C++ refreshes its language-owned compilation-database fragment and merges that ownership slice into the root database. Native C++ actions come from the same objects that generate Ninja; Python extension actions are captured from the exact command executed against the pinned interpreter ABI. A successful build with a stale or incomplete database is a build-system defect.

The root C++ manifest owns toolchain and profile policy only. Component-local
`build.toml` files own targets, tests, and adapters; local paths resolve from the
component directory and `//` paths resolve from `cpp/`. Discovery produces one
validated action graph, avoiding a global target-manifest edit bottleneck.

C++ unit tests use pinned doctest as a header artifact. One runner translation unit defines doctest's implementation and `main`, producing one object per target/profile. Every declared test executable links that shared object with its own test translation units and dependencies; the generated test inventory drives `cpp-test`, so adding a test target does not require shell orchestration changes.

The canonical database includes first-party, generated, vendored, and external third-party translation units. `--first-party-only` is an explicit diagnostic derivative, never the canonical database. Complete database coverage does not imply that ordinary lint runs policy checks over all upstream source; `lint --third-party` does that explicitly.

Every dependency adapter must export or capture its executed compile actions as a normalized fragment. The C++ graph importer canonicalizes paths, arguments, target triplets, and duplicates before merging the fragment into the same compile-action IR. Verification requires the adapter's executed translation-unit set, normalized imported set, and canonical compilation-database set to agree.

## C/C++ Third-Party Dependencies

Dependencies use either committed vendored source or immutable locked archives. Supported adapters are deliberately narrow:

```text
header-only | native-manifest | cmake | meson | autotools | custom
```

Prefer direct native-manifest compilation for small stable source sets. Use upstream-supported configuration for complex dependencies. CMake and similar tools are optional adapters installed only when enabled dependencies require them; they are not the first-party build system.

Dependency build identity includes source digest, patch digests, adapter revision, options, target, profile, toolchain digest, and dependency digests. Builds install into a private build prefix. Release builds never use undeclared host libraries.

## Go Lane

The research baseline is Go 1.26.5 from official assets. Set `GOTOOLCHAIN=local` and repo-local `GOROOT`, `GOMODCACHE`, `GOCACHE`, and `GOPATH` paths.

Green Tea GC is already the Go 1.26 default. Do not set `GOEXPERIMENT=greenteagc`. `jsonv2` remains experimental and outside Go 1 compatibility; test and benchmark it in an explicit profile rather than globally enabling it.

Default portable binaries use `CGO_ENABLED=0`, `-trimpath`, disabled VCS stamping, and measured stripping flags. CGO outputs enter the same dynamic-library closure analysis as C/C++.

## Python Lane

The default practical runtimes come from the upstream `python-build-standalone` release `20260610`:

- `cpython-3.14.6+20260610-x86_64-unknown-linux-musl-install_only_stripped.tar.gz` — approximately 27.6 MiB compressed;
- `cpython-3.14.6+20260610-aarch64-unknown-linux-musl-install_only_stripped.tar.gz` — approximately 27.8 MiB compressed.

`tools.lock.toml` pins each GitHub release asset and its upstream digest. Bootstrap measures installed size independently for each architecture and verifies the interpreter, ELF identity, standard-library imports, and a trivial extension built and loaded with the same GCC+musl toolchain.

These PBS musl interpreters request an absolute musl program interpreter and do not execute directly on a stock glibc Ubuntu host. The `runtime-python` package therefore includes the exact loader and libc closure from the matching pinned GCC+musl toolchain. Generated launchers invoke that bundled loader with an explicit private library path; package smoke tests run through the launcher, never by assuming `/lib/ld-musl-*.so.1` exists on the host.

The normal standalone asset does not advertise JIT support. Treat it as non-JIT unless `sys._jit.is_available()` proves otherwise for the exact pinned artifact. A source-built CPython `--enable-experimental-jit=yes-off` runtime is an optional experimental variant, disabled by default and benchmarked separately.

All extensions build against the exact resolved runtime release, libc, SOABI, and `EXT_SUFFIX`. A package refers to one compatible Python runtime identity rather than embedding an implicit interpreter. Application modules install into a package-owned module tree and launch through the resolved runtime in isolated mode. Runtime pruning belongs to the runtime package and is followed by declared import and consumer tests.

## Deno TypeScript And React

One pinned Deno asset supplies the runtime, bundled TypeScript checker, LSP, formatter, linter, tests, coverage, benchmarks, and local execution. `deno --version` is recorded in the tool stamp, including the bundled TypeScript version. Do not float a second TypeScript checker for ordinary Deno validation.

Use a root Deno workspace with strict common compiler options and separate runtime/browser member libraries. Use TSConfig only for external tooling such as Vite when needed; avoid duplicated settings or generate a compatibility TSConfig from one policy source.

Deno owns dependency installation, checking, formatting, linting, tests, coverage, and tasks. Vite owns React 19 development and production browser bundles. Browser assets require no packaged JavaScript runtime.

Local TypeScript programs share one independently released Deno runtime per resolved deployment closure. `deno compile` remains an opt-in standalone artifact because a minimal compiled program embeds roughly 70 MiB of runtime. Multi-entrypoint staging uses application modules plus least-privilege launchers referencing the resolved runtime.

## Code Generation

Schema source is the only hand-edited truth. A parser, validator, stable IR, and deterministic emitters generate C/C++, Go, Python, and TypeScript outputs. Compatibility, IDs, codecs, owners, and generated roots are declared per schema island. A freshness gate regenerates into a temporary tree and compares results.

Generated native outputs are real Ninja dependencies and appear in the complete compilation database. Cross-runtime fixtures validate consumer-visible values and canonical bytes where applicable.

## Packaging

`repo.sh package --package <name>` produces one independently versioned installation package:

```toml
[[package]]
name = "gateway"
version = "1.4.0"
changelog = "packages/gateway/CHANGELOG.md"
executables = ["gateway", "gateway-admin"]
runtime = "python"
runtime_version = "^3.14"
targets = ["x86_64-linux-musl", "aarch64-linux-musl"]

[[package]]
name = "schema-cli"
version = "0.8.2"
changelog = "packages/schema-cli/CHANGELOG.md"
executables = ["schema-cli"]
targets = ["x86_64-linux-musl", "aarch64-linux-musl"]
```

Every package entry owns its version, changelog, executables, runtime requirement, compatibility policy, and release assets. `targets` is an eligibility allow-list; a build selects only the native architecture's musl triplet. Runtimes are ordinary independently versioned package entries, conventionally named `runtime-python` and `runtime-deno`, selected by runtime name, compatibility constraint, and resolved output triplet. They use the same `packages/<name>/v<version>` release protocol and workflow as application packages. `repo.sh package --version` rejects a version that differs from the manifest. A release tag must agree with that package entry; unrelated application and runtime versions do not change. An application-package release may additionally mirror an already released compatible runtime asset with the identical runtime version, digest, signature, and provenance identity when consumers need an atomic download set; the runtime release remains canonical.

```text
<package>/
├── bin/                 C/C++/Go binaries and generated launchers
├── lib/native/          private dynamic-library closure
├── runtime-ref.json     resolved separately released runtime identity
├── app/python/
├── app/deno/
├── app/web/             Vite output, no runtime
├── include/
├── share/
├── manifest.json
├── VERSION
└── LICENSES/
```

All files come from declared build outputs and one normalized staging plan. The packager resolves ELF closure, validates relative runtime lookup paths, normalizes modes/ownership/timestamps/order, generates hashes and license inventory, and rejects source/build/cache/credential leakage.

The canonical payload is deterministic `tar.zst`. A pinned repository-local Zstandard tool streams deterministic tar directly into multithreaded compression. The default level is selected at the measured ratio/time knee; `--compact` uses a slower dense profile. A sealed stage digest keys an immutable package cache so unchanged packaging is near-instant.

Because host Zstandard is not guaranteed, releases also provide a static extractor or self-extracting `.run` artifact. The package plus its resolved runtime artifacts are unpacked into a clean temporary root and all entrypoints are tested with host Deno, Python, Go, compiler, source tree, and network unavailable.

## AI Layout

`.agents/md/overview.md` is the complete repository operating guide. `AGENTS.md` is a portable relative symlink to it. The overview names every repository-owned skill and tells agents to load only the smallest relevant set.

`.agents/skills/` is canonical and tracked. Vendor discovery links under `.codex/` or `.claude/` are created locally when required. Sessions, auth, caches, plugin state, and runtime databases are never committed.

Suggested initial skills:

- `manage-polyglot-monorepo`
- `build-hermetic-c`
- `build-deno-typescript`
- `package-versioned-artifact`
- `generate-cross-language-schema`
- `maintain-linting`
- `benchmark-regressions`

## Ignore Policy

```gitignore
/.local/
/build/
/dist/
/compile_commands.json

/.moon/cache/
/.moon/docker/
/.moon/toolchain/
/.moon/*.log

/.agents/state/
/.agents/cache/
/.agents/tmp/
/.agents/logs/
/.agents/sessions/

/.claude/
/.codex/
/.grok/
/.omx/
```

Track `.moon/workspace.yml`, `.moon/tasks/**`, `.agents/md/**`, `.agents/skills/**`, roles, and templates. Validate the exact generated moon roots against the pinned version, and require a clean worktree after CI commands.

## Benchmarking

Benchmark builds may be cached; benchmark execution is never cached. All lanes normalize into a common JSON envelope containing suite, toolchain digest, Git state, host fingerprint, units, direction, samples, median, and tail statistics.

PR smoke gates catch only gross regressions. Stable dedicated runners enforce tighter thresholds. Baseline updates are explicit and reviewable. Debug, coverage, and cache-population work never enters benchmark results; any future instrumentation lane is likewise excluded.

## Implementation Phases

1. Contracts: commands, directories, lock schema, offline, package, benchmark, AI overview.
2. Bootstrap and doctor: transactional installer, stamps, candidate updates, capability probes, temporary-home tests.
3. Native lane: manifest IR, Ninja, complete compdb, third-party adapters, lint, tests.
4. Expand the implemented Go, Python, and Deno examples into product-specific application lanes.
5. moon aggregation with coarse task inputs/outputs and affected execution.
6. Schema generation and cross-runtime fixtures.
7. Runtime-deduplicated packaging, deterministic compression, extractor, and consumer tests.
8. Benchmark normalization, baseline comparator, CI cache, and release validation.

## GitHub CI And Releases

The template installs the workflow documented in [CI-RELEASE.md](CI-RELEASE.md). Pull requests and main pushes restore toolchains from exact lock, bootstrap implementation, and target-keyed caches; run live then offline bootstrap; run `doctor --deep` and `repo.sh ci`; build and execute both C++ profiles; exercise and test pinned Python through musl; and check, lint, and test `ts/` and `tsweb/` with pinned Deno. An annotated `packages/<name>/v<version>` tag will select one independently versioned package once tag publication is enabled. GitHub workflow YAML never owns compilation, version calculation, release-note generation, or package staging logic.

## Completion Gate

The template is complete only when a fresh clone bootstraps verified pins, the second bootstrap is network-free, normal work succeeds with network denied, native builds always refresh the complete compdb, clangd consumes first- and third-party entries, codegen stays fresh, each resolved deployment closure contains one exact compatible runtime artifact per runtime identity, application packages do not embed duplicate runtimes, package consumers need no host runtimes, identical inputs yield identical archives, and CI leaves the worktree clean.

## Implemented Bootstrap Slice

The repository currently implements target resolution, transactional fail-closed bootstrap, doctor, exact x64/ARM64 tool pins, deterministic C++ Ninja/compdb generation, language-owned `lib`, `app`, and `test` roots, a pinned-ABI Python C++ extension, pure-Go verification, checked local TypeScript, a pinned React 19/Vite production build, runtime-closure validation, a real self-contained multi-language application package, clean-extraction consumer smoke, package-specific release notes, and tag-triggered separate x64/ARM64 GitHub Release assets. Sanitizers and a custom compiler fork are not requirements of this template.
