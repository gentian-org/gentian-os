# Gentian OS — Roadmap

Short index of **planned** work not yet fully implemented. For the current
platform design see [architecture.md](architecture.md). For the converged
Crossplane + operator model see [crossplane-convergence.md](crossplane-convergence.md).

---

## Crossplane & operator (convergence — complete)

The Crossplane convergence programme ([crossplane-convergence.md §3](crossplane-convergence.md))
is **closed**. All provision-path resources are owned by Crossplane via the manifest bridge;
the operator seeds secrets, writes the ConfigMap, and waits.

| Item | Status |
|------|--------|
| Manifest bridge (jobs + objects) | ✅ Done |
| Broker IdP in manifest bridge | ✅ Done |
| Gateway edge (Crossplane-owned policies) | ✅ Done |
| `function-sequencer` (App claims after identity/LDAP Jobs) | ✅ Done |
| `Phase=Ready` gated on `CrossplaneReady` | ✅ Done |
| P2 e2e, render goldens, schema unit tests | ✅ Done |
| Dead operator provision-path Creates removed | ✅ Done |

**Still operator-owned by design:** tenant deletion Jobs, mail/office, portal/UMC,
Keycloak browser-security header Jobs. Not part of the convergence programme.

---

## Mail & office (separate track)

Kernel-facing mail (Postfix/Dovecot virtual domains, tenant mail secrets) and
Collabora/office integration remain **operator-owned** today. Moving them into
kernel or tenant Compositions is planned as a **separate workstream**.

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

## Keycloak / `provider-keycloak` consolidation (deferred)

**Status:** Deferred — blocked on upstream `provider-keycloak` Realm support for
browser-flow tuning, LDAP federation sync, OIDC pack role mappings, and kernel
IdP brokering.

Today **kernel** OIDC clients are Crossplane MRs (`kernel/services/keycloak-config/`);
**per-tenant** realms and many app clients are manifest-bridge Jobs (Crossplane
Object MRs); some app clients use Composition Client MRs when `compositionRef`
is set. Mid-term: migrate tenant realm lifecycle to drift-safe **`provider-keycloak`
Realm MRs** once upstream supports the required settings.

---

## Related planned items (elsewhere)

| Topic | Where documented |
|-------|------------------|
| Converged architecture & closed convergence items | [crossplane-convergence.md](crossplane-convergence.md) |
| Tenant identity & LDAP (manifest bridge) | [design/tenant-identity-composition.md](design/tenant-identity-composition.md) |
| Per-app HTTP-01 issuers on `AppProfile` | [architecture.md](architecture.md) §6.1 (future); operator uses DNS-01 wildcard today |
| IntegrationBindings in Crossplane | [design/app-catalogue.md](design/app-catalogue.md) §8b |
| Gentian shell `browserProxy` / `/api/apps/…` | [gentian-ui/gentian-ui-architecture.md](../../gentian-ui/gentian-ui-architecture.md) (north star) |
| `RestoreTenant` CR | [design/operations.md](design/operations.md) §2 |
| Agentic / MCP layer | [design/agentic-ai.md](design/agentic-ai.md) |
