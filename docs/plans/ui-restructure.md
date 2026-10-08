# UI restructure: desktop, console, App Store

Three platform interfaces, one rule: **a UI carries no authority.** It renders
what an API returns and turns clicks and key presses into API calls made with
the signed-in human's token. It holds no ServiceAccount with write verbs, no
admin credential, no master key, and decides nothing — every "may this user
see or do X" is a verdict it received from a named enforcement point (AD-1,
[security-principles.md](../security-principles.md) §3), never a branch it
evaluated.

A single-page app cannot keep a confidential client secret or a refresh token
safely, so something server-side has to hold the session. In this architecture
that something is the **edge**: the gateway runs the code flow for the whole
tenant zone and forwards the token to the desktop — and to nothing else
(networking.md §4). A
*backend-for-frontend* therefore exists only for same-origin relaying and for
the small amount of UI state a desktop keeps. It runs no code flow, holds no
OIDC client secret, and has no Kubernetes RBAC — it is identity **relay**, and
after the edge terminates OIDC it is not even that.

Where each UI runs is [architectural-decisions.md](architectural-decisions.md)
AD-10 and AD-3; this document is about what each one is allowed to be.

## Who shows and does what

The current target. The sections below give the detail and the reasons.

| Interface | Where | For whom | Shows | Does |
| --- | --- | --- | --- | --- |
| **Desktop** (§1) | on the cluster, per tenant | everyone | the tiles the signed-in person may open | opens them. Nothing else: no install, uninstall, add-on, access, repository or store logic |
| **Admin console** (§2) | on the cluster, per tenant | administrators | people and groups, policies, resources, credentials, and under **Apps** every installed app: its state, who has access and whether it is for everyone, its integrations, its privileges | administers: gives and takes away access, sets "for everyone", approves privileges, uninstalls, purges. It installs nothing and lists no catalogue |
| **App Store app** (§3) | on the cluster, per tenant; absent when the licence report is off or no store is named | people who may install apps in the tenant | the store's data, at once and with no store account: apps, descriptions, pictures, reviews, evaluations and reports, versions, list prices. After a sign-in to the store: what the tenant has acquired | acquires from the store, which is what the sign-in to the store is for; installs through the director and sets the repository credential through the custodian |
| **The store** | outside the cluster, run by a vendor | — | nothing on the cluster. It serves data to the App Store app: the catalogue to anyone who asks, with no token; a tenant's standing and acquisitions to a person signed in to it | data and commerce only. It never calls the cluster |
| **Operations Console** | on the cluster, where installed | whoever looks after the cluster | [sovereignty-concept.md](sovereignty-concept.md) §5.1 | as described there; it is not changed by this document |
| **Command line** | the administrator's machine | administrators | — | `kubectl gentian apps install …` through the director. With no store it is the only way to install an app |

The three interfaces on the cluster are separate components in the
`gentian-ui` repository — the desktop, `apps/admin-console`,
`apps/app-store` — and none of them is a mode of another.

**The desktop is a relay for tiles.** It is the app of people who hold no
privilege. Code that installs, removes or grants, shipped to every member's
browser and reachable on every member's origin, is code a member can try to
drive; the checks behind it would hold, but the cheapest way to be sure a
path is not misused is for it not to be there. So administrative paths live
in the two interfaces only administrators are served.

**What comes from where.** The App Store app and the admin console never
show the same fact from two places:

| From the store, shown by the App Store app | From the cluster, shown by the admin console |
| --- | --- |
| What an app is: name, summary, description, pictures | What is installed, at which digest |
| Reviews and their summary | Its state: installing, ready, failing, and why |
| Evaluations and reports | Who has access, and the "for everyone" setting |
| Editions, versions and the digest of each | Its integrations |
| Prices | Its privileges: what it requests of the platform and what was approved |
| What the tenant has acquired | What its data is, when it is uninstalled and when it is purged |

The App Store app needs one fact from the cluster to be useful — which of
the store's entries this tenant has installed — and reads it from the
director like any caller. It sends the store none of it.

