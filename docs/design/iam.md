# Identity and Access Management (IAM)

This document describes identity, roles, and access control in Gentian OS.

**Companion docs:**
- [admin-console.md](admin-console.md) — the administration console
- [multi-tenancy.md](multi-tenancy.md) — namespace, network, and data isolation
- [security.md](security.md) — Suze, OpenFGA, MAC layers

---

## 1. Identity topology (Suze / Keycloak-native)

**Suze** (Keycloak + OpenFGA) is the identity authority. Identity is
**Keycloak-native per tenant** — each tenant realm is the authoritative
user and group store for that organisation.

### 1.1 Realms and login

| Realm | Role |
|---|---|
| `master` | Keycloak operator CLI only |
| `kernel` | The platform's administrators, and the platform tenant's clients |
| `<tenant>` | Authoritative user/group store for that tenant; per-app OIDC; **where tenant members authenticate** |

- **Members and tenant admins** are stored, and sign in, in the **tenant realm** — each has its own `Cookie → forms` browser flow, so there's no brokering on the sign-in path.
- The canonical, bookmarkable entry point is the tenant's desktop, **`https://desktop.<tenant>.<KERNEL_DOMAIN>/`** (on a single-tenancy cluster, `https://desktop.<KERNEL_DOMAIN>/`, §1.1a): the edge sends the browser to the tenant realm's form, which asks for email and password together.
- What the cluster's bare domain does (the apex; `www` leads where it does) depends on the cluster's tenancy mode (§1.1a). On a **multi-tenancy** cluster it is the **concierge**, a page the platform tenant publishes with no session in front of it. It asks for the email only and sends the browser to the desktop of the workspace the address belongs to (`@<tenant>.<KERNEL_DOMAIN>`, `@<KERNEL_DOMAIN>` for the platform's own people, or a tenant's custom domain); an address it cannot place is asked for the workspace's name. It asks the server nothing about accounts, and hands the address on as `login_hint`. On a **single-tenancy** cluster nobody is asked anything: the bare domain leads to the one user tenant's desktop.
- **The edge keeps the session**, not the app and not the desktop: on every host behind a session the Gateway runs the code flow against the zone's client (`gentian-edge-<zone>`), keeps the tokens in encrypted, host-scoped, `SameSite=Lax` cookies, renews them with the refresh token, and only then asks the bouncer whether this person may reach the host. The order, what the bouncer is shown and what it refuses are in [routing.md §4.1](routing.md).
- **Signing out** is `/oauth2/logout` on the host the person is on: the Gateway drops that host's cookies and sends the browser to the realm's end-session endpoint with the session's ID token as the hint, so Keycloak ends the realm session without asking and returns to the host's front page, which is the realm's sign-in again. Ending the realm session is what signs the person out of the zone's other hosts, within one access-token lifetime ([routing.md §4.2](routing.md)). Ending the realm session is also what tells the apps: the realm calls each app that can be told, inside the cluster, and the app ends its own session for that person. Which apps can be told, and how long a session lasts in one that cannot, is §1.12.
- **Tenant apps** use the same tenant realm for OIDC, so the realm session created at sign-in is reused silently by every app launch — no broker hop, no second login screen.
- The **platform admin** signs in in the kernel realm, at `https://platform.<KERNEL_DOMAIN>/`; there is no tenant realm for them to be routed to. The kernel realm holds the platform's administrators and nobody else: a cluster's users are never the kernel realm's, on a single-tenancy cluster any more than on a shared one.

### 1.1a The platform tenant, user tenants, and the two tenancy modes

Every cluster has the **platform tenant** (`Tenant/platform`). It adopts the
kernel realm instead of getting one of its own, so its people are the
platform's administrators, and it runs the platform's own components: the
desktop at `platform.<KERNEL_DOMAIN>`, the administration console at
`admin.platform.<KERNEL_DOMAIN>`, and the concierge on the bare domain. What
it runs is installed with the cluster. It takes no catalogue apps and no
add-ons: the director refuses both for a tenant whose manifest names a realm
other than its own (`spec.isolation.keycloakRealm`), with a message that says
why, and the command line and the console show that message as it is. It is
never counted as a tenant for users, under either mode.

Everybody else lives in a **user tenant**: a realm, namespaces and a zone of
its own. How many there may be, and where their hosts are, is the cluster's
**tenancy mode** (`spec.tenancyMode` on the Cluster claim, asked at install):

