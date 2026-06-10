# Identity and Access Management (IAM)

This document describes the IAM architecture in Gentian OS. It outlines the role-based and attribute-based access controls used across the platform and how tenant isolation is achieved while maintaining compatibility with the upstream openDesk ecosystem.

**Companion to:** [multi-tenancy.md](multi-tenancy.md) (for network, database, and namespace isolation details).

## 1. Multi-Tenant LDAP Architecture

Gentian OS relies on Univention Corporate Server (UCS) / Nubus for its core directory services. To support strict multi-tenancy inside a single shared OpenLDAP tree, the directory is segmented by Organizational Units (OUs).

* **Global Root (`dc=swp-ldap,dc=internal`)**: Contains global policies, groups (`cn=groups`), and system accounts.
* **Tenant OUs (`ou=<tenant>,dc=swp-ldap,dc=internal`)**: Each tenant receives its own OU containing its users (`cn=users_<tenant>`), admins (`cn=admins_<tenant>`), and computers.

Tenant admins are assigned an administrative UMC policy scoped exclusively to their own OU.
At the OpenLDAP layer, ACLs (via `slapd.conf` patches) strictly prevent users from reading objects inside another tenant's OU.

### 1.1 LDAP Topology (Target)

```
dc=swp-ldap,dc=internal
├── cn=users                   ← kernel service accounts ONLY (ldapsearch_*, svc-portal-server, …)
├── cn=groups                  ← kernel-level groups only (Tenant Admins, Domain Service Users, App Users, …)
│                                 cn=Domain Users is ABSENT — replaced by per-tenant cn=users_<t>
│
└── ou=<tenant>                ← one per tenant
    ├── ou=users               ← ALL human users (admin + regular users land here)
    │   ├── uid=admin-<tenant>
    │   └── uid=<username>
    ├── uid=app-keycloak-<tenant>         ← Keycloak LDAP federation bind account
    ├── uid=app-<app>-<tenant>            ← per-app service bind accounts
    ├── cn=users_<tenant>                 ← UDM group: all tenant users (primary group)
    ├── cn=admins_<tenant>               ← UDM group: tenant admins
    └── cn=managed-by-attribute-*        ← per-tenant app access groups (eight groups)
```

### 1.2 Keycloak Realm Topology

| Realm | LDAP federation scope | Who authenticates here |
|---|---|---|
| `master` | None | Keycloak admin CLI only |
| `kernel` | `dc=swp-ldap,dc=internal` (SUBTREE; login username = `mailPrimaryAddress`) | **All humans** at the shared portal (`portal.<kernel-domain>`) — platform admin and every tenant user/admin |
| `<tenant>` | `ou=users,ou=<tenant>,...` (one-level) | Tenant-scoped app OIDC (Jitsi, …); kernel IdP broker avoids a second login when the user already has a portal session |

The kernel realm imports tenant users from their OUs via SUBTREE LDAP federation
(see `kernel/services/keycloak-config/manifests/dev/ldap-federation-patch.yaml`).
Users sign in at the **single shared portal** with their email address at
`https://portal.<kernel-domain>/login/`. Tenant realms remain for per-app OIDC;
they do not host a separate portal or UMC stack.

### 1.3 OpenDesk OIDC packs (tenant realms)

Per-app OpenDesk OIDC configuration (client scopes, protocol mappers, client
roles, and LDAP group → role mappings) is declared in a YAML catalog embedded
in the operator (`internal/oidc/packs/opendesk.yaml`). When an `AppProfile`
sets `kernelRequirements.identity.oidc.clientId` to a catalog key (e.g.
`opendesk-jitsi`), the identity reconciler applies that pack in the tenant
realm via an idempotent Keycloak Admin API Job.

The catalog mirrors `opendesk/helmfile/apps/nubus/values-opendesk-keycloak-bootstrap.yaml.gotmpl`.
Apps without a pack entry fall back to a minimal OIDC client Job.

Once per tenant (when any OIDC app is installed), the reconciler also sets the
realm browser flow to `browser-kernel-idp` (auto-redirect to the kernel IdP)
so users are not prompted for a tenant-realm LDAP password after portal login.

The kernel IdP uses a custom first-broker-login flow (`first-broker-login-gentian`)
with **Detect existing broker user** + **Automatically set existing user** so
LDAP-preprovisioned tenant users are linked by email without the default
confirm/re-authenticate prompt (tenant users have no local Keycloak password).
The provisioner job also clears stale `kernel` federated-identity links left
from earlier manual linking attempts so auto-link does not hit duplicate-key errors.

