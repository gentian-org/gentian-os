# Implementation plan

The order things get built in, and where each one stands.
[work-packages.md](work-packages.md) says *what* each package is; this says
*when*, what blocks what, and what is true today.

Status is one of three things, and nothing is called done on the strength of a
build alone:

| | |
|---|---|
| ✅ | done, and seen working on a cluster |
| ◐ | built, not verified — or done in part, with the rest named |
| ☐ | not started |

Step labels (`S1`…`S8`, `S7A.1`…) exist so a conversation can point at one.
They are not a second plan, and they are not renumbered when something lands.

---

## 1. Sequence at a glance

### M1 — the platform administrator signs in and sees the cluster

| Step | What it is | |
|---|---|---|
| S1 | Vocabulary: the Keycloak groups model v1 names | ✅ |
| S2 | `Tenant/platform`, adopting the kernel realm | ✅ |
| S3 | The edge: two Gateways, the zone's client, the session, the enforcement point | ◐ kernel zone done, tenant zones unproven |
| S4 | The desktop as a `Component` | ✅ |
| S5 | The desktop without authority | ✅ |
| S6 | Kernel UIs behind the kernel session | ✅ |
| S7 | Retire the portal in `kernel-edge` | ✅ |
| **S7A** | **What is wrong that a reinstall would only reproduce** | ◐ see below |
| S8 | Purge and reinstall | ☐ |

### S7A — before the purge

| | | |
|---|---|---|
| S7A.1 | The operator produces a zone's Keycloak client and its secret | ✅ |
| S7A.2 | The director stops writing authorization state | ◐ entitlements left; read-only token impossible |
| S7A.3 | The platform administrator is an address | ✅ |
| S7A.4 | The admin console is an app, and it talks to the director | ◐ every screen wired; untested against a cluster that can push |
| S7A.5 | Keycloak looks like the rest of the product | ✅ |
| S7A.6 | The console and the desktop hold nothing | ◐ console yes, desktop still holds a Keycloak credential |
| S7A.7 | The zone cookie does not reach the applications | ☐ |
| S7A.8 | A read-only view of the authorization state | ☐ |
| S7A.9 | The kernel UIs are actually usable | ✅ |
| S7A.9b | A refusal a person can act on | ✅ |
| S7A.10 | The installer does what it claims | ◐ 1 of 7 done |
| S7A.11 | Signing out does not ask a second time | ◐ built, not verified |
| S7A.12 | The tile catalogue leaves the director | ✅ |
| S7A.13 | `denyPaths` promises a control it does not apply | ☐ |
| S7A.14 | A release reaches a cluster by an immutable name | ✅ |
| S7A.15 | A zone's hosts follow the components, not a list | ☐ |
| S7A.16 | The app-lifecycle API authenticates nobody | ☐ |

### What is left, in the order to do it

1. **S7A.4 — test the console against a cluster that can push.** Every
   screen reads real state and every write is a commit or an action, but no
   write has ever succeeded here: the director mounts
   `deployments-git-credentials` optionally and the Secret does not exist, so
   every write answers 503. Supplying `GENTIAN_DEPLOYMENTS_GIT_TOKEN` and
   applying the `deployments` Repository claim is what turns the screens on.
   Three refusals stay by design until the work behind them lands: minting a
   backup key, a group-scoped notification audience, and audit events beyond
   the change history (roadmap §1.12).
2. **S7A.6 — remove the bundled console from the desktop.** Its Resources tab
   still calls an operator `PUT` that no longer exists, and the image carries
   a Keycloak admin credential that goes with it.
3. **S7A.8 — the authorization view**, the last console screen.
4. **S7A.15 — zone hosts derived from what the operator routes.**
5. **S7A.16 — the app-lifecycle API authenticates nobody.**
6. **S7A.11 — verify sign-out**; it is built and unverified.
7. **S7A.7 — scope the zone cookie per host**, before any third-party
   application is routed.
8. **S7A.13 — `denyPaths`**: build it or take it out of the CRD.
9. **S7A.10 — the installer's remaining six**, of which tenant teardown
   blocks S8.
