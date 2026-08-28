# Polyglot Repository Template Decision Package

Status: proposed design baseline with executable pinned pure Python, Go, TypeScript, React 19/browser-TypeScript, bootstrap, package, and tag-release slices at the repository root. Schema work and fleet deployment remain proposed.

This document set defines a cloneable, non-JVM, AI-ready monorepo template for Go, pure Python, and Deno TypeScript/React. It records both the concrete design and the reasoning rules that produced it.

## Documents

This index lists every document under `docs/`. When this page and
[CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md) disagree about what exists,
the capability matrix wins: it is the executable-scope authority and this page
is a design baseline.

Current and verified:

- [CURRENT-CAPABILITIES.md](CURRENT-CAPABILITIES.md) — the compatibility and readiness contract; the authority over this document for what is executable today.
- [ARCHITECTURE.md](ARCHITECTURE.md) — repository layout, commands, toolchains, build graph, packaging, validation, and implementation phases.
- [LANGUAGE-GUIDE.md](LANGUAGE-GUIDE.md) — per-language source layout, commands, and validation.
- [mpsc-queue.md](mpsc-queue.md) — Go shared-memory queue API, format version 4, ownership, recovery, ordering, performance, and verification contracts.
- [TOOLCHAIN-LIFECYCLE.md](TOOLCHAIN-LIFECYCLE.md) — reviewed toolchain update, qualification, recovery, and rollback workflow.
- [CI-RELEASE.md](CI-RELEASE.md) — GitHub Actions verification jobs, packaging, target archives, evidence sidecars, and the tag-gated release contract.
- [EDITOR.md](EDITOR.md) — VS Code setup against the pinned toolchain and configured language roots.
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — bootstrap, Buck daemon, cache, and language-tool recovery.
- [IMPROVEMENT-ROADMAP.md](IMPROVEMENT-ROADMAP.md) — audit findings turned into phased work, with per-item completion status.
- [PRINCIPLES.md](PRINCIPLES.md) — durable principles, decision tree, evidence, boundaries, falsifiers, and meta-rules; each principle carries its own enforcement status.
- [../README.md](../README.md) — runnable target, lock/bootstrap, language source layout, package/runtime closure, AI guide, tests, and live two-runner CI.

Proposals — designs for systems that do not exist in this checkout:

- [proposals/DEPLOYMENT.md](proposals/DEPLOYMENT.md) — VPS application placement, package composition, systemd activation, HAProxy subdomain routing, wildcard DNS, and hot certificate delivery.
- [proposals/FLEET.md](proposals/FLEET.md) — optional infrastructure profile, inventory authority, OpenTofu provisioning, isolated Ansible convergence, host lifecycle, trust, locks, receipts, and scaling boundary.
- [proposals/FLEET-NEXT.md](proposals/FLEET-NEXT.md) — remaining provider, SLO, trust-implementation, service-policy, stateful-workload, and recovery questions.

Spikes — research, superseded or not adopted:


## Decision Summary

```text
repo.sh
  -> Buck2                        one in-graph scheduler, build/test/cache
      -> first-party go rules     Go modules/build/test
      -> first-party python rules pure Python build/test
      -> first-party deno rules   TypeScript/React (Deno + Vite)
      -> schema compiler          proposed shared generated contracts
      -> first-party package()    one independently versioned package at a time
```

The repository owns tool provenance, lifecycle policy, packaging, benchmarks, and verification. Language-native tools retain their language semantics. Buck2 never replaces those tools; it schedules and caches first-party rules that call them.

## Accepted Decisions

1. `repo.sh` is the stable human and CI interface.
2. All tools are pinned in a committed lock and installed transactionally under ignored `.local/`.
3. Normal bootstrap consumes exact committed pins; reviewed lock changes use the manual `toolchain-lock`/`toolchain-qualify` lifecycle rather than an automatic mutable-artifact updater.
4. Buck2 is the sole scheduler. It builds, tests, and caches every first-party rule in one graph. First-party rules live under `rules/`. `toolchains/lock.bzl` defines in-graph toolchains from `tools.lock.toml`. No coarse task runner invokes separate language build tools.
5. Go 1.27 produces native binaries with the release's default runtime and language settings.
6. Deno owns the implemented TypeScript checking, formatting, linting, testing, and local execution; Vite owns React 19 browser bundles.
7. Python applications reference one compatible, independently released runtime package. The default stripped standalone runtime makes no JIT claim; an experimental source-built JIT variant is separate.
8. Packages and runtimes are independently versioned and released. Package runtime requirements are declared in `packages/catalog.bzl`; resolution selects the matching host-triplet runtime closure. No global repository tarball is required.
9. The implemented package format is deterministic `.tar.gz`, with consumer smoke checks from clean extraction.
10. `.agents/md/overview.md` is the canonical AI guide and root `AGENTS.md` is a relative symlink to it.
11. `.agents/skills/` is the repository skill root when repository-owned skills are added. Vendor-specific `.claude/`, `.codex/`, `.grok/`, and runtime state remain ignored and machine-local.
12. Deployment groups select independently versioned packages and run directly under systemd on inexpensive VPS hosts.
13. HAProxy owns low-overhead TLS termination, exact subdomain routing, health checks, blue/green activation, and draining through durable plus runtime state. Mesh-safe service discovery supplies backend endpoints through a unicast registry or DNS-backed selector feed, and blue/green slots need not share a host.
14. Applications declare only a logical subdomain and scope; environments own TLDs, zones, certificate bindings, fleet selectors, and placement policy, while fleet-generated inventory supplies observed addresses.
15. Wildcard DNS removes per-application propagation, and a central ACME DNS-01 controller distributes wildcard certificates with HAProxy hot activation.
13. The optional `infra` profile uses OpenTofu for supported provider resources, minimal cloud-init for enrollment, and pinned isolated Ansible for host convergence.
17. Git-tracked fleet intent owns desired inventory; provider state and host probes generate observed inventory projections.
18. Host convergence and application deployment are separate lifecycles sharing selectors, bounded execution, resource locks, and signed receipts.

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

- one Go executable;
- one pure Python application;
- one Deno React 19 application and one local Deno command;
- one self-contained package with a deterministic archive and consumer smoke test.

Do not expand the template until this slice proves offline normal operation, runtime deduplication, package closure, and manageable configuration ownership.