LDAP `group-ldap-mapper` on the tenant realm federation imports
`managed-by-attribute-*` groups from the tenant OU so pack role mappings resolve.

**Provisioning order (operator):** realm registration (including the group-mapper)
runs first; the UDM OU Job creates per-tenant `managed-by-attribute-*` groups;
a Keycloak **LDAP group sync** Job imports them into the tenant realm; only then
do OpenDesk OIDC pack Jobs map each group to its client role. If the
`ldap-mba-groups` backfill Job completes after an earlier group sync (tenant
upgrade or new OIDC packs), the identity reconciler deletes the stale sync Job
and re-imports groups before client Jobs run. A final **LDAP user sync** runs
after the admin user is unlocked in LDAP so portal login stays consistent with
UDM `shadowExpire` handling.

## 2. Roles and User Templates

Gentian OS establishes distinct separation between normal users and administrators by employing specialized UMC User Templates.

### The "App User"
* **Purpose**: Day-to-day employees or members of the organization utilizing applications.
* **Template**: `cn=App User,cn=templates,ou=<tenant>,...` (UMC label **App User**; the LDAP RDN matches the template name). One per tenant; upstream `openDesk User` templates are removed. There is **no** kernel-level App User template—app accounts exist only inside tenant OUs. Tenant admins may optionally pick the kernel **Admin User** template for tenant IT accounts.
* **Characteristics**:
  * Pre-fills `mailPrimaryAddress` as `<username>@<tenant>.<kernel-domain>` (same openDesk `@domain` template syntax).
  * Automatically assigned the `opendeskFileshareEnabled`, `opendeskLivecollaborationEnabled`, and `opendeskVideoconferenceEnabled` attributes (granting access to Nextcloud, Element, Jitsi, etc.).
  * Added to the global `cn=App Users` group.
  * Can see and access application tiles in the Nubus Portal.
  * **Cannot** access the Univention Management Console (UMC) or manage other users.

### The "Admin User" (Tenant Admin)
* **Purpose**: IT Administrators responsible for managing their tenant's users and groups.
* **Template**: `cn=Admin User,cn=templates,cn=univention,...` (UMC label **Admin User**; kernel-wide; the only user template at the platform LDAP level)
* **Characteristics**:
  * Explicitly **lacks** app-enabling attributes (`opendeskFileshareEnabled=False`).
  * Not included in the `App Users` group.
  * Added to `cn=admins_<tenant>` (granting UMC management privileges).
  * Can see the UMC admin tiles in the Nubus Portal but **cannot** see application tiles like Nextcloud or CryptPad.

This mutually exclusive setup enforces the principle of least privilege, ensuring administrative accounts aren't consumed for daily tasks or inadvertently consuming enterprise software licenses.

## 3. App Profiles and Portal Visibility

When an application is added to the Gentian OS ecosystem via an `AppProfile`, its portal tile visibility must be scoped appropriately.

### OpenDesk Suite Apps (Attribute-Based)
Apps belonging to the core openDesk suite (like Nextcloud or OpenXchange) rely on the **Attribute-Based Access Control (ABAC)** pattern. Access is governed by boolean properties on the user object (e.g., `opendeskFileshareEnabled`). The Nubus listener pattern automatically translates these attributes into corresponding backend provisioning tasks (creating quotas, mailboxes, etc.) and placing the user in the necessary `managed-by-attribute-<AppName>` group.

### Custom Apps (Group-Based)
For third-party or custom apps deployed via generic `AppProfile` manifests (like CryptPad), Gentian OS defaults the `allowedGroup` to `App Users`.
* Because `App Users` encompasses all standard users but excludes Tenant Admins, the portal tiles for these custom apps seamlessly mirror the visibility rules of the core openDesk apps.

### Portal Tile Enforcement Summary

The Nubus portal shows tiles based on LDAP group membership, enforced at the OIDC claim layer — users without the required group membership will fail SSO even if they know the direct app URL.

| Tile category | `allowedGroups` (portal) | Who has this membership |
|---|---|---|
| Admin tools (UMC, Keycloak) | `cn=Domain Admins` | Tenant admin account only |
| User apps (Files, Email, Chat, …) | `cn=managed-by-attribute-<App>` | Regular users with that app enabled |
| Custom AppProfile apps | `cn=App Users` (Default) | Regular users (via `App User` template) |

## 4. Upstream Compatibility

By maintaining the `opendesk*Enabled` attribute checkboxes instead of solely relying on group membership, Gentian OS remains fully compatible with the upstream Univention directory listeners and UMC Web UI Wizards. This avoids the necessity of maintaining custom forks of the UMC interface while still offering clean Role-Based visual separation in the portal.
