# Gentian Admin Console — design

**Status:** Draft v0.1  
**Scope:** User and group administration, tenant-scoped notifications, member onboarding, and the identity/provisioning contracts that replace Nubus UMC + LDAP listeners in the Suze (`keycloak-native`) path.

**Companion docs:** [iam.md](iam.md), [new-security-architecture.md](new-security-architecture.md), [multi-tenancy.md](multi-tenancy.md), [tenant-identity-composition.md](tenant-identity-composition.md), [app-catalogue.md](app-catalogue.md).

---

## 1. Purpose

Gentian needs a **first-party administration product** that replaces the Univention Management Console (UMC) for:

| Legacy UMC module | Gentian Admin Console module | v1 scope |
|---|---|---|
| Users (`udm:users/user`) | **Members** | CRUD, invite, disable, password reset, MFA |
| Groups (`udm:groups/group`) | **Groups** | CRUD, membership, app entitlements |
| Announcements / notifications | **Notifications** | Publish scoped broadcasts (`admin-notifications` contract) |
| App Store (separate tile) | **Deferred** | Stays on `kubectl gentian apps` until Stage 2+ |

The console is **not** a fork of UMC. It is a Gentian BFF + React UI (`gentian-ui`, `ui_kits/console` aesthetic) calling **Keycloak Admin API** (Suze) and Gentian kernel services. Branding, copy, and flows are Gentian-native.

---

## 2. Placement in the security model

