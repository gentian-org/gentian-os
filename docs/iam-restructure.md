# IAM Restructure — Tenant-Realm-First Architecture

## Status: Phase A complete. Phase B is the active work item (blocking re-install).

See [design/ldap-restructuring.md](design/ldap-restructuring.md) for the full
LDAP audit, defect list, and step-by-step implementation plan.

---

## 1. Current state and its problem

**Phase A (realm rename opendesk→kernel) is done.** The current cluster has:

```
master          ← admin only
kernel          ← renamed from opendesk; LDAP federation: dc=swp-ldap,dc=internal SUBTREE ❌
                  clients: portal, opendesk-dovecot, opendesk-nextcloud, opendesk-oxappsuite
                  users: ALL users from ALL tenants imported here (federation bug)
gtn-demo        ← LDAP federation: ou=users,ou=gtn-demo (correct path, but EMPTY) ❌
                  clients: gtn-demo-element, gtn-demo-jitsi, gtn-demo-ox-appsuite
gtn-demo-2      ← LDAP federation: ou=users,ou=gtn-demo-2 (correct path, but EMPTY) ❌
                  clients: gtn-demo-2-element
shared-apps     ← no LDAP federation; clients: element, opendesk-synapse
```

Two critical bugs remain:
1. **kernel realm imports all tenant users** (LDAP scope is the full tree). UMC runs in the
   kernel realm, so tenant admins see every user from every tenant and can select any tenant's
   container in the user-creation wizard.
2. **Tenant realm federation is empty** — users are placed at `ou=<tenant>` root by the
   reconciler, but federation points to `ou=users,ou=<tenant>` which has no objects. SSO for
   all tenant-realm OIDC clients is therefore broken.

---

## 2. Philosophy (unchanged)

**User identity and application registrations belong in the same realm.**

Each tenant has exactly one Keycloak realm. That realm is both:
- the **identity namespace** (users, AI agents, service accounts)
- the **application namespace** (OIDC clients for every app the tenant has installed)

The kernel realm is scoped to kernel service accounts only — no human users belong there.

```
master          ← admin only (unchanged)
kernel          ← kernel services only
                  LDAP: cn=users,dc=swp-ldap,dc=internal (one-level, service accounts only)
                  clients: portal, opendesk-nextcloud, opendesk-dovecot, opendesk-oxappsuite
                  users: none (kernel service accounts are not imported as human users)
<tenant>        ← one per tenant (e.g. gtn-demo)
                  LDAP: ou=users,ou=<tenant>,dc=swp-ldap,dc=internal (one-level)
                  users: all tenant users (admin + regular)
                  OIDC clients: <tenant>-<app> for every installed app
                  UMC client registered here so tenant admins see only their own realm
shared-apps     ← no LDAP federation; shared app instances only
```

### 2.1 Dedicated vs. shared deployment mode

Apps can be deployed in one of two isolation modes, declared **per tenant per app**:

| Mode | Description | IAM topology |
|---|---|---|
| `dedicated` (default) | One deployment per tenant in its own namespace | OIDC client lives in tenant realm; no broker needed |
| `shared` | One shared deployment serving all tenants | OIDC client lives in the `shared-apps` realm; tenant realm brokers into it |

This flag is declared on the AppInstance in the Tenant CR:

```yaml
spec:
  apps:
    - profile: element
      isolationMode: dedicated   # optional; overrides AppProfile default
    - profile: nextcloud
      isolationMode: shared      # free tier
```

AppProfile declares which modes are supported and what the default is:

```yaml
spec:
  isolation:
    modes: [shared, dedicated]
    default: dedicated
```

---

## 3. Target architecture

### 3.1 LDAP structure (target — per [design/ldap-restructuring.md](design/ldap-restructuring.md))

The LDAP tree must follow the **one OU = one realm = one namespace** rule. All human users
for a tenant belong inside the `ou=users` sub-container of the tenant OU. Service accounts
and UDM groups stay at the tenant OU root.