10. **S8 — purge and reinstall.**
11. **After M1**, the work packages in the order in §5.

---

## 2. M1 — what it means

**The platform administrator signs in and sees the cluster, in the plans'
shape.** `install.sh --layout v5` runs end to end on a purged cluster;
`admin@<kernel>` signs in once at `console.<kernel>` and sees the platform
tenant's desktop; the kernel consoles are tiles the director answered from
that account's relations; each opens signed in, with no second login and no
token to paste. Nothing about it is a stand-in: the desktop is
`Tenant/platform`'s, the edge holds the session, the desktop holds no
authority.

What each step meant, and what landed:

- **S1 Vocabulary ✅** — `gentian:platform:admin` replaces the bootstrap's
  `superadmin` everywhere it is written or read: identity bootstrap, the
  claim's `platformRoles` default, Argo CD's policy, the proxy's binding, the
  OpenBao OIDC roles, the desktop's constant.
- **S2 Platform tenant ✅** — `Tenant/platform` with
  `isolation.keycloakRealm: kernel`, the realm adopted and never created,
  disabled or deleted; tenant namespaces carry `gentianos.io/tier: tenant`;
  the v5 ApplicationSets sync `clusters/<c>/tenants/*`.
- **S3 The edge ◐** — `authenticated` and `perimeter` Gateways in
  `kernel-edge` under `mergeGateways`, reconciled from the Cluster claim; the
  zone's confidential client; `SecurityPolicy` OIDC with the zone cookie on
  `.<kernel>`; the edge authorization service in `kernel-edge` with its route
  table written by the operator beside the routes. The kernel zone runs.
  **A second tenant zone has never been stood up**, which is what S7A.1's
  "done when" asks for and the one part of M1 still open.
- **S4 The desktop as a component ✅** — a `ComponentProfile` and a Component
  reconciler that gives every tenant its desktop: a provider-helm Release in
  `tenant-<t>`, the database fulfilled in the tenant's namespace, the route
  and `SecurityPolicy` from `expose[]`.
- **S5 The desktop without authority ✅** — the BFF consumes the token the
  edge forwards and runs no code flow; no client secret, no Keycloak admin
  credential, `rbac.yaml` empty; tiles from the director.
- **S6 Kernel UIs behind the kernel session ✅** — Argo CD, Headlamp and the
  Keycloak console carry the zone's `SecurityPolicy` and the edge's
  `can_configure` / `can_audit`; each tool's own OIDC login is the silent
  second factor. `id-admin.<kernel>` was retired when Keycloak turned out not
  to work across two hostnames; `/auth/admin/*` is served on `id.<kernel>`.
- **S7 Retire the portal ✅** — the portal in `kernel-edge`,
  `portal.<kernel>`, its secret and BFF client, and `kernelPortalHost` are
  gone; `www.<kernel>` is an alias of the console.
- **S8 Purge and reinstall ☐** — §4.

Everything above was verified on beefy1 only, never on a purged cluster. S8 is
what turns that into an install.

---

## 3. S7A — the steps

A purge and reinstall proves the installer. It does not fix a design, and
reinstalling with these open would only reproduce them.

### S7A.1 ✅ The operator produces a zone's Keycloak client and its secret

A **zone** is one sign-in domain: a hostname, the realm behind it, one
confidential Keycloak client for the edge to hold the session with, that
session's cookie names, and the Gateway listener that serves it. Everything
that *consumes* a zone was already general; nothing *produced* one except a
shell Job in `D-02`.

**Done.** The tenant composition emits it all: an External Secrets `Password`
generator and an `ExternalSecret` in the edge namespace, the `Client` pushed
that generated secret through `clientSecretSecretRef`, its `director-audience`
mapper, and a `ClientDefaultScopes` naming Keycloak's six own defaults so
`groups` stays off. It renders for a tenant that adopts the kernel realm too,
so the platform tenant composes `gentian-edge-kernel` like any other and the
client creation has left `portal-login-bootstrap.sh`.