| | `multi` (default) | `single` |
|---|---|---|
| User tenants | any number | exactly one, named `user`; any other is refused by the webhook, the reconciler and the director, with a message naming the mode |
| A user tenant's desktop | `desktop.<tenant>.<KERNEL_DOMAIN>` | `desktop.<KERNEL_DOMAIN>` |
| Its admin console, its apps | `admin.<tenant>.<KERNEL_DOMAIN>`, `<app>.<tenant>.<KERNEL_DOMAIN>` | `admin.<KERNEL_DOMAIN>`, `<app>.<KERNEL_DOMAIN>` |
| Its realm | `<tenant>` | `user` |
| Its administrator | the **tenant admin**, `admin@<tenant>.<KERNEL_DOMAIN>` | the **user admin**, `user-admin@<KERNEL_DOMAIN>` |
| The bare domain, `www` | the concierge's address form | the user tenant's desktop; or its public website, if it put one there ([routing.md §5](routing.md#5-redirects-and-url-control)) -- `<KERNEL_DOMAIN>/sign-in` then still leads to the desktop |
| `desktop.<KERNEL_DOMAIN>` | sent to the bare domain, like `www` | the user tenant's desktop |
| The platform admin | `admin@<KERNEL_DOMAIN>`, kernel realm, at `platform.<KERNEL_DOMAIN>` | the same |

The desktop's address was `console.<tenant>.<KERNEL_DOMAIN>` (`console.<KERNEL_DOMAIN>`
on a single-tenancy cluster, `console.<custom domain>` on a tenant's own domain)
until 2026-10-08. Nothing answers on the old name: a cluster is installed
fresh, so there is no alias. No app may take `console` either: people may
still type it, so it is kept free ([routing.md §3.1](routing.md)).

On a single-tenancy cluster the user tenant has no domain of its own: its base
domain is the cluster's. Two things follow.

- **Two realms share one address space.** The platform's people (kernel
  realm) and the user tenant's (realm `user`) both have `@<KERNEL_DOMAIN>`
  addresses. The platform admin is `admin@<KERNEL_DOMAIN>` and stays so -- the
  installer creates the account under that name and looks it up by it when it
  issues an activation link (§1.4) -- so the user tenant's first administrator
  is `user-admin@<KERNEL_DOMAIN>`. Nothing stops an
  administrator of either realm from creating a person whose address exists in
  the other; the two accounts are separate, and they share a mailbox
  ([multi-tenancy.md §3](multi-tenancy.md)).
- **Some names are the kernel's.** `id`, `platform`, `www`, `argocd`,
  `headlamp`, `llm`, `mail`, `imap`, `mail-egress` and `corp` directly under
  the cluster's domain are the kernel's or the platform tenant's. A component
  or an app of the user tenant whose host label is one of them is refused,
  with a `HostReserved` condition that lists them, and nothing of it is
  installed or routed.
  The platform's own names in a tenant (`desktop`, `admin`, `store`, and the
  names a sign-in page would have) are refused to apps under either tenancy
  mode ([routing.md §3.1](routing.md)).

Under `multi` a tenant named `user` is an ordinary tenant, at
`<label>.user.<KERNEL_DOMAIN>`.

**Changing the mode.** `single` to `multi` is always fine. `multi` to `single`
on a cluster that carries a user tenant other than `user` is refused by the
director before anything is written, naming the tenants in the way. Where the
claim is changed some other way, the operator holds each such tenant on its
next pass: it provisions nothing more for it and says why on its status
(`TenancyConstraint`), and removes nothing -- its namespaces, data and routes
stay as they were. The operator reads the mode when it starts.

### 1.2 Group taxonomy

Gentian uses explicit Keycloak group names for platform roles, tenant
membership, and access to each app:

| Group | Purpose |
|---|---|
| `gentian:platform:admin` | The platform administrator. The Cluster claim names the group that holds each platform role (`spec.platformRoles`); this is the default for `admin` |
| the groups the claim names for `securityOfficer`, `auditor`, `serviceAdmin`, `sharedAppsAdmin`, `breakGlass` | The other platform roles. None has a holder unless the claim names a group |
| `gentian:tenant:<t>:members` | All workspace members |
| `gentian:tenant:<t>:admins` | Tenant IT admins |
| `gentian:tenant:<t>:perimeter` | Perimeter approvers (§1.3) |
| `gentian:tenant:<t>:app-admins` | The App Admin role: who administers the tenant's apps. One group for the whole tenant, not one per app (§1.11) |
| `gentian:tenant:<t>:app:<profile>` | Access to one installed app (§1.6) |

