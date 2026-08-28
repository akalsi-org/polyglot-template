# CI Packaging And Releases

There are four checked-in workflows, and all of them call `./repo.sh`; none
implements a separate build or packaging path.

- [verify.yml](../.github/workflows/verify.yml) holds every required verification job.
  It is a reusable `workflow_call` workflow and is never triggered directly.
- [ci-release.yml](../.github/workflows/ci-release.yml) is the entrypoint for
  pull requests, pushes to `main`, and manual dispatch. It contains no logic of
  its own: it only calls `verify.yml`.
- [package-release.yml](../.github/workflows/package-release.yml) is the
  entrypoint for `packages/<name>/v*` tags. It calls the same `verify.yml` and
  then publishes the GitHub Release from the verified assets.
- [ordering-campaign.yml](../.github/workflows/ordering-campaign.yml) is a
  manual native ARM64 campaign for the Go MPSC memory-ordering mutants.

## Verification Contract

`verify.yml` runs three independent jobs — `lint`, `test-dbg`, and `test-opt` —
each across the native x64 (`ubuntu-24.04`) and ARM64 (`ubuntu-24.04-arm`)
runners. Every job first checks that `./repo.sh target` matches its native
runner, restores exact-key caches for the toolchain, Deno dependency graph,
`node_modules`, the Buck2 trees (`buck-out/v2/art`, `buck-out/v2/art-bxl`,
`buck-out/v2/cache`), and the Go build cache, enables unprivileged user
namespaces, and bootstraps with `./repo.sh bootstrap --offline` falling back to
a live `./repo.sh bootstrap`.

The `lint` job then:

1. runs `./repo.sh doctor --deep` after bootstrap;
2. builds `//toolchains:native` once and fails if the build log records any
   network download, proving the restored cache is a zero-network cache hit;
3. proves offline replay: inside an unprivileged network namespace with no
   route, it runs `buck2 clean` then `./repo.sh build` and `./repo.sh test` over
   a cold `buck-out`; and
4. runs the repository quality gates and infra tests — `./repo.sh lint`,
   `./repo.sh package-validate`, and `./repo.sh infra-test`, which itself runs
   the documentation, workflow, and Deno manifest contracts.

The `test-dbg` job runs `./repo.sh test dbg`, then regenerates the merged coverage report from a cold Buck output
tree (`buck2 clean` followed by `./repo.sh coverage`) and uploads it.

That upload carries three artifacts: the merged `lcov`, the plain-text
summary, and `coverage.html` — one self-contained interactive page
(`tools/coverage_html.py`) with per-file navigation, annotated sources
coloured by hit/miss/uninstrumented, and <kbd>n</kbd>/<kbd>p</kbd> stepping
through uncovered lines. It embeds its sources and carries no external
stylesheet, script, or font, so it opens over `file://` with no server and no
network.

The same generator writes the GitHub **job summary**: a headline total, a
per-directory rollup, an expandable per-file table listing each file's
uncovered line ranges worst-covered first, and — under *Uncovered lines in
context* — the actual source of every uncovered region with three lines of
surrounding context, rendered in a ```diff block so GitHub colours the
uncovered lines red without any stylesheet. That last section exists so the
common case needs no download at all: the artifact is the deep-dive, not the
only way to read the result. Excerpts are budgeted well under the 1 MiB
summary limit and state how many files they dropped. A link to the artifact
holding the interactive page sits at the top. Both renderings come from one in-memory model,
so the summary cannot disagree with the report it links to. The job summary is
Markdown because GitHub strips `<script>` and `<style>` from summaries — the
page itself cannot be inlined there. Both are published with `if: always()`,
so a run that fails the coverage floor still shows which lines are missing.

To read the interactive page from a CI run, download the artifact — GitHub
never serves artifact content as a page, so there is no URL to open:

```bash
token=$(secrets.sh get github.token)
repo=akalsi-org/polyglot-template
api="https://api.github.com/repos/$repo"
# Resolve the artifact for the newest run on this branch, then unzip it.
run=$(curl -fsSL -H "Authorization: Bearer $token" \
  "$api/actions/runs?branch=main&per_page=1" | ./repo.sh python -c \
  'import json,sys; print(json.load(sys.stdin)["workflow_runs"][0]["id"])')