Verified on the cluster on 2026-09-24, including the three things this plan
listed as unproven: the `Password` generator honours `secretKeys`,
`DeriveFromObject` readiness holds the `Client` back until its Secret exists,
and **the client adopts** — `platform-edge-zone-client` reached
`Synced=True Ready=True` against the existing `gentian-edge-kernel`. Taking
over on a live cluster costs one deleted Secret (External Secrets refuses to
adopt a Secret it does not own) and a sign-in outage of about a minute.

Two things to know. There is no back-channel logout URL: session revocation
was deleted in S7A.2 and re-adding the URL would restore a write to the
authorization graph that has no reader. And the zone secret is generated
rather than derived from the master password and is not written to OpenBao, so
`SECRET_MODE=derived` reproducibility no longer covers it — which is fine,
because both sides read the same Secret.

**Still open:** the "done when" of this step was *a second tenant* signing in
at `console.<t>.<kernel>` with no installer step having run for it. That has
not been tried. It is the same code path, and the kernel zone is the harder
case, but it is unproven.

### S7A.2 ◐ The director stops writing authorization state

The director's job is to read the graph to decide whether a caller may make a
call, and to write git. As built it also wrote OpenFGA in six places.

| Write | Where it went |
|---|---|
| the store object and the model at first start | operator, applied as configuration |
| cluster roles from the claim | operator (`AuthzProjectionReconciler`) |
| tenants, `operated_by`, tenant roles | operator |
| membership from Keycloak events | operator (`MembershipListener`) |
| session revocation | **deleted** |
| entitlement tuples | still the director's, gated on `DIRECTOR_STORE_KEYS` |

Session revocation existed to make a logout immediate while the realm's access
token lived twelve hours. The realm now issues five-minute tokens against a
twelve-hour session, so the edge's refresh fails within one token lifetime of
the session ending, and the tuple, the write, the endpoint and the background
sweep are all gone. Membership followed: Keycloak's listener posts to the
operator now — same path, same signed statements, a different host.

**What cannot be done as this plan assumed.** "Take the write capability off
the director's OpenFGA token" is not available: OpenFGA authenticates with a
preshared key, and a key carries no scope, so every key that may read may also
write. The rule is therefore enforced by the director having no code that
writes, which is weaker than a credential that cannot. The alternatives are an
authorizing proxy in front of OpenFGA, or OpenFGA's OIDC auth mode with
something that maps a subject to permitted operations. Neither is small.
Worth a decision rather than a silent assumption.

**Left:** the entitlement tuples, which the App Store's signed statements
produce and which are off on this cluster.

### S7A.3 ✅ The platform administrator is an address

The kernel realm maps the `email` claim to the username, so the email *field*
can hold the recovery address. That only works when the username is itself an
address. **Done:** the bootstrap creates `admin@<kernel>` — the same pattern
every tenant administrator follows — and deletes the `administrator` account
it replaces. The password derivation label is unchanged on purpose: it decides
the derived value, and renaming it would silently change the password on every
cluster. Nothing may create a username that is not an address.

### S7A.4 ◐ The admin console is an app, and it talks to the director

