# Identity and Access Management (IAM)

This document describes identity, roles, and access control in Gentian OS.

**Companion docs:**
- [admin-console.md](admin-console.md) — Gentian Admin Console (replaces UMC)
- [multi-tenancy.md](multi-tenancy.md) — namespace, network, and data isolation
- [new-security-architecture.md](new-security-architecture.md) — Suze, OpenFGA, MAC layers

---

## 1. Identity topology (Suze / `keycloak-native`)

When `IDENTITY_MODE=keycloak-native` (default on new installs), **Suze** (Keycloak + OpenFGA) is the identity authority. OpenLDAP, UDM, and UMC are **not** used.

### 1.1 Realms and login

| Realm | Role |
|---|---|
| `master` | Keycloak operator CLI only |
| `kernel` | Shared portal (`gentian-portal`), platform admins, identity-first login |
| `<tenant>` | Authoritative user/group store for that tenant; per-app OIDC |

All humans sign in at **`https://portal.<KERNEL_DOMAIN>/login`** with their **email address**.

- **Members and tenant admins** are stored in the **tenant realm**.
- The **kernel realm** routes login (OIDC broker) to the correct tenant realm and issues the portal session.
- **Tenant apps** use the tenant realm for OIDC; the tenant realm brokers to the kernel IdP so users are not prompted twice after portal login.

