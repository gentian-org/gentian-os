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

### The milestones

Seven of them, in order. Each is a thing a person can do, not a set of parts
that exist. Nothing here is called reached until somebody has done it on a
cluster.

| | Milestone | |
|---|---|---|
| **M1** | The platform administrator signs in and sees the cluster, and the installer leaves that cluster ready to be given a tenant | ◐ |
| **M2** | The first functional tenant | ☐ |
| **M3** | The first user invited by a tenant administrator | ☐ |
| **M4** | The first app a user can actually work in | ☐ |
| **M5** | A v0.4 backup imported into a v0.5 install | ☐ |
| **M6** | An update procedure from v0.4 to v0.5 | ☐ |
| **M7** | Complete cutover | ☐ |

**M1 is not the sign-in alone.** A cluster where the administrator can sign in
but half the kernel is not running is not a cluster anybody can provision a
tenant on. M1 is reached when `install.sh --layout v5` brings up every service
the platform needs and leaves the cluster in a state where M2 can begin. S8 is
what proves that, and S7A is what has to be true before S8 is worth running.

The steps below (`S1`…`S8`, `S7A.*`) are all M1. M2 onward are not broken into
steps yet; `work-packages.md` is where their content lives until they are.

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
| S7A.2 | The director writes authorization state the way it writes git | ✅ read-only token still impossible |
| S7A.3 | The platform administrator is an address | ✅ |
| S7A.4 | The admin console is an app, and it talks to the director | ◐ every screen wired; untested against a cluster that can push |
| S7A.5 | Keycloak looks like the rest of the product | ✅ |
| S7A.6 | The console and the desktop hold nothing | ◐ console yes, desktop still holds a Keycloak credential |
| S7A.7 | The zone cookie does not reach the applications | ◐ built, needs a browser |
| S7A.8 | A read-only view of the authorization state | ☐ |
| S7A.9 | The kernel UIs are actually usable | ✅ |
| S7A.9b | A refusal a person can act on | ✅ |
| S7A.10 | The installer does what it claims | ◐ 4 of 7; one decided, two open |
| S7A.11 | Signing out does not ask a second time | ◐ built, not verified |
| S7A.12 | The tile catalogue leaves the director | ✅ |
| S7A.13 | `denyPaths` promises a control it does not apply | ✅ built at L2 |
| S7A.14 | A release reaches a cluster by an immutable name | ✅ |
| S7A.15 | A zone's hosts follow the components, not a list | ◐ two of five; three are kernel tier |
| S7A.16 | The app-lifecycle API authenticates nobody | ✅ a shared token |
| S7A.17 | The director speaks for Keycloak | ☐ |

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
10. **S7A.17 — the director speaks for Keycloak.** In M1, not after it: the
    screens it brings back are the console's, and putting them in later means
    shipping the console twice.
11. **S8 — purge and reinstall**, which is what makes M1 reached rather
    than demonstrated.
12. **After M1**, the work packages in the order in §6.

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

**And the installer finishes the job.** The sign-in is the visible half; the
other half is that the same run brings up every kernel service, so the cluster
it leaves behind is one a tenant can be provisioned on. A cluster where the
console loads and the provisioning chain is half-applied has not reached M1,
it has reached a demo. Concretely: every step's `check()` honest, `--status`
all true, and the services the tenant Composition depends on — the edge, the
identity provider, the secret store, the GitOps chain, the operator and the
director — running and reconciling. That is the handover to M2, and it is why
S8 is the last step of M1 rather than a tidy-up after it.

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

### S7A.2 ✅ The director writes authorization state the same way it writes git

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

**Decided, 2026-09-25: the entitlement tuples stay the director's.** The step
was named "the director stops writing authorization state", and the premise
under it was wrong. What matters is not *which* source of truth the director
writes, it is *how*.

There are three sources of truth and the rule is the same for each:

| | what it holds | who writes it |
|---|---|---|
| Keycloak | **who** | the director, on the caller's behalf (S7A.17) |
| OpenFGA | **what** they may do | the director, for what cannot be derived |
| git | **how** the cluster is configured | the director, as commits |

