# Gentian OS — Roadmap

Short index of **planned** work not yet fully implemented. For the current
platform design see [architecture.md](architecture.md). For day-two commands
see [commands.md](commands.md).

---

## Secret rotation

Tenant and app credential rotation via annotations (for example
`gentian-os.io/rotate-credentials`) is **not implemented** in the operator
yet. Today, rotation is manual: update OpenBao paths and restart workloads, or
reconcile provisioning Jobs where applicable. A reconciler-driven rotation flow
is planned.

## SOC 2 hardening

SOC 2–oriented controls (audit logging, access reviews, backup verification
automation, change management evidence) are tracked here as platform hardening
work. Operational backup targets are described in [design/operations.md](design/operations.md);
formal control mapping and evidence collection are **planned**.

---

## Keycloak / `provider-keycloak` consolidation

Today **kernel** OIDC clients are Crossplane MRs (`kernel/services/keycloak-config/`);
**per-tenant** realms and many app clients are operator Jobs, with some overlap
from app Compositions (`openidclient.keycloak.crossplane.io/Client`). Mid-term:
either migrate tenant + app client lifecycle fully to `provider-keycloak`
(declarative, drift-detected) **or** remove the provider after retiring MR-based
kernel clients — not before one path owns all Keycloak objects.

## Crossplane convergence (Phase 3b)

Dual-path tenant provisioning (operator + `XTenant` Composition in parallel) is
technical debt. The step-by-step migration plan, test strategy, and progress
tracker live in [crossplane-convergence.md](crossplane-convergence.md).

## Related planned items (elsewhere)

| Topic | Where documented |
|-------|------------------|
| Crossplane convergence / Phase 3b | [crossplane-convergence.md](crossplane-convergence.md) |
| Per-app HTTP-01 issuers on `AppProfile` | [architecture.md](architecture.md) §6.1 (future); operator uses DNS-01 wildcard today |
| IntegrationBindings in Crossplane | [design/app-catalogue.md](design/app-catalogue.md) §8b |
| Gentian shell `browserProxy` / `/api/apps/…` | [gentian-ui/gentian-ui-architecture.md](../../gentian-ui/gentian-ui-architecture.md) (north star) |
| `RestoreTenant` CR | [design/operations.md](design/operations.md) §2 |
| Agentic / MCP layer | [design/agentic-ai.md](design/agentic-ai.md) |
