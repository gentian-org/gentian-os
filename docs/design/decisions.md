---
audience: architect, auditor
type: design
---

# Architectural decisions

The decisions that set the direction of the architecture. Each is stated once
here, by number, with its reason and a link to the design document that holds
the detail. Other documents cite them as "AD-n". A decision changes by editing
its entry, not by contradicting it elsewhere.

## AD-1 The security principles are normative

The nine rules in [security-principles.md](../security-principles.md) bind
every change; one that breaks a rule needs a written reason. Every request
passes exactly one named enforcement point (the Gateway, a publishing proxy,
the director, the custodian, the MCP gateway), and apps never decide platform
questions. One accepted limit: on a public entry of `authMode: app` the
publishing proxy enforces paths, stripped headers and rate, not who calls; the
credential is the app's, so a person removed from the tenant keeps access
through it until the app revokes it ([security.md §2.14](security.md),
[iam.md §1.11](iam.md)).

## AD-2 The director is the only writer to the deployment repository

Every write is authenticated by Keycloak and authorized by OpenFGA; commits
are signed, and Argo CD (`AppProject.spec.sourceIntegrity`) syncs only commits
signed by the director or the break-glass key. The operator writes to the
cluster only and never holds a git credential. Git is thereby the trusted,
audited source of cluster configuration
([control-plane.md §4.1](control-plane.md), [security.md §3.0](security.md)).

## AD-3 The store's data lives outside the cluster; its interface is an app on it

The store, a service run by a vendor, holds and serves what is known about
apps and what a tenant acquired; it never calls the cluster and decides
nothing for it. The App Store app, installed per tenant, renders the store's
data as data and installs through the director and the custodian under the
signed-in person's own token. There are no entitlements and no license
gating: supply is controlled at the source, by a credential for the
repository that holds a paid app's chart; the digest pins the profile bundle,
and payment never passes through the cluster. Browsing needs no sign-in; the
store depends on usage statistics ([store-contract.md](store-contract.md)).

## AD-4 One catalog kind, `ComponentProfile`, with classes

One kind describes system services, apps and agents. A profile lists the
classes it is certified for (`spec.classes`: `service`, `shared-app`, `app`);
the instance carries the one class the responsible administrator chose
(`Component.spec.class`), which must be in that list. `service` is exclusive;
`shared-app` requires `trustTier: platform`, a CRD validation invariant
rather than an admission policy ([app-catalogue.md §3](app-catalogue.md)).

## AD-5 Requirements and integrations are separate fields

A requirement is owed by the platform, gates admission, and its absence is
the platform's fault; an integration is a peer relationship, bound
continuously, absent peers normal. Privileges (egress beyond the baseline,
pod-security waivers, elevated roles) are requirements with one approval
path, so a profile cannot grant itself any of them
([app-catalogue.md §3.1](app-catalogue.md),
[security.md §3.4](security.md)).

## AD-6 Exposure carries a mandatory `authMode` and a `surface`

`authMode` has no default; `surface` is `gateway` or `perimeter`. A perimeter
surface is a capability the profile declares; a proxy exists in
`tenant-<t>-dmz` only for an entry a perimeter approver published, under
`can_expose`, never `admin`, so publishing is always its own audit line. The
same relation answers egress beyond the baseline. Who holds it is named in
git on the Cluster claim and projected by the operator
([security.md §2.14](security.md), [iam.md §1.1c](iam.md)). What the
platform does for a published entry is one of two things: `none` forwards the
declared paths and passes no credential; `app` passes the caller's
`Authorization` header to the app, at a lower rate. An approval is of a kind,
and the operator acts on it only while the entry still declares that kind.
`basic`, `signature`, `jwt` and `bearer` are refused on a perimeter entry,
because nothing could enforce them ([routing.md §7](routing.md)).

## AD-7 Namespaces are named by tier

`kernel-<function>`, `system-<function>`, `system-<function>-dmz`,
`shared-<app>`, `tenant-<t>`, `tenant-<t>-dmz`. Tier-0 operators are kernel
and are named by the function they perform (`kernel-provisioning`, not the
product), so the software can change. Policies select on the labels
`gentianos.io/tier` and `gentianos.io/function`, never on names
([kernel.md](kernel.md), [kernel/namespaces.yaml](../../kernel/namespaces.yaml)).

## AD-8 Kernel trust domains are separate namespaces

`kernel-authentication` (Keycloak, public route) apart from
`kernel-authorization` (OpenFGA, enforcement points only); `kernel-seal` apart
from `kernel-secrets`, because the seal's unseal key is an in-cluster Secret;
kernel identity on its own Postgres in `kernel-data`, not on the tenants' data
plane ([kernel.md](kernel.md), [security.md §2.8](security.md)).

## AD-9 System services have no public route

HTTP access from outside goes through a tenant's DMZ with that tenant's
credential. A system service whose protocol needs an internet listener (mail,
TURN) puts that listener in its own DMZ namespace, `system-<function>-dmz`,
as a stateless edge holding one credential to the backend. Data services are
one namespace per engine, named by the function a requirement declares
([networking.md](networking.md), [mail.md](mail.md)).

## AD-10 The desktop is the desktop, for everyone; the consoles are apps

