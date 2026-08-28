# Principles And Meta-Rules

Status: mixed. This is durable reasoning guidance, not a uniform enforcement
contract. Some principles are enforced by an executable gate in this checkout,
some are only partly enforced, some describe capabilities that do not exist
here, and one records a deliberate standing exception. Every block below carries
an `enforcement:` line stating which of those it is; read that line before
treating a principle as a current repository guarantee. The executable boundary
is [CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md).

These principles were extracted from the Flow ADRs, this repository's engineering skills, upstream tool research, and the Please feasibility spike. Each rule records the invariant, evidence, boundary, cheapest validation, and falsifier.

## Decision Tree

```text
Need one repository UX?
  -> one public entrypoint

Need cross-project ordering/caching?
  -> one in-graph scheduler at fine target granularity

Need language-specific correctness?
  -> native ecosystem retains semantic ownership

Need reproducible tools?
  -> committed exact lock + transactional local install

Need current releases?
  -> read-only discovery + explicit verified candidate update

Need self-contained packages?
  -> one staging truth + native binaries + separately released host-triplet runtimes

Need small and fast distribution?
  -> deduplicate/prune first, zstd at measured knee, cache by sealed stage digest

Need AI assistance without state leakage?
  -> tracked canonical .agents content; ignored vendor/runtime state

Need portable application exposure?
  -> app declares logical subdomain; environment supplies zone and certificate

Need zero-interruption edge mutation?
  -> durable generated state + equivalent HAProxy runtime transaction + end-to-end probe

Need cheap co-located services?
  -> immutable package version vector + systemd group before adopting a scheduler
```

The zstd, subdomain-compiler, HAProxy, and systemd-group branches above are
proposed, not implemented. The implemented package format is deterministic
`.tar.gz` and there is no deployment mutation path. See each principle's
`enforcement:` line.

## P1. One Public Surface, Several Semantic Owners

```text
principle: one stable entrypoint presents the product workflow; specialized tools retain semantic ownership below it
evidence: command runners simplify UX but cannot correctly replace Go modules, Python packaging metadata, or Deno resolution
boundary: repo.sh owns dispatch; Buck2 owns build/test graph ordering and caching; lane tools own language semantics
validate: help lists every canonical command and each executes through the same environment
falsifier: a single lower-level tool genuinely and maintainably owns all required language/package semantics
avoid: copying build logic into repo.sh or first-party Buck2 rules
enforcement: enforced - `./repo.sh help` is the canonical list and every lane command dispatches through the same pinned environment
```

## P2. One Truth Per Artifact Class

```text
principle: choose one source artifact and derive all presentations from it
evidence: Flow's native manifest derives build graph and compile tooling; schema skills derive all languages from one IR
boundary: BUCK targets own compile actions, generated code contracts, package identity, and staging
validate: deterministic regeneration produces no diff and drift checks compare normalized models
falsifier: the derived artifact contains independent user-authored information that cannot live in the source
avoid: separately maintaining target source lists, tests, and package file lists
enforcement: enforced - `toolchain-lock --check` and `package-validate` are drift gates
```

## P6. Current Is A Discovery Policy; Reproducible Is A Lock

```text
principle: initialize from current reviewed stable versions, then build only from exact committed pins
evidence: mutable latest URLs break rebuilds; upstream providers may lag language patch releases or expose experimental variants
boundary: check-updates is read-only; update creates a tested reviewable lock diff
validate: old lock rebuilds offline and candidate update passes capability plus consumer tests
falsifier: the upstream artifact is content-addressed and immutable by construction
avoid: resolving latest during normal bootstrap
enforcement: enforced - `tools.lock.toml` pins immutable URLs and SHA-256 values; bootstrap is the only fetching command and `bootstrap --offline` plus CI's cold-graph replay prove it. There is no `check-updates` command: lock changes go through the manual `toolchain-lock`/`toolchain-qualify` lifecycle
```

## P7. Stamps Follow Capability, Not Existence

```text
principle: install completion is recorded only after the promised capability works
evidence: tool presence does not prove correct target, linker, ABI, JIT, cache, or parser behavior
boundary: temp extraction -> probes -> atomic rename -> stamp
validate: corrupt archive, wrong binary, interrupted extraction, and stale stamp fixtures
falsifier: artifact identity cryptographically implies all required capabilities and layout
avoid: touching a version file immediately after extraction
enforcement: enforced - bootstrap probes each artifact before stamping, and `./repo.sh doctor --deep` plus `toolchain-qualify` re-probe capability rather than presence
```

## P8. Separate Stable Defaults From Experiments

```text
principle: unstable language/runtime features live in named profiles with dedicated compatibility and benchmark gates
evidence: Go experiments can lack compatibility guarantees; CPython JIT can regress workloads
boundary: stable release profile differs from named experimental profiles
validate: tests and benchmarks compare default and experimental lanes
falsifier: upstream graduates the feature and removes the compatibility distinction
avoid: global GOEXPERIMENT or PYTHON_JIT settings
enforcement: enforced - Go uses the pinned release defaults, and the standalone Python runtime makes no JIT claim
```

