# LDAP Restructuring — Audit, Target State, and Implementation Plan

**Status:** Decision taken — implement on next cluster reinstall (Phase 2, blocking)

**Principle:** one OU = one Keycloak realm = one Kubernetes namespace per tenant.
Every object in LDAP, every token in Keycloak, and every workload in Kubernetes
must be unambiguously owned by exactly one tenant. No cross-tenant references
at the data layer.

---

## 1. Audit — Current State (as of 2026-05-28)

### 1.1 LDAP tree (actual, verified by `ldapsearch`)

```
dc=swp-ldap,dc=internal                          ← global base DN
│
├── cn=users                                      ← kernel service accounts
│   ├── uid=readonly
│   ├── uid=Administrator
│   ├── uid=ldapsearch_keycloak                  ← kernel realm bind account
│   ├── uid=ldapsearch_ox
│   ├── uid=ldapsearch_nextcloud
│   ├── uid=ldapsearch_dovecot
│   ├── uid=ldapsearch_postfix
│   ├── uid=ldapsearch_element
│   ├── uid=ldapsearch_xwiki
│   ├── uid=ldapsearch_openproject
│   ├── uid=svc-portal-server
│   └── uid=oxSystemUser
│
├── cn=groups                                     ← FLAT, SHARED — ALL TENANTS MIXED ❌
│   ├── cn=Domain Users                          ← members: admin-gtn-demo, gtn-demo-test,
│   │                                                        admin-gtn-demo-2, gtn-demo-2-test
│   ├── cn=Domain Admins
│   ├── cn=Tenant Admins
│   ├── cn=Domain Service Users
│   ├── cn=IAM API - Full Access
│   ├── cn=managed-by-attribute-Groupware        ← cross-tenant membership
│   ├── cn=managed-by-attribute-Fileshare
│   ├── cn=managed-by-attribute-FileshareAdmin
│   ├── cn=managed-by-attribute-Videoconference
│   ├── cn=managed-by-attribute-Livecollaboration
│   └── cn=managed-by-attribute-LivecollaborationAdmin
│
├── cn=tenant-gtn-demo-admin                      ← UMC operation set at root ❌ (should be inside OU)
├── cn=tenant-gtn-demo-2-admin                    ← same problem
│
├── ou=gtn-demo                                   ← tenant A OU
│   ├── ou=users                                  ← EMPTY sub-container ❌ (federation target but no users here)
│   ├── uid=app-keycloak                          ← STALE: no tenant suffix ❌ (should be removed)
│   ├── uid=app-keycloak-gtn-demo                ← correct scoped bind account ✓
│   ├── uid=app-ox-appsuite-gtn-demo             ← correct scoped bind account ✓
│   ├── cn=users_gtn-demo                        ← UDM group (correct, per-tenant) ✓
│   ├── cn=admins_gtn-demo                       ← UDM group (correct, per-tenant) ✓
│   ├── uid=admin-gtn-demo                       ← tenant admin at OU ROOT ❌ (not in ou=users)
│   └── uid=gtn-demo-test                        ← regular user at OU ROOT ❌ (not in ou=users)
│
├── ou=gtn-demo-2                                 ← tenant B OU
│   ├── ou=users                                  ← EMPTY sub-container ❌
│   ├── uid=app-keycloak                          ← STALE: no tenant suffix ❌
│   ├── uid=app-keycloak-gtn-demo-2              ← correct ✓
│   ├── cn=users_gtn-demo-2                      ← correct ✓
│   ├── cn=admins_gtn-demo-2                     ← correct ✓
│   ├── uid=admin-gtn-demo-2                     ← at OU ROOT ❌
│   └── uid=gtn-demo-2-test                      ← at OU ROOT ❌
│
├── cn=univention, cn=open-xchange, cn=mail,      ← Nubus/UDM system containers (unchanged)
│   cn=dns, cn=dhcp, cn=samba, ...
└── cn=self registered users
```

### 1.2 Keycloak realms (actual, verified via Admin REST API)