```
dc=swp-ldap,dc=internal
├── cn=users                     ← kernel service accounts only
├── cn=groups                    ← kernel-level groups only (no cross-tenant user membership)
│
└── ou=<tenant>                  ← one per tenant
    ├── ou=users                 ← ALL human users (admin + regular users)
    │   ├── uid=admin-<tenant>   ← tenant admin (previously at OU root — must be moved)
    │   └── uid=<username>       ← regular users
    ├── uid=app-keycloak-<tenant>  ← Keycloak LDAP federation bind account
    ├── uid=app-<app>-<tenant>     ← per-app service bind accounts
    ├── cn=users_<tenant>          ← UDM group: all tenant users
    ├── cn=admins_<tenant>         ← UDM group: tenant admins
    └── cn=managed-by-attribute-*  ← per-tenant app access groups
```

Key changes from current state:
- `uid=admin-<tenant>` moves from `ou=<tenant>` root into `ou=users,ou=<tenant>`
- Regular users created via UMC now land in `ou=users,ou=<tenant>` (fixed via `settings/directory` default container)
- `cn=Domain Users` (cross-tenant) is removed; `cn=users_<tenant>` per tenant replaces it
- `managed-by-attribute-*` groups move inside `ou=<tenant>` (per-tenant isolation)

### 3.2 Keycloak realm per tenant — with LDAP federation

The realm provisioning job (`buildRealmScript`) currently creates only the realm record.
It must be extended to also register a Keycloak LDAP User Storage Provider scoped to the
tenant OU:

```
POST /admin/realms/{tenant}/components
{
  "name": "ldap",
  "providerId": "ldap",
  "providerType": "org.keycloak.storage.UserStorageProvider",
  "config": {
    "connectionUrl":  ["ldap://nubus-ldap.{kernel-ns}.svc.cluster.local:389"],
    "usersDn":        ["ou={tenant},{ldap-base}"],
    "bindDn":         ["uid=sys-keycloak-{tenant},{bind-base}"],
    "bindCredential": ["{ldap-bind-secret}"],
    "searchScope":    ["1"],          // one level — own OU only
    "importEnabled":  ["true"],
    "fullSyncPeriod": ["-1"]
  }
}
```

The LDAP bind account for Keycloak (`sys-keycloak-{tenant}`) is already provisioned by
`buildBindAccountScript` in `ldap_reconciler.go`; it just needs to be declared as a
`kernelRequirements.identity.ldap` entry on the "kernel-keycloak" internal consumer so
the reconciler creates the `users/ldap` object and seeds the password into OpenBao.

### 3.3 Kernel realm (renamed from `opendesk`)

The `opendesk` realm is renamed `kernel` everywhere:
- Nubus configmap: `KEYCLOAK_REALM: kernel`
- `identity_reconciler.go`: `buildOpendeskAdminEnableScript` / `buildRealmDisableScript`
  use the hardcoded string `"opendesk"` → replaced by `r.KernelRealm` (new field)
- `update.sh`: `_trigger_keycloak_ldap_sync` hardcodes `kc_realm="opendesk"` → `"kernel"`
- `install.sh`: comment references to `opendesk_standard` LDAP profile remain (those are
  UDM attribute names from Univention, not our realm name — no change needed)
- `install.env.template`: add optional `KERNEL_REALM` variable (default: `kernel`)

### 3.4 Tenant admin enable/disable flow

The `ensureOpendeskAdminEnableJob` currently re-enables the tenant admin user in the
`opendesk` realm after the LDAP shadowExpire race. With the rename, this job targets
the `kernel` realm (same logic, new realm name via `r.KernelRealm`).

Once per-tenant LDAP federation is in place, tenant admins live in the tenant realm, not
the kernel realm. The enable job will target the tenant realm directly and no longer
need to address the kernel realm at all. This is a follow-up simplification, not part
of this plan.

