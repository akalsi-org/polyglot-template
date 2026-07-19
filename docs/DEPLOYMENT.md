# Application Deployment And Edge Routing

Status: the architecture below remains proposed. Its first executable slice is
now a Buck-owned, read-only deployment-plan contract: `buck2 run
//infra/deploy:example-plan` materializes and prints a deterministic plan, and
`buck2 test //infra/deploy:readonly-contract` proves that matching observed
state is ready while route drift fails verification. Execution still requires
the inventory, enrollment, trust, and readiness substrate defined in
[FLEET.md](FLEET.md).

## Target Outcome

Deploy any selected subset of independently versioned repository packages as one co-located application unit on eligible Linux hosts. Applications declare only logical exposure; environments supply domains and placement intent. Routine route, backend, and certificate changes use HAProxy runtime transactions without reload; static listener/global changes use a separately verified graceful-reload path.

## Accepted Decisions

1. Start on ordinary VPS hosts rather than a managed container platform or dedicated servers.
2. Prefer three initial failure domains: NYC primary, EWR low-latency secondary, and Ashburn quorum/disaster-recovery host.
3. Keep provider diversity where practical; do not mistake NYC and EWR for independent disaster regions.
4. Use the repo-native deployment layer behind `repo.sh`; do not adopt Kamal, Nomad, Kubernetes, Fly, or Coolify initially.
5. Run packaged applications directly under systemd. OCI images may be an optional adapter, not the canonical package format.
6. Model one deployment as a version vector selecting `n` independently released packages from the repository's `m` packages.
7. Use HAProxy Community Edition for TLS termination, subdomain routing, health checks, backend activation, and connection draining. Mesh-safe service discovery resolves application backends across eligible hosts through a unicast registry plus local fallback gateway.
8. Use wildcard DNS records so ordinary application deployment never waits for DNS propagation.
9. Applications declare a subdomain label and scope, never a TLD, zone, public IP, certificate, or provider.
10. Environments map scope to DNS zone, certificate, edge addresses, and placement.
11. Issue wildcard certificates centrally through ACME DNS-01 using narrowly scoped Cloudflare API tokens.
12. Cloudflare tokens remain only on the certificate controller. Edge hosts receive certificate material, never DNS credentials.
13. Persist every HAProxy map/certificate change to disk and apply the equivalent runtime transaction. Runtime-only state is invalid.
14. Use Cloudflare R2 for immutable release artifacts, large public objects, and backups where its storage semantics fit; keep active application state on local storage or an explicit database system.

## Explicitly Deferred Or Rejected

| Candidate | Decision | Reason |
| --- | --- | --- |
| Fly Machines | deferred adapter | Excellent lifecycle API, but metered public and cross-region egress and image-level composition conflict with current cost/package goals. |
| Kamal 2 | rejected as core | Reimplements a useful container transaction, but requires Docker and does not understand package version vectors, native systemd units, placement, or data topology. |
| Coolify/Dokploy | deferred | Convenient control planes, but introduce a privileged long-lived platform and make Compose/OCI the primary contract. |
| Nomad | deferred migration target | Task groups precisely model co-location; adopt when continuous placement, replacement, autoscaling, or affinity becomes required. |
| Kubernetes/k3s | rejected | Excess ownership and resource cost for the current fleet and workload shape. |
| Caddy/Traefik/Envoy | rejected | Higher steady-state memory or complexity than required. |
| NGINX Open Source | alternate only | Lean, but active health checks and runtime upstream management are weaker or commercial. |
| Custom reverse proxy | rejected | TLS, parsing, smuggling defense, draining, and backpressure are not an acceptable custom security surface. |

## Ownership Model

```text
package manifest
  owns: package identity, executable contract, logical subdomain, scope,
        local port, protocol, health endpoint

deployment-group manifest
  owns: selected package versions, dependencies, resource policy,
        placement intent, rollout policy

environment manifest
  owns: scope -> DNS zone, placement selectors, certificate identity,
        and references to fleet-generated host inventory

deployment compiler
  owns: resolved version vector, installation layout, systemd units,
        HAProxy backends/maps, activation transaction, receipts

certificate controller
  owns: ACME account/key, Cloudflare token, issuance, renewal,
        certificate distribution transaction

HAProxy
  owns: live edge traffic state derived from committed manifests
```

No generated systemd unit, HAProxy map, DNS record, or deployment receipt becomes an independent source of truth.

## Application Contract

An application declares a single-label subdomain and has no knowledge of the deployment TLD:

```toml
name = "orders"

[[expose]]
subdomain = "orders"
scope = "public"
protocol = "http"
port = 8080
health = "/healthz"
```

Constraints:

- `subdomain` is one validated DNS label; nested labels require an explicit future contract.
- Hostname ownership is globally unique within an environment.
- Unknown hostnames never fall through to another application.
- An application cannot request arbitrary certificate names, zones, or DNS mutations.