| Realm | LDAP federation `usersDn` | searchScope | Issue |
|---|---|---|---|
| `master` | — | — | Correct, admin only |
| `kernel` | `dc=swp-ldap,dc=internal` | `2` (subtree) | **WRONG** — sees ALL users from ALL tenant OUs |
| `gtn-demo` | `ou=users,ou=gtn-demo,dc=swp-ldap,dc=internal` | `1` | Correct scope, but `ou=users` is **empty** |
| `gtn-demo-2` | `ou=users,ou=gtn-demo-2,dc=swp-ldap,dc=internal` | `1` | Same — empty |

### 1.3 Keycloak OIDC clients (actual)

| Realm | Clients |
|---|
| `kernel` | `portal`, `opendesk-dovecot`, `opendesk-intercom`, `opendesk-nextcloud`, `opendesk-oxappsuite`, `twofa-helpdesk` |
| `gtn-demo` | `gtn-demo-element`, `gtn-demo-jitsi`, `gtn-demo-ox-appsuite`, `opendesk-jitsi` |
| `gtn-demo-2` | `gtn-demo-2-element`, `opendesk-synapse` |

### 1.4 Defects found

| # | Severity | Location | Description |
|---|---|---|---|
| D1 | **Critical** | `kernel` realm LDAP | `usersDn=dc=swp-ldap,dc=internal` + subtree scan imports ALL tenant users into the kernel realm. UMC runs in the kernel realm context and therefore exposes all tenant users and all tenant containers to every tenant admin. This is the root cause of tenant admins seeing other tenants' users and realm selectors. |
| D2 | **Critical** | All tenant OUs | Users (`uid=admin-<tenant>`, regular users) are placed at `ou=<tenant>` root. Keycloak tenant realm federation points to the `ou=users,ou=<tenant>` sub-container, which is empty. **SSO for all tenant-realm OIDC clients is broken** — the tenant realm has no users to authenticate. |
| D3 | **High** | `ldap_reconciler.go` | `settings/directory` default container is set to `ou=<tenant>` (root), so UMC places new users at the OU root, not in `ou=users,ou=<tenant>`. |
| D4 | **High** | OX connector | `java.naming.provider.url` in ox-connector points to `dc=swp-ldap,dc=internal` (full tree), so OX contacts include users from all tenants. |
| D5 | **Medium** | Both tenant OUs | `uid=app-keycloak,ou=<tenant>` (no tenant suffix) is a stale service account from an earlier code version. Should be removed on reinstall. |
| D6 | **Medium** | `cn=groups` root | All `managed-by-attribute-*` groups are flat global objects. User memberships from all tenants are mixed. This is not a security vulnerability today (portal scopes tiles per user, not per group cross-tenant), but becomes one if group-based ACLs are added. |
| D7 | **Low** | Root level | `cn=tenant-<name>-admin` UMC operation sets are at the root rather than inside the tenant OU. Functionally harmless but creates clutter. |

---

## 2. Target State

### 2.1 Canonical rule

> **One OU · One realm · One namespace**
>
> `ou=<tenant>,dc=swp-ldap,dc=internal`  ↔  Keycloak realm `<tenant>`  ↔  `tenant-<tenant>` namespace
>
> All human users for a tenant live in `ou=users,ou=<tenant>`.
> All service accounts for a tenant live in `ou=<tenant>` root (not in `ou=users`).
> The kernel realm's LDAP federation is scoped to `cn=users,dc=swp-ldap,dc=internal` only.

### 2.2 Target LDAP tree

