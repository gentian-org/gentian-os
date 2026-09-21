# UI restructure: portal, console, App Store

Three user interfaces, one rule: **a UI carries no authority.** It renders
what an API returns and turns clicks and key presses into API calls made with
the signed-in human's token. It holds no ServiceAccount with write verbs, no
admin credential, no master key, and decides nothing — every "may this user
see or do X" is a verdict it received from a named enforcement point (AD-1,
[security-principles.md](../security-principles.md) §3), never a branch it
evaluated.

The one thing a browser UI cannot do without a server is hold its own OIDC
session: a single-page app cannot keep a confidential client secret or a
refresh token safely. So each UI may have a *backend-for-frontend* whose whole
job is token custody — cookie in, bearer out — plus same-origin relaying where
a browser needs it. That backend is identity **relay**, not authority: it has
no Kubernetes RBAC and no credential of its own beyond the OIDC client secret.

Where each UI runs is [architectural-decisions.md](architectural-decisions.md)
AD-10 and AD-3; this document is about what each one is allowed to be.

## 1. Portal

**What it is.** The tenant desktop: login, app tiles, embedded app windows,
notifications, an AI widget. In `gentian-ui`, `frontend/src/shell` and
`frontend/src/windows` (a window manager over iframes), served by the FastAPI
BFF in `backend/app`.

**What it holds today that it should not:**

| Held | Where | Why it is authority |
| --- | --- | --- |
| Tile visibility computed in Python from group names | `backend/app/core/shell_apps.py` — `is_admin`, `user_is_platform_admin`, `is_tenant_admin` | an authorization decision; `can_launch` tuples exist in OpenFGA and nothing reads them (G2) |
| The LiteLLM **master key**, read from `llm-sensitive-values` in `platform-kernel` and used to proxy chat | `backend/app/api/routes/llm.py` | the portal pod can spend every tenant's LLM budget; a kernel secret in a tenant-facing process |
| `patch` on `tenants` (the `app-privilege-requested` annotation) | `chart/templates/rbac.yaml` | a cluster write from a UI, used as a reconcile kick |
| A ServiceAccount that lists `appprofiles`, `tenants`, `apppackages` cluster-wide | same | reads for every tenant, filtered in Python |

**Target (AD-10).**

- The **shell** is a static bundle in `shared-shell`: no backend, no state.
- The **tenant desktop BFF** runs in `tenant-<t>` as a `tenancy: tenant`
  component of that tenant. It holds: the OIDC client secret for that
  tenant's realm, the session cookie ↔ bearer exchange, per-viewer
  preferences and the notification inbox (UI state, SQL in the tenant's own
  database, granted as a requirement), and the same-origin reverse proxy
  that embedded windows need. Nothing else.
- Tiles are `GET /v1/tenants/{t}/apps?viewer=me` on the director's read API:
  the list comes back already filtered by `can_launch` for the caller. The
  BFF does not know what an admin is.
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
token to the credential manager rather than holding an OpenBao token
(`credential_manager.py`). That is the pattern for everything else.

**Target.** Two deployments, one behaviour.

- The **platform-admin console** is a kernel service in `kernel-control`,
  authenticating against the kernel realm (AD-10). The **tenant admin
  screens** are part of the tenant desktop BFF — a tenant admin is a tenant
  user with more tiles, not a different application.
- Every write is a director call with the user's token
  ([operator-split-plan.md](operator-split-plan.md) §3.5): policies, grants,
  plans, backup settings and export/restore requests. Every read is a
  director read with the user's token, filtered by the caller's relations.
  The console's `rbac.yaml` has zero rules.
- **User and group administration** goes through the director too. The
  director is the platform's configuration API; identity writes are
  configuration, performed against Keycloak with a scoped service identity
  the director holds, after an FGA check on the human (`can_manage_users`
  on `tenant`). The console never sees a Keycloak admin credential. Whether
  identity writes deserve their own named PEP rather than the director is
  open (§4); either way the answer is not "the UI".
- Credentials keep going to the credential manager, as today.
- Audit is not a console feature. An admin action is a Keycloak event, an
  FGA decision and a commit joined by one request id (principle 7); the
  console shows that record, it does not keep one.

## 3. App Store

**What it is today.** `app-store-me`: a per-tenant app in `tenant-<t>` with
its own backend (`gentian-apps/apps/app-store`). It lists apps by reading
`AppProfile`, `AppCatalogue` and `AppPackage` cluster-wide with a
ServiceAccount, asks the commerce backend which profiles the tenant is
entitled to, and installs by calling the operator's lifecycle API with an
`X-Gentian-Actor` header. It also ships two dead install paths that must not
be resurrected: a direct `git push` to `gentian-deployments`
(`services/gitops.py`, `INSTALL_MODE=gitops`) and a direct `patch` of
`Tenant.spec.apps` (`k8s_client.add_tenant_app`). Neither is called from a
route; both are a fourth and fifth writer to the cluster's configuration
waiting for a config flag.

