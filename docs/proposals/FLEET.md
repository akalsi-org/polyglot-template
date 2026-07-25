# Fleet Substrate Decisions

Status: proposed implementation baseline for the optional `infra` profile.
The first executable deployment slice is strictly read-only: Buck materializes
deployment plans and checks observed state; it does not provision, connect to,
or mutate hosts.

This document defines the minimum host substrate required by [DEPLOYMENT.md](DEPLOYMENT.md). It does not make infrastructure tooling part of the default build/package template.

## Product Boundary

The template has two profiles:

```text
core
  toolchains, build, test, lint, benchmark, package, release,
  deployment-manifest validation

infra (optional bootstrap)
  provisioning, enrollment, convergence, host administration,
  application activation, routing, certificates
```

Repositories with no production hosts never download OpenTofu, Ansible, ACME, provider, or fleet tooling.

## Accepted Decisions

1. The repository owns fleet intent, host states, selectors, validation, and the `repo.sh` interface.
2. OpenTofu owns supported provider-resource provisioning and its state.
3. Minimal cloud-init establishes secure enrollment only; it does not own ongoing host convergence.
4. A pinned, isolated Ansible controller owns host convergence. Its Python runtime lives under `.local/infra/` and never enters product packages.
5. The repository-native deployer owns application package composition and activation because existing tools do not understand the package version-vector contract.
6. Host convergence and application deployment are separate state machines sharing inventory, transport policy, selectors, bounded execution, and receipts.
7. Direct bounded SSH fan-out is the initial execution mechanism. Do not implement an always-on host agent initially.
8. A scheduler is selected by capability need, not fleet size. Known inventory with declared placement remains repo-native.
9. Git-tracked fleet intent is the authoritative desired inventory. OpenTofu outputs and host probes are observed/generated state, never parallel hand-edited truth.
10. GitHub Releases are the canonical release record and provenance surface. R2 is an optional immutable deployment mirror/cache addressed and verified by digest.
11. Controllers use separated identities for provisioning, convergence, deployment, certificate issuance, release publication, and observation even if they initially run on one workstation.
12. Use a pinned standalone ACME client with maintained Cloudflare DNS support; prefer `lego` over custom DNS hook code. Its controller-only binary size is less important than eliminating custom credential-sensitive automation.

## Authoritative Inventory (Proposed; None Of These Paths Exist)

The layout below is the proposed inventory contract, not a description of this
checkout. `infra/` currently contains only `infra/deploy/BUCK` and
`infra/deploy/readonly.py` — the read-only plan/observation contract. No file or
directory named below exists, and nothing reads them.

```text
infra/fleet.toml                 logical desired hosts and profiles
infra/tofu/                     provider resources and OpenTofu state
infra/generated/hosts.json      provider outputs joined to declared intent
infra/ansible/inventory.yml     generated projection
infra/deploy/environments/      generated/declared deployment bindings
```

Example intent:

```toml
[[host]]
name = "nyc-1"
provider = "clouvider"
region = "nyc"
failure_domain = "nyc-60-hudson"
profile = "edge-data"
provisioning = "external"

[[host]]
name = "ewr-1"
provider = "vultr"
region = "ewr"
failure_domain = "vultr-ewr"
profile = "edge-data"
provisioning = "opentofu"
```

Provider IDs and discovered addresses live in state/generated output, not handwritten back into `fleet.toml`. External/manual provisioning remains supported for providers without an acceptable OpenTofu provider.

## Host Lifecycle

```text
declared
  -> provisioned
  -> enrolled
  -> converging
  -> ready
  -> active
  -> draining
  -> quarantined
  -> decommissioned
```

Application deployment targets only `ready` or `active` hosts. Quarantine immediately blocks new deploys and certificate distribution. Every transition has a precondition, postcondition, repair action, actor, and receipt.

## Provisioning And Enrollment

OpenTofu is pinned under `.local/infra/opentofu/` and may manage VPS resources, provider firewalls, addresses, volumes, and infrastructure DNS. Destructive plans require an explicit reviewed apply path and provider-side deletion protection where available.

Cloud-init performs only the minimum enrollment work:

- create the bootstrap identity;
- establish SSH CA trust or the initial enrollment key;
- disable password and reusable root-key login;
- apply a minimal ingress firewall;
- install only the prerequisite needed for convergence;
- optionally enroll the management mesh using a short-lived one-use credential;
- emit verifiable bootstrap completion.

Interrupted or outdated cloud-init is repaired by convergence or host replacement, not by growing an untestable first-boot script.

## Access And Trust

Initial contract:

- no reusable fleet-wide root SSH private key;
- host keys are verified through enrollment, not accepted blindly;
- automation uses a dedicated deploy/converge identity;
- privileged operations use a narrow reviewed sudo policy;
- operator access and automation access are distinct;
- credentials are revocable per controller and, where practical, short-lived SSH certificates are preferred;
- provider console access is the break-glass path;
- a quarantined host is removed from mesh access and credential distribution before investigation.

The exact SSH CA implementation and credential store remain implementation selections, but these invariants are prerequisites to remote mutation.

## Host Convergence

Use pinned Ansible from an isolated standalone Python environment:

```text
.local/infra/python/
.local/infra/ansible/
.local/infra/cache/
```

Ansible owns host-level convergence for users, sudo, SSH, nftables, time synchronization, sysctl, limits, mounts, system packages, HAProxy installation, systemd baseline, log policy, monitoring, and security updates.