Tenant isolation is enforced at **two independent layers** (see [new-security-architecture.md §2](new-security-architecture.md#21-core-principle--defense-in-depth-as-an-intersection)):

| Layer | Mechanism | Android analogue |
|---|---|---|
| **MAC backbone** | `tenant-{name}` namespace, default-deny NetworkPolicy, Kyverno | SELinux + per-app UID |
| **Identity domain** | **Per-tenant Keycloak realm** + kernel login façade | Work profile (separate user store) |

Keycloak **Organizations** (single realm, logical tenants) are **not** the primary isolation mechanism. They optimize B2B CIAM at scale but share one user database; Gentian prioritizes MAC + realm-per-tenant. See [iam.md §1](iam.md#1-identity-topology-suze--keycloak-native).

Authorization inside the console uses **RBAC groups in Keycloak** (ergonomic) backed by **OpenFGA** (authorization plane) as the bridge matures.

---

## 3. Identity topology (Suze / `keycloak-native`)

### 3.1 Realms

| Realm | Purpose | Human users |
|---|---|---|
| `master` | Keycloak operator CLI only | No |
| `kernel` | Shared portal OIDC (`gentian-portal`), **platform admins**, identity-first login router | Platform admins only |
| `<tenant>` | **Authoritative store** for tenant members, groups, app OIDC clients | All tenant members and tenant admins |

Users are **managed in the tenant realm**. The shared portal login flow uses the **kernel realm**, which **brokers** to the correct tenant realm by email / tenant resolution.

```
  portal.<kernel>/login
         │
         ▼
  ┌──────────────────┐
  │  kernel realm    │  gentian-portal client, platform admins
  │  identity-first  │
  └────────┬─────────┘
           │ OIDC broker (per tenant)
     ┌─────┴─────┬─────────────┐
     ▼           ▼             ▼
 tenant:demo  tenant:acme   …
 (users live here)
```

Tenant apps (Jitsi, Nextcloud, …) continue to use the **tenant realm** for OIDC. The tenant realm **brokers to the kernel IdP** so a user with an active portal session is not prompted again (existing `browser-kernel-idp` / `first-broker-login-gentian` pattern, adapted for native Keycloak users instead of LDAP federation).

### 3.2 Login identifiers

| Field | Rule |
|---|---|
| **Primary login (`username` / `email`)** | Email address — global uniqueness across the cluster (`user@demo.desk.gentian.org`) |
| **`inviteEmail`** | Optional secondary address for **invite**, **password reset**, and **account recovery** only |
| **Tenant admin bootstrap** | Username `admin-<tenant>`; login email from `Tenant.spec.adminEmail`; password from OpenBao `gentian-os/tenants/<tenant>/admin` (unchanged convention) |
| **Platform admin bootstrap** | `administrator@<KERNEL_DOMAIN>`; password derived from install `MASTER_PASSWORD` (unchanged convention) |

### 3.3 Group naming (replaces `managed-by-attribute-*`)

Legacy Univention groups (`managed-by-attribute-Fileshare`, `opendesk*Enabled` LDAP attributes) are **not** used in the Suze path.

| Gentian Keycloak group | Replaces (legacy) | Purpose |
|---|---|---|
| `gentian:platform:superadmin` | `cn=Domain Admins` (kernel) | Platform operator (bootstrap; broad access initially) |
| `gentian:platform:operator` | — | Future read-only / constrained platform role |
| `gentian:platform:break-glass` | — | Future emergency access (audited) |
| `gentian:tenant:<t>:members` | `cn=users_<t>` | All workspace members |
| `gentian:tenant:<t>:admins` | `cn=admins_<t>` | Tenant IT admins (Admin Console scope) |
| `gentian:tenant:<t>:app:<profile>` | `managed-by-attribute-<App>` | App entitlement (portal + future provisioning) |
| `gentian:role:member` | `cn=App Users` | Cross-tenant marker for “workspace member” (token claim) |

**OpenFGA sync** (authz bridge): group membership → tuples, e.g. `user:alice#member@group:demo-app-nextcloud`, `group:demo-app-nextcloud#parent@tenant:demo`.

**Portal visibility:** shell reads JWT groups / OpenFGA `can_launch` — not LDAP `allowedGroups` on Nubus portal entries.

### 3.4 Member vs administrator (mutually exclusive)

Same invariant as legacy UMC templates:

| Role | Groups | Portal desktop |
|---|---|---|
| **Member** | `gentian:tenant:<t>:members`, optional `gentian:tenant:<t>:app:*` | User app tiles only |
| **Tenant admin** | `gentian:tenant:<t>:admins` | Admin Console tiles (Users, Groups, Notifications) — **no** app tiles |
| **Platform admin** | `gentian:platform:superadmin` | Admin Console (all tenants) + future platform tools |

Enforced by group assignment in the Admin Console, not LDAP templates.

---

## 4. Gentian Admin Console — product shape

### 4.1 Single console, scoped by privilege

One web app embedded in the Gentian shell (builtin desktop apps). Menu items shown or hidden from JWT roles + OpenFGA checks:

| Module | Platform superadmin (bootstrap) | Tenant admin |
|---|---|---|
| Members | All tenants (initially) | Own tenant only |
| Groups | All tenants (initially) | Own tenant only |
| Notifications | Platform-wide publish | Tenant-scoped publish |
| Tenants | Yes | Hidden |
| App Store | Later | Later |

**BFF rule:** every Admin API call resolves `tenant` from the authenticated subject and **rejects** cross-tenant mutations unless the caller holds `gentian:platform:superadmin`.

### 4.2 Bootstrap flows (parity with install / `kubectl gentian tenants deploy`)

**A) Platform admin (install)**

1. `install.sh` seeds Suze + kernel realm.
2. Job or bootstrap script creates `administrator@<KERNEL_DOMAIN>` in **kernel realm** with `gentian:platform:superadmin`.
3. Admin signs in at `https://portal.<kernel>/login` → kernel broker not needed → Admin Console desktop.

**B) Tenant admin (`kubectl gentian tenants deploy demo`)**

1. Operator seeds OpenBao `gentian-os/tenants/demo/admin`.
2. Provisioning creates **tenant realm** `demo`, groups, tenant admin user, `gentian:tenant:demo:admins` membership.
3. CLI prints login email + password (after Keycloak user is ready).
4. Tenant admin signs in at shared portal → kernel brokers to tenant realm → Admin Console.

**C) Tenant admin invites members**

1. Admin Console → **Invite member** → email, name, optional app entitlements (group checkboxes).
2. BFF creates user in **tenant realm** (`enabled: true`, no password).
3. BFF calls Keycloak `execute-actions-email` with `VERIFY_EMAIL`, `UPDATE_PASSWORD` (and optionally `CONFIGURE_TOTP` when MFA required).
4. Gentian-branded Keycloak email theme; link returns to portal `/login`.

### 4.3 Password reset and recovery

| Action | Email target | Mechanism |
|---|---|---|
| Admin-triggered reset | `inviteEmail` if set, else primary `email` | `execute-actions-email` with `UPDATE_PASSWORD` |
| Self-service forgot password | Same | Future: Keycloak account console or portal link (same recovery path) |
| Invite (first login) | Primary `email` for username; optional copy to `inviteEmail` | `execute-actions-email` |

Store `inviteEmail` as Keycloak user attribute `gentian.inviteEmail`.

