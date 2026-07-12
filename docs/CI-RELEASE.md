# CI Packaging And Releases

The template workflow is [../.github/workflows/ci-release.yml](../.github/workflows/ci-release.yml). A clone receives it directly at `.github/workflows/ci-release.yml` without changing its build semantics.

## Contract

GitHub Actions is a caller of `repo.sh`, never an independent build implementation:

```text
pull request or main push
  -> bootstrap pinned tools
  -> doctor --deep
  -> repo.sh ci

packages/<name>/v<version> annotated tag
  -> resolve exactly one package and version
  -> package x64 and ARM64 independently on matching native runners
  -> clean consumer package smoke for each target
  -> package-specific checksums and notes
  -> provenance attestations
  -> one immutable GitHub Release for that package version
```

The workflow assumes these public commands:

```bash
./repo.sh bootstrap
./repo.sh doctor --deep
./repo.sh target
./repo.sh ci
./repo.sh package --package <name> --version <version>
./repo.sh package-smoke --package <name> --version <version>
./repo.sh release-check --package <name> --version <version> --tag <tag>
./repo.sh release-notes --package <name> --version <version> --output <path>
```

`package` must place only final release files beneath:

```text
dist/release/<package>/<version>/<target>/
```

That directory is flat and contains regular files only. Every package filename includes its package name, semantic version, and target triple, and uses the portable character set `[A-Za-z0-9._+-]`. The workflow rejects nested directories, special files, unsafe names, and duplicate release basenames.

There is no product-wide archive and no repository-wide package version. Each declared package owns its own version, changelog, artifacts, compatibility policy, and GitHub Release.

The workflow does not discover build-tree files, rebuild through ad hoc shell commands, install host-global compilers, or run release-specific compilation.

## Events

- Pull requests and main pushes run the aggregate `repo.sh ci` verification contract but cannot publish.
- Annotated `packages/<name>/v<version>` tags, including signed annotated tags, package only that named package and publish its GitHub Release after x64 and ARM64 succeed. Lightweight tags are rejected.
- Manual dispatch runs the normal non-release path unless it targets a tag ref.

The release job fetches full tag metadata and requires the ref to resolve to a Git tag object, rejecting lightweight tags. It also uses `gh release create --verify-tag`, which checks that the tag exists remotely. It fails if the release already exists; release assets are treated as immutable rather than silently overwritten.

## Package Release Identity

Tags encode package ownership and version independently:

```text
packages/gateway/v1.4.0
packages/schema-cli/v0.8.2
packages/python-worker/v3.1.1
```

Each tag produces a separate GitHub Release whose title and notes belong only to that package version. Release notes come from the package's governed changelog through `repo.sh release-notes`; generic repository-wide generated notes are not used.

The package manifest is authoritative. `repo.sh release-check` owns complete SemVer 2.0 validation and rejects disagreement among the tag version, requested `--version`, package manifest version, and changelog section. Workflow shell code validates only that tag components are safe to pass as arguments; it does not duplicate semantic-version policy.

One package release may contain several files, but each is a separate release asset—for example the x64 archive, ARM64 archive, and their target-qualified checksum files. Nothing combines unrelated executables or runtimes into one global tarball.

## Targets

The initial matrix performs native builds on two matching Ubuntu runners:

```text
x86_64-linux-musl   # ubuntu-24.04
aarch64-linux-musl  # ubuntu-24.04-arm
```

These are the only initial package targets. Each runner supplies the matching CPU architecture; the pinned GCC+musl toolchain supplies the output libc/ABI. Thus an Ubuntu glibc host still produces and executes a musl-linked artifact of its own CPU architecture. Cross-architecture compilation and emulation are not part of the initial system. Each job asserts that the resolved native-architecture musl triplet equals the matrix triplet before packaging. Do not add 32-bit or additional architectures without an explicit product requirement.

## Integrity

- Third-party actions are pinned to full commit SHAs.
- Checkout credentials are not persisted.
- Default permissions are read-only.
- Package jobs receive only `id-token: write` and `attestations: write` in addition to read access.
- Only the tag-gated release job receives `contents: write`.
- Every target directory contains a uniquely named `SHA256SUMS-<package>-<version>-<target>` file so release assets cannot collide.
- GitHub provenance binds released artifact digests to the workflow invocation.
- Release creation consumes only downloaded artifacts from successful package jobs.

Consumers can verify provenance using GitHub CLI:

```bash
gh attestation verify gateway-1.4.0-x86_64-linux-musl.tar.zst --repo OWNER/REPOSITORY
sha256sum --check SHA256SUMS-gateway-1.4.0-x86_64-linux-musl
```

## Caching

The workflow aggressively caches the expensive immutable inputs:

```text
.local/toolchain/
.local/downloads/
.local/cache/downloads/
```

The cache key includes:

- runner operating system and architecture;
- target triple for package jobs;
- the digest of `tools.lock.toml` and the bootstrap implementation under `toolchain/`;
- an explicit cache-schema version.

There are deliberately no broad restore prefixes for installed toolchains. An exact lock hit restores a complete candidate; a miss performs the normal checksum-verified installation. `repo.sh bootstrap` always runs after restoration and must recheck stamps and capability probes, so a corrupt or incomplete cache is repaired rather than trusted.

Downloaded archives are stored with the toolchain because they make repair and offline reinstallation cheap. They remain subject to the committed SHA-256 checks before use.

Build outputs and language dependency caches are not yet cached by this workflow. Add them independently after each lane has a complete content-addressed input contract. Workflow artifacts transport final packages between jobs; they are not a build cache. The packaged archive is uploaded as a file, so its internal executable modes and normalized metadata remain intact.

## Release Procedure

1. Ensure main CI is green and the package-specific version/changelog is committed.
2. Create a signed or annotated `packages/<name>/v<version>` tag pointing at the reviewed commit.
3. Push the tag.
4. The workflow builds and verifies only that package for x64 and ARM64.
5. GitHub publishes a package-specific release with its own notes, separate target assets, checksums, and attestations.

No developer workstation artifact is uploaded to a release.
