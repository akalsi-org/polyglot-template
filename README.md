# Polyglot Template Bootstrap Slice

This directory is the executable vertical slice associated with the parent decision package. It establishes the public command surface, native target policy, locked bootstrap model, package/runtime closure schema, and native build-graph generation without implementing fleet deployment.

```bash
./repo.sh help
./repo.sh lint
./repo.sh test
```

CPython 3.14.6, CPU-native GCC 16.1+musl, bundled mold 2.41, and Ninja 1.13.1 are pinned for x64 and ARM64 using immutable upstream URLs and SHA-256 values verified from the official release bytes. The Python artifacts are dynamically linked musl programs, so launchers invoke the exact pinned loader/libc closure explicitly on glibc hosts.

The userdocs compiler source is intentional rather than historical accident. Against cross-tools release `20260515`, userdocs release `2628` is about half the compressed download and 22–24% smaller unpacked. Its ARM archive also contains an ARM64-hosted compiler suitable for `ubuntu-24.04-arm`; the cross-tools ARM64-target archive inspected during selection contains an x86-64-hosted compiler. See the parent architecture document for the measured table.

The upstream userdocs toolchains are configured with `--disable-libsanitizer`, and this template deliberately leaves sanitizers outside its musl toolchain contract. The usable `dbg` profile provides symbols, assertions, low optimization, warnings, and frame pointers; `opt` remains the portable release profile. A future sanitizer lane should be an independently evaluated host-debug toolchain rather than a fork requirement for these release artifacts.

The checked-in GitHub workflow is a two-architecture bootstrap preflight: it verifies target resolution, validates the lock, and runs metadata/generator tests. Tag-triggered publishing remains intentionally disabled until package assembly and clean consumer smoke are promoted into the release workflow, but the local release gates already exist.

Implemented commands include target detection, transactional bootstrap, doctor, check-only linting, deterministic native configuration with a refreshed root compilation-database link, pinned native build entrypoints, reflection-probe generation, package/runtime model validation, exact closure resolution, deterministic package assembly, package smoke tests, release checks, release-note extraction, tests, and aggregate preflight CI.
