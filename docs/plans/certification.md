# Certification

What ISO 27001 and SOC 2 need from the code and the repositories, beyond the
written process. This plan orders the technical work so that each wave
produces an evidence population an auditor can sample, and puts the cheap
items first. It covers gentian-os, gentian-ui, gentian-apps and the GitHub
organization that holds them. The information-security management system
itself — scope, risk assessment, Statement of Applicability, policies,
supplier and personnel controls — is process work and is not planned here.

Both frameworks sample the same populations: changes, access grants and
reviews, vulnerabilities and their remediation, log entries, backups and
restores, incidents. A control that leaves no record is not a control for
either of them. A SOC 2 Type II period is six to twelve months and starts
only once the sampled controls run; the order below is the order in which
they start running.

## 1. State of the evidence populations

| Population | Mechanism today | State |
| --- | --- | --- |
| Change | GitOps through the director; commits carry a `Gentian-Authz:` trailer and are OpenPGP-signed when a key is configured; Argo CD verifies through `AppProject.spec.sourceIntegrity` | present, unverified on a cluster |
| Code review | rulesets on `main` and `develop` in all three repositories require a pull request, thread resolution and the CI status; release tags are immutable; the organization requires a second factor and defaults members to read; `CODEOWNERS` and `SECURITY.md` in each repository. Zero approvals: with one maintainer there is nobody to give one | present; review by a second person not available |
| Vulnerabilities | CodeQL default setup on all three repositories; Dependabot alerts, security updates and weekly grouped version updates for every ecosystem, against `develop`, the default branch; Actions and base images pinned by commit or digest; builds and tests install from the lockfiles (`uv sync --locked`, `npm ci`); govulncheck, pip-audit, npm audit and Trivy in CI, advisory until the backlog is triaged | present; blocking mode and a fix SLA open |
| Access | OpenFGA with fail-closed checks in bouncer, director, custodian, usher and registrar; registrar identity-action record, 400-day retention; Keycloak user and admin events on in tenant realms with a thirty-day expiry; no periodic review of who holds access, which is the record the frameworks sample | present; review record absent |
| Privileged access | A sign-in flow requires a second factor of everyone in an administrator group, in every realm; break-glass is a role holder with a reason and an expiry; the registrar holds one service account per realm, and the operator, its Jobs and provider-keycloak share the `gentian-operator` service account, which holds Keycloak's `admin` role (G20); the kernel's OpenFGA has one preshared key per service, each with write rights, and the app instance a key of its own, held by the arbiter; `/gentian:platform:admin` is bound to `cluster-admin` | partial |
| Audit log | One JSON envelope per decision and change from the bouncer and the director, the registrar's identity record, Keycloak events as JSON with a thirty-day expiry and the gateway's JSON access log, joined by the request id ([design/audit-events.md](../design/audit-events.md)); a forwarder ships them to the destination `spec.logForwarding` names; retention and immutability are the destination's, such as the Operations Console's evidence store; Kubernetes API audit depends on the provider | present, off until a destination is set; not exercised end to end |
| Monitoring | `monitoring.enabled`, a setting the director writes, composes kube-prometheus-stack with the platform's alert rules from the Cluster Composition; `monitoring.alerts.webhook` names the alert destination; the operator's ServiceMonitor comes with the stack | present, off by default; takeover of a running stack not exercised |
| Backup | `BackupPolicy`, `TenantExport`, `TenantRestore` with age encryption and signed manifests ([data-lifecycle.md](../design/data-lifecycle.md) §1); exports run on a cluster as part of app moves, scheduled backups not shown; no kernel Postgres point-in-time recovery, no OpenBao snapshot, no stated recovery objectives | partial |
| Encryption | TLS at the edge through cert-manager; internal CA from OpenBao PKI; Secret encryption in etcd is read from the API server or attested on the claim (`controlPlane.secretsAtRest`), unknown on the Aluvian cluster until the provider answers; kernel Postgres clients require TLS and do not verify the server; no workload mTLS (G8) | partial |
| Isolation | tenant namespaces default-deny with Kyverno pod security in Enforce; kernel-tier ingress policies on by default (`KERNEL_NETWORK_POLICIES`), kernel egress open (G28) | partial |
| Deletion | app purge, tenant retire and delete; OpenFGA tuples, Redis keys and tenant rights survive deletion, and an import carries approvals across clusters ([data-lifecycle.md](../design/data-lifecycle.md) §7) | partial |
| Authentication in the UIs | RS256 verification against the issuer's keys with audience and expiry tests. The ComponentProfiles the platform ships inject issuer and audience from OpenBao, set `ENVIRONMENT: production` (the desktop's does not) and expose every route with `authMode: oidc`, so the gateway's OIDC session and the bouncer (`failOpen: false`) stand in front of the backend and the tenant NetworkPolicy admits only the gateway. Without an issuer the backends refuse to start unless `AUTH_DISABLED` is true, which then returns a synthetic user (`apps/*/backend/app/core/auth.py`); production, the default, refuses that, and `local` is the opt-out | present; a dev default to harden |