```
dc=swp-ldap,dc=internal
│
├── cn=users                                      ← kernel service accounts ONLY (unchanged names)
│   ├── uid=ldapsearch_keycloak                  ← kernel realm LDAP bind
│   ├── uid=ldapsearch_ox                        ← OX connector global bind (read-only, base-DN scoped)
│   ├── uid=svc-portal-server
│   └── ... (other kernel service accounts)
│
├── cn=groups                                     ← kernel-level groups only (no cross-tenant user membership)
│   ├── cn=Tenant Admins                         ← contains cn=admins_<tenant> as nested groups
│   ├── cn=Domain Service Users
│   ├── cn=IAM API - Full Access
│   └── cn=2FA Users
│   ↑ cn=Domain Users REMOVED (replaced by per-tenant cn=users_<tenant> groups)
│   ↑ cn=managed-by-attribute-* MOVED into each tenant OU
│
├── ou=<tenant>                                   ← one OU per tenant
│   │
│   ├── ou=users                                  ← ALL human users land here
│   │   ├── uid=admin-<tenant>                   ← tenant admin
│   │   └── uid=<username>                       ← regular users
│   │
│   ├── uid=app-keycloak-<tenant>                ← Keycloak LDAP federation bind account
│   ├── uid=app-<app>-<tenant>                   ← per-app bind accounts (OX, Nextcloud, …)
│   │
│   ├── cn=users_<tenant>                        ← UDM group: all tenant users (primary group)
│   ├── cn=admins_<tenant>                       ← UDM group: tenant admin(s)
│   │
│   ├── cn=managed-by-attribute-Groupware        ← per-tenant app access groups
│   ├── cn=managed-by-attribute-Fileshare
│   ├── cn=managed-by-attribute-FileshareAdmin
│   ├── cn=managed-by-attribute-Videoconference
│   ├── cn=managed-by-attribute-Livecollaboration
│   └── cn=managed-by-attribute-LivecollaborationAdmin
│
└── cn=univention, cn=open-xchange, cn=mail, …   ← Nubus/UDM system containers (unchanged)
```

> **Note on `managed-by-attribute-*` groups:** Moving these inside the tenant OU
> requires updating the `portaltileGroup*` UCR variables in the Nubus base values
> (`kernel/services/nubus/manifests/dev/values/_base.yaml`) from the global path
> `cn=managed-by-attribute-Groupware,cn=groups,…` to the per-tenant path. This is
> handled in the implementation steps below.

### 2.3 Target Keycloak topology

| Realm | LDAP federation `usersDn` | searchScope | OIDC clients |
|---|---|---|---|
| `master` | — | — | Admin CLI only |
| `kernel` | `cn=users,dc=swp-ldap,dc=internal` | `1` (one-level) | `portal`, `opendesk-dovecot`, `opendesk-intercom`, `opendesk-nextcloud`, `opendesk-oxappsuite` |
| `<tenant>` | `ou=users,ou=<tenant>,dc=swp-ldap,dc=internal` | `1` | `<tenant>-<app>` per installed app |

The kernel realm now sees only service accounts in `cn=users`. Tenant admins
and regular users are **not** in the kernel realm at all. They are authenticated
exclusively through their tenant realm.

### 2.4 UMC access model (after restructuring)

UMC (admin UI) must be accessible per tenant without the kernel realm exposing
cross-tenant data:

- Each tenant admin authenticates through their tenant realm (`<tenant>` realm),
  not the kernel realm.
- The UMC OIDC client (`https://portal.desk.gentian.org/univention/oidc/`) is
  registered in **each** tenant realm (in addition to the kernel realm).
- UMC's `settings/directory` default user container is `ou=users,ou=<tenant>`,
  so the container dropdown only shows the tenant's own OU.
- UDM LDAP ACLs (configured via Nubus UCR) restrict `uid=admin-<tenant>` to
  write only within `ou=<tenant>`. This prevents a misconfigured call from
  writing into another tenant's subtree.

---

## 3. Implementation Plan

This plan is designed for a **clean reinstall**. Steps are ordered by dependency.
Steps 1–4 are code changes in `gentian-os`; steps 5–6 are configuration changes
in `kernel/services/nubus/`.

### Step 1 — Fix user placement: move human users into `ou=users` sub-container

**File:** `internal/controller/ldap_reconciler.go`

1a. In `buildAdminUserScript`: change the `position` for the admin user creation
from `${OU_POS}` to `${USERS_OU_POS}` (= `ou=users,${OU_POS}`).

```go
// Before:
ADMIN_DN="uid=${ADMIN_USERNAME},${OU_POS}"
// ...
-d "{\"properties\":{...},\"position\":\"${OU_POS}\"}"

// After:
USERS_OU_POS="ou=users,${OU_POS}"
ADMIN_DN="uid=${ADMIN_USERNAME},${USERS_OU_POS}"
// ...
-d "{\"properties\":{...},\"position\":\"${USERS_OU_POS}\"}"
```