See [admin-console.md §3](admin-console.md#3-identity-topology-suze--keycloak-native) for diagrams and broker details.

### 1.2 Group taxonomy (replaces LDAP `managed-by-attribute-*`)

Gentian uses explicit Keycloak group names — not Univention `managed-by-attribute-<App>` groups or `opendesk*Enabled` LDAP attributes.

| Group | Purpose |
|---|---|
| `gentian:platform:superadmin` | Platform operator (bootstrap; broad access initially) |
| `gentian:platform:operator` | Future constrained platform role |
| `gentian:platform:break-glass` | Future audited emergency access |
| `gentian:tenant:<t>:members` | All workspace members |
| `gentian:tenant:<t>:admins` | Tenant IT admins |
| `gentian:tenant:<t>:app:<profile>` | App entitlement (portal tile + future provisioning) |
| `gentian:role:member` | Token marker for workspace members |

The **authz bridge** syncs membership into **OpenFGA** for PEP checks (`can_launch`, etc.).

### 1.3 Roles: member vs administrator

Mutually exclusive — same invariant as legacy UMC templates:

| Role | Typical groups | Portal |
|---|---|---|
| **Member** | `members`, optional `app:*` | User app tiles only |
| **Tenant admin** | `admins` | Admin Console (Users, Groups, Notifications) — no app tiles |
| **Platform admin** | `gentian:platform:superadmin` | Admin Console (cross-tenant during bootstrap) |

Provisioning is via the [Gentian Admin Console](admin-console.md), not UMC wizards.

### 1.4 Bootstrap credentials

| Principal | Login email | Password source |
|---|---|---|
| Platform admin | `administrator@<KERNEL_DOMAIN>` | `MASTER_PASSWORD` → OpenBao / `nubus-credentials` (legacy path) or kernel bootstrap Job (Suze) |
| Tenant admin | `Tenant.spec.adminEmail` (username `admin-<tenant>`) | OpenBao `gentian-os/tenants/<tenant>/admin` |

Retrieved after `kubectl gentian tenants deploy <instance>` (see [commands.md](../commands.md)).

### 1.5 User attributes

| Attribute | Purpose |
|---|---|
| `email` / `username` | Primary login id (email) |
| `gentian.inviteEmail` | Secondary email for invite, password reset, recovery |
| `gentian.tenant` | Tenant id (if not implied by realm) |

### 1.6 App access and portal visibility

| App type | Entitlement mechanism |
|---|---|
| Catalogue apps (`AppProfile`) | `gentian:tenant:<t>:app:<profile>` group + OpenFGA |
| Custom / generic apps | Default: `members` group |

Portal shell filters tiles from JWT **groups** and OpenFGA — not Nubus portal `allowedGroups` LDAP DNs.

### 1.7 OIDC packs (tenant realms)

Per-app OIDC client scopes, protocol mappers, and client roles are declared in the operator OIDC pack catalog (`internal/oidc/packs/`). When an `AppProfile` sets `kernelRequirements.identity.oidc.clientId` to a catalog key, the identity reconciler applies that pack in the **tenant realm**.

**Suze migration:** pack entries map **entitlement groups** (`gentian:tenant:<t>:app:<profile>`) to client roles — not LDAP `managed-by-attribute-*` DNs. LDAP `group-ldap-mapper` and `ldap-mba-groups` Jobs are skipped when `IDENTITY_MODE=keycloak-native`.

Browser flow `browser-kernel-idp` and first-broker-login flow `first-broker-login-gentian` remain for SSO between tenant realm and kernel IdP.

### 1.8 Provisioning (replaces LDAP listeners)

User/group changes in Keycloak emit events consumed by a **provisioning bus** (CloudEvents + SCIM 2.0 payloads). App-specific logic lives in **`gentian-apps` provisioner plugins** referenced from `AppProfile`. See [admin-console.md §6](admin-console.md#6-app-entitlements-and-provisioning-bus).

---

## 2. Administration UI

| Legacy | Gentian |
|---|---|
| UMC (`/univention/management/`) | **Gentian Admin Console** (shell builtin apps) |
| UMC Users / Groups | Admin Console **Members** / **Groups** |
| UMC Announcements | **Notifications** (`admin-notifications` contract) |
| Nubus notifications-api | Gentian notifications gateway (CloudEvents) |

Full design: [admin-console.md](admin-console.md).

---

## 3. MAC and IAM (layering)

IAM does **not** replace the MAC backbone:

- **MAC** — `tenant-{name}` namespace, NetworkPolicy, Kyverno ([new-security-architecture.md](new-security-architecture.md))
- **Identity** — per-tenant Keycloak realm
- **Authorization** — OpenFGA (ReBAC) + group claims (RBAC veneer)

Effective access is the **intersection** of all layers.

---

## 4. `identityMode` switch

| Mode | Identity store | Admin UI | LDAP Jobs |
|---|---|---|---|
| `keycloak-native` | Suze Keycloak realms | Gentian Admin Console | Skipped |
| `legacy-ldap` | OpenLDAP + UDM | UMC (Nubus) | Full manifest bridge |

New work targets **`keycloak-native`** only. Legacy mode remains for cutover clusters.

---

## 5. Legacy (Nubus / LDAP path)

The following applied when Nubus + OpenLDAP were deployed (`IDENTITY_MODE=legacy-ldap`). Retained for migration reference only.

### 5.1 LDAP topology

```
dc=swp-ldap,dc=internal
├── cn=users                   ← kernel service accounts
├── cn=groups                  ← kernel-level groups
└── ou=<tenant>
    ├── ou=users               ← human users
    ├── cn=users_<tenant>
    ├── cn=admins_<tenant>
    └── cn=managed-by-attribute-*   ← Univention naming (deprecated)
```

### 5.2 Legacy Keycloak + LDAP federation

| Realm | LDAP scope |
|---|---|
| `kernel` | SUBTREE over `dc=swp-ldap,dc=internal`; login = `mailPrimaryAddress` |
| `<tenant>` | One-level under `ou=users,ou=<tenant>,...` |

### 5.3 Legacy templates and portal tiles

- **App User** template — `opendesk*Enabled` attributes, `cn=App Users`, UMC excluded
- **Admin User** template — no app attributes, `cn=admins_<tenant>`, UMC admin tiles only
- Portal tiles used LDAP `allowedGroups` (e.g. `cn=Domain Admins`, `managed-by-attribute-<App>`)

### 5.4 Legacy provisioning order

Realm → LDAP OU → `managed-by-attribute-*` groups → LDAP group sync → OIDC pack Jobs → LDAP user sync.

Do **not** implement new features on this path.

---

## 6. Upstream openDesk compatibility

The Suze path **does not** preserve Univention directory listeners or `opendesk*Enabled` attribute sync. Apps that depended on LDAP-driven provisioning receive accounts via the **SCIM/event provisioning bus** instead. OIDC client configuration patterns from openDesk remain useful and are preserved in the OIDC pack catalog.