**Changed 2026-10-06.** This document used to describe the App Store as a
service outside the cluster with its own interface, shown in a window on the
desktop and asking the desktop to install through a message bridge, and the
console as carrying a catalogue view of the cluster's own sources. Both are
withdrawn (AD-3, AD-14). What is outside the cluster is the store's data;
its interface is the App Store app on the cluster; the desktop relays tiles
and nothing else; installed apps are administered in the console's Apps
tab; and with no store, apps are installed by command only.

## 1. Desktop

**What it is.** The tenant desktop, once called the portal: login, app tiles,
embedded app windows, notifications, an AI widget. In `gentian-ui`, `frontend/src/shell` and
`frontend/src/windows` (a window manager over iframes), served by the FastAPI
BFF in `backend/app`.

**What it holds today that it should not:**

| Held | Where | Why it is authority |
| --- | --- | --- |
| Tile visibility decided in Python | `backend/app/core/shell_apps.py` — `is_admin`, `user_is_platform_admin`, `is_tenant_admin` | The **rule** is right and stays: entitlement is membership of the app's own group and nothing else, a base with activated addons is entitled by those addons' groups, and an admin account sees admin tiles only. What is wrong is that it is decided here and nowhere else, so an app's hostname goes around it (G3), and that the admin flags are computed from group names rather than received as a verdict. The rule moves into `app#entitled` and the answer comes from the director |
| The LiteLLM **master key**, read from `llm-sensitive-values` in `platform-kernel` and used to proxy chat | `backend/app/api/routes/llm.py` | the portal pod can spend every tenant's LLM budget; a kernel secret in a tenant-facing process |
| `patch` on `tenants` (the `app-privilege-requested` annotation) | `chart/templates/rbac.yaml` | a cluster write from a UI, used as a reconcile kick |
| A ServiceAccount that lists `appprofiles`, `tenants`, `apppackages` cluster-wide | same | reads for every tenant, filtered in Python |

**Target (AD-10).**

- The **shell** is the static frontend bundle, served by the tenant desktop
  BFF from the same image on the tenant's own origin: no separate
  deployment, no state of its own.
- The **tenant desktop BFF** runs in `tenant-<t>` as a `tenancy: tenant`
  component of that tenant. It holds: per-viewer preferences and the
  notification inbox (UI state, SQL in the tenant's own database, granted as
  a requirement), and the same-origin reverse proxy that embedded windows
  need. Nothing else. In particular **no OIDC client secret and no session of
  its own** — the edge holds the zone's one confidential client and forwards
  the token, so the desktop consumes an identity rather than establishing
  one. That is what keeps a kernel-realm client secret out of
  `tenant-platform`.
- Tiles are the usher's answer (`GET /v1/tenants/{t}/tiles`,
  [operator-split-plan.md](operator-split-plan.md) §4.5): the list comes
  back already filtered by `can_launch` for the caller. The BFF does not
  know what an admin is.
- **A relay for tiles, and nothing administrative.** The desktop shows what
  the signed-in person may open and opens it. It carries no install,
  uninstall, purge, add-on, access-granting, repository or store logic — no
  route in its backend, no screen in its bundle, no dialog that confirms
  such an act on another interface's behalf. An administrator's tiles lead
  to the admin console and the App Store app, which are where those acts
  are.
- The AI widget calls the LLM contract with the **desktop component's own
  granted credential** (a requirement of its profile, per tenant, per key
  budget — G4), never a master key. Which is to say the desktop is an
  ordinary tenant component with an `llm` requirement, not a special case.
- Kubernetes RBAC for the BFF: none. It talks to the director and to the
  apps it embeds, with the user's identity, and to nothing else.

## 2. Console