A director write into any of the three is admissible when it is
**authenticated, evaluated and recorded**. The operator's job is the other
half: turning declared state into what runs. So anything that *can* be derived
from the cluster, the tenant, its users or its apps belongs in git and is the
operator's to satisfy; anything that cannot is the director's to do as an
**action**, and an action is legitimate because of those three properties
rather than because it went through a file.

The entitlement path already meets all three, which is why this closes rather
than needing work:

- **Authenticated** twice over. The statement is a compact JWS the director
  verifies against keys pinned in the cluster's own configuration, never
  against a key it could fetch; and the caller presents their own token.
- **Evaluated.** A grant checks `can_install_app` on the tenant before it is
  applied. A revocation deliberately does not: it is the App Store's decision
  and its signature is the authority, and asking a tenant administrator to
  authorise their own revocation would be the wrong question.
- **Recorded.** The fact is committed to git before the tuple is written, with
  the person, the signing key that decided it and the request id that joins
  them. The tuple mirrors a commit; it is not the only trace.

An entitlement is exactly the case the rule is for: what a tenant is entitled
to install is a fact from outside the cluster and cannot be derived from
anything inside it.

**What remains open is not this.** The director's OpenFGA credential still
cannot be made read-only, because OpenFGA authenticates with a preshared key
and a key carries no scope. Under the rule above that is a smaller problem than
it looked — the director is *meant* to write here — but it still means nothing
stops a bug writing a tuple no action asked for. An authorizing proxy or
OpenFGA's OIDC auth mode would; neither is small, and neither is in M1.

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

**People are not in this console — reversed, see S7A.17.** The decision here
was that people belong in Keycloak's own console, because declaring them in
git collides with erasure: git is append-only, so a name and an address
committed there outlive the account. That half still holds and is not being
undone. What did not hold is the conclusion drawn from it — that because
people must not be in git, the screen must be Keycloak's. Embedding Keycloak's
console makes its information architecture the product's, and for the
administrators who will live in this product every day that is a worse
interface than the one they had. S7A.17 is the way back: the director speaks
to Keycloak on the caller's behalf and records what it did, so the screen is
ours and the state is still Keycloak's. Keycloak's console stays reachable for
anybody who wants the whole of it.

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

### S7A.7 ◐ The zone cookie does not reach the applications

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

**(1) is built**: the zone's `SecurityPolicy` no longer sets `cookieDomain`,
so each host's session cookie is that host's. What the browser hands an
application is now good only for that application.

**It needs a browser before it is called done**, and it is the one change in
this branch that alters how signing in behaves. Two things to watch:

- **The first request to each host does one extra redirect.** The Keycloak
  session already exists, so it should be invisible. If it is not, it is
  measurable here rather than a mystery later.
- **Components the desktop opens in a frame.** That first redirect now
  happens inside the frame. The realm sends no `X-Frame-Options` and the edge
  injects a permissive `frame-ancestors`, so it should complete; if a framed
  component comes up blank on first open, this is why.

Signing out everywhere at once is unaffected: the realm session ends and the
back-channel logout marks it revoked, which every host's shim honours whatever
cookie it read.

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

Two cold-start races are fixed, four of the seven below are done and one is
decided rather than built. The two still open are 4 and 5.

1. ✅ The OpenBao **`oidc` auth mount** — `B-09-vault-oidc-mount` enables it
   between the seeded secrets and the Cluster claim, so the roles the
   composition composes have a mount to attach to. Its *configuration* needs
   the realm's client secret and Keycloak serving discovery, and belongs after
   `D-02`.
2. ✅ The four **`Repository` claims** are scaffolded. `deployments` is
   writable and names its vault path, because it is the one repository the
   platform commits to. The other three — `gentian-os`, `gentian-apps` as role
   `apps`, and `gentian-ui` — are public by default and carry **no**
   `credential` block: naming a vault path for a secret that does not exist
   would put an unsatisfiable requirement in front of anybody running
   `make check-credentials`. A mirror sets the matching `_AUTH` and gets one.
   All four validate against the XRD.
