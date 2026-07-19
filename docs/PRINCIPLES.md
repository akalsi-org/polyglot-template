# Principles And Meta-Rules

These principles were extracted from the Flow ADRs, this repository's engineering skills, upstream tool research, and the Please feasibility spike. Each rule records the invariant, evidence, boundary, cheapest validation, and falsifier.

## Decision Tree

```text
Need one repository UX?
  -> one public entrypoint

Need cross-project ordering/caching?
  -> one in-graph scheduler at fine target granularity

Need language-specific correctness?
  -> native ecosystem retains semantic ownership

Need C/C++ incrementality and editor truth?
  -> the Buck2 action graph is the single compile-action source; compdb is a BXL query over it

Need complex third-party C configuration?
  -> use upstream build frontend as an isolated adapter

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

## P1. One Public Surface, Several Semantic Owners

```text
principle: one stable entrypoint presents the product workflow; specialized tools retain semantic ownership below it
evidence: command runners simplify UX but cannot correctly replace C dependency graphs, Go modules, Python ABI metadata, or Deno resolution
boundary: repo.sh owns dispatch; Buck2 owns build/test graph ordering and caching; lane tools own language semantics
validate: help lists every canonical command and each executes through the same environment
falsifier: a single lower-level tool genuinely and maintainably owns all required language/package semantics
avoid: copying build logic into repo.sh or first-party Buck2 rules
```

## P2. One Truth Per Artifact Class

```text
principle: choose one source artifact and derive all presentations from it
evidence: Flow's native manifest derives build graph and compile tooling; schema skills derive all languages from one IR
boundary: native manifest owns compile actions; schema owns generated code; package manifest owns staging
validate: deterministic regeneration produces no diff and drift checks compare normalized models
falsifier: the derived artifact contains independent user-authored information that cannot live in the source
avoid: separately maintaining compile source lists, compdb commands, tests, and package file lists
```

## P3. Tooling Products Are Build Correctness

```text
principle: consumer-critical tooling artifacts are mandatory build outputs, not optional conveniences
evidence: Please's stale compdb implementation built successfully while remaining unusable by clangd
boundary: compdb is derived on demand from the same Buck2 action graph that builds (`./repo.sh compile-commands`), never hand-maintained
validate: every executed compile action has an equivalent compdb entry and clangd checks representative sources
falsifier: no consumer uses the artifact and its absence cannot affect development or verification
avoid: a manual compile-commands step developers must remember
```

## P4. Third-Party Code Is Part Of The Effective Graph

```text
principle: if the toolchain compiles a translation unit, the canonical compilation database describes it
evidence: third-party configuration and generated headers affect first-party analysis and navigation
boundary: compdb is complete by default; lint scope remains separately selectable
validate: dependency compile counts equal normalized compdb counts
falsifier: dependency is consumed only as a verified prebuilt binary and no source is compiled
avoid: excluding upstream source merely to reduce editor noise
```

## P5. Prefer Upstream Semantics, Not Upstream Global State

```text
principle: reuse an upstream build frontend when it carries complex configuration knowledge, but isolate its inputs and outputs
evidence: rewriting Cargo/TypeScript/complex C dependency behavior in Please exceeded the ownership budget
boundary: CMake/Meson/Autotools may configure selected dependencies into private prefixes; they do not own first-party builds
validate: locked source + patches + options + toolchain reproduce declared artifacts offline
falsifier: source topology is small and more stable than the upstream configuration layer
avoid: making CMake mandatory because one optional dependency uses it
```

## P6. Current Is A Discovery Policy; Reproducible Is A Lock

```text
principle: initialize from current reviewed stable versions, then build only from exact committed pins
evidence: mutable latest URLs break rebuilds; upstream providers may lag language patch releases or expose experimental variants
boundary: check-updates is read-only; update creates a tested reviewable lock diff
validate: old lock rebuilds offline and candidate update passes capability plus consumer tests
falsifier: the upstream artifact is content-addressed and immutable by construction
avoid: resolving latest during normal bootstrap
```

## P7. Stamps Follow Capability, Not Existence

```text
principle: install completion is recorded only after the promised capability works
evidence: tool presence does not prove correct target, linker, ABI, JIT, cache, or parser behavior
boundary: temp extraction -> probes -> atomic rename -> stamp
validate: corrupt archive, wrong binary, interrupted extraction, and stale stamp fixtures
falsifier: artifact identity cryptographically implies all required capabilities and layout
avoid: touching a version file immediately after extraction
```

## P8. Separate Stable Defaults From Experiments

```text
principle: unstable language/runtime features live in named profiles with dedicated compatibility and benchmark gates
evidence: GCC C++26 is experimental; Go jsonv2 lacks compatibility guarantees; CPython JIT can regress workloads
boundary: stable release profile differs from cxx26/jsonv2/python-jit profiles
validate: tests and benchmarks compare default and experimental lanes
falsifier: upstream graduates the feature and removes the compatibility distinction
avoid: global GOEXPERIMENT or PYTHON_JIT settings
```

## P9. Runtime Cost Is Per Bundle, Not Per Script

```text
principle: interpreted applications in one product share one pinned runtime
evidence: deno compile embeds roughly 70 MiB per executable; Python standalone content is largely common across entrypoints
boundary: package metadata references at most one compatible Deno and one compatible Python runtime release; browser assets reference none
validate: stage manifest rejects duplicate runtime identities and every launcher runs without host runtimes
falsifier: a tool is distributed independently and zero-install single-file UX outweighs duplication
avoid: compiling every small Deno or Python command into its own runtime-bearing executable
```

## P10. Remove Entropy Before Compressing

```text
principle: deduplication, pruning, stripping, and one staging truth create more value than extreme compressor levels
evidence: maximum zstd levels trade large CPU cost for small marginal savings; runtime duplication dominates package size
boundary: compression begins only after stage is sealed and normalized
validate: component size report, duplicate-runtime check, package budget, and compression knee benchmark
falsifier: input is already irreducibly compressed and packaging latency is irrelevant
avoid: using level 22 as a substitute for package ownership
```

## P11. Optimize Repeated Work By Identity

```text
principle: deterministic content identity should turn unchanged work into a cache hit
evidence: sealed staging content plus compression policy completely determines the archive
boundary: package cache key includes stage, tool, runtime, epoch, and compression policy digests
validate: second identical package publishes the cached artifact and has the same SHA-256
falsifier: package intentionally contains nondeterministic signing or timestamps
avoid: recompressing an unchanged 150 MiB runtime bundle
```

## P12. Validate From The Consumer Side

```text
principle: tests cross the same seam as real users and hosts
evidence: build-tree imports and binaries can pass while packages leak source paths or host runtimes
boundary: package is unpacked under a clean root with host runtimes, source tree, and network unavailable
validate: execute all entrypoints, compile/link SDK consumer, inspect ELF closure, verify hashes and licenses
falsifier: artifact never crosses a process, machine, package, or ownership boundary
avoid: declaring packaging successful after archive creation alone
```

## P13. Complete Observability Does Not Mean Universal Policy Enforcement

```text
principle: describe the full graph, then choose review/lint scope separately
evidence: clangd needs third-party actions, while project-specific clang-tidy policy over all upstream code creates noise
boundary: complete compdb always; ordinary lint targets owned code; explicit third-party audit is available
validate: editor navigation works in dependency code and CI lint remains actionable
falsifier: upstream source is locally maintained and subject to repository policy
avoid: making the compdb incomplete to keep lint quiet
```

## P14. AI Instructions Are Source; AI State Is Liability

```text
principle: version repository operating knowledge and skills while excluding auth, sessions, caches, and vendor runtime state
evidence: agents need stable local contracts; generated agent state is private, stale, and often credential-bearing
boundary: .agents/md and .agents/skills tracked; .claude/.codex/.grok/.omx and .agents state ignored
validate: skill/frontmatter/link checks plus clean worktree after agent and CI workflows
falsifier: a vendor-specific config is deliberately portable, reviewed, secret-free, and required by every clone
avoid: committing sessions or duplicating the canonical overview into vendor trees
```

## P15. Applications Declare Intent, Environments Bind Infrastructure

```text
principle: portable applications declare logical exposure and health, never provider-specific domains, addresses, or credentials
evidence: the same package must deploy under development, staging, public, and internal zones without rebuilds
boundary: app owns subdomain/scope/port; environment owns zone/certificate/placement; compiler owns the FQDN and proxy route
validate: compile one app manifest under two environments and prove only derived infrastructure state changes
falsifier: the domain name is an application-level protocol identity that must be compiled or signed into the product
avoid: embedding a TLD, Cloudflare token, certificate path, or host IP in an app package
```

## P16. Hot State Must Also Be Durable State

```text
principle: every runtime mutation has an atomically persisted equivalent and an end-to-end proof
evidence: HAProxy map and certificate runtime updates vanish on restart; disk-only updates remain inactive
boundary: deployment compiler updates durable file and runtime transaction; receipt records their shared identity
validate: compare disk/runtime state, probe Host/SNI, restart HAProxy in a test fixture, and probe again
falsifier: the runtime reconstructs all desired state transactionally from the canonical store on every start
avoid: declaring success after only set-map, set-server, or set-ssl-cert
```

## P17. Compose Packages Before Introducing A Scheduler

```text
principle: known inventories with declared placement use immutable deployment groups and native supervision until dynamic placement is a measured requirement
evidence: Kamal, Fly, Nomad, and Kubernetes each solve broader scheduling or image lifecycle problems while package version vectors and data topology remain repository-owned
boundary: repo manifests select packages; systemd supervises processes; HAProxy activates traffic; a future scheduler consumes the same group model
validate: install, activate, health-check, drain, and rollback one multi-package group across a bounded selected inventory
falsifier: host churn, autoscaling, bin-packing, or automatic replacement becomes an operational requirement
avoid: adopting a scheduler solely because inventory contains multiple hosts
```

## P18. High-Impact Credentials Stay At The Narrowest Controller

```text
principle: centralize powerful provider credentials and distribute only derived artifacts
evidence: wildcard DNS-01 needs DNS mutation, but edge hosts need only the resulting certificate
boundary: certificate controller holds zone-scoped Cloudflare token and ACME key; HAProxy hosts receive PEM plus non-secret receipt
validate: scan packages/hosts/logs for token absence, exercise renewal, rotate token, and verify edges continue serving existing certificates
falsifier: a provider supplies narrowly scoped short-lived per-host credentials with lower aggregate exposure
avoid: installing a DNS-edit token on every reverse proxy
```

## Meta-Rules

### MR1. Extract The Boundary, Not The Proper Noun

`repo.sh`, Buck2, Deno, and zstd are current anchors. Durable guidance states what they own and the validation they enable. Replace an anchor when another tool satisfies the same boundary with less liability.

### MR2. Add Abstraction Only For Two Consumers Or One Safety Invariant

Shared installers, stage manifests, and benchmark envelopes qualify because multiple lanes consume them. A generic adapter framework without two real adapters does not.

### MR3. Every Durable Rule Needs A Falsifier

A rule without an exception condition becomes dogma. Record when upstream maturity, product shape, or measured evidence should change the choice.

### MR4. Cost Must Be Counted In Ownership, Not Only Runtime

Please was fast and extensible, but missing maintained TypeScript, Rust, packaging, and compdb support transferred too much critical rule ownership. Tool selection counts repository code, upgrade burden, diagnostics, and consumer risk.

### MR5. Experimental Features Need Exit Criteria

C++26, jsonv2, Python JIT, remote cache, and dense compression profiles need compatibility and performance evidence plus a policy for promotion or removal.

### MR6. Separate Discovery, Mutation, And Execution

Checking for updates is read-only. Updating pins is explicit and reviewable. Normal execution consumes the lock and stays offline. Mixing these phases destroys reproducibility.

### MR7. Stop When Another Principle Changes No Decision Or Check

Further abstraction is noise if it does not alter an artifact owner, command, validation gate, failure message, custody rule, or future branch.

## Evidence Map

- Flow ADRs: explicit scope, status honesty, one vocabulary, formal drift gates, behavior before formatting.
- Flow native infrastructure: manifest-derived build products, package consumer verification, capability-probing doctor, pinned toolchains.
- Please spike: strong codegen and C build primitives; weak maintained TS ecosystem; broken upstream compdb; high custom packaging ownership.
- Deno upstream: React 19/Vite support, bundled TypeScript checker, workspaces, compile size and cross-target behavior.
- Go upstream: Go 1.26 Green Tea GC default and experimental jsonv2 boundary.
- CPython/PBS upstream: experimental JIT boundary, standalone archive variants, ABI/libc constraints.
- Native toolchain comparison: userdocs release 2628 beats cross-tools 20260515 on compressed and extracted footprint and uniquely satisfies the native ARM64-host requirement among the compared assets; provider choice follows measured host/target capability, not project-name preference.
- Existing repo skills: one command surface, local toolchains, lint parity, schema islands, artifact consumer tests, progressive disclosure.
