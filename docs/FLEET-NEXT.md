# Next Design Domain: VPS Fleet And Host Administration

The minimum fleet substrate is now decided in [FLEET.md](FLEET.md), and application activation/routing is defined in [DEPLOYMENT.md](DEPLOYMENT.md). This file contains only decisions that remain workload- or implementation-specific.

## Questions To Resolve Together

### Fleet Shape And SLOs

- How many VPS hosts exist now and at expected scale?
- Which exact providers/facilities and Linux distribution satisfy purchase-time availability?
- Are hosts cattle, pets, or a deliberate mixture?
- Are workloads stateless, stateful, or replicated?
- Is there one operator or several?

### Trust Implementation

- Which SSH CA/short-lived credential implementation and secret store satisfy the accepted trust contract?
- How often are controller identities, provider tokens, and certificate keys rotated?
- What independent remote sink provides tamper-evident receipt custody?

### Host Implementation Selections

- Which base distribution/version is pinned?
- Which OpenTofu providers cover selected hosts, and which remain externally provisioned?
- Which standalone Python and Ansible versions are pinned under `.local/infra/`?
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

## Preliminary Principles

1. Desired state and observed state are separate artifacts.
2. Installation is content-addressed and immutable; activation is atomic.
3. A deployment is incomplete until health and version evidence converge.
4. Parallel fleet work has bounded concurrency, per-host timeouts, and aggregate stop conditions.
5. Rollback is designed before rollout, but migrations may make it asymmetric.
6. Host bootstrap and product deployment are separate lifecycles.
7. Administration commands default to read-only plans and require explicit mutation modes.
8. Every host operation records actor, target, intent, result, and repair path.
9. Secrets never enter product archives or generic logs.
10. Multi-version support includes configs, data formats, protocols, services, and observability—not only directories.

## Suggested Conversation Order

1. Select exact providers, facilities, base OS, and workload SLOs.
2. Pin OpenTofu providers, standalone Python, Ansible, and `lego`.
3. Define retention, garbage collection, and rollback policy within the accepted `/opt/solution/` layout.
4. Finalize service sandboxing, resource caps, health, and logging policy.
5. Set rollout concurrency, failure budgets, and maintenance policy from measurements.
6. Select credential, receipt, observability, backup, recovery, and decommissioning implementations.

Do not reopen accepted substrate choices without contrary implementation evidence. Revisit the lack of an agent or scheduler only when measured placement, churn, recovery, or rollout needs exceed direct bounded execution.
