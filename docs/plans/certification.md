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
| Code review | rulesets on `main` and `develop` in all three repositories require a pull request, thread resolution and the CI status; release tags are immutable; the organisation requires a second factor and defaults members to read; `CODEOWNERS` and `SECURITY.md` in each repository. Zero approvals: with one maintainer there is nobody to give one | present; review by a second person not available |
| Vulnerabilities | CodeQL default setup on all three repositories; Dependabot alerts, security updates and weekly grouped version updates for every ecosystem, against `develop`, the default branch; Actions and base images pinned by commit or digest; builds and tests install from the lockfiles (`uv sync --locked`, `npm ci`); govulncheck, pip-audit, npm audit and Trivy in CI, advisory until the backlog is triaged | present; blocking mode and a fix SLA open |
| Access | OpenFGA with fail-closed checks in bouncer, director, custodian, usher and registrar; registrar identity-action record, 400-day retention; Keycloak user and admin events on in tenant realms, no expiration set; no periodic review of who holds access, which is the record the frameworks sample | present; review record absent |
| Privileged access | TOTP is a required action at the activation of every administrator account, the platform administrator's included (`issue_admin_activation`, `identity_reconciler.go`), unless the tenant manifest sets `requireMFA: false`; there is no realm-wide rule, so a person added to an administrator group later is not forced to enrol, only asked to per person through the registrar; the operator and every provisioning Job act as Keycloak's master administrator through the `keycloak-admin` Secret (G20); OpenFGA has one shared preshared key with write rights; `/gentian:platform:admin` is bound to `cluster-admin` | partial |
| Audit log | decision log to stdout only (G10); no log shipper, no central store, no retention; Kubernetes API audit depends on the provider | absent |
| Monitoring | operator metrics behind an off-by-default ServiceMonitor; the platform installs no Prometheus, Alertmanager or probes | absent |
| Backup | `BackupPolicy`, `TenantExport`, `TenantRestore` with age encryption and signed manifests; not run on a cluster since the rework ([data-lifecycle.md](../design/data-lifecycle.md) §1); no kernel Postgres point-in-time recovery, no OpenBao snapshot, no stated recovery objectives | partial |
| Encryption | TLS at the edge through cert-manager; internal CA from OpenBao PKI; no Secret encryption in etcd (G17); OpenFGA and registrar connect with `sslmode=disable`; no workload mTLS (G8) | partial |
| Isolation | tenant namespaces default-deny with Kyverno pod security in Enforce; kernel-tier policies exist and ship off (`KERNEL_NETWORK_POLICIES`), kernel egress open (G28) | partial |
| Deletion | app purge, tenant retire and delete; OpenFGA tuples, Redis keys and tenant rights survive deletion, and an import carries approvals across clusters ([data-lifecycle.md](../design/data-lifecycle.md) §7) | partial |
| Authentication in the UIs | RS256 verification against the issuer's keys with audience and expiry tests. The ComponentProfiles the platform ships inject issuer and audience from OpenBao, set `ENVIRONMENT: production` (the desktop's does not) and expose every route with `authMode: oidc`, so the gateway's OIDC session and the bouncer (`failOpen: false`) stand in front of the backend and the tenant NetworkPolicy admits only the gateway. The code's default path without an issuer returns a synthetic administrator (`apps/*/backend/app/core/auth.py`); the platform never exercises it | present; a dev default to harden |

## 2. Work items

Framework columns name the ISO 27001:2022 Annex A control and the SOC 2
Trust Services criterion the item satisfies. Roadmap and gap numbers are
given where the item is already tracked.

