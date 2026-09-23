# Implementation plan

The order things get built in. Milestone M1 is specified in
[work-packages.md](work-packages.md) under WP-10; this document is the running
order, what is done, and what has to land before each next step. The work
packages say *what* each piece is, this says *when*.

Step labels `S1`…`S8` are the eight numbered items inside M1. They exist so a
conversation can point at one; they are not a second plan.

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

### S7A.1 The operator produces a zone's Keycloak client and its secret

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

**Done when** a second tenant signs in at `console.<t>.<kernel>` against its
own realm, with no installer step having run for it.

### S7A.2 The director stops writing authorization state

The director's job is to read OpenFGA to decide whether a caller may make a
call, and to write git. Argo CD syncs git and the operator turns it into
cluster state. As built it also writes OpenFGA in six places: it creates the
store and the model, projects cluster roles from the claim, projects tenants
and tenant roles from the manifests, applies Keycloak membership events,
records session revocations, and writes entitlement tuples.

Move them:

| Write | New home |
| --- | --- |
| store and model at first start | shipped as configuration the operator applies, not created at runtime |
| cluster roles from the claim | operator |
| tenants, `operated_by`, tenant roles | operator |
| membership from Keycloak events | operator |
| entitlement tuples | operator, from the fact the director committed to git |
| session revocation | the edge authorization service, which is the only reader |

Then take the write capability off the director's OpenFGA token, so the rule
is enforced by the credential and not by care.

**Ask first**: how much of the store can be static rather than written at all.
See "How much has to be written" below.

### S7A.3 The platform administrator is an address

The kernel realm's `email` claim is mapped to the username so that the email
*field* can hold the recovery address Keycloak mails a reset to. That only
works when the username is itself an address. The platform administrator is
created as the bare name `administrator`, so the claim reads `administrator`.

Rename it to `admin@<kernel>`, produced by the same code path that names every
tenant's administrator rather than by a separate constant, and delete the old
account. Nothing may create a username that is not an address.

### S7A.4 The admin console is an app, and it talks to the director

Today it is a route inside the desktop image, and its screens were built
against a Keycloak admin credential the desktop no longer holds. It becomes a
component like any other, visible only to holders of the relation, whose only
job is to be a GUI over the director's API.

The breakdown, screen by screen:

| Screen | Reads | Writes |
| --- | --- | --- |
| Tenants: list, create, retire | director, from git | director → `clusters/<c>/tenants/<t>/tenant.yaml` |
| Apps in a tenant: install, remove, addons | director, from git | director, endpoints that already exist |
| Entitlements | director, from `entitlements.yaml` | the store signs, the director records |
| Cluster settings: kernel domain, platform roles, certificates, LLM | director, from the Cluster claim | director → `clusters/<c>/kernel/claims/cluster.yaml` |
| People and groups | director, from git plus a read of the store | **open question below** |
| Authorization view: who holds what | director, read-only from OpenFGA | nothing; changes are made on the screens above |

Two things follow. The director needs write endpoints it does not have yet
(tenants and the Cluster claim), each guarded by a relation and each a commit.
And membership is the open question: accounts live in Keycloak, not git, so
either tenant membership is declared in git and the operator reconciles
Keycloak from it, which keeps the rule intact, or the console keeps a path to
Keycloak that is not the director, which breaks it. The first is the plan's
shape; it needs deciding before the screens are built.

### S7A.5 A read-only view of the authorization state

Part of the same console, worth naming separately because it replaces the idea
of exposing OpenFGA's own playground. OpenFGA's read APIs answer "which groups
hold which relations on which objects" without any write surface. The console
renders that; anything a person wants to change is changed on the screens
above, through the director, into git. No development-only UI is exposed and
no second write path exists.

### S7A.6 The kernel UIs are actually usable

- **Argo CD** showed an empty list to a full administrator. The groups claim
  carries the full path, `/gentian:platform:admin`, because OpenBao's roles
  need it; Argo CD's policy named the bare form and matched nothing. Fixed by
  naming both spellings. Verify after the next `D-02`.
- **Headlamp** asks for a second sign-in. It runs its own OIDC flow with its
  own client and keeps the result in its own cookie, and the kubeconfig it
  proxies with has no token until that flow has run. It costs a click, not a
  password. Decide between starting that flow from the tile and giving
  Headlamp an authenticating sidecar that turns the edge session into what it
  expects, which is the pattern an app with no OIDC support would use anyway.
- **The Keycloak console** shows a spinner. Not an iframe problem: embedding
  works, the silent SSO and the token exchange both complete in the frame, and
  it then dies on its first Admin REST call with a 401 because `KC_HOSTNAME`
  and `KC_HOSTNAME_ADMIN` differ. Upstream closed this as not planned, so a
  new tab fails identically. Drop the split hostname and serve `/auth/admin/`
  on `id.<kernel>` behind the kernel session. While there, stop clearing the
  realm's clickjacking defences wholesale.

### S7A.7 The installer does what it claims

Recorded in WP-10. Two cold-start races are fixed. These remain, in priority
order:

1. The OpenBao **`oidc` auth mount** and its configuration. The Cluster
   composition already composes auth roles against a mount no v5 step creates,
   so OpenBao accepts no Keycloak login.
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

## S8 — purge and reinstall

`install.sh --layout v5` from nothing, every `check()` honest, `--status` all
true, and the sign-in confirmed in a browser rather than by a script that
cannot execute the page.

## After M1

The work packages in order. Each is specified in `work-packages.md`.

| Order | Package | Why here |
| --- | --- | --- |
| 1 | WP-1 Director | S7A.2 is its first item; the rest of the API follows |
| 2 | WP-3 Authorization | who projects into the store, and the naming rule |
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
The authorization **model** is a file in the repository and changes only when
the model version does. The **relation structure** — that a cluster has
tenants, that a tenant has admins, members and a perimeter group, which
relation each role implies — is the model, not data. What is genuinely
per-cluster is small: which groups exist, who is in them, which tenants this
cluster has, and which entitlements are current.

So the store should arrive mostly built: the model shipped and applied like a
CRD, the role-to-relation structure derived from the model rather than written
tuple by tuple, and only the names, the memberships and the facts written at
runtime. That is both less code and a smaller blast radius: a bug in a
projector can then add or remove a membership, but it cannot invent a relation
that was never in the model.