3. ✅ **Tenant teardown.** Every namespace the teardown removed was one a
   step created: A-01 makes the kernel set and its `destroy()` removes the
   same list. A tenant's namespaces are on no list, because the Composition
   made them — so a purge stripped and deleted the `Tenant` object and left
   `tenant-<name>`, its `-dmz` and any `shared-*` or `system-*` standing, with
   no owner left to finalize them and nothing that names them. A reinstall
   onto that does not fail cleanly: the Composition adopts what is there,
   PVCs bound to the previous install's data included.
   `purge_tenant_namespaces` runs after the `gentianos.io` sweep, where the
   operator and Crossplane are already gone, and before the kernel namespaces.
   It finds them by tier label and by the prefixes the layout reserves, drains
   their PVCs first so the volumes are reclaimed, and leaves everything else
   alone.
4. ☐ The **credential catalogue** that `make check-credentials` reads.
5. ☐ The **recovery kit** and **bootstrap token revocation**, so an install
   does not end with the installer's root token still valid.
6. ◐ **Mail** and **LLM serving** — decided, not yet enforced. Both are
   deliberate drops for M1, and neither is a live defect: this cluster runs
   `mail.serviceMode: external` and `llm.enabled: false`, and both are gated
   on exactly that.

   What is wrong is narrower than "the operator still writes Postfix entries",
   and it is the same shape in both: **an opt-in v5 cannot honour, which fails
   silently.** A tenant that asks for `mail.mode: selfhosted` is registered
   into a Postfix and Dovecot that no v5 step deploys — the reconciler is
   explicit that empty means external and that a tenant asking for selfhosted
   still gets it. A cluster that sets `llm.enabled: true` gets a route for
   `llm.<kernel>` pointing at `litellm-proxy`, which v5 has no step to deploy
   and no ApplicationSet to sync; v4 had `D-05-llm-serving` and `E-02`.

   So the answer is not to port the steps for M1. It is to **refuse the
   opt-in** while the layout cannot honour it, so the failure is a refusal
   somebody can act on rather than a route to nothing and a tenant whose mail
   is silently undeliverable. That is a schema statement on the Cluster claim
   and on the Tenant, and it is not built here.
7. ✅ `B-08-seed-secrets` declared a dependency on a step that runs after it.
   It required `C-01-cluster-claim`, nine steps later. The install was never
   wrong, because the driver reads the line as documentation — but the
   documentation was, and a reorder would have trusted it. It requires
   `B-07-crossplane-secrets`, which is what actually has to be true: a vault
   up, unsealed and holding the derived credentials. Writing a KV path uses
   the root token and needs no policy; what needs C-01 is reading, and these
   paths must exist *before* the claims that consume them.
   `make lint-step-order` now refuses a forward dependency, and found this one
   as its first act. 65 steps across both sets check out.

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

### S7A.13 ✅ `denyPaths` promises a control it does not apply

`ComponentProfile.spec.expose[].denyPaths` is declared, documented as "refused
even where Paths admits them. Deny wins regardless", and read by no code
outside tests. `buildExposureRoute` uses `paths`, `authMode`, `backend`,
`forwardToken` and `subDomain`, and nothing else.

That is worse than a missing feature: a component author reading the CRD has
every reason to believe that listing an administrative path there keeps it off
the edge, and it does not — the path is served. The same is true of
`stripPrefix` and `source`, though neither reads as a security control.

**Built, at L2.** Not as a route rule: a gateway route matches by prefix, so
the denied path is already inside the rule that serves the host, and the more
specific rule that would shadow it still needs a backend to send the request
to. Gateway API has no direct-response filter here. The edge authorization
service is the one place that already sees every request to a host, so the
exposure's list travels there on the route as an annotation, the operator
unions it per host — two exposures share a host and deny wins — and the shim
refuses a match with 403 before it looks at identity. The profile said the
path is not published, so who is asking does not enter into it.

Below the edge's own endpoints, because denying `/oauth2/` would refuse the
sign-in the deny rule exists to sit behind. Prefixes stop at segment
boundaries, so a denied `/admin` does not take `/administrators` with it, and
a query string cannot defeat the rule.