The desktop runs per tenant in `tenant-<t>` with that tenant's credentials
only, and the platform is a tenant with a desktop like every other. What
differs between a platform administrator, a tenant administrator and a member
is the tiles their relations grant, never the image; administrator and member
are disjoint roles. The desktop is a relay for tiles and carries no
administrative code path. The administration console and the App Store app
are components installed per tenant from their own profiles. Nothing with
authority leaves `kernel-control`; nothing without authority stays in it. No
console holds a client secret: the edge is the only session authority (AD-13)
([admin-console.md](admin-console.md), [iam.md §1.3](iam.md)).

## AD-11 The layout applies to fresh installs

Stateful kernel components (OpenBao, Keycloak, CNPG clusters) are never
renamed in place; an existing cluster rebuilds from its recovery kit or keeps
its names and adopts the labels ([kernel.md](kernel.md),
[recovery-playbook.md](../recovery-playbook.md)).

## AD-12 The authorization store is a projection; Keycloak's events are the only source of membership

Group membership is stored in OpenFGA as `group#member` tuples written by the
operator from the signed statements Keycloak's event listener sends it, never
edited in place; nothing re-reads them, and a membership the store lacks
returns as each person's groups next change. No group travels in a token for
a platform decision. Structure (role-to-group assignments, tenants, installs,
grants) is projected by the operator from what git declares, once Argo CD has
applied it. The operator projects; the director asks. Git holds the defaults,
and everything beyond them is an action through the director
([authorization.md](authorization.md)).

## AD-13 The edge is the only session authority

One confidential OIDC client per tenant zone, on the `authenticated` Gateway,
mints the one edge cookie. No platform component downstream runs a code flow
or holds a client secret, with one exception: the bouncer holds each realm's
`gentian-edge-exchange` client secret, to exchange a session's token for one
valid at a single app (`exchangeToken`). The Gateway forwards the edge token
only on a route whose entry sets `forwardToken`; every other backend receives
identity headers. An approved entry may keep the app's own `Authorization`
header (`clientAuthorization: app`); the session stays required. Ending a
session at Keycloak is what ends it: the access token's lifetime is the bound
on how long a signed-out person is still let through, so that lifetime is the
control, and no revocation list exists. The sign-in sidecar is the one
component beside an app that signs people in; the four conditions it is held
to and what it costs are in [security.md §2.12](security.md) and
[iam.md §1.11](iam.md). A call to a partner on a person's behalf is not an
exception (AD-17). The detail is [routing.md §4](routing.md).

## AD-14 Without a store, apps are installed by command only

`kubectl gentian apps install` asks the director on the same route and under
the same question as every other install (AD-3). The cluster renders no
catalog of its own in any interface; a catalog is what the director fetches
from: an https address serving an index and one profile bundle per file, or
an OCI artifact read with the credential of a repository its owner declared,
never a directory of the deployments repository. Addresses stay public. One
bundle, one digest. A profile reaches a cluster in this way and in no other;
the director answers an index's `ce`, `me` and `ie` entries so a digest can
be looked up without a store. A catalog exists for the whole cluster, for one
tenant by the cluster's administrator, or for one tenant by its own
administrator where that was delegated. One exception: the installer's
default profiles, placed before there is a director to ask and held to the
same digest rule ([store-contract.md §9](store-contract.md),
[custom-catalogues.md §4.4](../custom-catalogues.md),
[install-reference.md](../install-reference.md), "The default profile").

## AD-15 The OS is multi-lingual

Every word a person reads is translatable, and a language is added by adding
a message catalog, never by changing a component. The platform's own chrome
(desktop, console, Keycloak's pages) carries a message catalog per locale and
no inline user-visible string; a component's catalog text carries a required
fallback plus an optional map keyed by locale (`de_DE`, `en_US`). Matching is
forgiving within a language and never across one (`de-CH` is served `de_DE`);
a string with no translation renders its fallback, never a blank or a key.
The viewer's language comes from their account, and from the browser until
they have said.

## AD-16 Every cluster role but `admin` and `breakGlass` is held by a person named in git

`admin` and `breakGlass` are held by a group of the kernel realm
(`spec.platformRoles`). Every other role is held by name on the Cluster claim
(`spec.platformAccess.roleHolders`), written through the director under
`can_configure`, with a reason and an optional `expiresAt`; the operator
writes the tuple as the account of its realm and removes it with the entry.
Only a token of that realm matches it. The cluster's administrator cannot
take over a holder of the platform tenant: the registrar refuses every write
to such an account. The reverse direction, the cluster's administrators
signing in to a tenant, is the tenant's own switch
(`spec.platformAccess.adminsEnter`) ([iam.md §1.1c](iam.md)).

## AD-17 Identity crosses clusters only as a partner link, by standards

A tenant's realm declares partners, bounded by the Cluster claim's
`allowedIssuers`. A person links their two accounts once by signing in at the
partner through their own realm; an app then gets an assertion towards a
partner by standard token exchange at its own realm, authenticated with its
service account token, and the other side turns it into a local account by
the JWT bearer grant. No secret crosses, and only minimal information crosses:
a pairwise subject, and `gentian_roles` for administrators. Nothing in it is
specific to an app ([partners.md](partners.md)).