The environment supplies the missing context:

```toml
[domains.public]
zone = "example.com"
certificate = "public-wildcard"

[domains.internal]
zone = "internal.example.com"
certificate = "internal-wildcard"

[certificates.public-wildcard]
names = ["example.com", "*.example.com"]
issuer = "letsencrypt"
dns_provider = "cloudflare"
```

The compiler derives `orders.example.com`; the application remains reusable across development, staging, and production zones.

## Deployment Group And Version Vector

```toml
name = "ingestion"
strategy = "blue-green"
placements = ["nyc", "ewr"]

[[package]]
name = "receiver"
version = "3.2.1"

[[package]]
name = "decoder"
version = "8.0.4"
after = ["receiver"]

[[package]]
name = "writer"
version = "5.1.0"
after = ["decoder"]
```

The deployment identity is the digest of the normalized group manifest, resolved package manifests, architecture, environment policy, and generated configuration. Executable/configuration rollback restores the entire package version vector, not whichever component happened to fail last; persistent-state rollback remains migration-contract controlled.

## Installation And systemd Shape

```text
/opt/solution/
├── content/{packages,runtimes,configs}/<digest>/
├── deployments/<group>/<digest>/
│   └── manifest.json
├── active/<group> -> ../deployments/<group>/<digest>
├── previous/<group> -> ../deployments/<group>/<old-digest>
└── receipts/
```

The compiler generates one systemd target per group and one service per process within the fleet-provided unit policy. Fleet convergence owns users, privileges, directory ownership, systemd/journald baseline, HAProxy installation, mesh service discovery, and host limits. Deployment supplies executable/configuration identities, resource requests, dependencies, readiness, and version metadata.

## HAProxy Routing

DNS is provisioned once per environment:

```dns
*.example.com          A/AAAA  <public edge addresses>
<internal DNS policy remains environment-specific>
```

HAProxy uses exact host maps as the fast path:

```text
orders.example.com orders
quotes.example.com quotes
```

An unknown host never falls through to another application's backend. A dedicated fallback backend may send the request to a small local discovery gateway. That gateway queries a mesh-safe unicast registry or DNS-backed selector feed, validates the returned service identity and deployment generation, and proxies the request to the discovered endpoint. Discovery has a short bounded timeout, caches only lease-bounded positive answers, and fails closed with `503`; it never redirects clients to private mesh addresses. Successful fallback observations feed the controller's normal map reconciliation, keeping the fallback off the steady-state path.

The durable edge-state generation records host maps, certificate fingerprints, service selectors, active slots, administrative state, and drain deadlines. Equivalent desired fields are updated through the HAProxy Runtime API. Resolved unicast-discovered endpoints are leased observations, not durable desired state. A deployment succeeds only when durable and runtime desired state agree and requests through every intended edge using the intended SNI and Host header reach the expected deployment.

Use two logical backend slots per routable group. Service discovery may resolve them to different hosts. If placement co-locates slots or unrelated instances, the deployment compiler assigns or validates distinct concrete bind endpoints before activation:

```text
orders/blue  -> discovered endpoint set for old deployment
orders/green -> discovered endpoint set for new deployment
```

Ordinary rollout:

```text
resolve and verify packages
  -> stage immutable deployment
  -> generate and validate units/config
  -> start inactive slot
  -> wait for process and semantic health
  -> mark new HAProxy backend ready
  -> verify through public frontend/SNI
  -> mark old backend drain
  -> wait within bounded drain timeout
  -> switch active/previous links
  -> stop old systemd target
  -> write signed/hashed receipt
```

Failure before commit removes the new slot. Failure after traffic activation restores the previous executable/configuration version vector and route/backend state, then verifies it before reporting rollback success.

Automatic rollback does not claim to reverse persistent data changes. Database schemas, durable queues, replicated state, and irreversible external effects require an explicit migration contract declaring compatibility window, backup, pause/resume, irreversible points, restore procedure, and whether rollback is allowed. The deployer fails closed rather than report a false rollback.

## Certificate Issuance And Hot Distribution

Use centralized certificate issuance with a Cloudflare token limited to DNS edit and zone read for one named zone. Prefer separate tokens and private keys per environment/scope. The token is a certificate-controller secret and never enters packages, CI artifacts, process arguments, logs, or edge hosts. Controller execution and authority separation follow [FLEET.md](FLEET.md).

Issue apex plus wildcard certificates through ACME DNS-01:

```text
example.com + *.example.com
internal.example.com + *.internal.example.com
```

Renewal transaction:

```text
renew when remaining lifetime crosses policy threshold
  -> create DNS-01 TXT record
  -> observe authoritative propagation
  -> finalize order and remove TXT record
  -> verify key match, SAN set, chain, validity, and policy
  -> assemble HAProxy PEM
  -> deploy to one secondary edge
  -> persist PEM atomically
  -> set ssl cert + commit ssl cert through runtime socket
  -> verify fresh TLS handshake/fingerprint
  -> continue bounded rolling distribution
  -> record non-secret fleet receipt
```