The repository must not create a generic remote convergence DSL or rebuild package-manager, filesystem, firewall, and service-manager semantics. Custom modules are allowed only for repository-specific invariants with no adequate existing module.

## Runtime Boundary

Fleet convergence owns:

- system users and groups;
- sudo and host permissions;
- baseline directories and mount ownership;
- systemd/journald host policy;
- HAProxy binary, base configuration, socket permissions, and service lifecycle;
- firewall and network policy.

Application deployment owns:

- immutable deployment content;
- generated application unit definitions within the fleet-provided policy;
- package version vectors and application configuration;
- application readiness and health;
- HAProxy route/backend desired state;
- activation, draining, executable rollback, and deployment receipts.

The mesh VPN carries backend traffic. Service discovery uses a small unicast registry or DNS-backed selector feed, and the local discovery gateway consumes those records over unicast. Records bind environment, group, slot, deployment digest, protocol, address, port, generation, expiry, and advertiser identity. Only placement-authorized hosts may register a service identity. HAProxy uses durable exact maps first and a local discovery gateway as a bounded fallback; deployment operations consume validated records rather than assuming that an application is local to an edge host. Blue and green slots may be placed on different eligible hosts.

## Canonical Installation Layout

```text
/opt/solution/
├── content/
│   ├── packages/<sha256>/
│   ├── runtimes/<sha256>/
│   └── configs/<sha256>/
├── deployments/<group>/<deployment-digest>/
│   └── manifest.json
├── active/<group> -> ../deployments/<group>/<digest>
├── previous/<group> -> ../deployments/<group>/<digest>
├── state/<group>/
└── receipts/
```

Mutable state never lives inside immutable content or deployment directories. Content garbage collection retains everything referenced by active, previous, pinned, or in-progress deployments.

## Controller Separation

| Identity | Authority |
| --- | --- |
| provisioner | provider resource mutation and OpenTofu state |
| converger | restricted host administration |
| deployer | package, application systemd, and HAProxy application state |
| certificate issuer | ACME account plus zone-scoped Cloudflare DNS edit |
| release publisher | GitHub release plus R2 mirror publication |
| observer | read-only provider, host, deployment, route, and certificate state |

Initial placement:

- operator workstation: interactive provisioning, convergence, and deployment;
- GitHub Actions protected environments: package/release publication, optionally guarded deployment later;
- small management VPS: certificate renewal and expiry monitoring only.

The always-on certificate controller does not receive provider provisioning or unrestricted fleet administration authority. Loss of any controller prevents changes but does not interrupt running applications.

## Locks And Receipts

Use resource-scoped locks:

```text
inventory:<environment>
host:<host>
group:<environment>/<group>
route:<fqdn>
certificate:<certificate>
```

Acquire multiple locks in the canonical order shown above. An environment-wide inventory lock is reserved for inventory/schema migrations and must not serialize unrelated application deployments.

Every mutation receipt records actor, command, intent digest, target, previous receipt hash, generation, start/end time, per-target observation, result, and repair path. Receipts exclude secret values, are signed by the acting identity, hash-chain to prior receipts in the same resource stream, and are copied to a remote append-only audit location. Host-local receipts alone are not authoritative.

## Artifact Authority

```text
GitHub Release
  canonical version, notes, checksums, attestations, provenance

R2
  optional immutable mirror/cache keyed by content digest

deploy apply
  may fetch from either but accepts only the declared digest and provenance
```

The mirror cannot promote or redefine a release. A tampered or missing mirror object fails closed or falls back to the canonical source.

## Scaling And Scheduler Boundary

Repo-native deployment remains appropriate while:

- inventory is known and reviewed;
- placement is operator-declared;
- host churn is low;
- capacity planning is human-driven;
- bounded rollout time and operational toil meet the declared SLO.

Re-evaluate a scheduler when the system must continuously select placement, replace failed capacity, autoscale, rebalance, bin-pack, or enforce dynamic affinity/anti-affinity. The trigger is capability pressure and measured toil, not a fixed host count.

Execution may evolve from direct SSH fan-out to regional relays or signed pull-based convergence without changing application manifests. If dynamic scheduling becomes necessary, compile deployment-group manifests into Nomad jobs rather than adding scheduling logic to the deployer.

## Proposed Command Surface (Not Implemented)

```text
./repo.sh bootstrap --profile infra

./repo.sh host plan [selector]
./repo.sh host provision <host>
./repo.sh host enroll <host>
./repo.sh host converge [selector]
./repo.sh host verify [selector]
./repo.sh host status [selector]
./repo.sh host drift [selector]
./repo.sh host drain <host>
./repo.sh host quarantine <host>
./repo.sh host replace <host>
./repo.sh host decommission <host>
```

Mutations have explicit concurrency, per-target timeout, failure budget, rollout order, and resume token. Read-only plan/status/drift commands never repair implicitly.

## Verification Gates

- A fresh host reaches `ready` from committed intent and minimal cloud-init.
- A second convergence reports no unintended changes.
- Revoking one automation identity blocks its future access without breaking other identities.
- Quarantine removes a host from deployment and certificate targets.
- Interrupted convergence resumes or reports a precise repair path.
- Generated inventory is reproducible from fleet intent plus provider outputs.
- Drift distinguishes desired-state violations from transient observations.
- Tampered artifacts, receipts, inventory, and provider outputs fail closed.
- Loss and restoration of each controller identity is exercised without application interruption.