Membership reaches **OpenFGA** as events: Keycloak's event listener posts signed statements to the
operator, which writes them as `group#member` tuples (`internal/controller/membership_listener.go`).
Nothing polls Keycloak for it. A tenant's realm can speak only for that tenant's groups
(`internal/membership`). The front door asks `can_use` on an app
([routing.md §4.1](routing.md)).

### 1.3 Roles: member vs administrator

Mutually exclusive roles — a tenant admin account must not double as a
day-to-day app user:

| Role | Typical groups | Desktop |
|---|---|---|
| **Member** | `members`, optional `app:*` | User app tiles only |
| **Tenant admin** | `admins` | Admin Console (Users, Groups, Notifications) — no app tiles |
| **Perimeter approver** | `perimeter` | Approves what the tenant publishes to the internet (`can_expose`), and nothing else. The group is created with the tenant and starts empty. The platform admin approves too, in a tenant the cluster operates; the tenant admin only where the platform admin switched that on, and only somebody who may approve changes who is in the group ([security.md §2.14](security.md)) |
| **Platform admin** | `gentian:platform:admin` | Admin Console (cross-tenant during bootstrap) |

Provisioning is via the [Gentian Admin Console](admin-console.md).

### 1.4 Bootstrap credentials

| Principal | Login email | Password source |
|---|---|---|
| Platform admin | `admin@<KERNEL_DOMAIN>`, in the kernel realm | Set by its holder through a single-use activation link, which the installer issues (`./install.sh --activate-admin` for a new one). Never stored or printed |
| Tenant admin | The address on the tenant's `status.adminEmail`: `admin@<tenant-domain>`, both the username and the address; `user-admin@<KERNEL_DOMAIN>` for the user tenant of a single-tenancy cluster | The same, issued by the registrar: `kubectl gentian tenants activate-admin <tenant>` |

The link is mailed to a recovery address or shown once (see [commands.md](../commands.md)).

**No administrator's password follows the cluster's `secretMode`.** It is
neither derived from the master password nor drawn at random: the account is
created without a password, in both modes, and the only password it ever has
is the one its holder sets through the link. Nothing can recompute it and
nothing holds a copy, so it does not depend on the account's name, on the
master password or on the vault.

What does follow `secretMode` is the credential the link is issued with. The
installer issues the platform admin's link as Keycloak's own bootstrap
administrator (the `master` realm, Secret `keycloak-admin`, vault path
`gentian-os/kernel/identity/keycloak-bootstrap`). That password is a machine
credential like every other kernel one: with `secretMode: derived` it is
computed from the master password and the cluster's salt, and with
`secretMode: random` it is drawn once and the vault holds the only copy.

**Recovery** is a new link, never a recovered password:

- The platform admin: `./install.sh --activate-admin`, run on a host with
  access to the cluster. It needs nobody to be signed in. It reads Keycloak's
  bootstrap credential from the cluster, looks the account up by its name
  (`admin@<KERNEL_DOMAIN>` in the kernel realm) and issues a link even when
  the account already has a password; following it sets a new one and, where
  the platform tenant requires it, a second factor.
- A tenant admin: `kubectl gentian tenants activate-admin <tenant>`, by
  somebody who may configure the cluster.

### 1.5 User attributes

| Attribute | Purpose |
|---|---|
| `email` / `username` | Primary login id (email) |
| `gentian.inviteEmail` | Secondary email for invite, password reset, recovery |

### 1.6 App access and what the desktop shows

Who may use an installed app is membership of the app's group,
`gentian:tenant:<t>:app:<profile>`, in the tenant's realm. There is no other
grant, and nothing a store states gives access.

- **The group** is created with the install, one per installed app and one
  per activated addon. A tenant's administrator puts people in it, in the
  administration console. An app installed for everyone
  (`Tenant.spec.apps[].defaultGrant`) has its current members added once, and
  is pre-selected when a person is invited later.