HAProxy runtime certificate updates are memory-only, so persisting the PEM before the runtime transaction is mandatory. PEM files are root-owned, minimally readable by HAProxy, and separate per environment/scope. Keep the prior PEM only for the bounded rollout window. A compromised edge triggers quarantine, wildcard-key reissue, fleet redistribution, and revocation where useful.

## Regional Placement

Initial topology:

```text
NYC      primary application and latency-sensitive data
EWR      low-latency synchronous secondary
Ashburn  third voter, asynchronous data replica, disaster-recovery site
```

NYC and EWR reduce normal quorum latency but share meaningful regional risks. Ashburn supplies geographic separation at modest latency. A three-voter system cannot stay writable when both NYC and EWR are lost; that requires either five voters or an explicit guarded disaster-promotion procedure.

Do not synchronously replicate large blobs between VPS hosts. Use R2 or another declared object store. Database replication policy is database-specific and must not be inferred from host count.

## Current Read-Only Command Surface

The initial implementation intentionally has no remote transport and no
mutation commands. Deployment intent is declared by `deployment_plan()` BUCK
attributes (`group`, `environment`, native target, package version vector,
eligible hosts, and route ownership). Buck emits the normalized JSON artifact;
the read-only tool accepts only `plan`, `status`, and `verify` operations.

```text
./repo.sh buck2 run //infra/deploy:example-plan
./repo.sh buck2 test //infra/deploy:readonly-contract
python3 infra/deploy/readonly.py status --plan <plan.json> --observation <observed.json>
python3 infra/deploy/readonly.py verify --plan <plan.json> --observation <observed.json>
```

`verify` fails closed when group/environment/target/digest differ, an expected
unit is not active, or any declared route does not resolve to the plan digest.
The observation JSON is an external read-only projection, not desired-state
configuration. There is deliberately no `apply`, `rollback`, certificate, or
SSH command yet.

## Future Mutation Surface (Not Implemented)

```text
./repo.sh deploy apply <group> --env <environment>
./repo.sh deploy status [<group>] --env <environment>
./repo.sh deploy rollback <group> --to <digest> --env <environment>
./repo.sh deploy drain <host>
./repo.sh deploy verify <group> --env <environment>

./repo.sh cert plan --env <environment>
./repo.sh cert renew <certificate> --env <environment>
./repo.sh cert deploy <certificate> --env <environment>
./repo.sh cert verify [<certificate>] --env <environment>
```

Mutation commands acquire the resource-scoped locks defined in [FLEET.md](FLEET.md). Every remote action has bounded concurrency, per-host timeouts, an aggregate stop condition, and a signed hash-chained receipt copied to remote audit storage. `plan`, `status`, and `verify` are read-only.

## Verification Gates

A deployment is complete only when:

- every package checksum/attestation and target architecture match;
- the normalized version vector equals the receipt;
- systemd reports the intended units and deployment digest;
- every required process and semantic health check passes;
- durable HAProxy desired state equals runtime desired fields for maps, backend endpoints, active slots, administrative state, and drains;
- a request through the public listener reaches the expected deployment;
- the previous deployment remains a proven rollback target;
- no secret appears in generated configuration, receipts, logs, or command output.

A certificate rollout is complete only when every intended edge serves the expected fingerprint for each declared SNI, the on-disk PEM matches runtime state, and expiry monitoring observes the new validity window.

## Implementation Sequence

1. Define and validate package, group, environment, route, certificate, and receipt schemas.
2. Implement read-only planning, collision detection, and deterministic generated artifacts.
3. Implement one-host immutable install plus systemd activation and rollback.
4. Add HAProxy static configuration, runtime map updates, blue/green slots, draining, and frontend verification.
5. Add ACME DNS-01 issuance with Cloudflare least-privilege tokens and HAProxy certificate hot updates.
6. Add bounded multi-host rollout with secondary-first certificate and application policies.
7. Add drift detection, audit receipts, failure injection, and recovery drills.
8. Re-evaluate Nomad, Fly, or another platform when continuous placement/replacement, autoscaling, bin-packing, or dynamic affinity becomes a measured need.

## Review Questions

The following remain intentionally open:

1. Which concrete VPS providers and facilities satisfy NYC, EWR, and Ashburn availability at purchase time?
2. Which database and consensus systems will use the three-site topology, and what are their exact durability/latency contracts?
3. Which credential store and protected execution policy implement the accepted controller separation?
4. Are internal domains publicly delegated DNS names, split-horizon names, or names reachable only over the mesh?
5. Is automatic failover required, or is guarded operator promotion acceptable at initial scale?
6. What availability, latency, traffic, recovery-time, and recovery-point objectives trigger migration to a scheduler or managed platform?
