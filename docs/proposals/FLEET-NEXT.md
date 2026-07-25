# Next Design Domain: VPS Fleet And Host Administration

Status: proposed, and open questions only. Nothing here is implemented; the
fleet substrate itself remains a proposal in [FLEET.md](FLEET.md), and
application activation/routing in [DEPLOYMENT.md](DEPLOYMENT.md). The only
executable deployment surface in this repository is the read-only
`//infra/deploy` plan/observation contract.

[FLEET.md](FLEET.md) settles the substrate and [PRINCIPLES.md](../PRINCIPLES.md)
P15-P18 state the durable rules. This file adds nothing to either: it holds only
the workload- and implementation-specific choices that those documents
deliberately leave open. Do not restate an accepted decision here. Where a
question below touches a settled decision, it asks only which implementation
satisfies it.

## Questions To Resolve Together

### Fleet Shape And SLOs

- How many VPS hosts exist now and at expected scale?
- Which exact providers/facilities and Linux distribution satisfy purchase-time availability?
- Are hosts cattle, pets, or a deliberate mixture?
- Are workloads stateless, stateful, or replicated?
- Is there one operator or several?

### Trust Implementation

FLEET.md scopes the trust contract and states that the exact SSH CA
implementation and credential store remain selections. Those selections are:

- Which SSH CA/short-lived credential implementation and secret store satisfy that contract?
- How often are controller identities, provider tokens, and certificate keys rotated?
- What independent remote sink provides tamper-evident receipt custody?

### Host Implementation Selections

FLEET.md already accepts pinned isolated Ansible under `.local/infra/` and
OpenTofu for supported providers. Only the exact versions and coverage remain:

- Which base distribution/version is pinned?
- Which OpenTofu providers cover selected hosts, and which remain externally provisioned?
- Which exact standalone Python and Ansible releases are pinned under `.local/infra/`?
- Which host profiles and tuning policies are required by measured workloads?

### Stateful Workloads And Migrations

- Database/schema migration ordering and rollback limits?
- Compatibility across binaries, configs, persistent data, and protocols?
- Side-by-side ports and canary instances?

### Deployment Operations Still To Resolve

- Controller custody, backup, and high-availability policy?
- Maintenance windows and operator approval boundaries?
- Host replacement and inventory mutation workflow?
- Disaster-promotion authority and safeguards?
- How are long-running migrations paused, resumed, and audited?

### Service Policy Details

- Which unit templates and sandbox directives become the fleet baseline?
- Which resource requests are app-declared versus fleet-policy capped?
- Which services benefit from socket activation or watchdogs?
- Config ownership and environment file policy?
- Log retention and structured journal export?

### Administration

- What generated read-only status index, if any, improves UX without becoming a mutable desired-state database?
- Which drift categories are automatically repairable versus approval-gated?
- What default concurrency, timeout, and failure budgets satisfy measured provider and host behavior?
- Reboot orchestration and kernel/security updates?
- Disk pressure, backup verification, restore drills, and certificate expiry?
- Host quarantine, drain, replace, and decommission workflows?

### Observability

- Metrics, logs, traces, health endpoints, and alert ownership?
- Deployment/version labels in every signal?
- Per-host and fleet-wide views?
- SLOs and rollback triggers?
- Audit evidence for administrative actions?

## Principles

There are no separate principles for this domain. The durable rules are
[PRINCIPLES.md](../PRINCIPLES.md) P15-P18 plus FLEET.md's accepted decisions;
restating them here would create a second source of truth. Two rules that are
specific to answering the questions above, and are not stated elsewhere:

1. Multi-version support includes configs, data formats, protocols, services, and observability—not only directories.
2. Migrations may make rollback asymmetric, so each stateful workload declares its own rollback limit rather than inheriting a fleet default.

## Suggested Conversation Order

1. Select exact providers, facilities, base OS, and workload SLOs.
2. Pin the exact OpenTofu provider, standalone Python, Ansible, and `lego` releases. FLEET.md already accepts `lego` as the ACME client; only its version is open.
3. Define retention, garbage collection, and rollback policy within the accepted `/opt/solution/` layout.
4. Finalize service sandboxing, resource caps, health, and logging policy.
5. Set rollout concurrency, failure budgets, and maintenance policy from measurements.
6. Select credential, receipt, observability, backup, recovery, and decommissioning implementations.

Do not reopen accepted substrate choices without contrary implementation evidence. Revisit the lack of an agent or scheduler only when measured placement, churn, recovery, or rollout needs exceed direct bounded execution.
