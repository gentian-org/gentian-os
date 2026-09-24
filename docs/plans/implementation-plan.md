# Implementation plan

The order things get built in. Milestone M1 is defined below, moved here from
WP-10. [work-packages.md](work-packages.md) says *what* each package is, this
says *when*, and what blocks what.

Step labels `S1`…`S8` are the eight numbered items inside M1. They exist so a
conversation can point at one; they are not a second plan.

## Milestone M1

Moved here from WP-10, unchanged, because this is where the order lives.

**M1 — the platform administrator signs in and sees the
cluster, in the plans' shape.** `install.sh --layout v5` runs end to end
on the purged cluster; `admin@<kernel>` signs in once at
`console.<kernel>` and sees the platform tenant's desktop; the kernel
consoles are tiles the director answered from that account's relations;
each opens in a window on the desktop, signed in, with no second login
and no token to paste. Nothing about it is a stand-in for the plan: the
desktop is `Tenant/platform`'s, the edge holds the session, the desktop
holds no authority. In order, each landing with its installer step and
verified on the cluster before the next:
1. `[x]` **Vocabulary** (WP-3): the Keycloak groups exactly as model v1
   names them — `gentian:platform:admin` replaces the bootstrap's
   `superadmin` everywhere it is written or read: the identity
   bootstrap, the claim's `platformRoles` default, Argo CD's policy,
   the proxy's binding, the OpenBao OIDC roles, the desktop's constant.
2. `[x]` **Platform tenant** (WP-2, WP-8): `Tenant/platform` with
   `isolation.keycloakRealm: kernel`, realm adopted and never created,
   disabled or deleted — the tenant composition honours the realm
   override and emits no Realm and no kernel broker for a tenant that
   adopts the kernel realm; tenant namespaces carry
   `gentianos.io/tier: tenant` beside `gentianos.io/tenant`; the v5
   ApplicationSets sync `clusters/<c>/tenants/*`; the installer
   scaffolds `tenants/platform/`; the director writes
   `tenant:platform#cluster` and `#operated_by` at start, as it writes
   the cluster roles.
3. `[ ]` **The edge** (WP-4): `authenticated` and `perimeter` Gateways
   in `kernel-edge` under `mergeGateways`, reconciled by the operator
   from the Cluster claim; the kernel zone's one confidential client
   (`gentian-edge-kernel`: no groups scope, secret in `kernel-edge`,
   back-channel logout at the director); `SecurityPolicy` OIDC with the
   zone's cookie on `.<kernel>` and ext-auth on every kernel-zone route;
   the ext-auth shim as a new binary in `kernel-edge` — gRPC, verifies
   the token, asks the route's relation, caches per `(sub, sid, route)`,
   evicts on `ReadChanges`, denies `session#revoked`, fails closed with
   cached allows carrying — with its route table written by the
   operator beside the routes; `id.<kernel>` serves `/realms/*` only on
   the perimeter Gateway, and `/auth/admin/*` behind the kernel session
   on that same hostname — `id-admin.<kernel>` was retired from
   networking.md §3 once Keycloak turned out not to work across two
   hostnames, and this is the only line of M1 that moved with it.
4. `[x]` **The desktop as a component** (WP-5, WP-2): a
   `ComponentProfile` `desktop` (`tenancy: [tenant]`, `trustTier:
   platform`, a database requirement, one gateway exposure with
   `authMode: oidc` and `forwardToken: true`); a Component reconciler
   that gives every tenant its desktop from that profile — a
   provider-helm Release in `tenant-<t>`, the database fulfilled in
   the tenant's namespace, the route and `SecurityPolicy` from
   `expose[]` — so `tenant-platform` serves `console.<kernel>`.
5. `[x]` **The desktop without authority** (WP-7, WP-1): the BFF
   consumes the token the edge forwards and runs no code flow; no
   client secret, no Keycloak admin credential, `rbac.yaml` empty; the
   database from the granted requirement; tiles from the director —
   the kernel consoles from `/v1/clusters/{c}/tiles`, the admin tile by
   `can_administer` from a `GET /v1/tenants/{t}/me` relations read.
6. `[x]` **Kernel UIs behind the kernel session** (WP-4): Argo CD,
   Headlamp and `id-admin` routes carry the zone's `SecurityPolicy` and
   the shim's `can_configure` / `can_audit`; each tool's own OIDC login
   is the silent second factor.
7. `[x]` **Retire** the portal in `kernel-edge`, `portal.<kernel>`, the
   portal secret and BFF client in the identity bootstrap, and
   `kernelPortalHost`; `www.<kernel>` is an alias of the console.
8. `[ ]` **Purge and reinstall** — the confirmation cycle; every
   `check()` honest; `--status` true.
Earlier partial results (kernel tiles served by the director, Headlamp
through the impersonating proxy, the event listener wired, the claim's
`platformRoles` projected) stand and are reused; they are not the
milestone.

## Where M1 stands

