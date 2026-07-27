# Current Capabilities

This is the compatibility and readiness contract for the executable repository
slice. A command or guarantee is current only when this page identifies its
verification. Design material in [DEPLOYMENT.md](proposals/DEPLOYMENT.md),
[FLEET.md](proposals/FLEET.md), and [DESIGN-README.md](DESIGN-README.md) remains a
proposal unless it is listed here.

| Area | Current, verified contract | Boundary / deferred work | Evidence |
| --- | --- | --- | --- |
| Host and targets | Linux x86-64 hosts build `x86_64-linux-musl` at the x86-64-v3 baseline (`-mtune=generic`, writable-prefetch enabled); Linux ARM64 hosts build `aarch64-linux-musl` at Armv8.2-A. Builds are host-native. | Deployment hosts must satisfy the emitted ISA baseline; no cross-compilation and no macOS or Windows support. | `./repo.sh target`; native x64 and ARM64 CI matrix |
| Bootstrap and normal builds | Pinned, checksum-verified tools; named Buck-backed build, test, lint, package, and release commands are offline after bootstrap. | Raw `./repo.sh go` and `./repo.sh deno` passthroughs remain caller-controlled and can use network-capable tool operations. | `./repo.sh bootstrap --offline`; `./repo.sh doctor --deep`; CI cold-graph replay |
| Language lanes | C++, Python, pure Go, Deno TypeScript, and React static-site build/test lanes run through the pinned closure. | Sanitizers are explicitly deferred to preserve the hermetic Linux-musl toolchain contract; a future sanitizer lane requires a separately evaluated host-debug toolchain. | `./repo.sh build`; `./repo.sh test`; `./repo.sh cpp-build dbg` |
| Packages | Buck-owned `package()` targets produce deterministic native `.tar.gz` archives and clean-extraction smoke tests. Catalog-only declarations fail closed. | `gateway` and `schema-cli` are declarations, not releasable packages. Deno application packaging supports only zero-npm-dependency closures; npm-dependent runtime packaging is deferred. | `./repo.sh package-validate`; `./repo.sh package <name>`; `./repo.sh package-smoke <archive> <name>` |
| Release | Package-tag CI requires annotated tags, validates each archive/checksum/SBOM/provenance sidecar, creates GitHub artifact attestations, and reconciles a GitHub Release with a deterministic release manifest. | Repository CI cannot establish a signing trust root; maintainers must sign release tags and configure the host trust policy separately. | [CI-RELEASE.md](CI-RELEASE.md); package-release workflow |
| Deployment | A Buck-owned, read-only deployment-plan and observation-verification contract is available. | No `repo.sh deploy` interface, remote transport, host mutation, certificate automation, apply, or rollback exists. | `./repo.sh buck2 test //infra/deploy:readonly-contract` |
| Editor | VS Code tasks use the pinned toolchain and configured language roots. | Editor integration is development support, not a cross-platform compatibility claim. | **Workspace: bootstrap pinned tools**; **Workspace: verify editor prerequisites** |

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