## 2. Work items

Framework columns name the ISO 27001:2022 Annex A control and the SOC 2
Trust Services criterion the item satisfies. Roadmap and gap numbers are
given where the item is already tracked.

| # | Item | Fix | ISO | SOC 2 | Step |
| --- | --- | --- | --- | --- | --- |
| C1 | Repository governance | **Done 2026-10-19** (gentian-os#308). Rulesets on every default and release branch: pull request, no bypass actors, status checks required, release tags immutable. Second factor required at the organization level; default member permission `read`. Dependabot alerts and security updates on. `CODEOWNERS` and `SECURITY.md` in each repository. Third-party Actions pinned by commit SHA. CONTRIBUTING no longer asks for `Signed-off-by`. Open: one approval per pull request, which needs a second maintainer; until then the Statement of Applicability records segregation of duties as not achievable and the automated checks as the compensating control | A.8.32, A.5.15 | CC8.1, CC6.1 | done |
| C2 | Dependency hygiene | **Done 2026-10-20** (gentian-os#308) except the blocking step: scanners run advisory until the backlog is triaged, then `continue-on-error` is dropped. In place: CodeQL default setup, Dependabot alerts and security updates, the Actions ecosystem in `dependabot.yml`; `develop` is the default branch in all three repositories. Done in step 1: the `gomod`, `pip` and `npm` ecosystems, with `target-branch: develop` while `develop` is the integration branch, or a promotion of `develop` so that the default branch is what runs. Installs honor the lockfile: `uv sync --frozen`, `npm ci`, lockfile copied into the image. Base images pinned by digest and bumped by the same bot. `govulncheck`, `pip-audit` and `npm audit` as CI jobs, advisory first, blocking after the backlog is clear. Open alerts are triaged against `develop`, not the default branch | A.8.8 | CC7.1 | done |
| C3 | Fail-closed UI authentication (defense in depth) | **Done 2026-10-30** (gentian-os#362, gentian-ui#177). Production is the default environment and `local` the opt-out, so the anonymous path needs both `AUTH_DISABLED` and an explicit `local`; both backends refuse to start without an issuer unless `AUTH_DISABLED` is true; the desktop profile sets `ENVIRONMENT: production` like the other two; the extensions route requires a caller; tests assert the anonymous path needs the opt-out. The scratch scripts and the dead portal bridge are gone | A.8.5 | CC6.1 | 4 |
| C4 | Documentation accuracy | **Done 2026-10-20** (gentian-os#333). [security-principles.md](../security-principles.md) §6 and §7 and the kube-oidc-proxy comment describe default-deny at every layer, a joined three-log audit and API auditing as present; they are targets. Say so, and point to this plan. Create `docs/compliance/controls.md` as [#604](https://github.com/gentian-org/gentian-os/issues/604), [#605](https://github.com/gentian-org/gentian-os/issues/605), [#611](https://github.com/gentian-org/gentian-os/issues/611) specifies, one row per control with mechanism and evidence query, starting from §1 above | A.5.37 | CC2.2 | done |
| C5 | Image scanning, SBOM, signing ([#602](https://github.com/gentian-org/gentian-os/issues/602), G11) | **Done 2026-10-20** (gentian-os#308) in Audit: the Kyverno rule reports, and moves to Enforce once every image a cluster runs is signed; Trivy is advisory until its backlog is triaged. Signatures are written in cosign's legacy form because Kyverno 1.13 reads that one; the bundle form comes with the Kyverno upgrade. Still open: the catalog validators requiring image digests. As built: Trivy on every image built in CI; `sbom: true` and `provenance: true` on the existing build-push steps; cosign keyless signatures; a Kyverno `verifyImages` rule in the kernel baseline, audit mode first; the catalog validators require image digests, not tags | A.8.8, A.8.29 | CC7.1 | done |
| C6 | Administrator second factor and break-glass ([#604](https://github.com/gentian-org/gentian-os/issues/604), [#605](https://github.com/gentian-org/gentian-os/issues/605), [#611](https://github.com/gentian-org/gentian-os/issues/611)) | **Built 2026-10-30** (gentian-os#362); not yet run on a cluster. A sign-in flow in tenant realms and the kernel realm requires a second factor of anyone in a group marked `gentianRequiresMFA` and enrolls them if they have none, so the rule follows the group; shown against a real Keycloak. Default password policy: twelve characters, the last five kept. Break-glass is a `breakGlass` role holder on the Cluster claim, set by a platform administrator, with a reason and a required expiry, dropped by the operator at that time and recorded as a `break-glass` audit event. The operator, its Jobs and provider-keycloak sign in as the `gentian-operator` service account, and the master password stays with the installer and break-glass. One OpenFGA key per kernel service. Open: the service account holds Keycloak's `admin` role, so per-realm clients (G20) stay open; preshared keys cannot be read-only, which needs OpenFGA's experimental access control (G21). The group path of `platformRoles.breakGlass` stays by decision and has no expiry; no group is set by default. `requireMFA: false` exempts no administrator at sign-in | A.5.17, A.8.2, A.8.5 | CC6.1, CC6.2, CC6.3 | 4 |
| C7 | Transport encryption inside the cluster | **Built 2026-10-30** (gentian-os#362); not yet run on a cluster. Every kernel Postgres client asks for `sslmode=require`, which encrypts and does not verify the server. Argo CD serves TLS under the internal CA and the Gateway checks it. The shared MariaDB offers TLS. Open: refusing plain Postgres connections, which waits on tenant apps; `require_secure_transport` on MariaDB, which waits on ERPNext and the database Jobs. Workload mTLS stays G8 | A.8.24 | CC6.7 | 4 |
| C8 | Audit events, forwarding and monitoring ([#601](https://github.com/gentian-org/gentian-os/issues/601), G10) | **Done 2026-10-30** (gentian-os#362) except two things: the flow that switches the evidence store on, and testing. Monitoring moved into the Cluster Composition. Not yet exercised: the pinned-image rule against a real Kyverno, the release publish of the store's image digest, and a running monitoring stack taken over by the Composition. As built: the platform's sources write one JSON envelope per decision, change and identity event, joined by the request id: bouncer decisions on the edge path, director commits, the registrar's actions, Keycloak events as JSON with a thirty-day expiry, the gateway's JSON access log ([design/audit-events.md](../design/audit-events.md)). An optional forwarder ships them to a destination the Cluster claim names; retention, immutability, signed manifests and review are the destination's, such as the Operations Console's evidence store. `monitoring.enabled` installs kube-prometheus-stack with the platform's alert rules | A.8.15, A.8.16 | CC7.2, CC7.3 | 3 |
| C9 | Secret encryption at rest (G17, [#543](https://github.com/gentian-org/gentian-os/issues/543)) | **Done 2026-10-20** (gentian-os#308). Not an installer step after all: the platform runs on control planes it does not own and cannot read etcd. Pre-flight reads the API server's flag where it is visible (kubeadm, k3s, rke2) and otherwise the Cluster claim's `controlPlane.secretsAtRest` with `evidence`, a warning until it says `provider` or `cluster` (`GENTIAN_REQUIRE_SECRET_ENCRYPTION=true` refuses); `scripts/tools/rewrite-secrets.sh` re-encrypts the Secrets written before a provider was switched on; GETTING-STARTED names it a prerequisite. Open: the provider's written answer for the Aluvian cluster, recorded on its claim | A.8.24 | CC6.7 | done |
| C10 | Backups that prove themselves ([#588](https://github.com/gentian-org/gentian-os/issues/588), [#589](https://github.com/gentian-org/gentian-os/issues/589)) | CNPG `barmanObjectStore` for the kernel and tenant Postgres clusters; scheduled OpenBao Raft snapshots to the same bucket; a `BackupPolicy` rendered for every tenant by default; a monthly restore drill into a scratch namespace that writes a signed result; recovery point and time objectives stated per cluster in the claim and measured by the drill | A.8.13, A.8.14 | A1.2, A1.3 | 6 |
| C11 | Deletion and residency records ([#604](https://github.com/gentian-org/gentian-os/issues/604), [#605](https://github.com/gentian-org/gentian-os/issues/605), [#611](https://github.com/gentian-org/gentian-os/issues/611)) | Tenant deletion removes OpenFGA tuples, Redis keys and rights; an import re-queues approvals on the receiving cluster; a signed record per deletion and export; a `residency` field on the Cluster claim shown to tenants and in exports | A.8.10, A.5.34 | C1.2, P4.3 | 5 |
| C12 | Kernel-tier isolation (G28) | **Built 2026-10-30** (gentian-os#362); the re-install of the test cluster is its proof. Kernel NetworkPolicies are on by default for ingress, and step A-01 stops on a cluster whose CNI does not enforce them, proven with a connection a rule must refuse. Open: kernel egress, which needs an inventory of what each namespace reaches and a change to the generator (G28) | A.8.22 | CC6.6 | 4 |
| C13 | Availability baseline | Decide whether the Availability criterion is in scope for the first period. If yes: Keycloak, OpenFGA, the operator and the kernel CNPG cluster at two or more replicas with PodDisruptionBudgets, OpenBao Raft with three members, requests and limits on every kernel service. If no: say so in the system description and keep single replicas | A.8.14 | A1.1 | 7 |

## 3. Sequence

A step is done when its evidence can be pulled for an arbitrary week of the
period, not when the code merges. The order was agreed on 2026-10-20; C1
landed the day before.

1. **Vulnerability management** — C2, C4, C5. **Done 2026-10-20**, in the advisory and Audit forms the rows describe. Was gated by `develop` becoming
   the default branch, so that the scanners and Dependabot describe the code
   that runs. C4 rides along because an auditor reads the documents before
   the code.
2. **Secret encryption at rest** — C9. **Done 2026-10-20**: a pre-flight check
   that reads the API server where it can and the claim's attestation where it
   cannot, a rewrite tool, and the prerequisite documented. The Aluvian
   cluster's attestation waits for the provider's answer.
3. **Audit events, forwarding and monitoring** — C8. **Done 2026-10-30**, with
   two exceptions: the flow that switches the evidence store on, and testing
   on a cluster (the row names what). The platform makes every decision,
   change and sign-in a structured record and can ship it to a destination;
   monitoring is one setting. The retained, locked store is the destination's.
   The Type II clock ([#603](https://github.com/gentian-org/gentian-os/issues/603)) starts when a cluster has both the sources
   and a retaining destination.
4. **Access control** — C6, C7, C3, C12. **Built 2026-10-30**: C3 is done, and
   C6, C7 and C12 wait on the re-install of the test cluster. The rows name
   what stays open. Rotation of the platform master password is a design
   decision for [architectural-decisions.md](architectural-decisions.md), not
   a build item here.
5. **Data lifecycle and data protection** — C11, with the company half
   (retention and erasure in the company services, the processing register,
   processor agreements under the GDPR and the Swiss nDSG) outside this
   repository. Both halves share the deletion record. **Open.**
6. **Backups that prove themselves** — C10, as a recurring drill whose first
   run is the stress test, on the cloud cluster rather than a lab host, and
   measured against recovery objectives stated per cluster. **Open.**
7. **Assurance** — C13 decided and, if in scope, built. Type I readiness
   assessment against `docs/compliance/controls.md`; the Statement of
   Applicability maps to the same rows. Then the Type II period runs. **Open.**

## 4. Not planned here

The management system and its documents; supplier and personnel controls;
an annual penetration test, which both frameworks expect and which is
procured, not built; and an evidence-collection service that reads the
GitHub and provider APIs, which shortens the period but replaces none of
the items above.