**What it is.** The management screens: users and groups, notifications,
resources and plans, credentials, backups, MAC waivers, app grants,
customization debt. Today one set of routes in the same BFF
(`backend/app/api/routes/admin.py` and the `k8s_*` services), one
ServiceAccount, and the tenant boundary enforced by `isPlatformAdmin` checks
and `spec.tenant` filters in Python — which
[rbac.yaml](https://github.com/gentian-org/gentian-ui/blob/main/chart/templates/rbac.yaml)
says of itself: *"these verbs are the console's, not any admin's."*

**What it holds today that it should not:**

| Held | Why it is authority |
| --- | --- |
| `create/update/delete` on `backuppolicies`, `platformsecuritypolicies`, `appgrants`, `tenantexports`, `tenantexportschedules` | writes to the cluster as the console, for any tenant |
| `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | Keycloak admin over every realm, used for user and group administration |
| The admin-action audit log in SQL (`sql_audit_store.py`) | the only record of what an admin did lives in the UI's database, not in the three logs of principle 7 |

Two things it already does right and keeps: it verifies the user's token
(`backend/app/core/auth.py`, JWKS, issuer and audience) and it forwards that
token to the custodian rather than holding an OpenBao token
(`custodian.py`). That is the pattern for everything else.

**Target.** Two deployments, one behaviour.

- **One console, deployed per tenant.** The admin console is a component of
  its own (`gentian-ui/apps/admin-console`), installed for every tenant from
  its profile, not a mode of the desktop (AD-10). An admin account sees
  **only** admin tiles, and a member account only app tiles. Least privilege
  is per account, not per person: the account that installs apps and sets
  privileges holds no `members` or `app:*` group and cannot launch or sign
  in to any app; someone who needs both has two accounts
  ([roles-and-authorizations.md](roles-and-authorizations.md) §1,
  [iam.md §1.3](../design/iam.md)). The desktop enforces none of this — it
  renders what the director's read API returns for the account's relations,
  and `can_launch` derives from the app's **own entitlement group**, not from
  tenant membership — the rule `shell_apps.py` enforces today, answered by the
  director instead of recomputed here. A tenant admin never sees
  "deploy tenant" because no relation grants it, not because a flag hides
  it; a member never sees "install app" for the same reason.
- **The platform is a tenant** (AD-10). `Tenant/platform` adopts the kernel
  realm (`isolation.keycloakRealm: kernel`), and the platform-admin console
  is that tenant's desktop in `tenant-platform`: the same image and profile,
  with the platform screens unlocked by `admin from cluster`. A UI with
  no authority does not belong in `kernel-control`; what stays there is what
  has authority — the operator, the director, the custodian. The
  platform tenant is undeletable and its realm is adopted, never created or
  disabled, which is the one change the identity reconciler needs.
- **Per tenant, not shared, until certified.** The BFF holds one tenant's UI
  state and relays on one tenant's origin — per-tenant state in one instance
  is exactly what AD-4 refuses for `tenancy: shared`. (It no longer holds a
  client secret; the edge does.) The profile certifies
  `[tenant]` today; when the BFF is stateless-per-request or verifies the
  tenant natively, `shared` is added to the list and the platform admin may
  choose it per instance (target-component-structure.md §1) — a deployment decision,
  no schema change. The static bundle needs no such wait but gains nothing
  from sharing either (AD-10).
- Every write is a director call with the user's token
  ([operator-split-plan.md](operator-split-plan.md) §4.1): policies, grants,
  plans, backup settings and export/restore requests. Every read is a
  director read with the user's token, filtered by the caller's relations.
  The console's `rbac.yaml` has zero rules.
- **User and group administration** goes through the registrar
  ([operator-split-plan.md](operator-split-plan.md) §4.4). Identity writes
  are performed against Keycloak with a per-realm service identity the
  registrar holds, after an FGA check on the human (`can_manage_users` on
  `tenant`). The console never sees a Keycloak admin credential, and neither
  does the director: identity writes have their own enforcement point, so
  that the process that pushes to git cannot also write a realm.
- **The privilege queue.** An install whose profile asks for a privilege
  nobody has granted waits rather than failing, so the console shows it:
  pending requests with what is asked, the profile's stated reason, and who
  can say yes. A tenant administrator resolves the tenant-scope ones
  (`can_approve_privilege`); cluster-scope ones appear in the security
  officer's queue on the platform desktop (`can_approve`), which is the same
  screen across tenants. Approving writes a `PrivilegeGrant` through the
  director carrying the approver, the reason in their own words and an
  expiry — the console records nothing itself (target-component-structure.md §4.3).
- **The Apps tab.** Per tenant, every installed app, from the cluster's own
  reads (the director for what git declares, the usher for what the cluster
  made of it; [store-contract.md](../design/store-contract.md) §8). For each
  app:

  | Shown | Done here |
  | --- | --- |
  | Its state — installing, ready, failing — with the reason | |
  | Who has access | adding and removing people (`can_grant`) |
  | Whether it is "for everyone" | setting and clearing it (`can_grant`) |
  | Its integrations: what it is bound to | giving and withdrawing consent to one |
  | Its privileges: what it requests of the platform, and what was approved, by whom, until when | approving or refusing, in the queue below |
  | | **Uninstall** (`can_install_app`) |
  | What an uninstalled app left behind | **Purge** (`can_install_app`) |

  **Uninstall and purge are two different acts, and the difference is
  critical.** Uninstalling removes the app and **keeps its data**: the
  databases, the files and the secrets stay, and installing the app again
  finds them. Purging **destroys the data** of an app that is no longer
  installed, and cannot be undone. The cluster refuses a purge while the app
  is still installed or still being taken down, so no single act does both;
  the console shows them apart and names what a purge destroys.

  The tab installs nothing. Getting an app is the App Store app's (§3), or
  the command line's.
- **No catalogue.** The cluster renders no catalogue of its own in any
  interface (AD-14). The console's existing **Catalogues** tab — the bare
  index of the cluster's sources — is hidden, not deleted: the reads behind
  it remain, and nothing links to it. With no store, apps are installed by
  command only.
- Credentials keep going to the custodian, as today.
- Audit is not a console feature. An admin action is a Keycloak event, an
  FGA decision and a commit joined by one request id (principle 7); the
  console shows that record, it does not keep one.

## 3. App Store

**What it was.** `app-store-me`: a per-tenant app in `tenant-<t>` with
its own backend (`gentian-apps/apps/app-store`). It listed apps by reading
`AppProfile`, `AppCatalogue` and `AppPackage` cluster-wide with a
ServiceAccount, asked a commerce backend which profiles the tenant was
entitled to, and installed by calling the operator's lifecycle API with an
`X-Gentian-Actor` header. It also shipped two dead install paths that must
not be resurrected: a direct `git push` to `gentian-deployments`
(`services/gitops.py`, `INSTALL_MODE=gitops`) and a direct `patch` of
`Tenant.spec.apps` (`k8s_client.add_tenant_app`). What was wrong with it was
never that it ran on the cluster. It was that it held a ServiceAccount, read
every tenant's objects, and installed as itself.

**Target (AD-3).** Two things with one name between them, and the line
between them is the point.

**The store** is a service outside the cluster, run by a vendor. It holds
and serves **data**: what apps are, their descriptions and pictures, reviews,
evaluations and reports, editions and versions with their build digests,
prices, and the record of what a tenant has acquired. It does commerce: the
account a tenant's administrator has with the vendor, the checkout, the
credential for the vendor's repository. It never calls the cluster and has
no page inside the cluster's interface.

**The App Store app** is the store's interface, and it is on the cluster: a
platform UI of its own, component `app-store`, in the `gentian-ui`
repository beside the desktop and the admin console.

- **Per tenant, for people who may install.** Installed for each tenant but
  the platform tenant; its tile is shown only to people who hold
  `can_install_app` there, and its address asks the same of whoever opens
  it.
- **Absent without licence reporting.** The store depends on the cluster's
  licence report ([operations.md §6.2](../design/operations.md)). A cluster
  with reporting off, or one that names no store, has no App Store app.
- **Placed by the operator.** Built: the operator chart ships its profile,
  and the operator places and removes the component by those two conditions
  ([store-contract.md §6](../design/store-contract.md)).
- **It renders data.** What it fetches from the store's API is treated as
  data: plain text, a restricted Markdown subset for descriptions and
  release notes, images by address. Never as code — no HTML, no script, no
  frame from the store.
- **It does the installing**, on the cluster's side, under the signed-in
  person's own token: the repository and the install through the director,
  the repository's credential through the custodian. Both ask of it what
  they ask of every other caller. Like every UI here it carries no
  authority: no ServiceAccount with write verbs, no credential of its own.
- **It shows the catalogue at once.** Browsing needs no store account: the
  store's catalogue reads take no token, and the app makes them anonymously.
  An anonymous read tells the store only the address the request comes
  from; the app sends no cluster or tenant identifier with it.
- **It signs the person in to the store only for what concerns the tenant**
  — acquiring, and seeing what the tenant has acquired — with their account
  at the vendor, as a public client with no secret. Its backend exchanges
  the code and keeps the token, bound to the administrator's cluster
  session and never handed to the browser; the issuer redirects to the
  app's one callback and nowhere else (the exception AD-13 states). That
  token is valid at the store and nowhere on the cluster, and it is not
  what pulls images. Whether the store serves the tenant
  is learned at that sign-in, not before; the catalogue is readable either
  way.
- **Not an entitlement issuer, and neither is the store.** The store decides
  nothing for the cluster and the cluster does no licence gating (AD-3).
  There is no signed grant, no store key on the Cluster claim, and no tuple
  for a tenant's right to an app. Whether something has been paid for is the
  business of whoever sells it, and it is enforced where the thing sold is
  handed over: at the repository the app's chart and images are pulled from.

What the store hands over when it confirms an app is therefore not a
permission but an identification of the build, and where needed the key to
its repository:

| Part | Nature | Where it goes |
| --- | --- | --- |
| `(catalogue, app, digest)` | which build | the install request; the digest is recorded in git with the install |
| the repository's address | configuration | declared through the director: a commit, with an author |
| the credential for it | a secret, the tenant's, minted by the store and checked by the store's repository | OpenBao, set through the custodian like any other tenant credential; never in git, never in the install request |

That is what makes a private (proprietary) app work: its images are
reachable only with a credential the tenant holds, so "available only after
payment" is true where it is enforced, and a cluster holds no standing
credential to any vendor's repository. A free app is the same flow with
nothing to hold.

The contract between the two is [store-contract.md](../design/store-contract.md).
The format of the store's API — every endpoint, field and error — is an
artefact of this plan, written so that a store can be built against it:
[artefacts/store-api.md](artefacts/store-api.md) in prose and
[artefacts/store-api.openapi.yaml](artefacts/store-api.openapi.yaml)
machine-readable, the second being the one that counts where they differ.
The licence report the store depends on is
[artefacts/licence-report.openapi.yaml](artefacts/licence-report.openapi.yaml).

**The flow.**

```
tenant admin ─(browser, edge session)─► App Store app, its own tile

App Store app ─► store   GET /v1/meta, /v1/apps, …   no token, no sign-in — rendered as data

  admin asks for app A — the first thing that needs the store account
App Store app ─► store   sign-in at the store's issuer (PKCE, tenant_url)
              ─► store   GET /v1/tenant           served? notices? — shown as they are
              ─► store   POST /v1/acquisitions {coordinate}
       ◄─ 201 confirmation                        a free app, or no checkout needed
       ◄─ 202 {id, checkoutUrl}                   paid: the checkout opens in a separate window, at the store;
                                                  then GET /v1/acquisitions/{id} until it is confirmed
   confirmation: {coordinate, version, digest, repository?: {type, url, credential}}
   the app computes the repository's name from the tenant and the url (store-contract.md §6.4)

  admin says install, for everyone or not          ◆ every call below carries the admin's own token
App Store app ─► director   PUT  /v1/tenants/{t}/repositories/{name}   {role: apps, type: oci, url}
              ─► custodian  PUT  /v1/credentials/repository-{name}     the credential
              ─► director   POST /v1/tenants/{t}/apps/{A}              {coordinate, digest, defaultGrant}
                                                                       ◆ a reference, not the profile

director:
  1. verify the token (kernel or tenant realm, JWKS)
  2. OpenFGA Check: user can_install_app tenant:{t}        the only question asked
     (and can_grant, when the install is for everyone)
  3. fetch the profile bundle A from the catalogue source, check it hashes
     to the digest in the request; refuse and write nothing if it does not
  4. commit ComponentProfile A to the cluster's catalogue directory
     (the only profiles the cluster holds are the installed ones)
  5. commit tenants/{t}/tenant.yaml with A added and its digest as a field of
     the entry — signed, trailer with the decision and the request id
  6. 202 + the commit

Argo CD syncs the commit ─► operator reconciles Tenant.spec.apps into a Component
   carrying the digest ─► the app's chart and images are pulled
   with the tenant's credential for their repository, if it holds one
The admin console's Apps tab reads the app's state and shows progress
No credential: the pull fails and the app reads as `failing`, with the reason
```

◆ **Reference, not profile.** If the director accepted a profile document
from its caller, then anyone who can call install could inject an arbitrary
chart repository, image, requirement or privilege into a tenant — the store
would be a supply-chain hole regardless of how well it authenticates. The
request carries `(coordinate, digest)`, the director fetches the bundle from
the catalogue source the Cluster claim names and verifies the digest. The
technical spec never crosses the store boundary in either direction. The
digest is not signed; it does not need to be, because it grants nothing — it
only says which bytes, and the person stating it is the one already
authorised to change the tenant. A store that names a wrong digest gets a
refused install, not a different build
([store-contract.md](../design/store-contract.md) §7).

◆ **Whose identity.** Every call to the cluster carries the human's token,
so the FGA check is on the human and the commit is authored as the human.
The App Store app has **no identity of its own** toward the cluster, the
store has none either, and the cluster holds no key of the store's. A store
that could install with its own credential, or whose signature could admit
an install, would be a component with power, which is what this document
exists to remove.

◆ **No secret in git, so no private repo for the sake of it.** A credential
committed to git — however private the repo — is readable by Argo CD, every
clone, CI, break-glass and the history after rotation: a credential with no
revocation. The repository's address is a commit; its credential goes to the
vault. The deployments repository stays public-capable; whether it is
private is decided by whether the *facts* in it (tenant names, plans,
installs) are sensitive, never by the need to hold a secret.

"Added to the catalogue" then means step 4: a `ComponentProfile` CR appears
in the cluster for this app at this digest, because a tenant installed it —
not because a catalogue was synced. The `catalogue-<repo>` ApplicationSet
that syncs every profile to every cluster today is retired with this
(operator-split-plan.md §4.1).

**Without a store.** The same director route, asked from the command line:
`kubectl gentian apps install …`. No interface on the cluster lists what
could be installed (AD-14).

## 4. Open decisions

- **Identity writes.** User and group administration is a Keycloak write,
  not a git write. This document routes it through the director because
  the director is already the configuration PEP and holds a scoped
  identity; the alternative is a fifth named PEP in AD-1's list. Decide
  before the platform console is split out — it is the only console
  function that does not map onto an existing director endpoint.
- **The App Store app's calls are the user's own.** Settled: the app calls
  the director and the custodian with the signed-in person's token and has
  no identity of its own. An unattended act — a scheduled upgrade, say —
  has no caller in this design (a repository credential does not expire,
  so none is needed for it); what would make one is an agent identity with `act` (principle 5),
  and it is not built.
- **What the store's owner still has to decide** is listed with the API
  format: [artefacts/store-api.md](artefacts/store-api.md) §9 — token
  lifetimes, how an account becomes a tenant's, who may write a review.
  Decided since: the tenant administrator's account acts for the tenant,
  and a repository credential does not expire for now.
- **Where the desktop's UI state lives.** Preferences and the notification
  inbox are the only state the BFF keeps. A per-tenant database granted as a
  requirement (the `{tenant}_shell` database exists today) is the default;
  if the desktop is ever a static bundle plus the director alone, that state
  moves to the director's read/write API as per-user documents.