- **The model.** The operator writes, for each installed app, the tuple that
  makes the group's members `entitled` on `app:<tenant>/<profile>`
  (`internal/director/authz/apps.go`). `can_use` is *entitled, and not an
  administrator of the tenant*; `can_launch` is `can_use`
  (`authz/model/v1/model.fga`). An app with activated addons is entered
  through the addons' groups, not its own.
- **The front door** asks `can_use` on every request to the app's host
  ([routing.md §4.1](routing.md)).
- **The desktop** shows the tiles the usher answers for the signed-in person:
  each tile names a relation and an object, and the person holds it or the
  tile is not listed. An app's tile asks `can_launch` on the app; an
  administration tile asks `can_administer` on the tenant. Nothing reads
  groups out of a token to decide this.

### 1.7 OIDC packs (tenant realms)

An app whose OIDC client needs its own client scope, protocol mappers or a
client role declares them in an **`OIDCPackCatalog`** (cluster-scoped). It
travels in the app's profile bundle, named `<profile>-oidc`, and only a
catalogue of the whole cluster may bring one.

A pack is looked up by the profile's `requires.services.identity.oidc.clientId`,
or by `oidcPackRef` where that is set. For an app with a pack the tenant's
identity provisioning runs a Job that creates the client, the scope and its
mappers in the **tenant realm** and maps the pack's `entitlementGroup` (the
app's group, §1.6) to its `clientRole`, so that the app's tokens carry the
role for people who were given the app. An app without a pack gets its client
from its app Composition.

A pack with `serviceClient: true` registers a confidential client for a
service that only validates tokens (`gentian-dovecot`); it has no scope, role
or group.

The tenant realm keeps Keycloak's built-in `browser` flow, so it
authenticates its own people with a credential form. Nobody reaches a tenant
realm through the kernel realm: `tenant-default` no longer composes the
identity provider `kernel` in a tenant realm, its mappers or the flow
`first-broker-login-gentian`, and the realm Job no longer makes the client
`broker-<tenant>` in the kernel realm. All four are switched off in place
(`$kernelBroker` in the Composition, `kernelBrokerEnabled` in the operator),
not removed.

### 1.8 Provisioning accounts in apps

The platform does not create, change or remove a person's account in an app.
An app makes the account when the person first signs in to it, from the token
or assertion of that sign-in. There is no event bus and no SCIM delivery.

One thing is synchronised: **who administers an app**. A profile may declare
the role an administrator holds in the app and a Job that applies it
(`spec.hooks.provisioning.privilegedRole`, `.syncJob`). The operator resolves
the members of `gentian:tenant:<t>:app-admins`, hands the list to the app's
own script in a Job in the tenant's namespace, and runs it again when the
membership changes (`internal/controller/app_privilege_reconciler.go`,
`internal/provisioning/privilege`). The script speaks the app's protocol; the
kernel knows none.

### 1.9 Tenant identity provisioning sequence

A tenant's realm is provisioned through the `tenant-default` Composition:
partly as `provider-keycloak` resources it declares, partly as Jobs that call
the Keycloak Admin API. The operator builds the Jobs' manifests
(`internal/controller/identity_reconciler.go`, `keycloak_*.go`, helpers in
`internal/keycloak/shell_helpers.go`; group names in
`internal/keycloak/groups.go`), hands them to the Composition in a ConfigMap,
and the Jobs run in `kernel-authentication`.

```mermaid
sequenceDiagram
  participant TR as TenantReconciler
  participant KC as Jobs in kernel-authentication
  participant K as Keycloak

  TR->>KC: realm Job
  KC->>K: create the tenant realm
  TR->>KC: gentian-groups Job
  KC->>K: ensure members/admins/app-admins/perimeter/app:* groups
  TR->>KC: admin Job
  KC->>K: create the tenant admin account, without a password
  opt apps with an OIDC pack, or with SAML
    TR->>KC: per-app client Jobs
  end
  TR->>KC: kernel-realm flow Job, mail-server Job
  TR->>TR: IdentityReady=True
```

Job names follow `{purpose}-{tenant}` (e.g. `keycloak-gentian-groups-demo`).
The reconciler waits for each Job via `waitForProvisioningJob` before
advancing. The zone's sign-in client (`gentian-edge-<zone>`), the fixed
groups, scopes and mappers are not Jobs: the Composition declares them. The
platform tenant adopts the kernel realm and gets only the groups Job.

What is declared and what is still a Job:
[tenant-identity-composition.md](tenant-identity-composition.md).

