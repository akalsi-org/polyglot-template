# CI Packaging And Releases

The checked-in workflow is
[ci-release.yml](../.github/workflows/ci-release.yml). GitHub Actions calls
`./repo.sh`; it does not implement a separate build or packaging path.

## Verification Contract

Pull requests, pushes to `main`, matching release tags, and manual dispatches
run the native x64 and ARM64 matrix. Each job:

1. checks that `./repo.sh target` matches its native runner;
2. restores exact-key caches for the toolchain, Deno dependency graph, and the
   Buck2 toolchain tree (`buck-out/v2/art` + `buck-out/v2/cache`);
3. runs live bootstrap, offline bootstrap, and `./repo.sh doctor --deep`;
4. enables unprivileged user and network namespaces, which the Buck Deno cache
   action requires to enforce offline resolution;
5. builds `//toolchains:native` twice to prove the second build is a zero-network
   cache hit, then runs `buck2 test //...` (the primary gate: every lane's
   build, test, and lint-as-test targets in one pass);
6. runs `./repo.sh lint`, `./repo.sh package-validate`,
   `./repo.sh build`, and `./repo.sh test` (each buck2-backed, as a repo.sh
   command-surface check on top of step 5's direct buck2 invocation);
7. builds and runs both C++ profiles; and
8. exercises the pinned Python runtime.

The cache keys include the target, relevant lock files, and bootstrap or
Buck2-graph inputs. A cache hit remains untrusted until bootstrap and doctor
revalidate it.

## Release Tags

A pushed ref matching `packages/<name>/v*` triggers the release path. The
workflow derives `<name>` from that ref, then executes:

```bash
./repo.sh package <name> opt
./repo.sh release-check <name> packages/<name>/v<version>
```

For each native target, package output includes immutable archive evidence:

```text
dist/<name>-<version>-<target>.tar.gz
dist/<name>-<version>-<target>.tar.gz.sha256
dist/<name>-<version>-<target>.tar.gz.sbom.json
dist/<name>-<version>-<target>.tar.gz.provenance.json
```

`repo.sh package` produces the deterministic SBOM and provenance sidecars after
it verifies the archive's embedded package and closure metadata. Both sidecars
bind their `subject` SHA-256 to the exact archive. The release job checks those
bindings, then writes and publishes the deterministic
`release-manifest.json`, which lists every target archive, checksum, SBOM, and
provenance sidecar.

The verification matrix uploads those verified files as cross-job artifacts and
uses GitHub's `actions/attest` action to create an artifact attestation for each
target archive. After both targets pass, the release job downloads the archives
and evidence, verifies checksums and evidence bindings, and runs:

```bash
./repo.sh release-notes packages/<name>/v<version>
./repo.sh python -I tools/release_evidence.py release-manifest ...
gh release create packages/<name>/v<version> artifacts/* --verify-tag
```

The release notes are the matching section from the repository-root
`CHANGELOG.md`. Package versions and target eligibility come from
`packages/catalog.bzl`; runtime closure validation comes from the same
Buck-owned catalog.

## Tag Policy And Current Boundaries

- Release tags must be annotated (`git tag -a packages/<name>/v<version>`) and
  must be signed by a maintainer (`git tag -s ...`). CI fetches the tag object
  and fails closed on a lightweight tag, an enforceable repository-local check.
- Repository code cannot safely declare the public-key trust root needed for
  `git verify-tag`: that keyring belongs to the release host/organization, not
  the source tree. Configure that trust policy separately and require signed
  tags there; CI deliberately does not claim it has verified a signature merely
  because a tag name matched.
- It uploads gzip tar archives, checksum sidecars, deterministic SBOM and
  provenance sidecars, and `release-manifest.json`. It does not publish
  Zstandard archives, static extractors, or self-extracting installers.
- The package-release workflow grants `attestations: write`,
  `artifact-metadata: write`, and `id-token: write` to its verification caller;
  each tagged target archive receives a GitHub artifact attestation. The release
  job separately adds `contents: write` to publish or reconcile release assets.
- `gh release create --verify-tag` confirms that the tag exists remotely. It
  does not make an existing release mutable.

## Release Procedure

1. Commit the package version and its matching root changelog section.
2. Create and push a `packages/<name>/v<version>` tag.
3. Let the native matrix assemble and smoke-check the target archives.
4. The release job publishes the target archives, checksum sidecars, and
   changelog-derived notes.

Do not upload workstation-built artifacts to a GitHub Release.