| Step | What it is | State |
| --- | --- | --- |
| S1 | Vocabulary: the groups model v1 names | done |
| S2 | `Tenant/platform` adopting the kernel realm | done |
| S3 | The edge: two Gateways, the zone's client, the session and the enforcement point | kernel zone done, tenant zones not |
| S4 | The desktop as a `Component` | done |
| S5 | The desktop without authority | done |
| S6 | Kernel UIs behind the kernel session | reachable, two of three not usable |
| S7 | Retire the portal in `kernel-edge` | done |
| **S7A** | **What is wrong that a reinstall would only reproduce** | **not started** |
| S8 | Purge and reinstall | not started |

Everything above was verified on beefy1 only, never on a purged cluster. S8 is
what turns that into an install.

## S7A — before the purge

A purge and reinstall proves the installer. It does not fix a design, and
reinstalling with these open would only reproduce them. In order.

✅ done and verified on the cluster · ◐ partly done · ☐ not started

### S7A.1 ◐ The operator produces a zone's Keycloak client and its secret

A **zone** is one sign-in domain: a hostname, the realm behind it, one
confidential Keycloak client for the edge to hold the session with, that
session's cookie names, and the Gateway listener that serves it.

Everything that *consumes* a zone is already general. The operator derives a
tenant's zone from the tenant, and the route table, the session policy, the
cookie names and the host all follow with no kernel special case. Nothing
*produces* one except for the kernel, and that is done by a shell Job in
`D-02` (`scripts/lib/portal-login-bootstrap.sh`), which creates
`gentian-edge-kernel` and writes `edge-kernel-oidc` into the edge namespace.

So: a tenant gets a client the same way it already gets its other Keycloak
objects, from the tenant composition
(`crossplane/compositions/tenant-default.yaml`), which already composes
clients and their secrets. Per tenant it emits `gentian-edge-<t>`,
confidential, code flow only, no groups scope, the director in its audience,
back-channel logout at the director, redirect URIs for that zone's hosts; and
a Secret `edge-<t>-oidc` in the edge namespace for the `SecurityPolicy` to
read. The kernel zone then stops being a special case: the platform tenant
composes its own like any other, and the bootstrap Job's client creation goes.

The pieces, since the shape is decided and only the writing is left:

1. **The secret exists first.** External Secrets is deployed and its
   `Password` generator is available on the cluster, so the composition emits
   an `ExternalSecret` in the edge namespace named `edge-<t>-oidc` whose value
   comes from a generator and whose target key is `client-secret`, which is
   the key Envoy Gateway's `SecurityPolicy` reads.
2. **The client is pushed that secret**, not given one. `clientSecretSecretRef`
   on the `Client` resource, exactly as the retired portal BFF client did it.
   A client with no secret reference has Keycloak mint one, and then the two
   sides disagree for ever.
3. **The client itself**: `gentian-edge-<t>`, confidential, standard flow only,
   `fullScopeAllowed: false` so no roles ride in the token, no groups scope,
   the director in its audience, back-channel logout at the director, and a
   redirect URI per host in that zone.
4. **The kernel stops being special.** Once a tenant composes its own, the
   platform tenant composes its own too and the client creation leaves
   `portal-login-bootstrap.sh`. Do this second, after a tenant zone is proven,
   because it is the sign-in everything else on the cluster depends on.

Watch for the provider quirk the composition already documents at length: this
provider cannot adopt an object it did not create, and `Observe`-only left the
retired portal client uncreated in tenant realms while every tenant hung in
Provisioning. Declare `Create` as well as `Observe` from the start.

**Done when** a second tenant signs in at `console.<t>.<kernel>` against its
own realm, with no installer step having run for it.

**Built, not yet verified on a cluster.** All four pieces are in
`tenant-default.yaml`: an External Secrets `Password` generator and an
`ExternalSecret` in the edge namespace, the `Client` pushed that secret through
`clientSecretSecretRef`, its `director-audience` mapper, and a
`ClientDefaultScopes` naming Keycloak's six own defaults so `groups` stays off.
The block renders for a tenant that adopts the kernel realm as well, so the
platform tenant composes `gentian-edge-kernel` and the client creation has left
`portal-login-bootstrap.sh`. One thing departs from piece 3 above: there is no
back-channel logout URL, because the Job had already dropped it once session
revocation was deleted in S7A.2, and re-adding it would restore a write to the
authorization store that has no reader. The post-logout redirect URIs S7A.11
needs are written.

**It is not on `test-cb`, on purpose.** Taking over on a cluster that already
has these objects costs a sign-in outage of a minute or two on the kernel
zone, so it waits for a moment when that is acceptable rather than arriving
inside somebody's test run. To promote it, move `test-cb` onto this branch and
then, in this order:

1. **Delete the existing Secret.** External Secrets refuses to adopt a Secret
   of that name it does not own, and the installer wrote this one with
   `kubectl`:

   ```
   kubectl delete secret edge-kernel-oidc -n kernel-edge
   ```

   Let Argo CD sync, then confirm the replacement is owned by an
   `ExternalSecret` rather than by nothing.