---

## 4. Implementation plan

### Phase A — Rename `opendesk` → `kernel` ✅ COMPLETE

Realm is named `kernel` in the live cluster. All code references have been updated.

---

### Phase B — Fix LDAP structure + per-tenant federation (blocking re-install)

See [design/ldap-restructuring.md](design/ldap-restructuring.md) for the full
step-by-step plan. Summary:

**B.1 — Fix user placement (ldap_reconciler.go)**
- `buildAdminUserScript`: place admin user in `ou=users,ou=<tenant>` (currently at OU root)
- `buildTenantOUScript`: change `settings/directory` default container to `ou=users,ou=<tenant>`
  so UMC places new users in the correct sub-container
- Remove stale `uid=app-keycloak` (no tenant suffix) creation

**B.2 — Fix kernel realm LDAP scope (identity_reconciler.go or Nubus values)**
- Change kernel realm LDAP federation `usersDn` from `dc=swp-ldap,dc=internal` to
  `cn=users,dc=swp-ldap,dc=internal` with `searchScope=1` (one-level)
- This prevents tenant users from appearing in the kernel realm and fixes the
  cross-tenant visibility bug in UMC

**B.3 — Register UMC OIDC client in each tenant realm (identity_reconciler.go)**
- After creating the tenant realm, register the UMC OIDC client
  (`https://portal.<domain>/univention/oidc/`) so tenant admins can authenticate
  to UMC through their tenant realm instead of the kernel realm

**B.4 — Add per-tenant managed-by-attribute groups (ldap_reconciler.go)**
- Create six `cn=managed-by-attribute-*` groups inside `ou=<tenant>` rather than
  the global `cn=groups` container

**B.5 — Fix OX connector LDAP scope (gentian-apps/profiles/ox-appsuite.yaml)**
- Set `connector.ldap.uri` to `ou=users,ou=<tenant>,dc=swp-ldap,dc=internal`
  per-tenant deployment to prevent OX contacts from leaking across tenants

**B.6 — Tenant admin enable job re-target (identity_reconciler.go)**
Once B.1–B.3 are in place, tenant admins live in the tenant realm. The
`ensureOpendeskAdminEnableJob` must target the tenant realm instead of the kernel realm.

**B.5 Remove `ensureOpendeskAdminEnableJob` (follow-up)**

Once tenant admins live in the tenant realm via LDAP federation, the shadowExpire race
no longer involves the kernel realm. The enable/disable jobs can be simplified to target
the tenant realm directly, and the separate kernel-realm enable job can be removed.
This is deferred to a follow-up PR after Phase 2 is verified in production.

---

### Phase C — Shared app instance support

Phase C adds the `isolationMode` field and proves out shared deployment with a real app.
Element/Synapse is the natural first candidate: Matrix natively supports multiple
homeservers and federation, so a shared Synapse with per-tenant namespacing is a
well-understood deployment pattern.

**C.1 `AppProfile` API extension**

Add `spec.isolation.modes []string` and `spec.isolation.default string` fields to
`AppProfile`. This is a purely additive CRD change — existing profiles gain the field
with a default of `[dedicated]` and are unaffected.

**C.2 `Tenant.spec.apps` API extension**

Add `isolationMode string` field to `AppInstance` (valid values: `dedicated`, `shared`).
Omitting the field inherits the `AppProfile` default.

**C.3 Identity reconciler — shared realm provisioning**

When `isolationMode == shared`, the reconciler:
1. Provisions the platform-level `shared-apps` realm if absent (idempotent, one realm
   for all shared app types).
2. Registers an OIDC client named after the app type (e.g. `element`) in `shared-apps`
   instead of in the tenant realm.
3. Registers an identity broker in the tenant realm pointing to `shared-apps`, so users
   authenticate via their tenant realm and the token is issued by `shared-apps`.