## P9. Runtime Cost Is Per Bundle, Not Per Script

```text
principle: interpreted applications in one product share one pinned runtime
evidence: deno compile embeds roughly 70 MiB per executable; Python standalone content is largely common across entrypoints
boundary: package metadata references at most one compatible Deno and one compatible Python runtime release; browser assets reference none
validate: stage manifest rejects duplicate runtime identities and every launcher runs without host runtimes
falsifier: a tool is distributed independently and zero-install single-file UX outweighs duplication
avoid: compiling every small Deno or Python command into its own runtime-bearing executable
enforcement: partly enforced - `polyglot-demo` does stage exactly one CPython runtime and its launchers run from a clean extraction without host runtimes, but the "stage manifest rejects duplicate runtime identities" validation does not exist. `package_smoke` supports only `commands`/`checks` plus an optional `smoke_script`; nothing scans the staged manifest for duplicate runtime identities
```

## P10. Remove Entropy Before Compressing

```text
principle: deduplication, pruning, stripping, and one staging truth create more value than extreme compressor levels
evidence: maximum zstd levels trade large CPU cost for small marginal savings; runtime duplication dominates package size
boundary: compression begins only after stage is sealed and normalized
validate: component size report, duplicate-runtime check, package budget, and compression knee benchmark
falsifier: input is already irreducibly compressed and packaging latency is irrelevant
avoid: using level 22 as a substitute for package ownership
enforcement: not implemented - the zstd reference is aspirational. No zstd exists in `rules/`, `tools/`, or `repo.sh`; the implemented package format is deterministic `.tar.gz`, and ARCHITECTURE.md lists Zstandard packaging as deliberately unimplemented. There is no component size report, duplicate-runtime check, package budget, or compression knee benchmark. What does hold is the sealed single staging truth: `package()` stages in-graph before archiving
```

## P11. Optimize Repeated Work By Identity

```text
principle: deterministic content identity should turn unchanged work into a cache hit
evidence: sealed staging content plus compression policy completely determines the archive
boundary: package cache key includes stage, tool, runtime, epoch, and compression policy digests
validate: second identical package publishes the cached artifact and has the same SHA-256
falsifier: package intentionally contains nondeterministic signing or timestamps
avoid: recompressing an unchanged 150 MiB runtime bundle
enforcement: partly enforced - archives are deterministic, so an unchanged package reproduces the same SHA-256, and Buck2 owns action-level caching for the staged content. The "compression policy digest" in the boundary clause does not exist: there is no compression policy and no separate package cache key composed of stage/tool/runtime/epoch/compression digests
```

## P12. Validate From The Consumer Side

```text
principle: tests cross the same seam as real users and hosts
evidence: build-tree imports and binaries can pass while packages leak source paths or host runtimes
boundary: package is unpacked under a clean root with host runtimes, source tree, and network unavailable
validate: execute all entrypoints, compile/link SDK consumer, inspect ELF closure, verify hashes and licenses
falsifier: artifact never crosses a process, machine, package, or ownership boundary
avoid: declaring packaging successful after archive creation alone
enforcement: partly enforced - `./repo.sh package-smoke` and the in-graph `package_smoke` targets do extract into a clean temporary root and execute the declared entrypoints without host Python, Deno, Go, compiler, or source-tree state, and `polyglot-demo`'s smoke script additionally validates every referenced web asset. The rest of the validate clause is not implemented: nothing compiles or links an SDK consumer, inspects the ELF closure, or verifies licenses. `package_smoke` accepts only `commands`/`checks` and an optional `smoke_script`
```

## P14. AI Instructions Are Source; AI State Is Liability

```text
principle: version repository operating knowledge and skills while excluding auth, sessions, caches, and vendor runtime state
evidence: agents need stable local contracts; generated agent state is private, stale, and often credential-bearing
boundary: .agents/md and .agents/skills tracked; .claude/.codex/.grok/.omx and .agents state ignored
validate: skill/frontmatter/link checks plus clean worktree after agent and CI workflows
falsifier: a vendor-specific config is deliberately portable, reviewed, secret-free, and required by every clone
avoid: committing sessions or duplicating the canonical overview into vendor trees
enforcement: enforced - `.agents/md/overview.md` is tracked and exposed through the root `AGENTS.md` symlink while vendor and runtime agent state stays ignored. `.agents/skills/` has no repository-owned skills yet, so the skill/frontmatter/link part of the validate clause has nothing to check
```

## P15. Applications Declare Intent, Environments Bind Infrastructure

