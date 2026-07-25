# Repository Operating Guide

This repository is a pinned, offline-after-bootstrap polyglot workspace. Use `./repo.sh` for all documented operations; language tools and generated files are implementation details behind that interface.

## Invariants

- The current CPU architecture selects the build architecture. Native artifacts use the pinned GCC+musl ABI and never cross CPU architectures.
- `tools.lock.toml` owns tool provenance. Missing or placeholder digests fail closed.
- `packages/catalog.bzl` is the sole source of package declarations and exact runtime closure resolution.
- Buck2 is the sole scheduler; there is no separate task runner. `BUCK` files plus `config/flags.bzl` own C++ targets and profile (`dbg`/`opt`) flags — `cpp/cpp.toml` no longer exists. Every first-party rule lives under `rules/` (no prelude in this repo). `compile_commands.json` is materialized by `./repo.sh compile-commands` from a BXL compdb query (`bxl/compdb.bxl`) over the buck2 action graph, not hand-generated.
- Each language owns `lib/<name>/`, `app/<name>/`, and `test/`; its `lib/` directory is an import/include root.
- `.vscode/settings.json` mirrors repository discovery: clangd uses the root compdb, Deno owns `ts/` and `tsweb/`, and Python analysis includes `python/lib` plus `python/app`.
- Python native extensions build against the exact pinned interpreter ABI with pinned GCC/musl; pure Go uses the pinned repo-local toolchain with `CGO_ENABLED=0` and isolated caches.
- Normal build, test, lint, and package operations do not fetch.
- Build products live under `build/`, release products under `dist/`, and toolchains/caches under ignored `.local/`.

## Commands

Run `./repo.sh help` for the canonical command list. Start with:

```bash
./repo.sh target
./repo.sh bootstrap --dry-run
./repo.sh bootstrap
./repo.sh doctor --deep
./repo.sh test
```

This checkout supports Linux x86-64 and Linux ARM64 hosts only, producing the corresponding CPU-native musl target. Bootstrap must complete before normal development commands; afterward the named Buck-backed commands are offline. The raw `./repo.sh go <args...>` and `./repo.sh deno <args...>` passthroughs deliberately preserve the underlying tool's behavior, so caller-supplied commands may use the network. Use `go-*`, `ts-*`, and `tsweb-*` for the repository's offline-after-bootstrap contract.

Lane commands are thin wrappers over Buck2: `build [dbg|opt]` builds every lane's primary outputs discovered by rule kind (no hand-listed //:build group), `coverage` merges dbg coverage via bxl/coverage.bxl's same rule-kind discovery, `test` runs `buck2 test //...`, `lint` runs `buck2 test //... --labels lint` plus shell/Python infra checks (`infra-lint`). `cpp-build`/`cpp-run`/`cpp-test`, `python-build`/`python-test`, `ts-build`/`ts-test`, `tsweb-build`/`tsweb-test`, and `go-build`/`go-test` each drive their lane's Buck2 targets and only ever use the pinned toolchain, never host compilers or runtimes. `./repo.sh buck2 [args...]` runs the pinned Buck2 binary directly for anything not covered by a named lane command. CI performs real cached bootstrap, deep doctor, language-lane verification, and a cold-buck-out full-graph offline replay inside a no-network namespace, on x64 and ARM64.

## TL;DR: Effective Daily Workflow And Style

