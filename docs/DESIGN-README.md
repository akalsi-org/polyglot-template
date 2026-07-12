# Polyglot Repository Template Decision Package

Status: proposed design baseline with an executable pinned bootstrap/native/package slice at the repository root. Full Go/Deno/schema lanes, tag publication, and fleet deployment remain gated as documented.

This document set defines a cloneable, non-JVM, AI-ready monorepo template for C/C++, Go, Python with native extensions, and Deno TypeScript/React. It records both the concrete design and the reasoning rules that produced it.

## Documents

- [ARCHITECTURE.md](ARCHITECTURE.md) — repository layout, commands, toolchains, build graph, packaging, validation, and implementation phases.
- [PRINCIPLES.md](PRINCIPLES.md) — durable principles, decision tree, evidence, boundaries, falsifiers, and meta-rules.
- [DEPLOYMENT.md](DEPLOYMENT.md) — VPS application placement, package composition, systemd activation, HAProxy subdomain routing, wildcard DNS, and hot certificate delivery.
- [FLEET.md](FLEET.md) — optional infrastructure profile, inventory authority, OpenTofu provisioning, isolated Ansible convergence, host lifecycle, trust, locks, receipts, and scaling boundary.
- [FLEET-NEXT.md](FLEET-NEXT.md) — remaining provider, SLO, trust-implementation, service-policy, stateful-workload, and recovery questions.
- [CI-RELEASE.md](CI-RELEASE.md) — GitHub Actions packaging, provenance, artifact retention, and tag-gated release contract.
- [spikes/serialization/README.md](spikes/serialization/README.md) — nested-array IDL spike comparing Fory, Bebop, FlatBuffers, and a minimal borrowed-view lower bound.
- [../README.md](../README.md) — runnable target, lock/bootstrap, native graph, package/runtime closure, AI-guide, test, and two-runner preflight slice.

## Decision Summary

```text
repo.sh
  -> moon                         coarse cross-project scheduling
      -> native manifest + Ninja  C/C++ semantic graph
      -> Go toolchain             Go modules/build/test
      -> Python backend           wheels/native extensions
      -> Deno + Vite              TypeScript/React
      -> schema compiler          shared generated contracts
      -> package staging          one independently versioned package at a time
```

The repository owns tool provenance, lifecycle policy, packaging, benchmarks, and verification. Language-native tools retain their language semantics. moon never replaces those tools.

## Accepted Decisions

1. `repo.sh` is the stable human and CI interface.
2. All tools are pinned in a committed lock and installed transactionally under ignored `.local/`.
3. `toolchain check-updates` discovers current stable releases without mutation; explicit updates produce reviewed lock diffs and run affected verification.
4. moon schedules coarse project tasks and optionally caches declared final outputs.
5. C/C++ uses one manifest-derived compile-action model to emit Ninja and a complete `compile_commands.json` on every native build.
6. The canonical compilation database includes first-party, generated, vendored, and external third-party translation units by default.
7. Upstream CMake, Meson, Autotools, or custom builds are optional dependency adapters, not the repository build system.
8. Go produces native binaries; Green Tea GC is the Go 1.26 default. `jsonv2` remains an explicit experimental validation profile.
9. Deno owns TypeScript checking, formatting, linting, testing, coverage, benchmarks, and local execution. Vite owns React 19 browser bundles.
10. Python applications reference one compatible, independently released runtime package. The default stripped standalone runtime makes no JIT claim; an experimental source-built JIT variant is separate.
11. Packages and runtimes are independently versioned and released. A package declares a runtime requirement such as `runtime = "python"`; resolution selects the matching host-triplet runtime release. A package release may carry a compatible runtime artifact when an incompatible runtime upgrade requires atomic distribution. No global repository tarball is required.
12. The canonical package is deterministic `tar.zst`; consumers receive a static extractor or self-extracting `.run` artifact so host Zstandard is not required.
13. `.agents/md/overview.md` is the canonical AI guide and root `AGENTS.md` is a relative symlink to it.
14. `.agents/skills/` is the tracked repository skill root. Vendor-specific `.claude/`, `.codex/`, `.grok/`, and runtime state remain ignored and machine-local.
15. Deployment groups select independently versioned packages and run directly under systemd on inexpensive VPS hosts.
16. HAProxy owns low-overhead TLS termination, exact subdomain routing, health checks, blue/green activation, and draining through durable plus runtime state. Mesh-safe service discovery supplies backend endpoints through a unicast registry or DNS-backed selector feed, and blue/green slots need not share a host.
17. Applications declare only a logical subdomain and scope; environments own TLDs, zones, certificate bindings, fleet selectors, and placement policy, while fleet-generated inventory supplies observed addresses.
18. Wildcard DNS removes per-application propagation, and a central ACME DNS-01 controller distributes wildcard certificates with HAProxy hot activation.
19. The optional `infra` profile uses OpenTofu for supported provider resources, minimal cloud-init for enrollment, and pinned isolated Ansible for host convergence.
20. Git-tracked fleet intent owns desired inventory; provider state and host probes generate observed inventory projections.
21. Host convergence and application deployment are separate lifecycles sharing selectors, bounded execution, resource locks, and signed receipts.

## Explicit Non-Goals

- No JVM build orchestrator.
- No Rust lane in the initial template.
- No replacement for Go modules, Python packaging metadata, Deno dependency semantics, or upstream complex C dependency configuration.
- No host-global mutable toolchain requirement.
- No automatic resolution of mutable `latest` artifacts during ordinary bootstrap.
- No one-runtime-per-script packaging for Python or TypeScript suites.
- No assumption that CMake is required when selected dependencies do not need it.
- No general-purpose scheduler, managed PaaS, or container runtime requirement in the initial deployment layer.
- No claim that three VPS hosts alone solve database consistency, full NYC-metro loss, or automated disaster promotion.

## Implementation Entry Gate

Start implementation with one vertical slice only:

- one C or C++ library and executable;
- one vendored or locked third-party native dependency;
- one Go executable;
- one Python application with a tiny native extension;
- one Deno React 19 application and one local Deno command;
- one schema generating C, Python, Go, and TypeScript outputs;
- one complete compilation database validated by clangd;
- two independently versioned self-contained packages, each with a deterministic archive and consumer smoke test.

Do not expand the template until this slice proves offline normal operation, runtime deduplication, package closure, and manageable configuration ownership.