| # | Item | Fix | ISO | SOC 2 | Step |
| --- | --- | --- | --- | --- | --- |
| C1 | Repository governance | **Done 2026-10-19** (gentian-os#308). Rulesets on every default and release branch: pull request, no bypass actors, status checks required, release tags immutable. Second factor required at the organisation level; default member permission `read`. Dependabot alerts and security updates on. `CODEOWNERS` and `SECURITY.md` in each repository. Third-party Actions pinned by commit SHA. CONTRIBUTING no longer asks for `Signed-off-by`. Open: one approval per pull request, which needs a second maintainer; until then the Statement of Applicability records segregation of duties as not achievable and the automated checks as the compensating control | A.8.32, A.5.15 | CC8.1, CC6.1 | done |
| C2 | Dependency hygiene | **Done 2026-10-20** (gentian-os#308) except the blocking step: scanners run advisory until the backlog is triaged, then `continue-on-error` is dropped. In place: CodeQL default setup, Dependabot alerts and security updates, the Actions ecosystem in `dependabot.yml`; `develop` is the default branch in all three repositories. Done in step 1: the `gomod`, `pip` and `npm` ecosystems, with `target-branch: develop` while `develop` is the integration branch, or a promotion of `develop` so that the default branch is what runs. Installs honour the lockfile: `uv sync --frozen`, `npm ci`, lockfile copied into the image. Base images pinned by digest and bumped by the same bot. `govulncheck`, `pip-audit` and `npm audit` as CI jobs, advisory first, blocking after the backlog is clear. Open alerts are triaged against `develop`, not the default branch | A.8.8 | CC7.1 | done |
| C3 | Fail-closed UI authentication (defence in depth) | The platform deployment is authenticated at the gateway and configured with an issuer, so this hardens the backends against a misconfiguration rather than closing a live hole: `auth.py` returns 503 on every protected route when the issuer is unset and `AUTH_DISABLED` is not explicitly true; the production validator runs by default and `local` is the opt-out; the desktop profile sets `ENVIRONMENT: production` like the other two; the admin console's extensions route requires a caller; a test asserts the anonymous path is gone. Remove the tracked scratch scripts and the dead `portal-bridge.php` from the desktop | A.8.5 | CC6.1 | 4 |
| C4 | Documentation accuracy | **Done 2026-10-20** (gentian-os#333). [security-principles.md](../security-principles.md) §6 and §7 and the kube-oidc-proxy comment describe default-deny at every layer, a joined three-log audit and API auditing as present; they are targets. Say so, and point to this plan. Create `docs/compliance/controls.md` as roadmap 5.5 specifies, one row per control with mechanism and evidence query, starting from §1 above | A.5.37 | CC2.2 | done |
| C5 | Image scanning, SBOM, signing (roadmap 5.3, G11) | **Done 2026-10-20** (gentian-os#308) in Audit: the Kyverno rule reports, and moves to Enforce once every image a cluster runs is signed; Trivy is advisory until its backlog is triaged. Signatures are written in cosign's legacy form because Kyverno 1.13 reads that one; the bundle form comes with the Kyverno upgrade. Still open: the catalogue validators requiring image digests. As built: Trivy on every image built in CI; `sbom: true` and `provenance: true` on the existing build-push steps; cosign keyless signatures; a Kyverno `verifyImages` rule in the kernel baseline, audit mode first; the catalogue validators require image digests, not tags | A.8.8, A.8.29 | CC7.1 | done |
| C6 | Administrator second factor and break-glass (roadmap 5.5) | A realm-wide rule, or a Cluster claim setting, that requires TOTP of every member of an administrator group and of the kernel realm, so that enrolment is required of people added after activation and not only of the account the installer activates; a default tenant `passwordPolicy` instead of the empty one; break-glass as a group membership with reason and expiry, removed by the registrar at expiry; a per-service OpenFGA client, read-only where the service only checks; the operator's Keycloak credential rotated and scoped (G20) | A.5.17, A.8.2, A.8.5 | CC6.1, CC6.2, CC6.3 | 4 |
| C7 | Transport encryption inside the cluster | `sslmode=require` with the OpenBao-issued CA on every kernel database connection; Argo CD behind the gateway with TLS to the server; MariaDB `require_secure_transport` for tenant engines. Workload mTLS stays G8 | A.8.24 | CC6.7 | 4 |
| C8 | Log store and alerting (roadmap 5.1, G10) | One collector per node (Vector or Fluent Bit) into an append-only store with twelve-month retention and object lock; bouncer decisions, director commits, registrar record, Keycloak events (with `eventsExpiration` set to match) and gateway access logs all keyed by the request id; the platform installs Prometheus and Alertmanager with rules for the kernel services, and the alert route is a Cluster claim field | A.8.15, A.8.16 | CC7.2, CC7.3 | 3 |
| C9 | Secret encryption at rest (G17, roadmap 1.18) | Installer step that enables the provider's KMS or an `EncryptionConfiguration` for Secrets, with a `check()` that reads one back from etcd; documented as a prerequisite where the platform cannot set it | A.8.24 | CC6.7 | 2 |
| C10 | Backups that prove themselves (roadmap 5.2) | CNPG `barmanObjectStore` for the kernel and tenant Postgres clusters; scheduled OpenBao Raft snapshots to the same bucket; a `BackupPolicy` rendered for every tenant by default; a monthly restore drill into a scratch namespace that writes a signed result; recovery point and time objectives stated per cluster in the claim and measured by the drill | A.8.13, A.8.14 | A1.2, A1.3 | 6 |
| C11 | Deletion and residency records (roadmap 5.5) | Tenant deletion removes OpenFGA tuples, Redis keys and rights; an import re-queues approvals on the receiving cluster; a signed record per deletion and export; a `residency` field on the Cluster claim shown to tenants and in exports | A.8.10, A.5.34 | C1.2, P4.3 | 5 |
| C12 | Kernel-tier isolation (G28) | Kernel NetworkPolicies on by default with the egress half derived from `requires`; a startup check that the CNI enforces NetworkPolicy | A.8.22 | CC6.6 | 4 |
| C13 | Availability baseline | Decide whether the Availability criterion is in scope for the first period. If yes: Keycloak, OpenFGA, the operator and the kernel CNPG cluster at two or more replicas with PodDisruptionBudgets, OpenBao Raft with three members, requests and limits on every kernel service. If no: say so in the system description and keep single replicas | A.8.14 | A1.1 | 7 |

## 3. Sequence

A step is done when its evidence can be pulled for an arbitrary week of the
period, not when the code merges. The order was agreed on 2026-10-20; C1
landed the day before.

1. **Vulnerability management** — C2, C4, C5. **Done 2026-10-20**, in the advisory and Audit forms the rows describe. Was gated by `develop` becoming
   the default branch, so that the scanners and Dependabot describe the code
   that runs. C4 rides along because an auditor reads the documents before
   the code.
2. **Secret encryption at rest** — C9. A cluster setting with a check, or a
   documented prerequisite where the platform cannot set it.
3. **The evidence store** — C8. The single largest build and the one that
   starts the Type II clock (roadmap 5.4): two of the five logs exist only
   on stdout today. Retention is designed for personal data from the start,
   since step 5 reaches back into it: a stated basis for the twelve months,
   and an expiry on Keycloak's events.
4. **Access control** — C6, C7, C3, C12. The realm-wide second-factor rule
   and break-glass with expiry; the master administrator credential retired
   to break-glass behind per-service clients (G20); encrypted database
   connections; the UI backends hardened; kernel-tier isolation. Rotation of
   the platform master password is a design decision for
   [architectural-decisions.md](architectural-decisions.md), not a build
   item here.
5. **Data lifecycle and data protection** — C11, with the company half
   (retention and erasure in the company services, the processing register,
   processor agreements under the GDPR and the Swiss nDSG) outside this
   repository. Both halves share the deletion record.
6. **Backups that prove themselves** — C10, as a recurring drill whose first
   run is the stress test, on the cloud cluster rather than a lab host, and
   measured against recovery objectives stated per cluster.
7. **Assurance** — C13 decided and, if in scope, built. Type I readiness
   assessment against `docs/compliance/controls.md`; the Statement of
   Applicability maps to the same rows. Then the Type II period runs.

## 4. Not planned here

The management system and its documents; supplier and personnel controls;
an annual penetration test, which both frameworks expect and which is
procured, not built; and an evidence-collection service that reads the
GitHub and provider APIs, which shortens the period but replaces none of
the items above.