---

### 1.10 Keycloak version, features and upgrades

**Version.** Keycloak 26.8.0, from the `keycloakx` chart 7.3.2. The chart is
pinned in the `suze` Composition and XRD
([suze.yaml](../../crossplane/compositions/suze.yaml)); the image tag in
`keycloakVersion` ([values.yaml](../../kernel/services/keycloak-idp/manifests/values.yaml)),
because the chart trails upstream; the event listener is compiled against the
same version (`keycloak.version` in its `pom.xml`). The three move together.
`provider-keycloak` stays at v2.19.0.

**Features.** Nothing is switched on or off, so the release's defaults apply.
What 26.8 enables that 26.0 did not is inert until something asks for it:

| Feature | Stays off because |
|---|---|
| Standard token exchange, proof-of-possession (DPoP) tokens | per client; no client the platform creates sets `standard.token.exchange.enabled` or `dpop.bound.access.tokens` |
| SCIM API, fine-grained admin permissions v2 | per realm (`scimApiEnabled`, `adminPermissionsEnabled`), false in every realm the platform creates |
| JWT authorization grant, federated client authentication, Kubernetes service-account sign-in | need an identity provider or client authenticator configured for them; none is |
| Passkeys, update-email, workflows, client secret rotation | per realm policy, required action or client policy; none is configured |
| Recovery codes | the browser flow of a new realm lists them as disabled, so sign-in does not accept them and the account console does not offer them; only the required action exists |

**The API's audience is a custom audience.** `gentian-director` names the
platform's API, and no Keycloak client has that name. An audience mapper must
therefore use `included.custom.audience`: from 26.8 Keycloak leaves out an
audience that names a client which is absent or disabled, and the token then
carries no audience and every API call is refused.

**A mailed link verifies an address only if it says so.** From 26.8 an
admin-sent action link marks the address verified only when it names
`VERIFY_EMAIL`. Invitations and mailed activations name it; an activation link
that is shown instead of mailed does not, so that account's address stays
unverified.

**Token introspection checks the audience.** From 26.6 a client may introspect
only tokens that name it in `aud`. Dovecot validates an XOAUTH2 token by
introspecting it as `gentian-dovecot`, so a mailbox opens only for a token that
names `gentian-dovecot`, and which tokens do is declared:

- Each tenant realm on a cluster with its own mail server has the client scope
  `mailbox`. Its one mapper adds `gentian-dovecot` to the audience of an access
  token (`included.client.audience`: Keycloak leaves it out if that client is
  disabled or gone). Nothing else in the realm names that audience.
- The scope is an *optional* scope of the sign-in client of an app whose profile
  declares `requires.services.mail.imap.tokenSignIn`, and of no other client. A
  token carries the scope and the audience only when such an app asked for
  `mailbox` at that sign-in; Keycloak refuses the request of a client without
  the scope (`invalid_scope`).
- Dovecot requires the scope too (`scope = mailbox` in each realm's oauth2
  settings), opens the mailbox of the token's `email` claim, and refuses a
  mail program that names another address.

`gentian-dovecot` does not carry the client attribute that switches the audience
check off for it. The kernel realm has no `mailbox` scope, so no token of the
kernel realm opens a mailbox; app passwords are unaffected everywhere. Who may
do what with such a token: [security.md §2.16](security.md).

**Upgrading an existing cluster.** Keycloak migrates its database on first
start of the new version, and the migration is one-way: 26.0 cannot run on a
26.8 database, so going back means restoring the database.

1. Back up the `keycloak` database (and take the tenant exports).
2. Upgrade: sync the release. If the cluster's `Suze` claim sets
   `keycloak.chartVersion`, change it there; the default is 7.3.2. Keycloak is
   one replica, so sign-in is down while the pod restarts and migrates.
3. Verify: the pod is Ready, a person can sign in, the console lists people,
   and a tenant's Keycloak objects are Synced.

Sessions are kept in the database and survive the restart. Re-run the installer
step that configures the kernel realm once: it rewrites the CLI client's
audience mapper. 26.5 dropped PostgreSQL 13; the kernel's PostgreSQL is newer.

### 1.11 How a person gets into an app: three ways

Every app is behind the front door, which checks the session and whether this person may use this
app ([routing.md §4.1](routing.md)). What differs is how the app itself then learns who is there.
A profile says which, under `requires.services.identity`, and states exactly one:

| | The profile declares | Who speaks to the realm | What the platform registers | Holds the app's keys |
|---|---|---|---|---|
| **OIDC** | `identity.oidc` | the app | an OIDC client of the app's own (app Composition) | nobody but the app |
| **SAML** | `identity.saml` | the app | a SAML client with the address the app states (the tenant's identity Job) | nobody but the app |
| **Sidecar** | `identity.sidecar` | the platform's sign-in sidecar, beside the app | a SAML client for the sidecar, at an address the platform derives (app Composition) | the sidecar: what the profile declared for its handler |

Take them in that order. OIDC wherever the edition installed offers it; SAML for an app that
offers that and no OIDC; the sidecar only for an app that can do neither, because it is the one
way in which a program beside the app can become anybody in it.

**The sidecar's sign-in, step by step.** The person is signed in at the platform already.

1. They open the app. The front door admits them, or sends them to the realm first as for any app.
2. The app's front page is one of the profile's `entryPaths`, so the Gateway answers with a
   redirect to `/sso/login` on the same host.
3. `/sso/login` is a rule of the app's own route, behind the session and the bouncer. The request
   reaches the sidecar with the bouncer's identity headers. The sidecar remembers a new request
   and the address of the person it is for, sets a cookie that marks this browser, and sends the
   browser to the realm with a SAML request.
4. The realm recognises the person from their session and answers without asking: a page that
   posts a signed response to `https://<app host>/sso/acs`, the one address registered.
5. `/sso/acs` is a route of its own with no session, because that post comes from the realm's
   address ([routing.md §4.1](routing.md)). The sidecar checks the response
   ([security.md §2.12](security.md)) and runs the app's handler.
6. The handler finds or makes the person's account in the app, makes it an administrator's or an
   ordinary one, and makes a session for it. The sidecar writes the response: the app's session
   cookie, or a page that puts the session where the app's own page keeps it, and a redirect into
   the app.

**Who administers an app signed in to this way** is who holds the App Admin role: membership of
`gentian:tenant:<tenant>:app-admins`, given by the tenant's administrator in the admin console
(*Groups* → *Roles* → `app-admins`). The realm lists it in the signed assertion as the one role of the
sidecar's client, the sidecar reads it from the signed bytes, and the handler gives or takes the
app's administrator role at every sign-in. A tenant's administrator is not an app's administrator
for being that ([app-customization.md §2.3a](../app-customization.md)).

An app session made this way lasts at most an hour and never longer than the realm session it
came from. When it has run out the app sends the browser to its own sign-in page, which is an
entry path again: the steps repeat without the person being asked. A person who was removed, or
lost the right to the app, is refused by the front door on their next request, whatever session
the app still holds.

No password exists for the app. The sidecar is registered per app and per tenant, in the tenant's
realm, and is removed with the app.

**When the platform runs a sidecar at all.** Only for an install pinned to a digest, whose bundle
comes from a catalogue of the whole cluster and brings the handler; never from a tenant's own
catalogue, and never for a tenant that signs in in the kernel realm. Otherwise the Component is
held with `SignInSidecarRefused` and says why ([security.md §2.12](security.md)).

**What none of the three ways gives.** The identity headers an app or a sidecar receives are not
signed; network rules are what keep another pod from sending them. An app whose own page sends
a bearer token of the app's loses it on a session route, where the Gateway drops the client's
`Authorization` header, unless its entry declares `clientAuthorization: app` and the tenant's
perimeter approver approved it; sign-in stays required either way ([routing.md §4.1](routing.md)).
A client with no browser session reaches an app only through a public entry of `authMode: app`,
with a credential the app issued: the platform does not know that caller, and removing a person
from the realm does not end such a credential ([security.md §2.14](security.md)). And a sign-out ends an app's own session
only where the app can be told of it (§1.12).

### 1.12 Signing out: what ends, where, and when

A person signs out once, at `/oauth2/logout` on whichever host they are on. Four things have a
session for them, and each ends differently.

| Where the session is | What ends it | When |
|---|---|---|
| **The realm** (Keycloak) | the sign-out itself: the Gateway sends the browser to the realm's end-session endpoint with the session's ID token | at once |
| **The front door** (the Gateway's cookies, per host) | the host the person signed out on: its cookies are deleted. Every other host of the zone: its access token cannot be renewed against the ended realm session | at once on that host; within one access-token lifetime elsewhere — five minutes in a tenant realm ([routing.md §4.2](routing.md)) |
| **An app that signs people in itself** (its own OIDC client) and declares where it is told | the realm posts a logout token to the app, inside the cluster; the app checks it and ends its session | at once, if the app was running when the realm told it |
| **An app behind the sign-in sidecar** whose handler can end a session | the realm posts a signed logout request to the sidecar, inside the cluster; the sidecar checks it and has the handler end the person's sessions in the app | at once, if the sidecar was running when the realm told it |
| **Any other app session** | nothing the platform does. The app's own lifetime for a session | a sidecar's app: an hour at most. An app with its own client: as long as the app keeps a session |

**How the realm tells an app.** Server to server, at the app's own Service inside the cluster —
never at the app's public address, where every path is behind a session and the realm, which is
not a browser, has none. Nothing is opened for it at the front door.

- *An app with its own OIDC client* says in its profile which path of which of its entries takes
  the notice (`requires.services.identity.oidc.backchannelLogout`). The platform builds the
  address from that entry's own Service and registers it with the client
  ([app-customization.md §2.10](../app-customization.md)). The notice is an OpenID Connect
  logout token: a JWT the realm signs, naming the client, the person and the session.
- *An app behind the sign-in sidecar* (§1.11) declares nothing. The sidecar's client at the realm
  is registered with the sidecar's own Service as its SAML single-logout address; the realm posts
  a signed `LogoutRequest` there, and the sidecar calls the handler's `onLogout`.

**The realm tells once.** Keycloak 26.8.0 posts the notice when a person signs out or an
administrator ends their session. It does not post again if nothing answered, and it posts
nothing when a session merely runs out. So the notice shortens the common case to nothing; it
is not what bounds the worst case. The bounds are the lifetimes in the table.

**What the catalogue's apps do** (gentian-apps; each *yes* is shown end to end against the app's
real image and Keycloak 26.8.0):

| App | Told of a sign-out | The session then |
|---|---|---|
| Nextcloud (`nextcloud-base-ce`) | yes, its own client | ends for the browser that signed out |
| XWiki (`xwiki-ce`) | yes, its own client | ends, every session of that person |
| Docmost, OpenProject | yes, through the sidecar | ends, every session of that person |
| Activepieces 0.28.0 | the sidecar is told; the app has nothing to end a session with | lasts what is left of its hour |
| Nextcloud (`nextcloud-base-od`) | declared, not shown with its own image | unknown until it is |
| Open WebUI 0.10.2 | declared, not in effect: needs a switch and Redis ([roadmap.md](../roadmap.md) 2.27) | lasts as long as its own token |
| Element (Synapse) | no: nothing registers an address, and Synapse's switch is off | lasts as long as Synapse keeps it |
| Mathesar, Odoo | no: neither has an endpoint for it | lasts as long as the app keeps it |

**What an outlived session is worth.** Not a way in: the front door refuses a signed-out
person's next request to every app, within the five minutes above, whatever the app remembers.
What it costs is on a shared browser: the next person, admitted by the front door as
themselves, can be shown the previous person's account by an app that still has that person's
cookie. The security side of all of this is [security.md §2.15](security.md).

## 2. Administration UI

| Concern | Where |
|---|---|
| People and groups, app access | The administration console, `admin.<tenant domain>`: *Members*, *Groups*, *Apps*. It holds no Keycloak credential: the registrar does, one per realm, and checks the caller against OpenFGA |
| Realm session and lockout policy | The console's *Security* screen; a commit by the director, applied by `tenant-default` |
| Realm password policy | The same screen; the registrar's action `set-password-policy` on the realm (`can_set_policy`), in force at once. It is not in git: a realm that is rebuilt comes back without it, and it is then set again |
| Notices to a tenant's people | The console's *Notifications* screen |

Full description: [admin-console.md](admin-console.md).

---

## 3. MAC and IAM (layering)

IAM does **not** replace the MAC backbone:

- **MAC** — `tenant-{name}` namespace, NetworkPolicy, Kyverno ([security.md](security.md))
- **Identity** — per-tenant Keycloak realm
- **Authorization** — OpenFGA (ReBAC), fed from Keycloak group membership (§1.2)

Effective access is the **intersection** of all layers.