1. Start with `./repo.sh bootstrap --dry-run`, then `./repo.sh bootstrap` and `./repo.sh doctor --deep`. Bootstrap installs the pinned closure; named Buck-backed commands do not fetch afterward. Do not treat raw `go` or `deno` passthrough commands as a network restriction.
2. Use `./repo.sh format` to apply formatting and `./repo.sh format --check` before handoff. `./repo.sh lint` includes the non-mutating format check.
3. Use the lane test while iterating (`cpp-test`, `python-test`, `ts-test`, `tsweb-test`, or `go-test`), then `./repo.sh test` for a cross-lane change. Run `./repo.sh coverage` when changing executed behavior.
4. Use `./repo.sh build [dbg|opt]` rather than hand-maintaining aggregate build lists; it discovers primary rule kinds automatically. For an ad hoc graph question, use `./repo.sh buck2 ...`.
5. Never edit `.local/`, `build/`, `dist/`, `buck-out/`, generated `compile_commands.json`, or toolchain wrappers as source. Aggregate `build` and `test` refresh the matching-profile compilation database; use `./repo.sh compile-commands [dbg|opt]` after a clean when an editor-only refresh is needed.

Code style is two spaces for repository-authored code, including Python, Starlark, C/C++, shell, JSON, and TOML. Python and Starlark block nesting is enforced at two spaces; C/C++ editors use `.clang-format` with two-space normal and continuation indents. Deno formats TypeScript with two spaces. Go is the sole syntax-level exception: it stays `gofmt`-canonical with tabs, rendered at width two by `.editorconfig` and VS Code. Do not hand-align generated-looking text, use tabs outside Go/Makefiles, or rely on host formatters; use `./repo.sh format`.

## Adding Targets

Keep new code inside its lane: `cpp/{lib,app,test}`, `python/{lib,app,test}`, `go/{lib,app,test}`, `ts/{lib,app,test}`, or `tsweb/{lib,app,test}`. Put the target in the nearest `BUCK` file and copy the closest existing target before inventing attributes. First-party rule APIs, not a prelude or host build tool, are the contract:

- C++: load `cxx_library`, `cxx_binary`, and `cxx_test` from `//rules:cxx.bzl`; public headers live below `cpp/lib` and are included by their logical `cpp/lib`-relative path. Give runnable binaries a stable `pkg_name` when they will be packaged.
- Python: load `py_library`, `py_extension`, `py_binary`, and `py_test` from `//rules:python.bzl`. `py_test` defaults discovery to its own package, uses readable top-level `test_*` functions (including `async def`), and participates in dbg Python coverage. Native extensions use the pinned CPython ABI; C extensions declare `language = "c"`, and C++ header dependencies use `cxx_deps`. Keep import roots and `package` names explicit.
- Go: load `go_library`, `go_binary`, and `go_test` from `//rules:go.bzl`. Keep `CGO_ENABLED=0`; give binaries a `pkg_name` for packaging. A test that invokes `main()` depends on its sibling binary target so Buck tracks `main.go`.
- Deno/TypeScript: load `deno_library`, `deno_app`, `deno_check`, `deno_test`, or `vite_build` from `//rules:deno.bzl`. List every source in `srcs` and every source closure in `deps`; apps must list `main` in `srcs`. Add every new `deno_library` or `deno_app` to root `//:deno-cache`'s `deps`, or `./repo.sh lint` fails closed.
- Tests: add a native test rule, not an untracked script. The test macros are discoverable by `./repo.sh test` and dbg coverage automatically. Use `--target-platforms //config:<native-target>-opt` when an opt-only test is needed; `-m` does not override these rules' default target platform.

After adding a target, run its lane command, `./repo.sh lint`, and `./repo.sh test`; aggregate build/test refresh the matching-profile compilation database. Use `./repo.sh compile-commands [dbg|opt]` only when an editor-only refresh is needed. Do not add a hand-maintained root build/test group: rule-kind discovery owns aggregate participation.

### Adding A `test/` File — Is It Self-Referential?