url=$(curl -fsSL -H "Authorization: Bearer $token" "$api/actions/runs/$run/artifacts" |
  ./repo.sh python -c 'import json,sys
artifacts = json.load(sys.stdin)["artifacts"]
print(next(a["archive_download_url"] for a in artifacts
           if a["name"] == "coverage-x86_64-linux-musl"))')
curl -fsSL -H "Authorization: Bearer $token" -o coverage.zip "$url"
unzip -o coverage.zip 'buck-out/coverage-report/coverage.html'
# then open buck-out/coverage-report/coverage.html
```

`secrets.sh get github.token` decrypts the token with `age`, so it prompts on
a terminal; the token stays in a shell variable and is never written to a
file or a URL query.

Locally the same page is written directly by `./repo.sh coverage`, at
`buck-out/coverage-report/coverage.html`; no download step is involved.

Hosting it instead of downloading it would mean GitHub Pages, which is
deliberately **not** wired up: serving a private repository's Pages requires
GitHub Enterprise, so on this repository it would publish annotated source to
a public URL. It would also need `pages: write` and `id-token: write`, which
cannot live in `verify.yml` at all (see the token scope note below) and would
have to be a separate job in `ci-release.yml`.

The `test-opt` job runs `./repo.sh test opt`, exercises the pinned Python
runtime, and owns the
release path below. On non-tag refs it rehearses a full release for
`polyglot-demo` without publishing.

The cache keys include the target, relevant lock files, and bootstrap or
Buck2-graph inputs. A cache hit remains untrusted until bootstrap and doctor
revalidate it.

## Manual ARM64 Ordering Campaign

`ordering-campaign.yml` runs only through manual dispatch on
`ubuntu-24.04-arm`. It verifies the native `aarch64-linux-musl` target,
bootstraps the pinned repository, and runs `./repo.sh doctor --deep` before the
campaign.

The workflow runs `go/lib/mpsc/ordering_mutants.sh` with strict adjudication.
It defaults to 100 repetitions per mutant and rejects values below 20. Strict
mode fails if the runner is not native ARM64 or any valid mutant survives.
The workflow publishes verdicts in the job summary. It also uploads the doctor,
runner, and campaign logs for diagnostics.

This campaign is manual because weak-memory failures are probabilistic and the
run is slower than the required verification matrix. Run it when the MPSC
ordering implementation or its publication tests change. An amd64 runner and
QEMU user mode cannot adjudicate these mutants.

## Release Tags

A pushed ref matching `packages/<name>/v*` triggers the release path through
`package-release.yml`. Inside `verify.yml`'s `test-opt` job, the tag object is
first checked to be annotated, then `<name>` is derived from the ref and the job
executes:

```bash
./repo.sh package-target-check <name>
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

The verification matrix uploads those verified files as cross-job artifacts.
After both targets pass, the release job downloads the archives and evidence,
verifies checksums and evidence bindings, publishes, and only then uses
GitHub's `actions/attest` action to create an artifact attestation covering
every published archive — after the checksum verification, so the workflow's
identity is never bound to bytes nothing has checked. The verification
workflow itself mints no attestation and holds no write scope; see the token
scope note below. The release job runs:

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
- Token scope: `verify.yml` declares `contents: read` and no job in it
  requests more. That is a hard constraint, not a preference — a called
  workflow's job may request no more than its caller grants, and
  `ci-release.yml` (branch and PR runs) grants read only, so a single write
  scope in `verify.yml` makes the workflow file invalid on *every* branch
  push, whether or not the job that asks for it would ever run. Attestation
  therefore lives in `package-release.yml`'s release job, which lists every
  scope it needs (`contents: write` to publish, plus `attestations: write`,
  `artifact-metadata: write`, and `id-token: write` to attest) because a
  job-level block replaces the workflow-level one. Each published archive
  receives a GitHub artifact attestation.
- `gh release create --verify-tag` confirms that the tag exists remotely. It
  does not make an existing release mutable.

## Release Procedure

1. Commit the package version and its matching root changelog section.
2. Create and push a `packages/<name>/v<version>` tag.
3. Let the native matrix assemble and smoke-check the target archives.
4. The release job publishes the target archives, checksum sidecars, and
   changelog-derived notes.

Do not upload workstation-built artifacts to a GitHub Release.
