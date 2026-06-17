# Tenant Identity & LDAP — Crossplane Manifest Bridge

Companion to [architecture.md](../architecture.md) §3.1 and [roadmap.md](../roadmap.md).

## Overview

Tenant **Keycloak realm** and **LDAP (UDM)** provisioning run as Batch Jobs in
`platform-kernel`, owned by Crossplane via the **manifest bridge**:

1. Operator seeds OpenBao credentials and builds Job manifests in Go
2. Operator writes ConfigMap `tenant-{name}-provisioning-jobs` (`jobs.json`)
3. `tenant-default` emits `kubernetes.crossplane.io/Object` MRs for each Job
4. Operator **waits** for Job completion via `waitForProvisioningJob` — no direct `Job.Create` on the provision path

Details: [design/iam.md](iam.md).

## Why not `provider-keycloak` Realms yet?

Kernel OIDC clients already use `provider-keycloak` MRs. **Tenant realms** still
require browser-flow tuning, LDAP federation sync, OIDC pack role mappings, and
kernel IdP brokering — logic that lives in shell Jobs today. Until upstream
supports drift-safe Realm MRs for those settings, Jobs remain the source of truth,
with Crossplane as the owner (Object MRs).

## Jobs in the manifest bridge

| Job family | Examples |
|------------|----------|
| LDAP | `ldap-ou-{tenant}`, admin user/policy, bind accounts, portal entries, MBA groups |
| Keycloak | realm, admin, browser-flow, broker-first-login, OIDC clients/packs, ldap-sync |
| Kernel SSO | opendesk admin enable, kernel LDAP sync |

**Exception (resolved):** `keycloak-broker-idp-{tenant}` is included in the manifest bridge when a kernel realm is configured.

When an app `AppProfile` has `compositionRef`, the operator skips duplicate OIDC
client Jobs; the app Composition emits Keycloak Client MRs instead.

## Operator role

- Seed OpenBao credentials before updating the ConfigMap
- Patch `XTenant`; wait for composed Job Objects Ready
- Map Job/MR status → `Tenant.status.conditions` (`IdentityReady`, `LDAPReady`)
- On tenant **delete**, imperative cleanup Jobs may still run until XR cascade
  fully replaces them ([roadmap.md](../roadmap.md))

## Composition ordering

App Compositions that emit `ldap-search-init` or Keycloak Client MRs are gated in
`tenant-default` by **`function-sequencer`**: App claims are withheld until
Keycloak identity Jobs (`job-keycloak-.*`) and LDAP Jobs (`job-ldap-.*`) from
the manifest bridge are Ready. The operator still waits on Job completion and
maps status to `IdentityReady` / `LDAPReady`.

## Script bundle

LDAP and Keycloak Jobs share large shell scripts built in Go today
(`ldap_reconciler.go`, `identity_reconciler.go`). Extracting them to
`crossplane/scripts/tenant-identity/` as a cluster ConfigMap is tracked in
[roadmap.md](../roadmap.md).

## Testing

- Unit: `go test ./internal/controller/...`
- E2E tenant: `make e2e-p3` (shadow), `make e2e-p4` (cutover)
- Manual: `kubectl get jobs -n platform-kernel -l gentianos.io/tenant=<name>`
