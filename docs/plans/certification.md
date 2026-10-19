# Certification

What ISO 27001 and SOC 2 need from the code and the repositories, beyond the
written process. [Roadmap §5](../roadmap.md) states the targets;
[security-gap-closing.md](security-gap-closing.md) lists the gaps found by
reading the code. This plan orders the technical work so that each wave
produces an evidence population an auditor can sample, and puts the cheap
items first. It covers gentian-os, gentian-ui, gentian-apps and the GitHub
organisation that holds them. The information-security management system
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
| Code review | gentian-os ruleset on `main`/`develop` requires a pull request and zero approvals; gentian-ui and gentian-apps have no ruleset; the organisation does not require a second factor; CONTRIBUTING asks for `Signed-off-by` and nothing checks it | absent as a control |
| Vulnerabilities | no dependency, image or code scanner in any workflow; Dependabot alerts off on every repository; lockfiles present and bypassed by `uv pip install -e .`, `npm install` and `pip install -e` in Dockerfiles and CI; base images pinned by tag | absent |
| Access | OpenFGA with fail-closed checks in bouncer, director, custodian, usher and registrar; registrar identity-action record, 400-day retention; Keycloak user and admin events on in tenant realms, no expiration set | present |
| Privileged access | TOTP required of the tenant administrator only, switchable per tenant; no cluster-wide setting for platform administrators; the operator and every provisioning Job act as Keycloak's master administrator (G20); OpenFGA has one shared preshared key with write rights; `/gentian:platform:admin` is bound to `cluster-admin` | partial |
| Audit log | decision log to stdout only (G10); no log shipper, no central store, no retention; Kubernetes API audit depends on the provider | absent |
| Monitoring | operator metrics behind an off-by-default ServiceMonitor; the platform installs no Prometheus, Alertmanager or probes | absent |
| Backup | `BackupPolicy`, `TenantExport`, `TenantRestore` with age encryption and signed manifests; not run on a cluster since the rework ([data-lifecycle.md](../design/data-lifecycle.md) §1); no kernel Postgres point-in-time recovery, no OpenBao snapshot, no stated recovery objectives | partial |
| Encryption | TLS at the edge through cert-manager; internal CA from OpenBao PKI; no Secret encryption in etcd (G17); OpenFGA and registrar connect with `sslmode=disable`; no workload mTLS (G8) | partial |
| Isolation | tenant namespaces default-deny with Kyverno pod security in Enforce; kernel-tier policies exist and ship off (`KERNEL_NETWORK_POLICIES`), kernel egress open (G28) | partial |
| Deletion | app purge, tenant retire and delete; OpenFGA tuples, Redis keys and tenant rights survive deletion, and an import carries approvals across clusters ([data-lifecycle.md](../design/data-lifecycle.md) §7) | partial |
| Authentication in the UIs | RS256 verification against the issuer's keys with audience and expiry tests; a missing `OIDC_ISSUER` yields a synthetic administrator in all three gentian-ui backends (`apps/*/backend/app/core/auth.py`), and the production validator runs only when the chart sets `ENVIRONMENT`, which defaults to `local` | fail-open |

## 2. Work items

Framework columns name the ISO 27001:2022 Annex A control and the SOC 2
Trust Services criterion the item satisfies. Roadmap and gap numbers are
given where the item is already tracked.