**`source` is the same promise and is still unkept.** `expose[].source` pins
the caller, and the CRD says Collabora's WOPI callbacks are `authMode: none`
and "safe only because the caller is pinned". No code reads it either. That
reads as a security control whatever the earlier note here said, and it is the
next one to close. `stripPrefix` is also unread, and that one is a routing
convenience rather than a control.

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

### S7A.16 ✅ The app-lifecycle API authenticates nobody

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

**Option 2, built.** The chart mints one Secret, keeps it across upgrades by
reading back what is already in the cluster, and mounts it into both
deployments. The operator requires it as a bearer on everything under `/v1`,
reads included — a tenant's installed apps and its usage are its own business
— and the director presents it on every request. `/healthz` stays open.

Two decisions inside it. **No token refuses everything**, rather than
admitting everything: an operator whose Secret failed to mount must not
quietly become the open API this replaces. And the routes are registered
through a small wrapper rather than against the bare mux, so a future route
cannot be added unguarded by forgetting.

Option 3 remains the honest end state: there should be no HTTP write here at
all, only the director's. This is what makes the `X-Gentian-Actor` header
worth the paper it is written on until then, because only the director can
set it. Option 1's NetworkPolicy is still worth adding as depth, and is not
here.

### S7A.15 ◐ A zone's hosts follow the components, not a list

The zone client's redirect URIs are enumerated in the tenant composition —
`console`, `admin`, and for the kernel zone `argocd`, `headlamp`, `id`. A
component whose profile declares any other `subDomain` gets a Keycloak refusal
("Invalid parameter: redirect_uri") on an error page that says nothing about a
redirect URI list. The administration console hit exactly this the first time
its tile was opened.

**Built, for the components.** The operator projects, per tenant, the host
labels its components' gateway exposures serve, into a ConfigMap the tenant
Composition reads and unions with its base list. It rides on the same walk as
the tile catalogue, because it reads exactly the same objects and a second
watch over them would be a second thing to keep in step. Labels and not
hostnames: the Composition knows the tenant's effective domain and the
operator does not, so a tenant on a custom domain still gets the right URI.

Gateway entries only. A perimeter surface is published through its own proxy
with its own credential and never reaches the zone's client, so listing it
would widen that client for a host the zone does not serve.

**Three of the five hosts still cannot follow anything**, and this is why it
is ◐ rather than ✅. `argocd`, `headlamp` and `id` are kernel tier: installed
by `install.sh`, not components, so nothing projects them. They stay named in
the Composition. `console` and `admin` stay in the base list too, so a tenant
reconciling before the operator has projected still has a desktop to sign in
to.

Closing the remaining three means either giving kernel services profiles —
which the bootstrap order refuses, since the operator depends on them existing
— or a second declared list for the kernel's own hosts, which is what the
Composition already is.

**Three of those five hosts cannot be components at all today**, which is the
part of this that is not just plumbing. `argocd`, `headlamp` and `id` are
services with an operator console, and CEL forbids a service from having any
exposure. So they could not follow the components even if the projection
existed. Relaxing that rule to "a service's exposures are gateway-only" is a
prerequisite here, not a separate piece of work; it is in
[target-component-structure.md](target-component-structure.md) §5.

### S7A.17 ☐ The director speaks for Keycloak

**The same rule as S7A.2, one source of truth along.** Keycloak holds *who*,
OpenFGA holds *what they may do*, git holds *how the cluster is configured*,
and a director write into any of the three is admissible when it is
authenticated, evaluated and recorded. People cannot be derived from anything
and cannot go in git, so managing them is the director's to do as an action.
That is the whole justification for what follows.

**A reversal, stated as one.** S7A.4 decided that people and the realm's own
settings belong in Keycloak's console, embedded as the Identity tile. The
reasoning for keeping people out of git stands. The conclusion drawn from it
does not: embedding Keycloak's console makes Keycloak's information
architecture the product's, and it is a worse interface than the one it
replaced. Managed service providers and in-house tenant administrators are the
people who will spend the most hours in this product. Handing them a console
built for realm engineers, as their primary surface, is the wrong trade.

