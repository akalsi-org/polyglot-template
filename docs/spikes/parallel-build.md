# Parallel Build And Manifest Scalability Spike

Status: implemented.

## Question

Can the repository scale beyond one global C++ TOML file and run independent
language work concurrently without introducing hidden toolchains, output races,
or unbounded nested parallelism?

## Findings

- Moon 2.4.3 is a native Rust binary with musl releases for x64 and ARM64. It
  schedules dependency DAGs concurrently, bounds concurrency, hashes explicit
  inputs, caches declared outputs, and reports per-action timings.
- Language-native engines should remain below the coarse scheduler: Ninja owns
  C++, Go owns Go, Deno owns TypeScript, and Vite owns browser assets.
- The former compilation-database update was unsafe under parallel C++ and
  Python builds because each lane read and rewrote the root file.
- One global `cpp.toml` would create an ownership and merge bottleneck as target
  count grows. Component manifests are sufficient; a programmable build DSL is
  not yet justified.
- Blindly giving every nested tool all host CPUs oversubscribes the machine.

## Implemented Decision

1. `cpp/cpp.toml` owns only toolchain and profile policy.
2. Components own nearby `build.toml` files. Paths are component-relative;
   `//` denotes the C++ root. The generator discovers, normalizes, validates,
   and merges them into one action graph.
3. C++ and Python write atomic language-owned compilation-database fragments.
   A locked, atomic final action derives root `compile_commands.json` only from
   declared fragments. It never uses root read/modify/write state.
4. Pinned Moon schedules coarse build and test DAGs. Its proto, Wasmtime, global,
   and task caches all live under ignored repository-local paths.
5. `POLYGLOT_JOBS` is the total host budget. Moon receives at most five coarse
   lanes; nested Ninja and Go work receive a divided budget.
6. Test tasks depend on their build tasks in the DAG. Direct language test
   commands remain self-contained when invoked without Moon.
7. Moon output caching is enabled only where artifacts are location-independent.
   C++ and Python native outputs remain under their native incremental engines;
   absolute compilation-database paths are regenerated locally.

## Evidence

On the spike host with `POLYGLOT_JOBS=4`, the first aggregate build ran five
language lanes concurrently and completed with C++ as the critical path. After
disabling location-sensitive native artifact caching, an unchanged build took
684 ms: TypeScript, web, and Go were cache hits while Ninja reported no C++
work and Python refreshed its native extension and compilation database. The
parallel test DAG completed in 1.4 seconds, and the full lint/build/test/package
validation DAG completed in 1.5 seconds. Eight concurrent compilation-database
merge processes repeatedly produced valid, complete JSON.

These timings are evidence for the spike host, not universal performance claims.
Moon's detailed summary is the continuing critical-path report for every
aggregate build and test.

## Falsifiers And Next Thresholds

- Split the root project into multiple Moon projects only when affected-task
  selection or team ownership needs project-level boundaries.
- Add canonical cross-component target labels when two components need reusable
  C++ library dependency edges beyond the current adapter model.
- Add ccache only after measured C++ rebuild cost dominates and its pinned cache
  keys can include the exact compiler, flags, and headers.
- Introduce remote task caching only when CI reuse justifies operating it.