1b. In `buildTenantOUScript`: change `settings/directory` default container
from `ou=<tenant>` to `ou=users,ou=<tenant>`. UMC will then place all new
users created via the openDesk User wizard in the correct sub-container.

```go
// Before: NEW_USERS="[\"${OU_POS}\"]"
// After:  NEW_USERS="[\"ou=users,${OU_POS}\"]"
```

1c. In `buildTenantOUScript`: remove the creation of `uid=app-keycloak` (no
tenant suffix). Verify via grep that no call to `buildBindAccountScript` passes
an empty tenant name.

### Step 2 — Fix kernel realm LDAP scope

**File:** `internal/controller/identity_reconciler.go`

The kernel realm's LDAP User Storage Provider (seeded during Nubus keycloak-bootstrap)
must be changed to scope only to `cn=users,dc=swp-ldap,dc=internal` with
`searchScope=1` (one-level, not subtree).

This is configured in the Nubus keycloak-bootstrap job. Locate the keycloak-bootstrap
configmap/values that seed the `ldap-provider` component in the kernel realm and
update:

```yaml
# kernel/services/nubus/manifests/dev/values/_base.yaml or keycloak-bootstrap values
keycloak:
  ldapProvider:
    usersDn: "cn=users,dc=swp-ldap,dc=internal"   # was: dc=swp-ldap,dc=internal
    searchScope: "1"                                # was: 2
    customUserSearchFilter: "(uid=*)"               # unchanged
```

If this is not exposed as a Nubus chart value, add a post-install Job (similar
to `ox-activate-portal-tiles`) that patches the kernel realm's LDAP provider via
the Keycloak Admin REST API after keycloak-bootstrap completes.

### Step 3 — Register UMC OIDC client in each tenant realm

**File:** `internal/controller/identity_reconciler.go`, `buildRealmScript`

After creating the tenant realm, register the UMC OIDC client so tenant admins
can authenticate to UMC through their tenant realm:

```bash
# Add to buildRealmScript (after realm creation):
curl -sf -X POST ${KC_ADMIN_URL}/admin/realms/${REALM}/clients \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "https://portal.${KERNEL_DOMAIN}/univention/oidc/",
    "protocol": "openid-connect",
    "redirectUris": ["https://portal.${KERNEL_DOMAIN}/univention/oidc/*"],
    "publicClient": false
  }'
```

### Step 4 — Add per-tenant `managed-by-attribute-*` groups

**File:** `internal/controller/ldap_reconciler.go`, `buildTenantOUScript`

After creating the tenant OU, create the six `managed-by-attribute-*` groups
inside `ou=<tenant>` (not in `cn=groups` root). These are the groups that the
opendesk-a2g-mapper uses to grant app access per user.

Update `buildTenantOUScript` to create each group with
`position: ou=<tenant>,dc=swp-ldap,dc=internal`.

### Step 5 — Update Nubus portal tile group paths

**File:** `kernel/services/nubus/manifests/dev/values/_base.yaml`

The `portaltileGroup*` UCR variables must be templated per-tenant rather than
hardcoded to the global `cn=groups` path. Since these are Nubus-level values
seeded once at install time (not per-tenant), use a Helm-templated approach:

```yaml
# Values must be set dynamically per-tenant via the tenant reconciler's
# UDM REST calls, not statically in _base.yaml.
# Remove the global portaltileGroup* values from _base.yaml.
# The tenant reconciler sets these UCR variables via UDM after OU creation.
```

Until the reconciler is updated, keep the global `managed-by-attribute-*` groups
as a temporary fallback (app tile visibility still works, just without
per-tenant scoping). Mark as TODO in _base.yaml.

### Step 6 — Fix OX connector LDAP scope (per-tenant ox-connector)

**File:** `gentian-apps/profiles/ox-appsuite.yaml` and
`kernel/services/ox-appsuite/manifests/dev/configmap.yaml`

The ox-connector's `java.naming.provider.url` must point to the tenant's LDAP
subtree, not the global base DN:

```yaml
# ox-connector per-tenant AppProfile valueMapping:
connector:
  ldap:
    uri: "ldap://nubus-ldap.gentian-dev.svc.cluster.local:389/ou=users,ou={{ .TenantName }},dc=swp-ldap,dc=internal"
```