Client IDs within `shared-apps` are unique by construction (app type names are unique).
Adding a second shared app type adds a client to the same realm, not a new realm.

**C.4 Composition — conditional chart deployment**

The Crossplane composition for each app checks `isolationMode`:
- `dedicated`: deploys as today, one Helm release per tenant namespace.
- `shared`: deploys a single platform-level Helm release (idempotent across tenants).
  Per-tenant config (quota, display name) is injected via a separate values overlay
  rather than a new release.

**C.5 Shared Element trial**

Before wiring the full composition abstraction, validate the shared path manually:
1. Deploy a single `opendesk-synapse` release in the `platform-kernel` namespace.
2. Register an `element` OIDC client in the `shared-apps` realm.
3. Configure identity broker in `gtn-demo` realm → `shared-apps`.
4. Point `gentian-apps/profiles/element.yaml` at the shared issuer.
5. Verify that `gtn-demo` users can authenticate into the shared Element instance
   without a second login prompt.

This validates the IAM topology before investing in the composition scaffolding.

---

## 5. Files changed — summary

| File | Phase | Change |
|---|---|---|
| `internal/controller/identity_reconciler.go` | A | Add `KernelRealm` field; replace 4 hardcoded `"opendesk"` strings; rename job name function |
| `internal/controller/identity_reconciler_test.go` | A | Update 2 job name assertions |
| `cmd/main.go` | A | Wire `KERNEL_REALM` env var into reconciler |
| `charts/gentian-os/templates/deployment.yaml` | A | Add `KERNEL_REALM` env var |
| `charts/gentian-os/values.yaml` | A | Add `kernelRealm: ""` |
| `install.env.template` | A | Add commented `KERNEL_REALM=kernel` |
| `install.sh` | A | Pass `KERNEL_REALM` to envsubst; update banner |
| `update.sh` | A | Replace hardcoded `"opendesk"` in `_trigger_keycloak_ldap_sync` |
| Nubus bootstrap ConfigMap | A | `KEYCLOAK_REALM: opendesk` → `KEYCLOAK_REALM: kernel` |
| `docs/design/multi-tenancy.md` | A | Update realm name references |
| `docs/implementation-plan.md` | A | Update realm name references in identity sections |
| `internal/controller/identity_reconciler.go` | B | Extend `buildRealmScript` with LDAP federation call; wire new env vars into Job spec |
| `internal/controller/ldap_reconciler.go` | B | Add `keycloak` as bind account consumer; add `ou=users` sub-OU creation to `buildOUScript` |
| `api/v1alpha1/appprofile_types.go` | C | Add `Isolation` field |
| `api/v1alpha1/tenant_types.go` | C | Add `IsolationMode` to `AppInstance` |
| `internal/controller/identity_reconciler.go` | C | Shared realm (`shared-apps`) provisioning path |
| `crossplane/compositions/app-*.yaml` | C | Conditional dedicated/shared deployment |
| `gentian-apps/profiles/element.yaml` | C | Trial shared Element: point at `shared-apps` issuer |

---

## 6. Migration path for existing tenants

Phase A (rename) requires a one-time manual action for any live cluster:

1. In Keycloak admin UI: rename realm `opendesk` → `kernel` (Settings → Realm name).
2. Update the Nubus `keycloak-bootstrap` ConfigMap: `KEYCLOAK_REALM: kernel`.
3. Restart the Nubus keycloak-bootstrap job to re-sync.
4. Delete and re-run the `keycloak-opendesk-enable-*` Jobs for all tenants (they will
   be recreated with the new name `keycloak-kernel-enable-*` by the next reconcile).

Phase B (LDAP federation) for existing tenants: the reconciler is idempotent. After
deploying the new code, trigger a reconcile (`kubectl gentian tenants deploy {tenant}`)
for each tenant. The realm job will detect the existing realm (HTTP 200) and only add
the LDAP federation component if absent.