| # | Item | Fix | ISO | SOC 2 | Wave |
| --- | --- | --- | --- | --- | --- |
| C1 | Repository governance | Rulesets on every default and release branch: pull request, one approval, no bypass actors, status checks required. Second factor required at the organisation level; default member permission `read`. Dependabot alerts and security updates on. `CODEOWNERS` and `SECURITY.md` in each repository. Third-party Actions pinned by commit SHA. CI checks `Signed-off-by` or CONTRIBUTING drops the requirement | A.8.32, A.5.15 | CC8.1, CC6.1 | 0 |
| C2 | Dependency hygiene | Renovate or Dependabot configuration per repository. Installs honour the lockfile: `uv sync --frozen`, `npm ci`, lockfile copied into the image. Base images pinned by digest and bumped by the same bot. `govulncheck`, `pip-audit` and `npm audit` as CI jobs, advisory first, blocking after the backlog is clear | A.8.8 | CC7.1 | 0 |
| C3 | Fail-closed UI authentication | `auth.py` refuses to start, or returns 503 on every protected route, when the issuer is unset and `AUTH_DISABLED` is not explicitly true; the production validator runs by default and `local` is the opt-out; the admin console's extensions route requires a caller; a test asserts the anonymous path is gone. Remove the tracked scratch scripts and the dead `portal-bridge.php` from the desktop | A.8.5 | CC6.1 | 0 |
| C4 | Documentation accuracy | [security-principles.md](../security-principles.md) §6 and §7 and the kube-oidc-proxy comment describe default-deny at every layer, a joined three-log audit and API auditing as present; they are targets. Say so, and point to this plan. Create `docs/compliance/controls.md` as roadmap 5.5 specifies, one row per control with mechanism and evidence query, starting from §1 above | A.5.37 | CC2.2 | 0 |
| C5 | Image scanning, SBOM, signing (roadmap 5.3, G11) | Trivy on every image built in CI; `sbom: true` and `provenance: true` on the existing build-push steps; cosign keyless signatures; a Kyverno `verifyImages` rule in the kernel baseline, audit mode first; the catalogue validators require image digests, not tags | A.8.8, A.8.29 | CC7.1 | 1 |
| C6 | Administrator second factor and break-glass (roadmap 5.5) | One Cluster claim setting that requires TOTP of every member of the platform-administrator group and the kernel realm; a default tenant `passwordPolicy` instead of the empty one; break-glass as a group membership with reason and expiry, removed by the registrar at expiry; a per-service OpenFGA client, read-only where the service only checks; the operator's Keycloak credential rotated and scoped (G20) | A.5.17, A.8.2, A.8.5 | CC6.1, CC6.2, CC6.3 | 1 |
| C7 | Transport encryption inside the cluster | `sslmode=require` with the OpenBao-issued CA on every kernel database connection; Argo CD behind the gateway with TLS to the server; MariaDB `require_secure_transport` for tenant engines. Workload mTLS stays G8 | A.8.24 | CC6.7 | 1 |
| C8 | Log store and alerting (roadmap 5.1, G10) | One collector per node (Vector or Fluent Bit) into an append-only store with twelve-month retention and object lock; bouncer decisions, director commits, registrar record, Keycloak events (with `eventsExpiration` set to match) and gateway access logs all keyed by the request id; the platform installs Prometheus and Alertmanager with rules for the kernel services, and the alert route is a Cluster claim field | A.8.15, A.8.16 | CC7.2, CC7.3 | 2 |
| C9 | Secret encryption at rest (G17, roadmap 1.18) | Installer step that enables the provider's KMS or an `EncryptionConfiguration` for Secrets, with a `check()` that reads one back from etcd; documented as a prerequisite where the platform cannot set it | A.8.24 | CC6.7 | 2 |
| C10 | Backups that prove themselves (roadmap 5.2) | CNPG `barmanObjectStore` for the kernel and tenant Postgres clusters; scheduled OpenBao Raft snapshots to the same bucket; a `BackupPolicy` rendered for every tenant by default; a monthly restore drill into a scratch namespace that writes a signed result; recovery point and time objectives stated per cluster in the claim and measured by the drill | A.8.13, A.8.14 | A1.2, A1.3 | 2 |
| C11 | Deletion and residency records (roadmap 5.5) | Tenant deletion removes OpenFGA tuples, Redis keys and rights; an import re-queues approvals on the receiving cluster; a signed record per deletion and export; a `residency` field on the Cluster claim shown to tenants and in exports | A.8.10, A.5.34 | C1.2, P4.3 | 2 |
| C12 | Kernel-tier isolation (G28) | Kernel NetworkPolicies on by default with the egress half derived from `requires`; a startup check that the CNI enforces NetworkPolicy | A.8.22 | CC6.6 | 2 |
| C13 | Availability baseline | Decide whether the Availability criterion is in scope for the first period. If yes: Keycloak, OpenFGA, the operator and the kernel CNPG cluster at two or more replicas with PodDisruptionBudgets, OpenBao Raft with three members, requests and limits on every kernel service. If no: say so in the system description and keep single replicas | A.8.14 | A1.1 | 3 |

## 3. Waves

A wave is done when its evidence can be pulled for an arbitrary week of the
period, not when the code merges.

**Wave 0 — settings and small code.** C1, C2, C3, C4. GitHub settings, a
handful of config and auth changes, a doc correction. No architecture
change. After this wave every merge, dependency bump and authentication
decision leaves a record.

**Wave 1 — identity and supply chain.** C5, C6, C7. Signed and scanned
images, enforced second factor, break-glass with expiry, encrypted database
connections. After this wave the privileged-access and vulnerability
populations are complete.

**Wave 2 — the evidence store.** C8, C9, C10, C11, C12. The log store is
the single largest build item and the one that starts the Type II clock
(roadmap 5.4). Backups and deletion records join it so that the same store
holds every population.

**Wave 3 — assurance.** C13 decided and, if in scope, built. Type I
readiness assessment against `docs/compliance/controls.md`; the Statement
of Applicability maps to the same rows. Then the Type II period runs.

## 4. Not planned here

The management system and its documents; supplier and personnel controls;
an annual penetration test, which both frameworks expect and which is
procured, not built; and an evidence-collection service that reads the
GitHub and provider APIs, which shortens the period but replaces none of
the items above.
