# Current Capabilities

This page separates commands that work in this checkout from planned
deployment/fleet capabilities.

| Area | Available now | Primary command | Verification |
| --- | --- | --- | --- |
| Bootstrap | Pinned, checksum-verified local tools on Linux x86-64 and ARM64 hosts | `./repo.sh bootstrap` | `./repo.sh doctor --deep` |
| Build and test | C++, Python, Go, Deno, React, and package graph | `./repo.sh build`, `./repo.sh test` | `./repo.sh lint` |
| Packages | In-graph Buck package archives and clean-extraction smoke tests | `./repo.sh package-list`, `./repo.sh package <name>` | `./repo.sh package-smoke <archive> <name>` |
| Editor | VS Code tasks and pinned language tools | **Workspace: bootstrap pinned tools** | **Workspace: verify editor prerequisites** |
| Release | Tag validation, archive checksums, GitHub artifact attestations, and idempotent GitHub release reconciliation | package release workflow | release workflow verification |
| Deployment | Read-only contracts only while implementation is staged | not yet exposed | future `deploy plan/status/verify` gates |

`gateway` and `schema-cli` appear in the catalog without a Buck-backed package
target. They are declarations, not releasable artifacts: `./repo.sh package`
and tagged release CI fail closed for them, while `./repo.sh package-target-check
<name>` reports that distinction. Current Deno application package support is
limited to zero-npm-dependency closures.

## First ten minutes

```bash
./repo.sh bootstrap --dry-run
./repo.sh bootstrap
./repo.sh doctor --deep
./repo.sh test
./repo.sh package-list
```

For VS Code, install the recommended extensions, run **Workspace: bootstrap
pinned tools**, then reload the window. See [EDITOR.md](EDITOR.md).