The ox-appsuite and ox-bootstrap Helm releases remain as shared kernel services.
Only ox-connector is deployed per tenant (via AppProfile). See
[ox-as-optional-kernel-extension.md](../../memories/repo/ox-as-optional-kernel-extension.md)
for the OX architectural decision.

### Step 7 — Clean up stale objects (on reinstall)

The following objects exist in the current cluster due to earlier code versions.
They will be absent after a clean reinstall if steps 1–6 are implemented:

- `uid=app-keycloak,ou=gtn-demo,dc=swp-ldap,dc=internal` (no tenant suffix)
- `uid=app-keycloak,ou=gtn-demo-2,dc=swp-ldap,dc=internal` (no tenant suffix)
- `cn=tenant-gtn-demo-admin,dc=swp-ldap,dc=internal` → will move into `ou=<tenant>` in a future refactor (low priority)

---

## 4. LDAP ACL Policy (enforce one-OU-per-admin)

Add the following OpenLDAP ACL pattern to the Nubus LDAP configuration to enforce
the one-OU boundary at the directory level. This prevents a misconfigured
tenant admin account from writing into another tenant's subtree, even if the
UDM REST API is called with an incorrect `position`.

```ldif
# Tenant admin: full write access to own OU only
access to dn.subtree="ou=TENANT,dc=swp-ldap,dc=internal"
  by dn.exact="uid=admin-TENANT,ou=users,ou=TENANT,dc=swp-ldap,dc=internal" write
  by dn.exact="uid=Administrator,cn=users,dc=swp-ldap,dc=internal" write
  by * none

# Service account for Keycloak tenant realm: read own OU only
access to dn.subtree="ou=TENANT,dc=swp-ldap,dc=internal"
  by dn.exact="uid=app-keycloak-TENANT,ou=TENANT,dc=swp-ldap,dc=internal" read
  by * none
```

These ACLs are provisioned via the Nubus UCR mechanism during tenant creation.
The tenant reconciler must call UDM REST to set `ldap/acl` policy objects
for each tenant OU. This is a **Phase 2b** item (after the structural fixes above).

---

## 5. Summary: Before and After

| Concern | Before | After |
|---|---|---|
| Kernel realm LDAP scope | `dc=swp-ldap,dc=internal` subtree — sees ALL users | `cn=users,dc=swp-ldap,dc=internal` one-level — service accounts only |
| Tenant realm LDAP scope | `ou=users,ou=<tenant>` — correct path but **empty** | `ou=users,ou=<tenant>` — **populated** (all human users here) |
| New user placement (UMC) | `ou=<tenant>` root | `ou=users,ou=<tenant>` sub-container |
| Admin user placement | `ou=<tenant>` root | `ou=users,ou=<tenant>` sub-container |
| UMC container dropdown | Shows all tenant OUs (all tenants visible) | Shows only `ou=users,ou=<own-tenant>` |
| UMC authentication realm | kernel realm (sees all users) | tenant realm (sees own users only) |
| OX contacts leak | Yes — ox-connector sees full tree | No — ox-connector scoped to `ou=users,ou=<tenant>` |
| `cn=Domain Users` cross-tenant | Yes | Removed; replaced by `cn=users_<tenant>` per tenant |
| `managed-by-attribute-*` groups | Global (mixed membership) | Per-tenant (inside `ou=<tenant>`) |
| Service account naming | Mixed: `app-keycloak` (stale) + `app-keycloak-<tenant>` (correct) | `app-<app>-<tenant>` everywhere, consistently |
| SSO for tenant apps | Broken (tenant realm has no users) | Working (tenant realm imports from `ou=users,ou=<tenant>`) |

---

## 6. Files to change

| File | Change |
|---|---|
| `internal/controller/ldap_reconciler.go` | Steps 1, 4 |
| `internal/controller/identity_reconciler.go` | Steps 2, 3 |
| `kernel/services/nubus/manifests/dev/values/_base.yaml` | Step 5 |
| `gentian-apps/profiles/ox-appsuite.yaml` | Step 6 |
| `kernel/services/ox-appsuite/manifests/dev/configmap.yaml` | Step 6 |