`./repo.sh init-project` walks a fork through pruning this template's own
self-referential tests (`tools/init_project.py`'s `TEST_STRIP_CANDIDATES`):
files whose assertions are specific to THIS repository's own docs, CI YAML, or
demo catalog/example code, rather than testing reusable Buck2 rule/tooling
behavior. That list is not auto-discovered from `test/` — nothing scans for
new files, so a new self-referential test left off it silently ships into
every future fork and rots there, unpruned and eventually failing for reasons
that have nothing to do with the fork's own project.

When adding a file under `test/`, ask: does this assert exact strings from
this repo's own prose/CI, or does it only pass because of the demo
catalog/example code (`polyglot-demo`, `gateway`, the `go-cmp` import in
`go/test/greeting_test.go`)? If so, add a `TestStripCandidate` entry in
`tools/init_project.py` alongside the existing ones — same file, same
docstring's MAINTAINER NOTE. If the file tests a Buck2 rule or tool
mechanism generically (deriving its expectations from the tree rather than
hardcoding this repo's identity, the way `deno-manifest-contract.sh` does),
it does not belong on that list. `graph-compdb-contract.sh` is the in-between
case: it exercises the BXL compdb generically but names four specific graph
compile actions, two of which are demo sources — so it stays off
`TEST_STRIP_CANDIDATES` and is instead edited by the demo-pruning pass below.

### Adding Or Removing Demo Code

`./repo.sh init-project`'s second pass prunes leaf demo code
(`tools/init_project.py`'s `DEMO_STRIP_GROUPS`): the greeting/hello example
library and app in the C++ and Python lanes. Each group carries the exact
`Edit`s its removal requires elsewhere in the graph, so every group leaves
`./repo.sh build`, `test`, `lint`, and `infra-test` passing on its own and in
any combination.

Two rules when touching that list. First, a new file that only exists to give
a lane something that builds belongs to a group, and anything that starts
*referencing* demo code — a BUCK dep list, a catalog entry, a hardcoded path
in a `test/` contract, a prose link in `docs/` — has to be added as an `Edit`
on the owning group, or pruning silently produces a broken graph or dead
links. Second, keep it to LEAF demo code:
whole lanes (`go/`, `ts/`, `tsweb/`) and the demo package entries are
deliberately not candidates, because removing those means rewriting
`repo.sh`'s per-lane verbs, root `//:deno-cache`'s hand-listed closure,
`go/BUCK`'s lint dep lists, `go.mod`/`vendor/`, and
`.github/workflows/verify.yml`.

Template identity that contains no `polyglot-template` needs its own rename
list, because `RENAME_TARGETS` cannot catch it: package names
(`polyglot-demo`), the deployment schema ids (`polyglot.deployment-plan/v1`),
and the `pgt` C++ namespace each have one (`PACKAGE_RENAME_TARGETS`,
`SCHEMA_RENAME_TARGETS`, `NAMESPACE_RENAME_TARGETS`). Those run AFTER pruning
on purpose: the demo groups' edits anchor on the packages' original names, so
renaming first would leave the anchors unmatchable and fail the run closed
partway through.

The KEEP-LIST in that module's docstring records the infrastructure that is
never a candidate. Consult it before assuming a file is demo scaffolding:
`python/test/pyfast_test_ext.c` and friends read like a demo fixture but are
`pyfast.h`'s only test, and `cpp/test/reflection.cc` is
`rules/toolchain.bzl`'s reflection capability probe — deleting `cpp/test/`
breaks the toolchain, not just the tests.

## Packaging

`packages/catalog.bzl` declares package identity/version/executables/runtime and resolved runtime closure. `package()` targets in `packages/BUCK` (`rules/package.bzl`) assemble packages with their own Buck2 target (currently `polyglot-demo`, `polyglot-server`) entirely in-graph from a generic `PackageInfo` contract each lane's rules emit, staging a flattened sibling layout (`bin/`, `lib/`, `runtime/`, plus per-lane roots like `python/`, `ts/`, `web/`) at the package root. `./repo.sh package <name> [dbg|opt]` requires a matching `//packages:<name>` Buck2 target and fails closed for catalog-only declarations (`gateway`, `schema-cli`); it has no raw-build assembly fallback. Each in-graph package's `package_smoke` target (`checks` + optional `smoke_script`) verifies the assembled archive. The current Deno application package support is limited to zero-npm-dependency closures.

### Adding An In-Graph Package

1. Add or update the literal record in `packages/catalog.bzl`: name, semantic version, kind, executable names, supported native targets, runtime requirement when needed, and every required runtime-resolution entry. The catalog is a validated source input, not generated build output.
2. Add `package(name = "<name>", version = "<same version>", deps = [...])` to `packages/BUCK`. Depend on the app/binary/site targets that should stage themselves through their transitive `PackageInfo`; never duplicate file lists in the package rule.
3. Add `package_smoke(name = "<name>-smoke", package = ":<name>", commands = {...})`. Prefer structured `commands` (`"bin/program": ["arg"]`) over legacy raw-shell `checks`. Add `smoke_script` only for packaged web asset validation.
4. A Deno app package currently supports only a zero-npm-dependency closure. Package Python applications through `py_binary` so the package rule can stage the matching runtime and launchers.
5. Run `./repo.sh package-validate`, `./repo.sh package <name> dbg`, and the corresponding smoke target (`./repo.sh buck2 test //packages:<name>-smoke`). Use `./repo.sh package-resolve <name>` to inspect the exact native runtime closure.

Use a manifest-only package only when there is intentionally no Buck target. Do not make `package()` run a second hidden build: packages consume declared Buck outputs and must smoke-test from a clean extraction.

## Agent Skills

Repository-owned skills, when added, belong under `.agents/skills/`. This checkout currently has no repository-owned skills; load only the smallest applicable installed skill set needed for a task.

## Reviews

After any large pass (multi-file feature, migration stage, sweeping cleanup), obtain at least one independent review before committing. Independent means a reviewer that did not write the change. Prefer two independent reviewers for a change that crosses lanes, contracts, or the release path.

Write the review brief to a file and point the reviewer at that path rather than pasting a long task inline. Triage verdicts adversarially: confirm each finding against the source it cites before fixing, and record refuted claims rather than silently dropping them.

If a review cannot start, times out, or returns an infrastructure error, do not represent it as approval. Record the failed reviewer and the reason in the handoff, then run the equivalent local review with whatever tooling is available. A fallback review is useful evidence, but it does not erase the failure.

Which review tool, vendor, or model provides that review is machine-local configuration. It is not a repository contract and must not be pinned here — see PRINCIPLES.md P14.

## Agent Safety And Handoff

- Start every task by inspecting `git status --short` and the relevant diff. Preserve unrelated dirty-tree changes; never reset, clean, checkout, or overwrite files outside the owned scope.
- Do not create or switch worktrees unless the task explicitly requests one. If parallel work is authorized, assign disjoint files or isolate mutating workers in separate worktrees; never let concurrent workers share `buck-out/`, `build/`, `dist/`, or generated files.
- Keep concurrency bounded: parallelize independent read-only reviews, but serialize edits to the same workflow, contract, guide, or generated artifact. A failed worker must not trigger a blind retry that could duplicate changes.
- Destructive commands (`git reset --hard`, `git clean`, broad `rm -rf`, force-push, or deleting user files) are prohibited. Remove only task-owned temporary directories, and inspect before any replacement or deletion.
- End with a structured handoff containing: scope/files changed; preserved pre-existing changes; commands run and exact pass/fail results; unresolved risks or missing targets; reviewer status (including fallback and failure reasons); and the next action for the receiving agent.

## Collaboration Shorthand

`cnp` (also `c+p`) means commit the current task's intended changes and push the current branch. Exclude unrelated, generated, credential-bearing, and untracked state unless the user explicitly includes it.

Never commit authentication state, sessions, caches, downloaded tools, build output, or generated agent-runtime databases.

## Component agent guides

- `cpp/lib/pyfast/AGENTS.md` — pyfast: single-header CPython fastcall/vectorcall scaffolding (known defects, wiring gaps, test plan).
