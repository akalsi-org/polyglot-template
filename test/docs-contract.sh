#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)

for file in README.md .agents/md/overview.md docs/ARCHITECTURE.md; do
  grep -Fq 'Linux x86-64' "$root/$file"
done

grep -Fq './repo.sh bootstrap' "$root/README.md"
grep -Fq './repo.sh doctor --deep' "$root/README.md"
grep -Fq 'raw language-tool passthroughs' "$root/README.md"
grep -Fq 'raw Deno passthrough' "$root/docs/LANGUAGE-GUIDE.md"
grep -Fq 'intentionally passes its arguments to Go' "$root/docs/LANGUAGE-GUIDE.md"
grep -Fq 'Catalog-only declarations' "$root/docs/ARCHITECTURE.md"
grep -Fq 'no raw-build fallback assembler' "$root/docs/ARCHITECTURE.md"
grep -Fq 'zero-npm-dependency' "$root/docs/ARCHITECTURE.md"
grep -Fq 'GitHub artifact attestation' "$root/docs/CI-RELEASE.md"
grep -Fq 'actions/attest' "$root/docs/CI-RELEASE.md"
grep -Fq '## packages/polyglot-server/v0.1.0' "$root/CHANGELOG.md"
grep -Fq 'Status: the canonical catalog' "$root/docs/IMPROVEMENT-ROADMAP.md"
grep -Fq 'Status: the bootstrap-first README Quick Start' "$root/docs/IMPROVEMENT-ROADMAP.md"

if grep -RInE 'package\.toml|runtime-resolution\.lock\.toml|falls through to `tools/package_release\.py`|does not currently generate provenance attestations|only operation permitted to fetch dependencies' \
  "$root/README.md" "$root/.agents/md/overview.md" "$root/docs/ARCHITECTURE.md" "$root/docs/CI-RELEASE.md" "$root/docs/CURRENT-CAPABILITIES.md" "$root/docs/LANGUAGE-GUIDE.md" "$root/docs/TROUBLESHOOTING.md"; then
  printf 'docs contract: stale package, attestation, or network claim found\n' >&2
  exit 1
fi

printf 'docs contract: ok\n'
