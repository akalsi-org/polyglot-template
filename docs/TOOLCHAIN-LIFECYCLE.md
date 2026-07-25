# Toolchain Lifecycle

`tools.lock.toml` is the reviewed source of truth for bootstrap provenance.
`toolchains/lock.bzl` is its generated Buck2 projection. Toolchain changes are
source changes: update them on a branch, review the immutable URL, archive
layout, expected artifact, and SHA-256 for every supported target, then qualify
the complete closure before merging.

## Update And Qualification

1. Edit `tools.lock.toml`. Use an immutable release asset URL; never use a
   mutable `latest` reference. Keep the exact 64-character SHA-256 and the
   archive layout/expected-path metadata needed by bootstrap's capability
   probes. Update both Linux targets unless a tool is deliberately
   target-specific.
2. Run `./repo.sh toolchain-lock` to regenerate `toolchains/lock.bzl`. Do not
   edit the generated file by hand. Use `./repo.sh toolchain-lock --check` in
   review or before handoff to prove it is current.
3. Run `./repo.sh toolchain-qualify`. It regenerates the projection, atomically
   reinstalls every locked artifact, runs the bootstrap capability probes, and
   finishes with `doctor --deep`. This is the required online candidate check;
   any needed download is performed by `bootstrap`, the only command permitted
   to fetch toolchain artifacts.
4. Run the affected lane checks and the normal repository verification. An
   existing local installation is not qualification evidence: `--repair` is
   intentional so the candidate is extracted and probed rather than accepted
   only because its install stamp already exists.
5. For a cache-complete, no-network replay, run
   `./repo.sh toolchain-qualify --offline`. It must fail on an absent or
   checksum-mismatched archive instead of silently reaching the network.

`toolchain-lock` uses the host's `python3` only to generate/check the derived
Starlark lock before the repository's Python toolchain can exist. It never
fetches, builds, or runs a product lane. All artifact installation and probing
remain behind `bootstrap` and the pinned artifact metadata.

## Worktrees Share One `.local`

A linked `git worktree` does not bootstrap its own toolchain. `repo.sh`,
`toolchain/bootstrap.sh`, and `toolchain/doctor.sh` all resolve the directory
through `toolchain/localdir.sh`, which points a linked worktree at the main
worktree's `.local` and creates `<worktree>/.local` as a symlink to it. A
worktree is therefore usable immediately, with no second download and no
second 890M toolchain install.

The symlink is not cosmetic. `rules/toolchain.bzl`'s staging action reads
`.local/downloads/<sha256>-<archive>` as a path relative to the buck2 project
root, so the toolchain must be reachable *at* `<root>/.local` no matter where
it physically lives - setting `POLYGLOT_LOCAL_DIR` alone moves `repo.sh`'s
view but not the graph's, and every `stage_toolchain_archive` action then
fails with "run ./repo.sh bootstrap" even though bootstrap has run.

Two consequences worth knowing:

- Worktrees sharing a `.local` must agree on `tools.lock.toml`. `<local>/bin`'s
  wrappers are regenerated from that lock on every `setup_environment`, so a
  worktree on a toolchain-bump branch rewrites them for the others, and
  concurrent `./repo.sh` runs across worktrees race on that directory.
- An explicit `POLYGLOT_LOCAL_DIR` is honoured verbatim and no symlink is
  created, which is what a throwaway toolchain (`test/bootstrap-smoke.sh`)
  needs. Set it for a worktree that must not share.

If `<worktree>/.local` already exists as a real directory, resolution fails
closed rather than replacing somebody's installed toolchain.

## Recovery And Rollback

Use the public commands; do not patch `.local/toolchain`, `.local/downloads`,
or generated wrapper scripts by hand.

- If `doctor --deep` reports a missing or damaged installed artifact, run
  `./repo.sh bootstrap --repair`, then `./repo.sh doctor --deep`. `--repair`
  extracts each verified archive into a temporary directory, runs the same
  capability probe as first installation, and replaces the old installation
  only after that succeeds.
- Add `--offline` when the verified download cache is expected to be complete.
  A cache miss or digest mismatch is a recovery failure, not permission to use
  an unverified local artifact.
- For an online recovery, `bootstrap` discards only a cache file whose digest
  disagrees with the current lock, then downloads and verifies a replacement.
  Offline recovery fails closed on that condition. Do not bypass the digest
  check or substitute an unpinned binary.
- To roll back a bad candidate, revert `tools.lock.toml` and the matching
  generated `toolchains/lock.bzl` together, then run
  `./repo.sh toolchain-qualify` (or add `--offline` when the older archive is
  cached). The lock revision, not a mutable artifact name or a directory in
  `.local`, defines the rollback state.

The lifecycle commands only support the repository's declared Linux x86-64 and
Linux ARM64 native targets. A successful local qualification does not replace
CI coverage for the other target.
