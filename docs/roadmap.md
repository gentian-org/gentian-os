# Gentian OS — Roadmap

Short index of **planned** work not yet fully implemented. For the current
platform design see [architecture.md](architecture.md). For the converged
Crossplane + operator model see [crossplane-convergence.md](crossplane-convergence.md).

---

## Crossplane & operator (remaining)

| Item | Notes |
|------|-------|
| **Remove dead operator code** | Incremental C4.5 trim after P4 green — duplicate `ensureLDAPBase` / `ensureNextcloudGroup` calls removed; deletion Jobs still operator-owned | In progress |
| **Broker IdP in manifest bridge** | `keycloak-broker-idp-{tenant}` in `jobs.json`; operator wait-only | ✅ Done |
| **P2 e2e — Pattern B kernel** | `p2-pattern-b.sh` | ✅ Done |
| **`tenant-default` render goldens** | `crossplane/tests/unit/render/tenant-default/` | ✅ Done |
| **Gateway edge remainder** | DNS (Cloudflare), ReferenceGrants, BackendTrafficPolicy, stale route cleanup — still operator-owned; cert/Gateway/HTTPRoutes in manifest bridge. |
| **`function-sequencer`** | Gate app Compositions on tenant identity/LDAP Ready instead of operator wait ordering. |
| **`Phase=Ready` vs `CrossplaneReady`** | `Phase=Ready` requires operator paths and `CrossplaneReady=True`. | ✅ Done |
| **XTenant / App schema tests** | `crossplane/tests/unit/schema/valid|invalid/` fixtures | ✅ Done |

Open-item tracker with cluster audit: [crossplane-convergence.md §3](crossplane-convergence.md).

---

## Mail & office (separate track)

Kernel-facing mail (Postfix/Dovecot virtual domains, tenant mail secrets) and
Collabora/office integration remain **operator-owned** today. Moving them into
kernel or tenant Compositions is planned as a **separate workstream** — not part
of the Crossplane convergence open-items list.

See [design/mail.md](design/mail.md) and operator `ensureMail` / `ensureOffice`.

---

## Secret rotation

Tenant and app credential rotation via annotations (for example
`gentian-os.io/rotate-credentials`) is **not implemented** in the operator
yet. Today, rotation is manual: update OpenBao paths and restart workloads, or
reconcile provisioning Jobs where applicable. A reconciler-driven rotation flow
is planned.

---

## SOC 2 hardening

SOC 2–oriented controls (audit logging, access reviews, backup verification
automation, change management evidence) are tracked here as platform hardening
work. Operational backup targets are described in [design/operations.md](design/operations.md);
formal control mapping and evidence collection are **planned**.

---

## Keycloak / `provider-keycloak` consolidation

Today **kernel** OIDC clients are Crossplane MRs (`kernel/services/keycloak-config/`);
**per-tenant** realms and many app clients are manifest-bridge Jobs (Crossplane
Object MRs); some app clients use Composition Client MRs when `compositionRef`
is set. Mid-term: migrate tenant realm lifecycle to drift-safe **`provider-keycloak`
Realm MRs** once upstream supports the required browser-flow, LDAP federation,
and broker settings — or retire MR-based paths only after a single owner is
chosen for all Keycloak objects.

---

## Related planned items (elsewhere)

| Topic | Where documented |
|-------|------------------|
| Converged architecture & open items | [crossplane-convergence.md](crossplane-convergence.md) |
| Tenant identity & LDAP (manifest bridge) | [design/tenant-identity-composition.md](design/tenant-identity-composition.md) |
| Per-app HTTP-01 issuers on `AppProfile` | [architecture.md](architecture.md) §6.1 (future); operator uses DNS-01 wildcard today |
| IntegrationBindings in Crossplane | [design/app-catalogue.md](design/app-catalogue.md) §8b |
| Gentian shell `browserProxy` / `/api/apps/…` | [gentian-ui/gentian-ui-architecture.md](../../gentian-ui/gentian-ui-architecture.md) (north star) |
| `RestoreTenant` CR | [design/operations.md](design/operations.md) §2 |
| Agentic / MCP layer | [design/agentic-ai.md](design/agentic-ai.md) |
