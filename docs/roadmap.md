# Gentian OS — Roadmap

Planned work not yet fully implemented. For the current platform design see
[architecture.md](architecture.md).

---

## Crossplane & operator

Today Crossplane owns tenant infrastructure lifecycle via Compositions and the
manifest bridge (`tenant-{name}-provisioning-jobs`: `jobs.json`, `objects.json`).
The operator seeds OpenBao credentials, writes the ConfigMap, patches `XTenant`,
and waits on composed resources.

**Still operator-owned by design:** Cloudflare DNS and stale gateway cleanup,
tenant deletion Jobs, mail/office, portal/UMC convergence, Keycloak
browser-security header Jobs.

Set `tenantProvisioning.crossplaneOnly: true` (`TENANT_CROSSPLANE_ONLY=true`) to
skip shared-kernel side effects (portal/UMC, Nextcloud group, LDAP base helpers,
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
reconciles contract wiring. A follow-up is full Composition-only wiring: gate on
both provider and consumer Ready, write OpenBao paths, apply NetworkPolicy
patches, and surface status without a separate operator loop.

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
- Kyverno tier rules on prod clusters

---

## Commercial layer

Implementation plan:
[design/business-logic-plan.md](design/business-logic-plan.md) — OSS **`gentian-apps`**,
private **`gentian-premium`** profiles, **Odoo** for customers, orders, invoices,
and entitlements.

Catalogue metadata on **`AppProfile`**: [app-profile-versioning.md](design/app-profile-versioning.md).

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
| Tenant identity & LDAP (manifest bridge) | [design/tenant-identity-composition.md](design/tenant-identity-composition.md) |
| Gateway / ingress | [design/gateway.md](design/gateway.md) |
| `RestoreTenant` CR | [design/operations.md](design/operations.md) §2 |
| Bootstrap install | [getting-started.md](../getting-started.md) |
