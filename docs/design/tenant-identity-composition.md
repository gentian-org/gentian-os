# Tenant Identity — Crossplane Manifest Bridge

Companion to [architecture.md](../architecture.md) §3.1, [iam.md](iam.md), and [admin-console.md](admin-console.md).

## Overview

Tenant **Keycloak realm** provisioning runs as Batch Jobs in `platform-kernel`, owned by Crossplane via the **manifest bridge**:

1. Operator seeds OpenBao credentials and builds Job manifests in Go
2. Operator writes ConfigMap `tenant-{name}-provisioning-jobs` (`jobs.json`)
3. `tenant-default` emits `kubernetes.crossplane.io/Object` MRs for each Job
4. Operator **waits** for Job completion via `waitForProvisioningJob`

Identity is **Keycloak-native per tenant**. No directory replication or
legacy identity modes are supported on new installs.

## Why not `provider-keycloak` Realm MRs yet?

Kernel OIDC clients already use `provider-keycloak` MRs. **Tenant realms** still require browser-flow tuning, IdP brokering, OIDC pack role mappings, and entitlement group wiring — logic that lives in shell Jobs today. Until upstream supports drift-safe Realm MRs for those settings, Jobs remain the source of truth, with Crossplane as the owner (Object MRs).

## Jobs in the manifest bridge

### Keycloak-native (default)

| Job family | Examples |
|------------|----------|
| Keycloak | `keycloak-realm-{tenant}`, `keycloak-admin-{tenant}`, OIDC client/pack Jobs |
| Groups | Tenant realm bootstrap: `gentian:tenant:<t>:*` groups |

Tenant admin user is a **native Keycloak user** in the tenant realm.

When an `AppProfile` has `compositionRef`, the operator skips duplicate OIDC client Jobs; the app Composition emits Keycloak Client MRs instead. OIDC packs for catalogue apps are declared in `gentian-apps` and applied by pack Jobs.

## Operator role

- Seed OpenBao credentials before updating the ConfigMap
- Patch `XTenant`; wait for composed Job Objects Ready
- Map Job/MR status → `Tenant.status.conditions` (`IdentityReady`)
- On tenant **delete**, imperative cleanup Jobs may still run until XR cascade fully replaces them ([roadmap.md](../roadmap.md))

## Composition ordering

App Compositions that emit Keycloak Client MRs are gated in `tenant-default` by **`function-sequencer`**: App claims are withheld until identity Jobs from the manifest bridge are Ready.

**Keycloak-native ordering:** Tenant realm → groups → tenant admin user → broker IdP in kernel realm → OIDC pack Jobs.

The operator waits on Job completion and maps status to `IdentityReady`.

## Script bundle

Keycloak Jobs share large shell scripts built in Go today (`identity_reconciler.go`). Extracting them to `crossplane/scripts/tenant-identity/` as a cluster ConfigMap is tracked in [roadmap.md](../roadmap.md).

## Testing

- Unit: `go test ./internal/controller/...`
- E2E tenant: `make e2e-p3` (shadow), `make e2e-p4` (cutover)
- Manual: `kubectl get jobs -n platform-kernel -l gentianos.io/tenant=<name>`