```text
principle: portable applications declare logical exposure and health, never provider-specific domains, addresses, or credentials
evidence: the same package must deploy under development, staging, public, and internal zones without rebuilds
boundary: app owns subdomain/scope/port; environment owns zone/certificate/placement; compiler owns the FQDN and proxy route
validate: compile one app manifest under two environments and prove only derived infrastructure state changes
falsifier: the domain name is an application-level protocol identity that must be compiled or signed into the product
avoid: embedding a TLD, Cloudflare token, certificate path, or host IP in an app package
enforcement: proposed - P15 through P18 describe the deployment and fleet design in docs/proposals/. Only the read-only `//infra/deploy` plan/observation contract exists; there is no deployment compiler, environment binding, HAProxy integration, certificate controller, or systemd group in this checkout
```

## P16. Hot State Must Also Be Durable State

```text
principle: every runtime mutation has an atomically persisted equivalent and an end-to-end proof
evidence: HAProxy map and certificate runtime updates vanish on restart; disk-only updates remain inactive
boundary: deployment compiler updates durable file and runtime transaction; receipt records their shared identity
validate: compare disk/runtime state, probe Host/SNI, restart HAProxy in a test fixture, and probe again
falsifier: the runtime reconstructs all desired state transactionally from the canonical store on every start
avoid: declaring success after only set-map, set-server, or set-ssl-cert
enforcement: proposed - no runtime mutation path exists; see P15
```

## P17. Compose Packages Before Introducing A Scheduler

```text
principle: known inventories with declared placement use immutable deployment groups and native supervision until dynamic placement is a measured requirement
evidence: Kamal, Fly, Nomad, and Kubernetes each solve broader scheduling or image lifecycle problems while package version vectors and data topology remain repository-owned
boundary: repo manifests select packages; systemd supervises processes; HAProxy activates traffic; a future scheduler consumes the same group model
validate: install, activate, health-check, drain, and rollback one multi-package group across a bounded selected inventory
falsifier: host churn, autoscaling, bin-packing, or automatic replacement becomes an operational requirement
avoid: adopting a scheduler solely because inventory contains multiple hosts
enforcement: proposed - no deployment group, systemd unit, or install/activate/drain/rollback path exists; see P15
```

## P18. High-Impact Credentials Stay At The Narrowest Controller

```text
principle: centralize powerful provider credentials and distribute only derived artifacts
evidence: wildcard DNS-01 needs DNS mutation, but edge hosts need only the resulting certificate
boundary: certificate controller holds zone-scoped Cloudflare token and ACME key; HAProxy hosts receive PEM plus non-secret receipt
validate: scan packages/hosts/logs for token absence, exercise renewal, rotate token, and verify edges continue serving existing certificates
falsifier: a provider supplies narrowly scoped short-lived per-host credentials with lower aggregate exposure
avoid: installing a DNS-edit token on every reverse proxy
enforcement: proposed - no certificate controller or credential distribution exists; see P15
```

## Meta-Rules

### MR1. Extract The Boundary, Not The Proper Noun

`repo.sh`, Buck2, and Deno are current anchors; zstd is a proposed one and is not
used anywhere in this checkout. Durable guidance states what they own and the validation they enable. Replace an anchor when another tool satisfies the same boundary with less liability.

### MR2. Add Abstraction Only For Two Consumers Or One Safety Invariant

Shared installers, stage manifests, and benchmark envelopes qualify because multiple lanes consume them. A generic adapter framework without two real adapters does not.

### MR3. Every Durable Rule Needs A Falsifier

A rule without an exception condition becomes dogma. Record when upstream maturity, product shape, or measured evidence should change the choice.

### MR4. Cost Must Be Counted In Ownership, Not Only Runtime

Please was fast and extensible, but missing maintained TypeScript, Rust, packaging, and compdb support transferred too much critical rule ownership. Tool selection counts repository code, upgrade burden, diagnostics, and consumer risk.

### MR5. Experimental Features Need Exit Criteria

Go experiments, Python JIT, remote cache, and dense compression profiles need compatibility and performance evidence plus a policy for promotion or removal.

### MR6. Separate Discovery, Mutation, And Execution

Checking for updates is read-only. Updating pins is explicit and reviewable. Normal execution consumes the lock and stays offline. Mixing these phases destroys reproducibility.

### MR7. Stop When Another Principle Changes No Decision Or Check

Further abstraction is noise if it does not alter an artifact owner, command, validation gate, failure message, custody rule, or future branch.

## Evidence Map

- Flow ADRs: explicit scope, status honesty, one vocabulary, formal drift gates, behavior before formatting.
- Flow native infrastructure: manifest-derived build products, package consumer verification, capability-probing doctor, pinned toolchains.
- Please spike: strong codegen and C build primitives; weak maintained TS ecosystem; broken upstream compdb; high custom packaging ownership.
- Deno upstream: React 19/Vite support, bundled TypeScript checker, workspaces, compile size and cross-target behavior.
- Go upstream: Go 1.27 release defaults and the boundary for opt-in experiments.
- CPython/PBS upstream: experimental JIT boundary, standalone archive variants, ABI/libc constraints.
- Native toolchain comparison: userdocs release 2628 beats cross-tools 20260515 on compressed and extracted footprint and uniquely satisfies the native ARM64-host requirement among the compared assets; provider choice follows measured host/target capability, not project-name preference.
- Existing repo skills: one command surface, local toolchains, lint parity, schema islands, artifact consumer tests, progressive disclosure.