**Every screen is wired.** What is left is four things, each for a reason
rather than for want of time, and all four are in `NOT_YET_MAPPED` with that
reason beside them: minting a backup key (a key minted in the console is a
key the console held), a notification addressed to groups (the group list is
Keycloak's and nothing here holds a credential for it), the audit panes that
need stores which do not exist (roadmap §1.12), and the authorization summary
(S7A.8). The screens themselves all read real state and all writes commit or
act.

It was a route inside the desktop image, built against a Keycloak admin
credential the desktop no longer holds. It becomes a component like any other,
visible only to holders of the relation, whose only job is to be a GUI over
the director's API.

**Shipped and running.** It lives in `gentian-apps/apps/admin-console`, built
from the restated app template, described by the `admin-console`
`ComponentProfile` the operator chart ships, published as chart
`admin-console` with its own images, and installed into every tenant by
`defaultForTenants`. Its backend is a relay and nothing else: it holds no
credential, keeps no state, forwards the caller's own token, and hands back
what the director answered — including a refusal. Putting the product's own
console through the app template was the point, and it found three gaps the
template had (below).

**People are not in this console.** Decided, not deferred. Declaring people in
git is worse than it sounds — git is append-only, so a name and an address
committed there outlive the account, which collides with erasure. Keycloak's
own console already manages them, is maintained, and since 26.2 its
fine-grained admin permissions can be scoped so a tenant administrator manages
only that tenant's users without holding `realm-admin`. So the People screen
is the Identity tile, embedded like any other component.

**Screen by screen.** A screen whose director endpoints do not exist yet
answers 501 naming itself, so the console shows exactly that rather than a
spinner or a fabricated empty state; `NOT_YET_MAPPED` in its `admin.py` is the
worklist, and a screen leaves it by getting real routes.

| Screen | Reads | Writes | |
|---|---|---|---|
| Tenants: list, create, retire | director, from git | director → `tenants/<t>/tenant.yaml` | ✅ |
| Cluster settings | director, from the Cluster claim | director → `kernel/claims/cluster.yaml` | ✅ |
| Resources: plans, ceilings, usage | director, relaying the operator | director → `tenants/<t>/resource-plan.yaml` | ✅ |
| Apps in a tenant: install, remove, addons | director, from git | director, endpoints that exist | ◐ endpoints exist, screen not built |
| Backup and backup policy | operator state | policy → git; taking one is an action | ✅ |
| Backup schedules | operator state | through the policy they are derived from | ✅ |
| Security policies | git, applied to the realm by the composition | director → `tenants/<t>/security-policy.yaml` | ✅ |
| Audit: what changed, and what allowed it | director, from git | — | ✅ |
| Audit: sign-ins, refusals, reads of data | not recorded anywhere yet | — | ☐ roadmap §1.12 |
| Integrations: bindings and grants | operator state | director → `tenants/<t>/grant-<app>.yaml` | ✅ |
| Notifications | the desktop's own table, read by the operator | publishing is an action | ✅ |
| Platform security: MAC waivers | git, joined with what the catalogue asks | director → `kernel/claims/platform-security-policy.yaml` | ✅ `can_set_admission` is break-glass |
| Customization debt | `Customization` CRs | — | ✅ |
| Credentials | credential manager, as the caller | the same | ✅ |
| Authorization view | OpenFGA, read-only | — | ☐ S7A.8 |
| People and groups | **not here** — Keycloak's own console, embedded | | ✅ |

**Resources is the shape the rest should follow.** Its write already went to
git, but through the operator's own HTTP API, which took an actor from a
header and recorded the plan change when the request arrived. Now:
`console → director → git → Tenant → operator`. The director relays the
operator's reads under `can_view`, checks `can_set_plan` for the write,
validates the choice against the operator's own catalogue, and commits as the
person; the operator dropped its `PUT` and records the plan event when the
change *lands* on the Tenant, with the chooser carried in an annotation. What
is billed is then what the cluster enforced, and it still names who chose it.

**Credentials needed one thing first**, and it landed: a
`credentialManagerUrlKey` on the profile's platform mapping, the way
`directorUrlKey` works. Without it the component does not know where the
credential manager is and cannot relay to it.

**No write has succeeded on this cluster yet.** The director mounts
`deployments-git-credentials` optionally and that Secret does not exist, so a
push fails with `could not read Username` and every write answers 503 naming
`GENTIAN_DEPLOYMENTS_GIT_TOKEN`. The Secret is what the `deployments`
Repository claim's composition materialises from OpenBao, which is why the
installer now scaffolds that claim whether or not a token was supplied.

**Three gaps in the app template**, found by putting the console through it
and all three fixed there rather than worked around in the console: the chart
named a platform Secret unconditionally, so a component with no database sat
in `CreateContainerConfigError`; the tile field was called `on`, which YAML
1.1 reads as the boolean `true`, so a hand-written profile was refused by the
schema; and the profile had no way to say a tile's relation is held on the
tenant rather than on the app.

### S7A.5 ✅ Keycloak looks like the rest of the product

Embedding Keycloak's console makes its appearance the product's appearance.
The login screen was already themed; the administration and account consoles
were not. **Done** as a shared `gentian-tokens.css` generated from the design
system and applied to all three themes, plus the logo and favicon. Both
consoles are compiled React applications on PatternFly, so a theme changes
styling and not layout — and overriding the templates is a surface we
deliberately do not own, because Keycloak's own guidance is that custom
templates are reworked on every upgrade.

### S7A.6 ◐ The console and the desktop hold nothing

A rule to check before either is called finished: a UI offers a surface for
making requests, and every one of those requests is decided somewhere else.
Neither may hold an OIDC client secret, a Keycloak credential, or any
credential belonging to a person other than the caller; neither may hold a
Kubernetes identity (`rbac.create` false, ServiceAccount token unmounted); and
neither may hold a decision — showing or hiding a screen follows an answer the
director gave, and hiding a thing is never what stops someone reaching it.

**The desktop meets this. The new admin console meets it.** What does not is
the desktop's *bundled* copy of the old console: `Security` and `Audit` read
through a Keycloak admin client (`keycloak_security_policy_store.py`,
`keycloak_audit_fetcher.py`), so the desktop image still carries a credential
that can read and write the realm. Re-pointing those two screens at the
director is what lets the bundled console be deleted, and the credential goes
with it. That is why they are early in §1's order.

When either grows a screen that seems to need a credential, that is the signal
that an endpoint is missing from the director — not that the UI needs the
credential.

### S7A.7 ☐ The zone cookie does not reach the applications

The one place the edge session is weaker than a session per application.

The zone's cookie is scoped to `.<kernel>` so one sign-in covers every host in
the zone, which means the browser sends it to each of those hosts and nothing
removes it before the request reaches the application. An application that is
compromised, or careless with what it logs, sees a credential good for every
other application in the zone.

**Not in the authorization service**, which was the first idea and is wrong:
its header mutations are applied before the remaining filters run, and it runs
before Envoy's OIDC filter, so a cookie stripped there would be invisible to
the filter that has to validate it. Sign-in would break.

Three that do work:

1. **Scope the cookie to the host** — drop `cookieDomain`. Each host gets its
   own edge session; the first request to each does one silent round trip to
   Keycloak, because the Keycloak session already exists. Single sign-on is
   preserved, logout still works everywhere at once, and what the browser
   sends an application is good only for that application.
2. **Rewrite `cookie` at the router stage** with a Lua extension policy, after
   every filter has run. Envoy Gateway supports this from 1.3; the cluster
   runs 1.2.5.
3. **Remove the whole `Cookie` header** at the route. Available today and too
   blunt: applications behind the edge set their own cookies.

Recommendation: (1), measured first. Do it before any third-party application
is routed.

### S7A.8 ☐ A read-only view of the authorization state

Part of the same console, named separately because it replaces the idea of
exposing OpenFGA's own playground. OpenFGA's read APIs answer "which groups
hold which relations on which objects" with no write surface. The console
renders that; anything a person wants to change is changed on the screens
above, through the director, into git. No development-only UI, no second write
path.

### S7A.9 ✅ The kernel UIs are actually usable

All three verified on the cluster.

- **Argo CD** showed an empty list to a full administrator: the groups claim
  carries the full path `/gentian:platform:admin` because OpenBao's roles need
  it, and Argo CD's policy named the bare form. Fixed by naming both.
- **Headlamp** asked for a second sign-in that failed with
  `unauthorized_client`: it builds the code exchange from the `auth-provider`
  block of the kubeconfig it proxies with, which named `client-id: headlamp`
  with no secret — authenticating with nothing against a confidential client.
  The kubeconfig is now a Secret carrying a placeholder that the portal
  bootstrap rewrites with the real secret. The remaining question is cosmetic:
  whether to start that flow from the tile or give Headlamp an authenticating
  sidecar, which is the pattern an app with no OIDC support would need anyway.
- **The Keycloak console** showed a spinner. Not an iframe problem: embedding
  works and the silent SSO completes, then the first Admin REST call answers
  401 because `KC_HOSTNAME` and `KC_HOSTNAME_ADMIN` differ — closed upstream
  as not planned, so a new tab fails identically. `/auth/admin/*` is now
  served on `id.<kernel>` behind the kernel session and `id-admin.<kernel>` is
  retired.

That last fix produced a distinction worth keeping: the console mints a token
of its own inside the page, so the edge must leave that header alone, which is
neither of the two things it knew how to do. Stripping it — right everywhere
else, because a backend should get identity headers rather than a token it
cannot use — answered 401. Forwarding the edge's own token answered "Token
issued for an application that is not the admin console", which was true. So
`forwardToken` (the edge puts *its* token on the request) and
`keepClientToken` (the caller's own bearer survives untouched) are two flags,
and a route asks for the one it means.

### S7A.9b ✅ A refusal a person can act on

An account that is deleted or renamed leaves live sessions naming a subject
the graph no longer knows. Every relation is then denied, correctly, and the
answer was the bare word `Forbidden` on every page — including the desktop the
person would have signed out from. A refusal on an `oidc` route now carries a
small page naming the one link that can change the outcome, the edge's own
sign-out. It grants nothing: signing out is available to anyone holding a
session, refused or not. A `bearer` route still gets the bare status, because
a program is reading it.

### S7A.10 ◐ The installer does what it claims

Two cold-start races are fixed, and:

1. ✅ The OpenBao **`oidc` auth mount** — `B-09-vault-oidc-mount` enables it
   between the seeded secrets and the Cluster claim, so the roles the
   composition composes have a mount to attach to. Its *configuration* needs
   the realm's client secret and Keycloak serving discovery, and belongs after
   `D-02`.
2. ☐ The four **`Repository` claims**. Without them nothing composes Argo CD's
   repository Secret, the operator's push credential or the catalogue-sync
   ApplicationSet, and a private deployments repository has no credential path.
3. ☐ **Tenant teardown**, without which a purge cannot complete — S8 needs it.
4. ☐ The **credential catalogue** that `make check-credentials` reads.
5. ☐ The **recovery kit** and **bootstrap token revocation**, so an install
   does not end with the installer's root token still valid.
6. ☐ **Mail** and **LLM serving** have no v5 step and no ApplicationSet, while
   the operator still writes Postfix entries and still routes `llm.<kernel>`
   to a service nothing deploys. Decide whether these are deliberate drops.
7. ☐ `B-08-seed-secrets` declares a dependency on a step that runs after it.

### S7A.11 ◐ Signing out does not ask a second time

Pressing sign out lands on a Keycloak page asking whether you meant it.
Nothing is broken: since Keycloak 18, a logout that cannot prove which session
it means has to be confirmed, so a link on someone else's page cannot sign
people out. Envoy Gateway's `logoutPath` clears the zone cookies and hands the
browser to `end_session_endpoint` with no `id_token_hint`.

The hint is already in the browser — the zone keeps the ID token in
`gentian-<zone>-id` — so the fix is to send it, and the order matters because
`/oauth2/logout` clears that cookie: sign-out points at the edge authorization
service, which reads the ID cookie and answers 302 to the realm's logout with
`id_token_hint` set and `post_logout_redirect_uri` back at the zone's own
`/oauth2/logout`; Keycloak ends the session without asking; Envoy clears the
cookies. The composition writes the `post.logout.redirect.uris` this needs.

**Built, not verified.** The edge serves it and the desktop's sign-out was
changed to use it. Verify on the cluster.

Two alternatives, recorded so they are not rediscovered. **Admin REST**
(`POST .../users/{id}/logout`) ends every session server-side with no
redirect — rejected, because it needs `manage-users` in the director, which
S7A.2 just took away. **Account REST** (`DELETE .../account/sessions`) is the
right shape but needs `account` in the edge token's audience, which means an
audience mapper on every zone client, and it gives no redirect. Keep it as the
fallback.

### S7A.12 ✅ The tile catalogue leaves the director

The filtering belongs in the director; the list did not. Which components
exist, where each is served and what relation opens it is cluster state, and
the operator holds all of it.

**Done.** The operator projects a `gentian-tiles` ConfigMap in the control
namespace from the routes it composes plus every `ComponentProfile` exposure
that declares a tile; `internal/director/tiles` and its compiled YAML are
gone. Each kernel tile's hostname comes from its route rather than being
written twice, so a console with no route is absent instead of a link to
nothing. The director reads that ConfigMap as a **mounted file** rather than
through the Kubernetes API — it holds the git credential and no cluster
credential, and its ServiceAccount does not even mount a token.

Verified end to end on 2026-09-24: the administration console declared a tile
in its profile and the tile appeared, which is the first time a component
other than a kernel console has put itself on the desktop.

`ComponentProfile` gained the tile fields and its doc comment was rewritten
rather than left saying something untrue: the App Store still owns the
catalogue listing, and the cluster owns the tile, because the portal has to
show what the cluster routes without asking a service outside the cluster.
`AppProfile`'s existing tile fields were not reused — they are the v4 portal's
icon plumbing and carry no relation, which is the one field the per-caller
question needs.

One thing still to fix before an ordinary app can have a tile: the relation an
app's profile declares has to exist on `type app` in
`authz/model/v1/model.fga`, and that type has only `tenant` and `admin`.
`can_launch` is named in the design documents and is not in model v1. The
console sidesteps it by asking on the tenant (`object: tenant`), which is
right for a console that administers the tenant it runs in, and wrong as a
general answer.

### S7A.13 ☐ `denyPaths` promises a control it does not apply

`ComponentProfile.spec.expose[].denyPaths` is declared, documented as "refused
even where Paths admits them. Deny wins regardless", and read by no code
outside tests. `buildExposureRoute` uses `paths`, `authMode`, `backend`,
`forwardToken` and `subDomain`, and nothing else.

That is worse than a missing feature: a component author reading the CRD has
every reason to believe that listing an administrative path there keeps it off
the edge, and it does not — the path is served. The same is true of
`stripPrefix` and `source`, though neither reads as a security control.

Either build it or take it out, and prefer building it: deny rules are what a
component needs to expose a UI without exposing its own admin endpoints, and
the alternative is every app carrying that logic itself. Until one or the
other lands, the field is a false statement in a published API — the same
pattern the September threat-model exercise turned up, and the second time the
CRD has described a control we do not have.

### S7A.14 ✅ A release reaches a cluster by an immutable name

Found the hard way on 2026-09-24, twice in one afternoon, and it cost more
time than anything else in this plan.

- The operator and the director follow the **mutable** `:test-cb` tag.
  Nothing rolls them when CI overwrites it, and a pod that restarts for an
  unrelated reason silently picks up whatever the tag meant at that second.
  The director spent an afternoon serving a binary from before the branch
  because it restarted while CI was still building. The bootstrap chart is
  written to add Argo CD Image Updater annotations that pin `test-cb-<sha>`;
  on this cluster the Application carries none.
- The administration console's chart pinned the **mutable** image tag `0.1.0`.
  With `imagePullPolicy: IfNotPresent` the node kept its cached copy and went
  on serving an old build after a new one was published.
- A Helm release whose first install times out is left `pending-install`, and
  provider-helm retries an install that can never succeed. Clearing it needed
  a manual `helm uninstall`. Worth deciding whether the component reconciler
  should recognise that state.

The rule, now applied everywhere: **a cluster follows an immutable name.**
The installer resolves `<branch>-<sha7>` from the checkout's own commit rather
than taking the moving tag; every component chart is published twice, moving
as `<version>-<branch>` for humans and immutable as `<version>-<branch>.<sha>`
for clusters, with `appVersion` naming the immutable image; provider-helm is
only ever handed the immutable one, because it does not upgrade a release
whose version string has not changed.

The cost of this is that a fresh commit fails preflight until CI has published
its image. That refusal is correct and is now worded as "that commit has not
been published yet" rather than "no such tag in the registry", which used to
send the reader off to edit a values file.

### S7A.16 ☐ The app-lifecycle API authenticates nobody

The operator's HTTP API is reachable by anything that can reach the Service,
and it has writes: installing and uninstalling an app, setting addons, and now
taking and deleting a backup. Each takes the person's name from an
`X-Gentian-Actor` header, which is a claim rather than a proof — so a pod in
any namespace could take a backup and have somebody else's name recorded
against it.

This is not new and the backup actions did not create it; naming it here is
what stops it being rediscovered. Three ways out, in increasing order of what
they cost:

1. **A NetworkPolicy** admitting only the director's pod. Cheap, and it is
   topology rather than identity: it says who may connect, not who is asking.
   It also has to account for `kubectl port-forward`, which the plugin's reads
   use and which does not arrive from where a pod would.
2. **A token both sides hold**, issued by the chart, mounted by the director
   and required on every action. Real authentication, no CNI semantics to
   reason about, one more secret to rotate.
3. **The caller's own token**, verified here against the realm. The strongest,
   and the most work: the operator would need the issuer's keys and the graph,
   which is the director's job — so this is really "there should be no HTTP
   write here at all, only the director's".

Worth doing before a tenant application shares a cluster with this.

### S7A.15 ☐ A zone's hosts follow the components, not a list

The zone client's redirect URIs are enumerated in the tenant composition —
`console`, `admin`, and for the kernel zone `argocd`, `headlamp`, `id`. A
component whose profile declares any other `subDomain` gets a Keycloak refusal
("Invalid parameter: redirect_uri") on an error page that says nothing about a
redirect URI list. The administration console hit exactly this the first time
its tile was opened.

The operator is what knows every host a zone serves, because it composes the
routes. Projecting that per tenant — the way it already projects the tile
catalogue — would make the list follow the components instead of being
maintained beside them. Until then, a component with a host of its own has to
be added to the composition by hand.

---

## 4. S8 ☐ Purge and reinstall

`install.sh --layout v5` from nothing, every `check()` honest, `--status` all
true, and the sign-in confirmed in a browser rather than by a script that
cannot execute the page. Blocked on S7A.10's tenant teardown.

---

## 5. After M1

The work packages in order. Each is specified in `work-packages.md`.

| Order | Package | Why here |
|---|---|---|
| 1 | WP-1 Director | S7A.2 is its first item; the rest of the API follows |
| 2 | WP-3 Authorization | who projects into OpenFGA, and the naming rule |
| 3 | WP-7 UI | the admin console and the desktop, on the director's API |
| 4 | WP-5 Catalogue | `ComponentProfile` for everything, not only the desktop |
| 5 | WP-2 Operator | what the operator gives up and what it takes on |
| 6 | WP-4 Networking | tenant zones, the DMZ and exposure |
| 7 | WP-8 Namespaces | the fresh-install layout |
| 8 | WP-6, WP-14 Store | the external service and its reference implementation |
| 9 | WP-9 Security gaps | audit logging, break-glass |
| 10 | WP-11, WP-12, WP-13 | repository layout, documentation, certification |

**WP-5 has a concrete first step.** `ComponentProfile` now carries everything
an app needs — tenancy, trust tier, exposures, tiles, a platform value
mapping, `defaultForTenants` — and the administration console proves a
component can be built, published and installed through it end to end. But
`Tenant.spec.apps` still resolves `AppProfile` only, at every site that reads
it, so a profile of type app cannot yet be installed into a tenant by naming
it there. That is what "ComponentProfile for everything" means in practice,
and it is what lets an app be standalone rather than reached only through the
App Store.

---

## 6. Open decisions

**How much of the graph has to be written at all.** Raised while planning
S7A.2 and still unanswered. Most of what is written into OpenFGA is not
per-cluster: the authorization **model** is a file in the repository and
changes only when the model version does, and the **relation structure** —
that a cluster has tenants, that a tenant has admins, members and a perimeter
group, which relation each role implies — is the model, not data. What is
genuinely per-cluster is small: which groups exist, who is in them, which
tenants this cluster has, and which entitlements are current. So the graph
should arrive mostly built, with only names, memberships and facts written at
runtime. That is less code and a smaller blast radius: a bug in a projector
can then add or remove a membership, but it cannot invent a relation that was
never in the model.

**The director's OpenFGA credential** cannot be made read-only (S7A.2). Accept
the weaker guarantee, or pay for a proxy or OIDC auth mode.

**Headlamp's second sign-in** (S7A.9): start the flow from the tile, or give
it an authenticating sidecar.

A note on naming: OpenFGA, or the ReBAC graph, or the authorization store.
Never "the store" on its own — that is the App Store, which is a different
thing entirely.