**Target (AD-3).** The App Store is a service **outside the cluster**,
operated by Gentian Technologies: a UI and an API over the schema in
[app-store-schema.sql](app-store-schema.sql). The cluster holds no
catalogue and no store backend.

What the store is:

- **A listing.** Presentation and commercial data — names, texts in every
  locale, media, categories, keywords, tiles, editions, plans, prices,
  subscriptions. Everything that was removed from the profile because it is
  reference data, not a deployment contract (component-profile.md §2).
- **An entitlement authority.** It answers "may tenant T run app A" with a
  **signed grant** (`entitlement_grant`, `signing_key`) that a cluster
  verifies offline, and logs every check (`entitlement_check`). Clusters
  register with the store and authenticate to it with a key pair
  (`cluster.public_key`).
- **A trigger.** It may ask the director to install. It never supplies the
  artefact.

What the store is not: a reader of the cluster. Install progress, quota
headroom, "already installed" — the store shows these by asking the
director's read API with the user's token, the same way the desktop does.
The store's ServiceAccount, `secrets get`, `pods list` and `resourcequotas
list` go away with the in-cluster deployment.

**The install flow.** Your description, with two corrections marked ◆:

```
tenant admin ─(browser, Keycloak session)─► App Store UI
   clicks Install on app A, version V

App Store UI ─► App Store API                       lists; nothing decided here
App Store API ─► director  POST /v1/tenants/{t}/apps/{A}
                           body: {catalogue, app, version, digest}   ◆ a reference, not the profile
                           Authorization: the tenant admin's token   ◆ the human's identity, not the store's
                           (or an RFC 8693 exchanged token with act — principle 5)

director:
  1. verify the token (kernel or tenant realm, JWKS)
  2. OpenFGA Check: user can_install_app tenant:{t}
  3. entitlement: ask the store for a grant for (t, A) — cluster key pair —
     verify its signature against the store's published signing key;
     a denial carries a reason and is logged with the request id
  4. fetch the profile bundle for A@V from the catalogue git repository
     (Repository/gentian-apps, read credential) and check it matches digest
  5. materialise ComponentProfile A@digest in the cluster
     (the only profiles the cluster holds are the installed ones)
  6. commit tenants/{t}/tenant.yaml with A added, signed, trailer with the
     decision and the request id
  7. 202 + /v1/operations/{id}

Argo CD syncs the commit ─► operator reconciles Tenant.spec.apps ─► Crossplane provisions
App Store UI polls /v1/operations/{id} with the user's token and renders progress
```

◆ **Reference, not profile.** "The component profile is forwarded from the
App Store API to the director" is the one step that must not happen as
written. If the director accepted a profile document from its caller, then
anyone who can call install could inject an arbitrary chart repository,
image, requirement or privilege into a tenant — the store would be a
supply-chain hole regardless of how well it authenticates. AD-3 and the
schema's own header put it as *"the store may TRIGGER, it may not SUPPLY"*:
the request carries `(catalogue, app, version, digest)`, the director fetches
the bundle from the catalogue repository and verifies the digest. The
technical spec never crosses the store boundary in either direction.

◆ **Whose identity.** The install call carries the human's token, so the FGA
check is on the human and the commit is authored as the human. The store's
own identity (the cluster key pair) is used only in the *other* direction —
the director asking the store for an entitlement grant. A store that could
install with its own credential would be a component with power, which is
what this document exists to remove.

"Added to the catalogue" then means step 5: a `ComponentProfile` CR appears
in the cluster for this app at this digest, because a tenant installed it —
not because a catalogue was synced. The `catalogue-<repo>` ApplicationSet
that syncs every profile to every cluster today is retired with this
(operator-split-plan.md §3.6).

## 4. Open decisions

- **Identity writes.** User and group administration is a Keycloak write,
  not a git write. This document routes it through the director because
  the director is already the configuration PEP and holds a scoped
  identity; the alternative is a fifth named PEP in AD-1's list. Decide
  before the platform console is split out — it is the only console
  function that does not map onto an existing director endpoint.
- **Token shape for the store's install call.** The user's own token (the
  store is a pure client; the token's `aud` must include the director) or an
  exchanged token carrying `act` (the store is an agent in the principle 5
  chain). Start with the first; the second is what an autonomous store
  action — a scheduled upgrade — will need.
- **Where the desktop's UI state lives.** Preferences and the notification
  inbox are the only state the BFF keeps. A per-tenant database granted as a
  requirement (the `{tenant}_shell` database exists today) is the default;
  if the desktop is ever a static bundle plus the director alone, that state
  moves to the director's read/write API as per-user documents.