2. **Check whether the Keycloak client is adopted.** `docs/roadmap.md` §1.24
   says a `Client` whose external name is its clientId adopts an existing
   object, but the only live `Client` on this cluster carries a UUID, and
   provider-keycloak's identifier for an OIDC client is Keycloak's UUID. So it
   may or may not adopt, and the answer is in the resource's conditions. If it
   reaches `Synced=True Ready=True`, nothing more is needed and the provider
   pushes the generated secret onto the existing client. If it reports a
   duplicate clientId or a missing external resource, delete
   `gentian-edge-kernel` from the kernel realm in Keycloak's console and let it
   be recreated.

3. **Nothing to do for the copies.** The operator re-copies the zone secret
   into `tenant-platform` when the source changes, and Envoy Gateway re-reads
   what its policies name. Clearing the zone cookies is enough if a session
   misbehaves afterwards.

A fresh install needs none of this.

**Three things are unproven** and each has a named fallback: whether the
External Secrets `Password` generator honours `secretKeys` in this build (no
generator has ever run here; the fallback is a `target.template` or a
`rewrite`), whether `DeriveFromObject` readiness behaves as documented in
provider-kubernetes (the symptom would be the `Client` never being created;
the fallback is default readiness and dropping the sequencer rule), and
whether the client adopts, which is step 2 above.

One change of character worth knowing: the zone client's secret is generated
rather than derived from the master password, and it is not written to
OpenBao. A cluster rebuilt from a recovery kit regenerates it. That is fine
because both sides read the same Secret, but `SECRET_MODE=derived`
reproducibility no longer covers it.

### S7A.2 ◐ The director stops writing authorization state — only entitlements left

The director's job is to read OpenFGA to decide whether a caller may make a
call, and to write git. Argo CD syncs git and the operator turns it into
cluster state. As built it also writes OpenFGA in six places: it creates the
OpenFGA store object and the model, projects cluster roles from the claim, projects tenants
and tenant roles from the manifests, applies Keycloak membership events,
records session revocations, and writes entitlement tuples.

Move them:

| Write | New home |
| --- | --- |
| the OpenFGA store object and the model at first start | shipped as configuration the operator applies, not created at runtime |
| cluster roles from the claim | operator |
| tenants, `operated_by`, tenant roles | operator |
| membership from Keycloak events | operator |
| entitlement tuples | operator, from the fact the director committed to git |
| session revocation | **deleted**: a five-minute access token and a failing refresh do the same job |

Then take the write capability off the director's OpenFGA token, so the rule
is enforced by the credential and not by care.

**Done so far**: the structure (the OpenFGA store object and model, cluster
roles, tenants) moved to the operator's `AuthzProjectionReconciler`,
**membership moved** to the operator's `MembershipListener`, and
session revocation was **deleted** rather than moved. It existed to make a
logout immediate while the realm's access token lived twelve hours; the realm
now issues five-minute tokens against a twelve-hour session, so the edge's
refresh fails within one token lifetime of the session ending and the tuple,
the write, the endpoint and the background sweep are all gone.

Membership followed. Keycloak's listener now posts to the operator rather than
the director: same path, same signed statements, same projection, a different
host. The director has no membership endpoint, no listener key mounted, and no
code that writes a tuple except the entitlement applier. What is left is the
entitlement tuples the App Store's statements produce, which are gated on
`DIRECTOR_STORE_KEYS` and off on this cluster.

**One thing cannot be done the way this plan assumed.** "Take the write
capability off the director's OpenFGA token" is not available: OpenFGA
authenticates with a preshared key and a key carries no scope, so every key
that may read may also write. There is no read-only token to issue. The rule
is therefore enforced by the director having no code that writes and no reason
to, which is weaker than a credential that cannot. If that is not good enough,
the options are an authorizing proxy in front of OpenFGA or OpenFGA's OIDC
auth mode with something that maps a subject to permitted operations, and
neither is small. Worth a decision rather than a silent assumption.

**Ask first**: how much of the graph can be static rather than written at all.
See "How much has to be written" below.

### S7A.3 ✅ The platform administrator is an address

The kernel realm's `email` claim is mapped to the username so that the email
*field* can hold the recovery address Keycloak mails a reset to. That only
works when the username is itself an address. The platform administrator is
created as the bare name `administrator`, so the claim reads `administrator`.

**Done.** The kernel realm bootstrap now creates `admin@<kernel>`, the same
`admin@<domain>` pattern every tenant administrator follows, and deletes the
`administrator` account it replaces once the new one exists and is in the
group. The password derivation label is unchanged on purpose: it is an opaque
string that decides the derived value, and renaming it would silently change
the password on every cluster. Nothing may create a username that is not an
address.

### S7A.4 ☐ The admin console is an app, and it talks to the director

Today it is a route inside the desktop image, and its screens were built
against a Keycloak admin credential the desktop no longer holds. It becomes a
component like any other, visible only to holders of the relation, whose only
job is to be a GUI over the director's API.

The breakdown, screen by screen:

| Screen | Reads | Writes |
| --- | --- | --- |
| Tenants: list, create, retire | director, from git | director → `clusters/<c>/tenants/<t>/tenant.yaml` |
| Apps in a tenant: install, remove, addons | director, from git | director, endpoints that already exist |
| Entitlements | director, from `entitlements.yaml` | the App Store signs, the director records |
| Cluster settings: kernel domain, platform roles, certificates, LLM | director, from the Cluster claim | director → `clusters/<c>/kernel/claims/cluster.yaml` |
| People and groups | **not here** — Keycloak's own console, embedded; see S7A.4 |
| Authorization view: who holds what | director, read-only from OpenFGA | nothing; changes are made on the screens above |

The director needs write endpoints it does not have yet, for tenants and for
the Cluster claim, each guarded by a relation and each a commit.

**People are not in this console.** Decided rather than deferred. Accounts,
groups and memberships stay in Keycloak, and Gentian does not reimplement
managing them:

- Declaring people in git was the alternative and it is worse. Git is
  append-only, so a name and an address committed there outlive the account,
  which collides with erasure.
- Keycloak's own administration console already does this, is maintained, and
  since 26.2 its fine-grained admin permissions can be scoped so that a tenant
  administrator manages only that tenant's users and groups, without holding
  `realm-admin`. That scoping is what makes it safe to hand to a tenant at all,
  and it is a permission model we would otherwise write ourselves.
- So the People screen becomes the Identity tile: Keycloak's console, embedded
  like any other component, visible to holders of the relation.

Two consequences. The Keycloak hostname fix in S7A.7 stops being a nicety,
because that tile is how anyone reaches people at all. And the fine-grained
permissions have to be granted per tenant by whatever provisions the tenant,
which is a new piece of the tenant composition's work.

#### What the console should be

Not a form per setting. The people who use it are MSP employees and IT
administrators, and they do a handful of jobs: bring a tenant on, give it
apps, set what it may consume, check something is healthy, and answer a
question about who can do what. The console should be organised around those
jobs; the fourteen flat tabs it has today are a map of the systems underneath,
which is a different thing.

What exists to build on: about 7,000 lines across fourteen sections, and they
are on a good track — the resources, backup and security screens in particular
know what they are for. What has to change is where they get their answers.
Today they reach Keycloak and Kubernetes through the desktop's backend. They
should ask the director, which authorises the caller and reads git, and write
through it, which authorises the caller and commits.

Five things worth doing differently:

1. **A tenant is a page, not a filter.** Today every tab takes a tenant
   selector, so working on one tenant means re-choosing it fourteen times.
   Open a tenant and see its apps, entitlements, limits, health and the link
   to its people, and act there.
2. **Show the change before it happens, and the commit after.** Every write is
   a commit to git with the caller as author. A console that says "this will
   add `nextcloud` to `spec.apps`" and then "landed as `a1b2c3d`" is telling
   the truth about what the platform does, and no other admin console can.
   The director already answers whether a commit exists, so the same screen
   can follow it from committed to applied.
3. **Say that a change is on its way.** A write answers 202: git has it, the
   cluster does not yet. The screen should show committed, syncing, applied
   rather than pretending the save was the end of it.
4. **Screens follow relations, never a role string.** The director already
   returns what the caller holds; a screen appears because of that answer and
   for no other reason. Hiding a screen is never what stops someone reaching
   what is behind it.
5. **People stay in Keycloak**, deep-linked with the tenant in the URL. The
   Members, Groups, Invitations and Sessions tabs go (S7A.4), which is four of
   the fourteen and the four that need a credential the console must not have.
6. **The Templates tab was not what its name suggested, and what the name
   suggested is still worth building.** Read before removing: that screen
   copied one member's shell preferences onto another. It is a member screen
   under a different name, it went with the member screens, and nothing is
   owed to it. Its one real use — a new joiner should not start on an empty
   desktop — belongs to the desktop as a default preference set, not to an
   administrator pushing settings onto people one at a time.

   What the name should mean is a **tenant template**, and that does not exist
   yet. It is how an MSP brings on the twentieth customer in the time the
   first one took, and no upstream console offers it. The shape that fits the
   rest of this plan: a template is a file in git under the cluster listing
   apps, entitlements, limits and the tenant settings to seed, and applying
   one is a single director call that writes a tenant from it — one commit,
   one relation (`can_configure` on the cluster), reviewable in the
   deployments repository like everything else. The screen is then a list, a
   preview of the commit it would make, and an apply button, with no backend
   of its own. Build it when the tenant endpoints land, not before.

The director needs endpoints these screens do not have yet. In order:
tenants (list, create, retire), resource plans and ceilings, backup policies
and schedules, security policies, and the authorization view of S7A.8. Each is
the same shape as the app and settings endpoints that already exist: a
relation, a read of git, a commit.

#### Where it stands