**What it should be instead.** The screens come back into the administration
console, against the director. Keycloak's own console stays reachable, as the
place to go when somebody wants the whole of it — the detail view behind the
product view, not the way in.

**The director gets a Keycloak administrative credential.** This is the part
worth being explicit about, because it changes an invariant the plans have
stated more than once: the director held a git credential and an OpenFGA
token and no Kubernetes credential, and that was the whole of what it could
do. Now it also holds a credential that can write a realm.

The trade is still the right way round, and for two reasons rather than one:

1. **One holder instead of many.** The credential exists today — the desktop
   image carries it (S7A.6). Moving it to the director takes it out of an
   image every tenant runs and puts it in the one component that is already
   the platform's single writer.
2. **The one component that already asks who is calling.** Every director
   route is authorised against OpenFGA with the caller's own token before it
   does anything. A Keycloak write behind that check is a write somebody was
   entitled to make; a Keycloak write from a UI that holds the credential is
   a write nobody checked.

**Indirect is a rule, not a description.** The director must never make a
Keycloak call it cannot name a caller, a relation and an object for. The
credential is the director's; the authority is always the caller's. Two
things follow that are easy to get wrong:

- **Scope the credential per realm.** A tenant administrator's request must
  not be able to reach another tenant's realm even through a bug. Keycloak
  26.2's fine-grained admin permissions are what make that expressible — the
  same mechanism S7A.4 was going to use to scope a human administrator, used
  to scope the director instead. One `realm-admin` for every realm would make
  a single missing OpenFGA check a cross-tenant breach.
- **Refuse rather than fall back.** If the check cannot be made — OpenFGA
  unreachable, no relation for this object — the answer is a refusal, not the
  call.

**Recording, and why it cannot be a commit.** Git gives every change an
author, a time and a diff, and that is the standard the rest of the console is
held to. Keycloak writes cannot meet it the same way, because the thing that
makes git good here — append-only history — is exactly what makes it wrong for
personal data. The record has to live somewhere with a retention policy.

**Two halves, joined by a request id, and both already have a home.**

- **What changed** is a Keycloak admin event, and the event listener in
  `kernel/extensions/keycloak-event-listener/` already receives realm events
  and posts signed statements to the operator. It projects group membership
  and drops the rest. Recording admin events is roadmap §1.12's "extend the
  event listener" item, and it needs no credential anywhere.
- **Who was allowed to ask for it** is the director's to record: the caller,
  the relation and the object that permitted the call, and a request id. This
  is the same trailer the director already writes on every commit.

Keycloak's own event is the better record of the change, because it is written
whether the change came through the director or through Keycloak's console.
The director's record is the better record of the authority, because Keycloak
sees only the director's service account. Neither is sufficient alone, which
is why the request id matters.

**The fields, not the values.** "Alice's address was changed by Bob at 14:02,
under `can_manage_people`" is the record. The old and new addresses are not,
or the log becomes the problem the git decision avoided. Keycloak admin events
carry a representation of the changed object by default, so this is a
configuration decision and not a thing that happens by itself.

**A change made in Keycloak's console has one half and not the other**, which
is the same case as a commit pushed to `gentian-deployments` by hand: it
happened, it is recorded, and nothing authorised it through the platform. The
Changes pane already reports that for git and should report it the same way
here, rather than hiding it.

**Build one store, not two.** This is the same store roadmap §1.12 needs for
sign-ins, refused requests and actions. The Changes pane already reads git
history; this is its second source, and one timeline is the point.

**Done when** a tenant administrator can invite a person, change a group
membership and adjust the realm's password policy from the administration
console; each one appears in the change log with the caller and the relation;
the same administrator attempting it against another tenant's realm is
refused; and no image other than the director's holds a Keycloak credential.

**In M1, and it gates M3.** Inviting a user is a Keycloak write, and today
nothing a tenant administrator can reach is allowed to make one, so M3 cannot
start without this. It is inside M1 rather than after it because what it
brings back are the administration console's own screens: shipping the console
without them and adding them later is shipping the console twice.

