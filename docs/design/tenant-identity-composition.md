# Tenant Identity — Crossplane Manifest Bridge

Companion to [architecture.md](../architecture.md) §3.1, [iam.md](iam.md), and [admin-console.md](admin-console.md).

## Overview

Tenant **Keycloak realm** provisioning runs as Batch Jobs in `platform-kernel`, owned by Crossplane via the **manifest bridge**:

1. Operator seeds OpenBao credentials and builds Job manifests in Go
2. Operator writes ConfigMap `tenant-{name}-provisioning-jobs` (`jobs.json`)
3. `tenant-default` emits `kubernetes.crossplane.io/Object` MRs for each Job
4. Operator **waits** for Job completion via `waitForProvisioningJob`

### `identityMode` paths

| `IDENTITY_MODE` | Jobs emitted | `LDAPReady` condition |
|---|---|---|
| `keycloak-native` | Keycloak realm, admin user, broker, OIDC packs (no LDAP) | Skipped (`SkippedKeycloakNative`) |
| `legacy-ldap` | LDAP OU, MBA groups, admin user/policy, bind accounts, **plus** Keycloak/LDAP sync | Waited |

See [iam.md §4](iam.md#4-identitymode-switch).

## Why not `provider-keycloak` Realm MRs yet?

Kernel OIDC clients already use `provider-keycloak` MRs. **Tenant realms** still require browser-flow tuning, IdP brokering, OIDC pack role mappings, and entitlement group wiring — logic that lives in shell Jobs today. Until upstream supports drift-safe Realm MRs for those settings, Jobs remain the source of truth, with Crossplane as the owner (Object MRs).

## Jobs in the manifest bridge

### Keycloak-native (`keycloak-native`)

| Job family | Examples |
|------------|----------|
| Keycloak | `keycloak-realm-{tenant}`, `keycloak-admin-{tenant}`, `keycloak-broker-idp-{tenant}`, OIDC client/pack Jobs |
| Groups | Tenant realm bootstrap: `gentian:tenant:<t>:*` groups (replaces LDAP MBA Jobs) |

No LDAP Jobs. Tenant admin user is a **native Keycloak user** in the tenant realm.

### Legacy LDAP (`legacy-ldap`)

| Job family | Examples |
|------------|----------|
| LDAP | `ldap-ou-{tenant}`, admin user/policy, bind accounts, portal entries, MBA groups |
| Keycloak | realm, admin, browser-flow, broker-first-login, OIDC clients/packs, ldap-sync |
| Kernel SSO | kernel LDAP sync Jobs |

When an `AppProfile` has `compositionRef`, the operator skips duplicate OIDC client Jobs; the app Composition emits Keycloak Client MRs instead.

## Operator role

- Seed OpenBao credentials before updating the ConfigMap
- Patch `XTenant`; wait for composed Job Objects Ready
- Map Job/MR status → `Tenant.status.conditions` (`IdentityReady`, `LDAPReady`)
- On tenant **delete**, imperative cleanup Jobs may still run until XR cascade fully replaces them ([roadmap.md](../roadmap.md))

## Composition ordering

App Compositions that emit Keycloak Client MRs are gated in `tenant-default` by **`function-sequencer`**: App claims are withheld until identity Jobs from the manifest bridge are Ready.

**Legacy LDAP ordering:** Keycloak realm → LDAP OU → `managed-by-attribute-*` groups → LDAP group sync → OIDC pack Jobs → LDAP user sync.

**Keycloak-native ordering:** Tenant realm → groups → tenant admin user → broker IdP in kernel realm → OIDC pack Jobs.

The operator waits on Job completion and maps status to `IdentityReady` / `LDAPReady`.

## Script bundle

LDAP and Keycloak Jobs share large shell scripts built in Go today (`ldap_reconciler.go`, `identity_reconciler.go`). Extracting them to `crossplane/scripts/tenant-identity/` as a cluster ConfigMap is tracked in [roadmap.md](../roadmap.md).

## Testing

- Unit: `go test ./internal/controller/...`
- E2E tenant: `make e2e-p3` (shadow), `make e2e-p4` (cutover)
- Manual: `kubectl get jobs -n platform-kernel -l gentianos.io/tenant=<name>`