First cut landed in `gentian-ui` on `feat/console-is-a-director-client`. The
four people tabs and Templates are gone, replaced by a People tab that opens
Keycloak's console with the session the person already holds, and Cluster
settings arrived as the first screen that asks the director: no catalogue of
its own, no credential, the caller's token forwarded, and a commit named on
screen rather than a save claimed. Fourteen tabs are now eleven.

One thing that blocks the rest. The backend's Keycloak administrator
credential cannot go with the member screens, because `Security` and `Audit`
read through the same admin client (`keycloak_security_policy_store.py`,
`keycloak_audit_fetcher.py`). Until those two move to the director, the
desktop still holds a credential that can read and write the realm, so S7A.6
is not closed by this. Move them next, and the credential goes with them.

Since then the console has moved out of the desktop into
`gentian-apps/apps/admin-console`, built from the restated app template and
described by the `admin-console` ComponentProfile the operator chart ships.
Its backend is a relay to the director and nothing else; every screen whose
director endpoints do not exist yet answers 501 naming the screen
(`NOT_YET_MAPPED` in its `admin.py`), and leaves that list by getting them.

Wired so far, in the order the table above asks for: **Tenants**, **Cluster
settings**, **Resources**. Resources was the first screen whose write already
went to git, only through the operator's own HTTP API; it now goes
`console → director → git → Tenant → operator`. The director gained
`/v1/tenants/{t}/resources` (reads under `can_view`, relayed from the
operator's app-lifecycle API; `PUT` under `can_set_plan`, validated against the
operator's catalogue and committed as the person) and
`/v1/clusters/{c}/resources` for the all-tenants view. The operator lost its
`PUT`, marks each blocked plan with the rule behind it (`blockedBy`), and
records the plan event when the change lands on the Tenant rather than when it
was asked for, with the chooser carried in a second annotation. Self-service
is decided by the director from `can_configure`, not asserted by the screen.
`kubectl gentian resources set` now says where plans are set and exits; the
reads stay. Left for this screen: none. Left in `NOT_YET_MAPPED`: Backup,
Backup policy, Backup schedules, Security, Integrations, Notifications, Audit,
Platform security, Customization; and Credentials needs a credential-manager
mapping on the profile.

#### How it ships

As an app component, built from the app template and described by a
`ComponentProfile` — the same path a customer's app takes, with nothing
reserved for the kernel's own console.

**The template cannot do this yet.** `gentian-app-template` knows only
`AppProfile`; `ComponentProfile` does not appear in it, and the template ships
no CI at all, so there is nothing to build and publish an image with. The only
working reference for the publish flow is `gentian-ui`'s own workflow. So the
sequence is: teach the template `ComponentProfile` and give it a CI workflow,
then move the console onto it. Discovering that gap was the point of choosing
the console as the test. That is deliberate: the template and
the profile are the contract every app is asked to meet, and the fastest way
to find out whether they actually carry a real application is to put the
product's own console through them. If the console needs something the
template cannot express, that is a gap in the template, and better found here
than by the first partner who packages an app.

What it exercises, specifically: an image built by the template's pipeline; a
`ComponentProfile` declaring one HTTP exposure on its own subdomain, the
relation that may open it (`can_configure` on the cluster) and its tile; the
generic provisioning path rather than a bespoke Job; and the route table entry
the operator derives from the profile instead of the compiled-in kernel list.
It needs no database, no secret of its own and no identity beyond the caller's,
which makes it the smallest honest test of the contract.

### S7A.5 ✅ Keycloak looks like the rest of the product

Embedding Keycloak's console makes its appearance the product's appearance, and
it does not currently match anything. The login screen is already themed
(`kernel/services/keycloak-idp/theme/login`); the administration and account
consoles are not.

What is possible, and what is not:

- Both are theme types Keycloak supports, so a `gentian` theme can carry them
  alongside the login one. Both render from a single template, and both are
  compiled React applications built on PatternFly, so what a theme can change
  is the styling, the logo and the favicon, not the layout.
- PatternFly exposes its palette, typography and spacing as CSS custom
  properties, so mapping the `--gtn-*` tokens from `gentian-ui` onto them gets
  most of the way. Dark mode comes with it.
- Overriding the templates themselves is technically allowed and a bad idea:
  Keycloak's own guidance is that custom templates have to be reworked on every
  upgrade, and this is a surface we do not want to own.

So: a shared `gentian-tokens.css` generated from the design system, applied to
the login, account and admin themes, plus the logo and favicon. Accept the
layout as Keycloak draws it.

### S7A.6 ◐ The console and the desktop hold nothing — the desktop does, the admin console does not

A rule to apply to both, and to check before each is called finished: a UI
offers a surface for making requests, and every one of those requests is
decided somewhere else. It holds no authority and sits on no critical path
beyond rendering.

Concretely, neither may hold:

- an OIDC client secret, a Keycloak credential, or any credential belonging to
  a person other than the caller;
- a Kubernetes identity — `rbac.create` stays false and the ServiceAccount
  token stays unmounted;
- a decision. Showing or hiding a screen follows an answer the director gave;
  it is never a rule written in the UI, and hiding a thing is never what stops
  someone reaching it. Every surface behind it is its own enforcement point.

What they may hold is the minimum to be useful: the token the edge forwards,
for the length of the request it relays, and their own store of per-person
display state such as window positions.

The desktop already meets this. The admin console does not, because it was
built against a Keycloak admin credential, which is why S7A.4 rebuilds it as a
GUI over the director's API. When either grows a screen that seems to need a
credential, that is the signal that an endpoint is missing from the director,
not that the UI needs the credential.

### S7A.7 ☐ The zone cookie does not reach the applications

The one place the edge session is weaker than a session per application, and
it is fixable.

The zone's cookie is scoped to `.<kernel>` so that one sign-in covers every
host in the zone. That means the browser sends it to each of those hosts, and
nothing currently removes it before the request reaches the application behind
the route. Neither Envoy's OIDC filter nor our routes strip it. So an
application that is compromised, or simply careless with what it logs, sees a
credential that is good for every other application in the zone — which is
exactly the isolation a per-application cookie would have given.

**Not in the authorization service**, which was the first idea and is wrong.
Its header mutations are applied to the request before the remaining filters
run, and it runs before Envoy's OIDC filter, so a cookie stripped there would
be invisible to the filter that has to validate it. Sign-in would break.

Three ways that do work, in increasing order of what they cost:

1. **Scope the cookie to the host instead of the zone.** Drop `cookieDomain`
   and each host gets its own edge session. The first request to each host
   does one silent round trip to Keycloak, because the Keycloak session
   already exists, so single sign-on is preserved and what the browser sends
   to an application is a cookie good only for that application. Logout still
   works across all of them, because every one of those sessions carries the
   same Keycloak session, which ends everywhere at once. This is the old
   model's isolation with the new model's single implementation, and it needs
   no new component.
2. **Rewrite `cookie` at the router stage**, after every filter has run, with
   a Lua extension policy. Envoy Gateway supports this from 1.3; the cluster
   runs 1.2.5, so it means an upgrade. An upgrade is wanted anyway for the
   logout confirmation in S7A.9.
3. **Remove the whole `Cookie` header** with a route-level header modifier.
   Available today and too blunt: applications behind the edge set their own
   cookies, Argo CD and Keycloak included, and this would take those too.

Recommendation: (1), measured first, because the extra round trip per host is
the only cost and it happens once per session. Do it before any third-party
application is routed.

### S7A.8 ☐ A read-only view of the authorization state

Part of the same console, worth naming separately because it replaces the idea
of exposing OpenFGA's own playground. OpenFGA's read APIs answer "which groups
hold which relations on which objects" without any write surface. The console
renders that; anything a person wants to change is changed on the screens
above, through the director, into git. No development-only UI is exposed and
no second write path exists.

### S7A.9 ◐ The kernel UIs are actually usable — all three built, none verified since

- **Argo CD** ✅ showed an empty list to a full administrator. The groups claim
  carries the full path, `/gentian:platform:admin`, because OpenBao's roles
  need it; Argo CD's policy named the bare form and matched nothing. Fixed by
  naming both spellings. Verify after the next `D-02`.
- **Headlamp** ◐ asks for a second sign-in, and until today that second
  sign-in failed: the callback answered `unauthorized_client`, "Invalid client
  or Invalid client credentials". Headlamp builds the code exchange out of the
  `auth-provider` block of the kubeconfig it proxies with, and that block named
  `client-id: headlamp` with no secret, so it authenticated with nothing
  against a confidential client. Proved inside the pod against the live realm:
  the real secret answers `400` for a spent code, a wrong one and no one at all
  both answer `401`. The kubeconfig is now a Secret carrying a placeholder that
  the portal bootstrap rewrites with the client's real secret before restarting
  Headlamp. **Verify after the next `D-02`.** The click itself remains: decide
  between starting that flow from the tile and giving Headlamp an
  authenticating sidecar that turns the edge session into what it expects,
  which is the pattern an app with no OIDC support would use anyway.
- **The Keycloak console** ✅ showed a spinner. Not an iframe problem: embedding
  works, the silent SSO and the token exchange both complete in the frame, and
  it then dies on its first Admin REST call with a 401 because `KC_HOSTNAME`
  and `KC_HOSTNAME_ADMIN` differ. Upstream closed this as not planned, so a
  new tab fails identically and there is nothing to wait for. **Decided**:
  networking.md §3 now serves `/auth/admin/*` on `id.<kernel>` behind the
  kernel session and retires `id-admin.<kernel>`. To build: drop
  `KC_HOSTNAME_ADMIN`, move the route and its policy, add the redirect URI,
  and repoint the tile. While there, stop clearing the realm's clickjacking
  defences wholesale — one tile should not cost every login page in the realm
  its protection. This tile is now also how tenant administrators manage
  people (S7A.4), so it is reached by more than the platform administrator and
  its relation has to allow for that.

  Two things surfaced behind that fix, both now built. The console mints a
  token of its own inside the page and calls the Admin REST API with it, so the
  edge must leave that header alone — and "leave it alone" is neither of the
  two things the edge knew how to do. Stripping it, which is right everywhere
  else because a backend should get identity headers rather than a token it
  cannot use, answered `401` and left the console on its spinner. Forwarding
  the edge's own token instead answered "Token issued for an application that
  is not the admin console", which was true: that token is minted for the
  zone's client. So the single `forwardToken` flag is now two. `forwardToken`
  still means the edge puts its token on the request, and only the desktop asks
  for it; `keepClientToken` means the caller's own bearer survives untouched,
  and the console asks for that. **Verify after the next `D-02`.**

### S7A.9b ✅ A refusal a person can act on

An account that is deleted or renamed leaves live sessions naming a subject
the graph no longer knows. Every relation is then denied, correctly, and the
answer was the bare word `Forbidden` on every page — including the desktop the
person would have signed out from, so there was no way out but clearing
cookies by hand. This happened the moment the `administrator` account was
replaced in S7A.3.

A refusal on an `oidc` route now carries a small page naming the one link that
can change the outcome, the edge's own `/oauth2/logout`. It grants nothing:
signing out is available to anyone holding a session, refused or not. A
`bearer` route still gets the bare status, because a program is reading it.

### S7A.10 ◐ The installer does what it claims — two races and the OIDC mount done

Recorded in WP-10. Two cold-start races are fixed. These remain, in priority
order:

1. The OpenBao **`oidc` auth mount**. **Done**: `B-09-vault-oidc-mount`
   enables it between the seeded secrets and the Cluster claim, so the roles
   the composition composes have a mount to attach to. Its *configuration*,
   which needs the realm's client secret and Keycloak serving discovery, is
   still missing and belongs after `D-02`.
2. The four **`Repository` claims**. Without them nothing composes Argo CD's
   repository Secret, the operator's push credential or the catalogue-sync
   ApplicationSet, and a private deployments repository has no credential path.
3. **Tenant teardown**, without which a purge cannot complete — which S8 needs.
4. The **credential catalogue** that `make check-credentials` reads.
5. The **recovery kit** and the **bootstrap token revocation**, so an install
   does not end with the installer's root token still valid.
6. **Mail** and **LLM serving** have no v5 step and no ApplicationSet, while
   the operator still writes Postfix entries and still routes `llm.<kernel>`
   to a service nothing deploys. Decide whether these are deliberate drops.
7. `B-08-seed-secrets` declares a dependency on a step that runs after it.

### S7A.11 ☐ Signing out does not ask a second time

Pressing sign out lands on a Keycloak page asking whether you meant it, and
only then returns. Nothing is broken; it is what Keycloak does when a logout
request arrives without an `id_token_hint`. Since Keycloak 18 a logout that
cannot prove which session it means has to be confirmed by the person, so that
a link on someone else's page cannot sign people out. The edge sends exactly
such a request: Envoy Gateway's `logoutPath` clears the zone cookies and hands
the browser to the realm's `end_session_endpoint` with no hint.

The hint is already in the browser. The zone keeps the ID token in its own
named cookie, `gentian-kernel-id`, next to the access token. So the fix is to
send it, and the order matters, because `/oauth2/logout` clears that cookie:

1. Sign out points at the edge authorization service, which already serves the
   zone's refusal page and knows which zone the host belongs to.
2. It reads `gentian-kernel-id` and answers `302` to
   `{issuer}/protocol/openid-connect/logout` with `id_token_hint` set and
   `post_logout_redirect_uri` set to the zone's own `/oauth2/logout`.
3. Keycloak ends the SSO session without asking, because the hint names it,
   and returns the browser to `/oauth2/logout`.
4. Envoy clears the zone cookies and the person lands on the portal, signed
   out of the realm and not only of the edge.

One thing has to be provisioned for it: `post.logout.redirect.uris` on the
zone's confidential client, which the composition of S7A.1 should write along
with the redirect URIs it already writes. Keycloak rejects an unregistered
`post_logout_redirect_uri` and the person would end on an error page instead.

Two alternatives, recorded so they are not rediscovered:

- **Admin REST**, `POST /admin/realms/{realm}/users/{id}/logout`, ends every
  session server-side with no redirect at all. Rejected: it needs
  `manage-users` in the director, which S7A.2 just took away, and it would
  make signing out depend on a credential rather than on the person's own
  session.
- **Account REST**, `DELETE /realms/{realm}/account/sessions`, ends the
  person's own sessions with the person's own token and no admin rights. It is
  the right shape, but it needs `account` in the edge token's audience, which
  means an audience mapper on every zone client, and it gives no redirect, so
  the console would still have to drive the browser afterwards. Keep it as the
  fallback if the hint route hits something unexpected.

### S7A.12 ◐ The tile catalogue leaves the director — built, not yet verified

`GET /v1/clusters/{c}/tiles` is what the portal asks for the links it should
show. The director answers it from `internal/director/tiles/tiles.yaml`, a
list of three kernel UIs compiled into the binary, filtering each by whether
the caller holds one of its relations on the cluster.

The filtering belongs there. The list does not. Which components exist, where
each is served and what relation opens it is cluster state, and the operator
already holds all of it: it writes the `HTTPRoute` for those same hostnames
and it reads the `ComponentProfile` of every installed app. A catalogue
compiled into the director means the director has a second, hand-maintained
copy of that, which will drift, and it means an installed app cannot appear on
the portal without a director release — which is the wrong answer for a
platform whose point is installing apps.

To build:

1. The operator projects the catalogue from what it actually routes: the
   kernel routes it composes plus every `ComponentProfile` exposure that
   declares a tile, into one ConfigMap in `kernel-control`.
2. `ComponentProfile` gains the tile fields an app needs to describe itself —
   display name, description, icon, the path within the host, and the relation
   that may open it. An app that declares none gets no tile.
3. The director reads that ConfigMap instead of the compiled list and keeps
   doing the one thing that is its job: asking the graph, per caller, which of
   those the caller may open. `internal/director/tiles` and its YAML go.

The endpoint itself stays where it is. A tile the caller cannot open must not
be on the page, that decision is an authorization read, and authorization
reads are what the director is for.

**Built.** The operator projects a `gentian-tiles` ConfigMap in the control
namespace from the routes it composes plus every `ComponentProfile` exposure
that declares a tile; `internal/director/tiles` and its YAML are gone. Each
kernel tile's hostname now comes from its route rather than being written
twice, so a console with no route is simply absent instead of being a link to
nothing.

Three decisions worth knowing, none of them forced:

- **`ComponentProfile` gained the tile fields**, and its doc comment, which
  said the type carries no presentation because presentation is the App
  Store's, was rewritten rather than left saying something untrue. The store
  still owns the catalogue listing; the cluster owns the tile, because the
  portal has to show what the cluster routes without asking a service outside
  the cluster. `AppProfile`'s existing tile fields were not reused: they are
  the v4 portal's icon plumbing and they carry no relation, which is the one
  field the per-caller question needs.
- **The director reads that ConfigMap as a mounted file**, not through the
  Kubernetes API. It holds the git push credential and no cluster credential,
  and its ServiceAccount does not even mount a token; an API read would have
  meant giving it one, plus a Role and a RoleBinding, for one ConfigMap. A
  cluster whose operator has not projected yet has no file, and the endpoint
  answers an empty list.
- **No app declares a tile yet**, so a real cluster's catalogue today is the
  same three kernel consoles it was before. The desktop declares none on
  purpose: it *is* the page the tiles are shown on.

One thing to fix before an app can have a tile: the relation an app's profile
declares has to exist on `type app` in `authz/model/v1/model.fga`, and today
that type has only `tenant` and `admin`. `can_launch` is named in the design
documents and is not in model v1.

### S7A.13 ☐ `denyPaths` promises a control it does not apply

`ComponentProfile.spec.expose[].denyPaths` is declared, documented as "refused
even where Paths admits them. Deny wins regardless", and read by no code
outside tests. `buildExposureRoute` uses `paths`, `authMode`, `backend`,
`forwardToken` and `subDomain`, and nothing else.

That makes it worse than a missing feature. A component author reading the CRD
has every reason to believe that listing an administrative path under
`denyPaths` keeps it off the edge, and it does not: the path is served. The
same is true of `stripPrefix` and `source`, though neither reads as a security
control, so neither misleads in the same way.

Either build it or take it out, and prefer building it: deny rules on a route
are what a component needs to expose a UI without exposing its own admin
endpoints, and the alternative is every app carrying that logic itself. Until
one or the other lands, the field is a false statement in a published API.

Found while putting the console through the app template. It is the same
pattern the September threat-model exercise turned up, which is worth saying
out loud: a declared field is not a control, and the CRD is a place we have
now twice described one we do not have.

## S8 — purge and reinstall

`install.sh --layout v5` from nothing, every `check()` honest, `--status` all
true, and the sign-in confirmed in a browser rather than by a script that
cannot execute the page.

## After M1

The work packages in order. Each is specified in `work-packages.md`.

| Order | Package | Why here |
| --- | --- | --- |
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

## How much has to be written

Raised while planning S7A.2 and worth answering before building it.

Most of what the director writes into OpenFGA today is not per-cluster at all.
A note on naming: OpenFGA, or the ReBAC graph. Never "the store" — that is the
App Store, which is a different thing entirely.
The authorization **model** is a file in the repository and changes only when
the model version does. The **relation structure** — that a cluster has
tenants, that a tenant has admins, members and a perimeter group, which
relation each role implies — is the model, not data. What is genuinely
per-cluster is small: which groups exist, who is in them, which tenants this
cluster has, and which entitlements are current.

So the graph should arrive mostly built: the model shipped and applied like a
CRD, the role-to-relation structure derived from the model rather than written
tuple by tuple, and only the names, the memberships and the facts written at
runtime. That is both less code and a smaller blast radius: a bug in a
projector can then add or remove a membership, but it cannot invent a relation
that was never in the model.
