# CI Packaging And Releases

The checked-in workflow is
[ci-release.yml](../.github/workflows/ci-release.yml). GitHub Actions calls
`./repo.sh`; it does not implement a separate build or packaging path.

## Verification Contract

Pull requests, pushes to `main`, matching release tags, and manual dispatches
run the native x64 and ARM64 matrix. Each job:

1. checks that `./repo.sh target` matches its native runner;
2. restores exact-key caches for the toolchain, Deno dependency graph, and Moon
   task state;
3. runs live bootstrap, offline bootstrap, and `./repo.sh doctor --deep`;
4. runs `./repo.sh lint`, `./repo.sh package-validate`,
   `./repo.sh build`, and `./repo.sh test`;
5. builds and runs both C++ profiles; and
6. exercises the pinned Python runtime.

The cache keys include the target, relevant lock files, and bootstrap or Moon
inputs. A cache hit remains untrusted until bootstrap and doctor revalidate it.

## Release Tags

A pushed ref matching `packages/<name>/v*` triggers the release path. The
workflow derives `<name>` from that ref, then executes:

```bash
./repo.sh package <name> opt
./repo.sh release-check <name> packages/<name>/v<version>
```

For each native target, package output is a top-level file:

```text
dist/<name>-<version>-<target>.tar.gz
dist/<name>-<version>-<target>.tar.gz.sha256
```

The workflow uploads those verified files as cross-job artifacts. After both
targets pass, the release job downloads them and runs:

```bash
./repo.sh release-notes packages/<name>/v<version>
gh release create packages/<name>/v<version> artifacts/* --verify-tag
```

The release notes are the matching section from the repository-root
`CHANGELOG.md`. Package versions and target eligibility come from
`package.toml`; runtime closure validation comes from
`runtime-resolution.lock.toml`.

## Current Boundaries

- The workflow is triggered by the tag-name pattern. It does not currently
  require an annotated or signed tag, nor inspect tag-object metadata.
- It uploads gzip tar archives and sidecar SHA-256 files. It does not publish
  Zstandard archives, static extractors, or self-extracting installers.
- The workflow has read-only default permissions; the release job adds
  `contents: write`. It does not currently generate provenance attestations or
  use `id-token` or `attestations` permissions.
- `gh release create --verify-tag` confirms that the tag exists remotely. It
  does not make an existing release mutable.

## Release Procedure

1. Commit the package version and its matching root changelog section.
2. Create and push a `packages/<name>/v<version>` tag.
3. Let the native matrix assemble and smoke-check the target archives.
4. The release job publishes the target archives, checksum sidecars, and
   changelog-derived notes.

Do not upload workstation-built artifacts to a GitHub Release.
