# Polyglot Template Bootstrap Slice

This repository is the executable vertical slice associated with the decision package. It establishes the public command surface, native target policy, language-owned source layout, locked bootstrap model, package/runtime closure schema, and C++ build-graph generation without implementing fleet deployment.

```bash
./repo.sh help
./repo.sh lint
./repo.sh test
```

CPython 3.14.6, CPU-native GCC 16.1+musl, bundled mold 2.41, Ninja 1.13.1, and Deno 2.9.2 are pinned for x64 and ARM64 using immutable upstream URLs and official SHA-256 values. The Python artifacts and native outputs are dynamically linked musl programs, so repository commands invoke them through the exact pinned loader/libc closure on glibc hosts.

The userdocs compiler source is intentional rather than historical accident. Against cross-tools release `20260515`, userdocs release `2628` is about half the compressed download and 22–24% smaller unpacked. Its ARM archive also contains an ARM64-hosted compiler suitable for `ubuntu-24.04-arm`; the cross-tools ARM64-target archive inspected during selection contains an x86-64-hosted compiler. See the parent architecture document for the measured table.

The upstream userdocs toolchains are configured with `--disable-libsanitizer`, and this template deliberately leaves sanitizers outside its musl toolchain contract. The usable `dbg` profile provides symbols, assertions, low optimization, warnings, and frame pointers; `opt` remains the portable release profile. A future sanitizer lane should be an independently evaluated host-debug toolchain rather than a fork requirement for these release artifacts.

Source is organized under `cpp/`, `python/`, `ts/`, and `tsweb/`. Every language owns `lib/<name>/`, `app/<name>/`, and `test/`. Library roots are configured by the repository commands, so code uses logical names such as `example/example.hh`, `example.example`, `greeting/greeting.ts`, and `title/title.ts` instead of relative traversal.

The checked-in GitHub workflow performs a real native bootstrap on x64 and ARM64. It restores only an exact target/lock/bootstrap-keyed toolchain cache, installs on a miss, proves the second bootstrap succeeds offline, runs deep capability checks, builds and executes both C++ profiles through musl, exercises and tests Python through musl, and checks/lints/tests the local and browser TypeScript lanes with pinned Deno. Tag-triggered publishing remains intentionally disabled until package assembly and clean consumer smoke are promoted into the release workflow, but the local release gates already exist.

Implemented commands include target detection, transactional bootstrap, doctor, check-only linting, deterministic C++ configuration with a refreshed root compilation-database link, pinned `cpp-*`, `python-*`, `ts-*`, and `tsweb-*` checks, reflection-probe generation, package/runtime model validation, exact closure resolution, deterministic package assembly, package smoke tests, release checks, release-note extraction, tests, and aggregate CI.
