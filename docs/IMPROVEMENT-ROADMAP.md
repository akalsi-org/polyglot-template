# Improvement Roadmap

This roadmap turns the July 2026 human-use, agent-operations, and systems
architecture audits into executable work. It distinguishes verified current
behavior from future deployment proposals and orders work by correctness,
delivery risk, and dependency.

## Success criteria

- Every declared application package either builds, smokes, and releases through
  the documented command, or fails before a release tag can be accepted.
- Editor commands use the pinned tools and diagnose a missing bootstrap or language-tool bootstrap directly.
- The target graph, not duplicated inventories or rendered command strings,
  owns dependency closure and editor metadata.
- Deployment commands remain unavailable until their planning, receipt,
  activation, rollback, and recovery contracts are executable and tested.

## Phase 0 — Correctness and user feedback

Status: complete for the retained language lanes and profile contract.

| Item | Outcome | Evidence |
| --- | --- | --- |
| Deno editor cache | The language server uses the frozen repository cache and resolves locked npm types. | Pinned `deno check` plus editor contract. |
| Profile contract | Complete: docs, wrappers, and package builds use `--target-platforms` for `opt`. | Opt artifact/configuration test. |

## Phase 1 — Package and release truth

Status: the canonical catalog, in-graph target gate, target-native package and
smoke path, idempotent publication, checksums, GitHub artifact attestations, and
SBOM/provenance sidecar publication are complete. Release-tag signature trust
remains outside repository ownership.

1. Complete: `packages/catalog.bzl` defines the package lifecycle inputs,
   runtime closure, archive identity, smoke contract, and release tag.
2. Complete as a release safety gate: tagged CI requires an in-graph
   `package()` target and rejects catalog-only applications; there is no raw
   build-directory fallback. Migrating a catalog-only declaration is separate
   application work.
3. Complete for releasable packages: the tag workflow runs package and smoke
   commands on each supported native target.
4. Complete: immutable tag publication does not auto-cancel, reconciles an
   existing release, and uploads only verified missing assets.
5. Complete: checksums and GitHub artifact attestations bind each target archive
   to the workflow, and `tools/release_evidence.py` writes the deterministic
   `.sbom.json` and `.provenance.json` sidecars that `package-release.yml`
   publishes with the archives and `release-manifest.json`. Remaining: release
   tags must be signed and the public-key trust root configured on the release
   host/organization, which repository code cannot declare.

## Phase 2 — Developer and agent workflow

Status: the bootstrap-first README Quick Start and capability matrix are
complete. The remaining editor diagnostics and generated command reference can
proceed independently after Phase 0's editor-path contract settles.

1. Complete: README has a bootstrap-first Quick Start and points to the current
   capability matrix, including Linux host scope and raw Go/Deno network bounds.
2. Partial: `.vscode/tasks.json` covers bootstrap, editor-prerequisite
   verification, running the active Python file, Deno type check, React site build, and Go test. Still
   missing tasks for `doctor`, `ts-test`, and
   `tsweb-test`, and there are no launch configurations.
3. Not started: `editor-sync` and `doctor --editor` do not exist in `repo.sh`.
   Provide them with actionable diagnostics for tool wrappers, Deno cache, and interpreter path.
4. Complete: `./repo.sh package-list` and `./repo.sh package-explain <name>`
   provide package discovery and explanation, and
   [TROUBLESHOOTING.md](TROUBLESHOOTING.md) covers bootstrap, Buck daemon,
   caches, and language tools.
5. Generate command reference material from `repo.sh help` rather than
   duplicating it across prose documents.

## Phase 3 — Graph simplification and performance

These require design review because they change internal build-model ownership.

1. Replace the manual root Deno cache inventory with provider/BXL discovery;
   derive the bootstrap seed from the same checked-in closure.
2. Make the locked Deno closure a tracked build input rather than ambient cache
   state, preserving offline execution while improving remote-execution and
   developer-host portability.
4. Split rule internals by toolchain resolution, launcher creation, staging,
   coverage, and package-kind handling. Keep public macros small and move
   historical workaround narratives into ADRs and regression tests.
5. Either implement cross compilation or rename the model consistently as
   native-per-architecture builds.

### Buck ownership boundary

Move repository-owned package, runtime-closure, deployment-schema, and build
policy data into Buck/Starlark providers. Keep only formats that an external
consumer must parse: `deno.json`/`deno.lock` for Deno, GitHub Actions YAML for
GitHub, and the bootstrap artifact trust lock until a pre-Buck bootstrapper can
read an equally auditable replacement. Do not replace those external protocols
with generated mirrors.

Completed: `packages/catalog.bzl` supplies every Buck package target and the
release-facing catalog. `package.toml` and `runtime-resolution.lock.toml` are
retired; `repo.sh`, CI, and release verification consume the Buck-owned catalog.

## Phase 4 — Deployment foundations

The deployment and fleet documents are proposals, not current commands. Build
only this sequence:

1. Typed inventory, desired-state, receipt, and artifact/provenance schemas.
2. Read-only `deploy plan`, `status`, and `verify` with deterministic receipts.
3. One-host installation, activation, rollback, and interrupted-operation tests.
4. Multi-host scheduling, routing, certificate rotation, drift monitoring, and
   recovery drills.

No `deploy apply` command is exposed before the immediately preceding phase has
an executable gate and fault-injection coverage.

## Parallelism map

```text
Phase 0: editor workflow ─────────┐
                                  ├──> Phase 2 workflow hardening
         package/release truth ───┼──> Phase 1 release integrity
         profile normalization ───┘

Phase 1 package contract ─────────┐
Phase 3 Deno internals ────┼──> Phase 4 deployment schemas and gates
Phase 2 diagnostics ──────────────┘
```

The integration gates are the package lifecycle contract, the target/profile
contract, and a clean targeted-to-full verification sequence. They prevent
parallel changes from creating a second source of truth.