**S7A.6 is unchanged by this.** The desktop still holds nothing. The bundled
console still goes; its screens come back in the administration console
against the director, which is a different thing from leaving them where they
are.

---

## 4. S8 ☐ Purge and reinstall

`install.sh --layout v5` from nothing, every `check()` honest, `--status` all
true, and the sign-in confirmed in a browser rather than by a script that
cannot execute the page. Blocked on S7A.10's tenant teardown.

---

## 5. M2–M7 — what each one means

Stated now so the work in §6 can be pointed at one of them. None is broken
into steps yet.

**M2 — the first functional tenant.** A tenant claim in
`gentian-deployments` becomes a running tenant: its namespace, its realm, its
zone with its own client and session, its desktop, its quota and its backup
policy. Functional means a tenant administrator can sign in to it and see it,
not that the objects exist. This is the first time a second zone is stood up,
which is the open half of S3 and the "done when" of S7A.1.

**M3 — the first user invited by a tenant administrator.** The tenant
administrator invites somebody by address, that person sets a password and
signs in, and lands in that tenant and no other. Inviting is a write to
Keycloak, so M3 is gated on S7A.17: today no component that a tenant
administrator can reach is allowed to make it.

**M4 — the first app a user can actually work in.** Not "the Helm release is
Ready": a member of the tenant opens a tile, is already signed in, and does
the thing the app is for — writes a document, sends a mail — with their own
identity and the tenant's data. This is what proves the catalogue, the zone
session, the database and storage fulfilment, and the grant model together.

**M5 — a v0.4 backup imported into a v0.5 install.** Data written under the
old architecture is readable under the new one. This is the first milestone
whose failure is not recoverable by reinstalling, so it is also where the
export format stops being an implementation detail.

**M6 — an update procedure from v0.4 to v0.5.** M5 proves the data can move;
M6 is the procedure that moves a running cluster, with the order of steps, what
is reversible at each one, and what the downtime is.

**M7 — complete cutover.** No v0.4 cluster left, and the v4 layout, its
namespaces and the code paths that carry it are removed rather than kept
working. Until M7 every `ternary "kernel-x" "old-x" $v5` in the Compositions
is a branch that has to stay correct.

---

## 6. After M1

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

## 7. Open decisions

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

**The catalogue's vocabulary**, raised 2026-09-25 and not settled. Three
questions, one answer each, none of them started. The shape they resolve to is
written out in [target-component-structure.md](target-component-structure.md).

- **`ComponentProfile` or `AppProfile`.** AD-4 already says Component, and
  36 of the 38 catalogue entries are still `AppProfile`. What is unsettled is
  not the name but whether the rename gets finished, and the load-bearing
  blocker is that `Tenant.spec.apps` resolves `AppProfile` only.
- **`tenancy: system | shared | tenant` becomes `class: service | app |
  shared-app`.** The current values mix a placement word, an adjective and a
  scope word for what is one question: who this component serves. The code's
  own comment gives the game away — `system` is documented as "serves
  contracts to other components", which is a service.
- **`kernelRequirements` names a fulfiller that is not the fulfiller.** A
  database comes from a component of class `service`, mail may come from a
  relay outside the cluster. `requires.contracts` is where it landed on
  `ComponentProfile`, which collides with the open named set already called
  contracts (`provides: wiki, project-management`). `requires.services` is the
  proposal.

Two defects found while looking, both admissible on `ComponentProfile` today
and neither on `AppProfile`: a package may declare `deploymentMethod: api`
and carry a chart, and a package may carry a chart and an API integration at
once. The one-of rule is an OR where it should be an exactly-one, which the
same file already writes correctly for egress.

**The director's OpenFGA credential** cannot be made read-only (S7A.2). Accept
the weaker guarantee, or pay for a proxy or OIDC auth mode.

**Headlamp's second sign-in** (S7A.9): start the flow from the tile, or give
it an authenticating sidecar.

A note on naming: OpenFGA, or the ReBAC graph, or the authorization store.
Never "the store" on its own — that is the App Store, which is a different
thing entirely.