### 4.4 MFA (v1)

Use **Keycloak built-in TOTP** (`CONFIGURE_TOTP` required action) — no custom crypto.

| Capability | v1 |
|---|---|
| Admin enables TOTP per user | Yes |
| Realm policy “require TOTP for admins” | Yes |
| WebAuthn / passkeys | Phase 2 |

---

## 5. `admin-notifications` contract

Cross-app contract (published in `gentian-apps/contracts/admin-notifications.yaml` when the catalogue repo adds it).

| Property | Value |
|---|---|
| **Name** | `admin-notifications` |
| **Wire format** | [CloudEvents 1.0](https://cloudevents.io/) over HTTP |
| **Event type** | `gentian.admin.notification.published.v1` |
| **Consumers (v1)** | None required — storage + Admin Console inbox only |
| **Future consumers** | Email (Postfix), Element (Matrix), shell notification tray |

**Audience extension** (`gentianaudience` — Gentian-specific, not part of CE core):

```json
{
  "scope": "tenant",
  "tenant": "demo",
  "groups": ["gentian:tenant:demo:members"]
}
```

| Publisher | Allowed audiences |
|---|---|
| Platform superadmin | `scope: platform` (all users) or any group under their admin scope |
| Tenant admin | `scope: tenant` + own tenant id; groups ⊆ `gentian:tenant:<t>:*` |

BFF validates audience ⊆ caller's scope before publish. There is no universal RFC for “admin broadcast”; CloudEvents is the industry-standard **envelope**; the REST publish API is Gentian-specific (same pattern as Okta/Auth0 admin event streams).

---

## 6. App entitlements and provisioning bus

### 6.1 Console structures (v1 — no app-side workers required)

When creating/editing a member, the Admin Console sets **Keycloak group membership** only:

```yaml
# Conceptual — stored as group joins, not a separate CRD in v1
entitlements:
  apps: [nextcloud, element]   # → gentian:tenant:demo:app:nextcloud, …
  roles: [member]              # member | tenant-admin
```

Downstream app provisioning (Nextcloud account, Matrix ID, …) is **deferred** but the **data model exists on day one**.

### 6.2 Provisioning bus (v1 design, v2 implementation)

Replace LDAP listeners with a **standards-based event pipeline**:

```
Keycloak admin event (user/group CRUD)
        │
        ▼
Gentian Provisioning Controller (gentian-os)
        │  normalize to SCIM 2.0 User / Group resource or PatchOp
        ▼
CloudEvents bus   type: gentian.identity.user.updated.v1
        │
        ├──► AppProfile.provisioning.mode: native-scim
        │         → HTTP POST to app's SCIM endpoint
        │
        └──► AppProfile.provisioning.mode: plugin
                  → gentian-apps provisioner plugin
```

**AppProfile extension (planned):**

```yaml
spec:
  provisioning:
    mode: plugin              # none | native-scim | plugin
    pluginRef: nextcloud-user-provisioner
    scimEndpoint: ""          # when mode=native-scim
```

Provisioner plugins live in **`gentian-apps`** (app-developer authored); the controller loads them by reference. **SCIM 2.0 (RFC 7643/7644)** is the canonical payload; **CloudEvents** is the transport — compatible with SCIM-native SaaS and custom OSS apps.

Keycloak **26.6+ experimental SCIM Realm API** is complementary (external IdPs syncing **into** a tenant realm), not the primary internal trigger.

### 6.3 OIDC packs (legacy name: “openDesk OIDC packs”)

Per-app tenant-realm OIDC client scopes and role mappings remain in the operator catalog (`internal/oidc/packs/`). Migration:

| Legacy field | Suze field |
|---|---|
| `ldapGroup: managed-by-attribute-Jitsi` | `entitlementGroup: gentian:tenant:<t>:app:jitsi` (template; tenant suffix applied at reconcile) |
| Pack file `opendesk.yaml` | Rename to `catalog.yaml` when touched (not required for doc-only) |

LDAP `group-ldap-mapper` sync Jobs are **skipped** when `IDENTITY_MODE=keycloak-native`.

---

## 7. Platform admin least-privilege (phased)

**Bootstrap (now):** `gentian:platform:superadmin` may manage all tenants — operational convenience.

**Target (flip via cluster config):** `platformAdminMode: constrained`

| Mode | Platform admin can |
|---|---|
| `bootstrap` (default) | Cross-tenant user/group visibility, help tenants directly |
| `constrained` | Tenant metadata + break-glass only; **no** routine cross-tenant member access |

Structures baked in from day one:

- Separate Keycloak groups (`superadmin`, `operator`, `break-glass`)
- OpenFGA relations `platform#admin` vs `tenant#admin`
- BFF checks on every route
- Audit log on all admin mutations (Keycloak admin events + BFF)

See [roadmap.md § Platform admin least-privilege](../roadmap.md#platform-admin-least-privilege).

---

## 8. Implementation phases

| Phase | Deliverable |
|---|---|
| **P0** | Suze bootstrap: kernel + tenant realm Jobs without LDAP; group taxonomy; platform + tenant admin users |
| **P1** | Admin Console BFF: Members + Groups (Keycloak Admin API, tenant-scoped) |
| **P2** | Invite + reset password (`inviteEmail`, Gentian email theme) |
| **P3** | TOTP MFA policies |
| **P4** | `admin-notifications` gateway + publish UI |
| **P5** | Provisioning controller + CloudEvents/SCIM bus (no-op plugins OK) |
| **P6** | Shell desktop: admin builtin apps; OpenFGA `can_launch` for admin modules |
| **Later** | App Store in console; `platformAdminMode: constrained`; WebAuthn |

**Explicitly not in P0–P4:** tenant app install, LDAP Jobs, UMC iframes, app-side provisioner execution.

---

## 9. Design conflicts and migration notes

Issues when moving from documented Nubus/LDAP behaviour to this design:

| # | Prior decision | New decision | Resolution |
|---|---|---|---|
| 1 | **Kernel realm SUBTREE LDAP** imports all humans ([iam.md](iam.md) old §1.2) | Users authoritative in **tenant realm**; kernel brokers | Update iam.md; disable LDAP federation on kernel realm in `keycloak-native`; implement broker IdP per tenant in kernel realm |
| 2 | **LDAP OU** is source of truth for users | Keycloak tenant realm is source of truth | `ensureLDAP` skipped when `IDENTITY_MODE=keycloak-native`; tenant Jobs emit Keycloak-only manifests |
| 3 | **`managed-by-attribute-*` / `opendesk*Enabled`** | `gentian:tenant:<t>:app:<profile>` groups | New groups only in Suze path; migration Job for cutover clusters optional |
| 4 | **UMC delegated admin policy** (OU-scoped) | BFF tenant scope + OpenFGA | No UMC; portal admin tiles → shell builtin apps |
| 5 | **Portal `allowedGroups` LDAP DNs** | JWT `groups` claim + OpenFGA | Deprecate Nubus portal-server entries for admin tiles; gentian-portal catalog filters on groups |
| 6 | **Tenant lifecycle** lists LDAP in architecture.md step 4 | LDAP optional per `identityMode` | Document dual path in tenant-identity-composition.md |
| 7 | **OIDC pack `ldapGroup` field** | `entitlementGroup` (name TBD in code) | Alias during migration; packs work on group **name** in Keycloak not LDAP DN |
| 8 | **`kernel-portal-*` HTTPRoutes** to Nubus UMC | Routes to gentian-portal admin modules | Already skipped in `keycloak-native` gateway reconciler |
| 9 | **Authz bridge** polls Keycloak users | Should sync **groups** and entitlements | Extend bridge in P6; not blocking Admin Console P1 |
| 10 | **install.sh** still seeds `nubus` OpenBao paths | Suze bootstrap uses Keycloak-native paths | Install paths can coexist until Nubus removed; portal bootstrap Job targets kernel realm |

**Open design point:** Email-domain → tenant routing at kernel login must handle `multi` tenancy (`user@demo.desk.gentian.org`) and `single` tenancy (`user@desk.gentian.org`). Reuse `Tenant.EffectiveDomain()` logic in the broker authenticator.

---

## 10. References

| Topic | Location |
|---|---|
| MAC backbone | [new-security-architecture.md §4 Stage 0](new-security-architecture.md#stage-0--foundations-mac-backbone-first) |
| OpenFGA model | `authz/model/v0/model.fga` |
| Legacy LDAP / UMC | [iam.md § Legacy](iam.md#5-legacy-nubus--ldap-path) |
| Tenant provisioning Jobs | [tenant-identity-composition.md](tenant-identity-composition.md) |
| App contracts | [app-catalogue.md](app-catalogue.md) |
| UI shell | `gentian-ui/legacy/design-system/ui_kits/console/` |
