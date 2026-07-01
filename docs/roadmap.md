# Gentian OS — Roadmap

Planned work not yet fully implemented. For the current platform design see
[architecture.md](architecture.md). **Stage 0** (MAC backbone) and **Stage 1**
(OpenFGA) progress are tracked in
[design/new-security-architecture.md §4](design/new-security-architecture.md#4-implementation-plan--replacing-nubus).

---

## Stage 0 — MAC backbone (new security)

Stage 0 is **complete for dev/homelab** on `feat/new-security`. Detailed status tables
and Stage 1 gap analysis: [new-security-architecture.md §4](design/new-security-architecture.md#stage-0--foundations-mac-backbone-first).

| Item | Status | Notes |
|------|--------|-------|
| Per-tenant namespace + default-deny NetworkPolicy | **Done** | `tenant-default` baseline + operator-managed allowlists |
| Kernel egress from `AppProfile.kernelRequirements` | **Done** | `kernel-access-*` NetworkPolicies |
| Contract egress from active `IntegrationBinding` | **Done** | `contract-*` NetworkPolicies intersect binding capabilities with AppGrant |
| Kyverno baseline admission | **Done** | `kernel/appsets/05-admission.yaml`, `kernel/security/kyverno/policies/` |
| **Cilium** (FQDN/L7 egress, Hubble) | Planned | Optional upgrade when standard NetworkPolicy is insufficient |
| **Service mesh + SPIFFE/SPIRE** | Planned | Stage 3 — workload mTLS for autonomous in-cluster agents |
| AppGrant-governed contract allowlists (OpenFGA) | **Done** | Stage 2 — netpolicy ∩ AppGrant; OpenFGA `granted` tuples |

### Cilium (planned)

Replace or augment the CNI with [Cilium](https://cilium.io/) when Gentian needs FQDN-based egress allowlists, L7 HTTP policy, or Hubble flow visibility. Not required for Stage 0 on clusters where the stock CNI enforces Kubernetes NetworkPolicy.

```
[ ] Evaluate Cilium as optional CNI profile in install / gentian-deployments
[ ] Migrate tenant contract allowlists to CiliumNetworkPolicy where FQDN rules are needed
[ ] Enable Hubble UI / metrics for MAC audit trails
```

### Service mesh + SPIFFE/SPIRE (planned)

Deploy a service mesh (Istio or Linkerd) and [SPIFFE/SPIRE](https://spiffe.io/) when autonomous in-cluster agents need secret-less mTLS — not for edge TLS (Envoy Gateway covers that). See [new-security-architecture.md](design/new-security-architecture.md) §3.5 and Stage 3.

```
[ ] SPIRE server + agent deployment model
[ ] Mesh control plane integrated with Gentian install
[ ] Map gentianos.io/app / tenant labels to SPIFFE ID templates
[ ] Wire mesh policy to IntegrationBinding / future AppGrant tuples
```

### MAC hardening: mediated data plane (planned)

Horizon A goal from [new-security-architecture.md](design/new-security-architecture.md): apps and Gentian-native services reach data only through **contract-mediated APIs** that pass the PEP — no standing off-PEP SQL credentials in workload pods. Depends on **AppGrant/AuthZEN** (Stage 2) and **SPIFFE/mesh** (Stage 3).

```
[ ] Gentian-native stores (portal shell, audit, notifications) — tenant-scoped DB today; add store sidecar / proxy next
[ ] Postgres proxy pilot on one catalogue app (e.g. app-store)
[ ] Service/port-scoped kernel egress (Cilium FQDN) replacing coarse infra-namespace allowlists
[ ] Catalogue-wide: apps connect via localhost proxy; operator holds DATABASE_URL
```

Per-tenant **`{tenant}_shell`** databases (settings, audit, notifications) are **Done** — same CNPG pattern as catalogue apps; portal API resolves `portal-shell-{tenant}` Secrets.

---

## Stage 1 — Identity + authorization (Suze)

**Suze** (**S**ecure **U**niversal **Z**ero-trust **E**nvironment) is the Gentian IdP Crossplane
composite — Keycloak plus OpenFGA. Status tables and gap analysis:
[design/new-security-architecture.md §4](design/new-security-architecture.md#stage-1--identity--authorization-core).

---

## Gentian Admin Console (replaces UMC)

**Design:** [design/admin-console.md](design/admin-console.md) · **IAM:** [design/iam.md](design/iam.md)

The Univention Management Console (UMC) is **not** part of the Suze path. User/group
administration, tenant notifications, and member onboarding move to the **Gentian Admin
Console** — shell builtin apps backed by a Gentian BFF (Keycloak Admin API + Gentian
kernel services).

| Phase | Deliverable | Status |
|-------|-------------|--------|
| **P0** | Suze bootstrap: kernel + tenant realm Jobs; group taxonomy; platform + tenant admin users | **Done** — [admin-console.md §8.1](design/admin-console.md#81-p0--p1-status) |
| **P1** | Admin Console BFF: Members + Groups (Keycloak Admin API, tenant-scoped) | **Done** (`gentian-ui`, deploy pending) |
| **P2** | Invite + password reset (`inviteEmail`, Gentian email theme) | **Done** (`gentian-ui`) — [admin-console.md §8.2](design/admin-console.md#82-p2-status) |
| **P3** | Per-user TOTP enablement | **Done** (`gentian-ui`) — [admin-console.md §8.3](design/admin-console.md#83-p3-status) |
| **P4** | Security policies — password, session, lockout, MFA realm rules | **Done** (`gentian-ui`) — [admin-console.md §8.4](design/admin-console.md#84-p4-status) |
| **P5** | Sessions — list/revoke; auto-revoke on member disable | **Done** (`gentian-ui`) |
| **P6** | Audit — sign-in + admin-action log, CSV/JSON export | **Done** (`gentian-ui`) |
| **P7** | `admin-notifications` gateway + publish UI | **Done** (`gentian-ui`) |
| **P8** | Provisioning controller + CloudEvents/SCIM bus | Planned |
| **P9** | OpenFGA `can_launch` for admin modules (shell tile in P1) | Planned |
| **Stage 2** | Integrations & grants, agents, access requests, federation — [admin-console.md §9](design/admin-console.md#9-stage-2--authorization-and-governance) | **Partial** — see below |

**Stage 2 — authorization (partial):**

| Item | Status |
|------|--------|
| AppGrant CRD + OpenFGA tuple sync | **Done** |
| PlatformSecurityPolicy + MAC waiver flow | **Done** |
| Admin Console — Platform + Integrations tabs | **Done** |
| Netpolicy ∩ AppGrant (contract egress) | **Done** |
| Effective access preview (tenant admin) | **Done** (`gentian-ui`) |
| AuthZEN PEP in shell BFF | **Done** — `OPENFGA_AUTHZEN_ENABLED` |
| Agent identities & delegation (RFC 8693) | Deferred |
| Gateway external auth (Envoy ext-auth → AuthZEN) | Deferred |


### Platform admin least-privilege

Bootstrap uses `gentian:platform:superadmin` with cross-tenant visibility for operational
convenience. Target state flips via cluster config `platformAdminMode: constrained` — platform
admins see tenant metadata and break-glass only, not routine cross-tenant member access.

```
[ ] BFF tenant-scope enforcement on every Admin Console route
[ ] Separate Keycloak groups: superadmin, operator, break-glass
[ ] OpenFGA relations platform#admin vs tenant#admin
[ ] Audit log on admin mutations (Keycloak admin events + BFF)
[ ] platformAdminMode: constrained cluster setting + docs
```

---

Today Crossplane owns tenant infrastructure lifecycle via Compositions and the
manifest bridge (`tenant-{name}-provisioning-jobs`: `jobs.json`, `objects.json`).
The operator seeds OpenBao credentials, writes the ConfigMap, patches `XTenant`,
and waits on composed resources.

**Still operator-owned by design:** Cloudflare DNS and stale gateway cleanup,
tenant deletion Jobs, mail/office, portal shell convergence, Keycloak
browser-security header Jobs.

Set `tenantProvisioning.crossplaneOnly: true` (`TENANT_CROSSPLANE_ONLY=true`) to
skip shared-kernel side effects (portal shell, Nextcloud group, legacy LDAP base helpers,
browser-security Jobs).

---

## Mail & office

Kernel-facing mail (Postfix/Dovecot virtual domains, tenant mail secrets) and
Collabora/office integration remain **operator-owned** today. Moving them into
kernel or tenant Compositions is a separate workstream.

See [design/mail.md](design/mail.md) and operator `ensureMail` / `ensureOffice`.

---

## Secret rotation

Tenant and app credential rotation via annotations is **not implemented** in the
operator yet. Today, rotation is manual: update OpenBao paths and restart
workloads, or reconcile provisioning Jobs where applicable.

Planned interface:

```bash
kubectl annotate tenant demo gentian-os.io/rotate-credentials=<app-name>
kubectl annotate tenant demo gentian-os.io/rotate-credentials=all
```

When implemented, a reconciler will write new credentials to OpenBao and let ESO
+ Reloader propagate the change. See [design/security.md](design/security.md).

---

## SOC 2 hardening

SOC 2–oriented controls (audit logging, access reviews, backup verification
automation, change management evidence) are platform hardening work.
Operational backup targets are in [design/operations.md](design/operations.md);
formal control mapping and evidence collection are not yet in place.

---

## Keycloak / `provider-keycloak` consolidation

**Blocked upstream** — `provider-keycloak` Realm MRs do not yet support
browser-flow tuning, LDAP federation sync, OIDC pack role mappings, and kernel
IdP brokering.

Today **kernel** OIDC clients are Crossplane MRs (`kernel/services/keycloak-config/`);
**per-tenant** realms and many app clients are manifest-bridge Jobs (Crossplane
Object MRs); some app clients use Composition Client MRs when `compositionRef`
is set. Target end state: drift-safe **`provider-keycloak` Realm MRs** for tenant
realms once upstream supports the required settings.

---

## IntegrationBindings

`IntegrationBinding` CRs are emitted via the manifest bridge; the operator
reconciles contract wiring and applies **`contract-*` NetworkPolicies** so
consumers may reach providers only for active bindings. Contract egress is
intersected with tenant-approved **AppGrant** capabilities; empty grants deny
contract NetworkPolicies.

A follow-up is full Composition-only wiring: gate on both provider and consumer
Ready, write OpenBao paths, and surface status without a separate operator loop.

See [design/app-catalogue.md](design/app-catalogue.md).

---

## Broadcast contracts

The current `IntegrationBinding` model is **point-to-point**. A possible
addition is a **broadcast bus** (NATS with per-tenant subject namespaces and
CloudEvents schemas) for pub/sub between apps. Most existing apps do not natively
produce or consume broker events; the agentic AI layer may cover overlapping
needs via MCP-driven orchestration.

---

## App catalogue delivery

Deliver the app catalogue via an OCI artefact + `Cluster` XR instead of a
separate ArgoCD Application (`gentian-appprofiles`).

---

## App catalogue security

Implement controls in [design/app-catalogue-security.md](design/app-catalogue-security.md):

- `AppProfile` validating webhook (`catalogue-tier`, registry allow-list,
  `compositionRef` / sidecar gates)
- CI policy in `gentian-apps` (schema, render goldens, registry/digest checks)
- Platform **sidecar catalogue** (`sidecarRef`) before generic sidecars in
  `app-default`
- Kyverno tier rules on prod clusters (**baseline policies shipped** in Stage 0)

---

## Commercial layer

Implementation plan:
[design/business-logic-plan.md](design/business-logic-plan.md).

| Tier | Git repo | Licence | Access |
|---|---|---|---|
| **Community** | **`gentian-org/gentian-apps`** (public) | OSS SPDX on `AppProfile` | Open catalogue + install |
| **Pro** | **`gentian-org/gentian-pro`** (private) | `license: proprietary` | **Controller entitlement** after CRM payment |

Catalogue metadata on **`AppProfile`**: [app-profile-versioning.md](design/app-profile-versioning.md).

### Near-term delivery

- **`gentian-apps`** — community profiles and charts; publish public packages to
  `ghcr.io/gentian-org`.
- **`gentian-pro`** — commercial profiles, charts, and mirrored images in one private
  repo; publish **private** GHCR packages under the same `gentian-org` org.
- **CRM (Odoo)** — customers, orders, invoices; webhooks drive fulfillment.
- **Controller** — `ProfileRequiresEntitlement()` (implemented); extend **AppCatalogue**
  and **Tenant** reconcilers to list/install Pro apps only when entitled (other tenants
  on the same cluster must not receive Pro releases).

### Future: separate GitHub / GHCR org

When commercial volume or compliance requires hard isolation, split supply chain to
**`gentian-org-pro`** (Git) and **`ghcr.io/gentian-org-pro`** (registry). Not
introduced until the Community/Pro + entitlement model is working on a single org.

```
[ ] Entitlement CR or cluster ConfigMap synced from CRM fulfillment
[ ] Tenant webhook: reject spec.apps[] for proprietary profiles without entitlement
[ ] AppCatalogue: hide or mark unavailable Pro profiles until entitled
[ ] gentian-pro ArgoCD Application (private repo) — sync profiles/charts to cluster
[ ] Optional: namespace-scoped imagePullSecret on Pro app install
[ ] Future: migrate Pro packages to ghcr.io/gentian-org-pro + gentian-org-pro Git org
```

---

## Per-app HTTP-01 issuers

Support per-app HTTP-01 issuers on `AppProfile` ([architecture.md](architecture.md)
§6.1). The operator uses DNS-01 wildcard certificates today.

---

## Gentian shell browser proxy

Cross-origin API calls the Gentian shell makes on behalf of embedded apps may use
proxy paths under `/api/apps/{name}/…` with forwarded bearer tokens. See
[gentian-ui/gentian-ui-architecture.md](../../gentian-ui/gentian-ui-architecture.md).

---

## Agentic / MCP layer

| Milestone | Capability |
|-----------|------------|
| **v1** | MCP registry + per-app `mcp:` block in AppProfile + 2–3 reference apps (Nextcloud, OpenProject, Element) exposing read-scope capabilities |
| **v2** | Shell AI assistant (Portal extension) using OIDC token exchange + cross-app aggregation queries |
| **v3** | Workflow agents (scheduled + event-driven), AppProfile generator, tenant provisioning assistant |

See [design/agentic-ai.md](design/agentic-ai.md).

---

## Operations

| Topic | Where documented |
|-------|------------------|
| Tenant identity (manifest bridge) | [design/tenant-identity-composition.md](design/tenant-identity-composition.md) |
| Admin Console (UMC replacement) | [design/admin-console.md](design/admin-console.md) |
| Gateway / ingress | [design/gateway.md](design/gateway.md) |
| `RestoreTenant` CR | [design/operations.md](design/operations.md) §2 |
| Bootstrap install | [getting-started.md](../getting-started.md) |
| Deployment environments | [deployment.md](deployment.md) |
