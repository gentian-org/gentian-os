---
audience: architect, app-author
type: concept
---

# App authorization

How an app gets an authorization store of its own. Authorization is a
service a profile declares, like a database: the platform makes the app a
store with the app's own model, gives the app a key good for that store and
nothing else, and keeps the store with the tenant's data.

## 1. Two OpenFGA instances

The platform decides with OpenFGA in two places, and the two never share a
server.

| | Kernel authorization | App authorization |
|---|---|---|
| Holds | the platform's model: clusters, tenants, apps, groups ([authorization.md](authorization.md)) | one store per tenant and component, each with the component's own model |
| Runs in | `kernel-authorization` | `system-authorization` |
| Asked by | the enforcement points: bouncer, director, usher, custodian, registrar, the operator | the arbiter, and nothing else |
| Written by | the operator's projection and the membership listener | the arbiter, for the component that holds the store |
| Its data | structure rebuilt from git and the Cluster claim; memberships return from Keycloak as each person's groups next change | tenant data: exported, restored and purged with the tenant |
| Database | `kernel-postgres` | the tenants' data plane, `system-postgresql` |

OpenFGA's credential opens every store on its server, so a key that reached
the kernel instance from an app would be a key to the cluster. Keeping the
apps on an instance of their own makes the worst an app store can suffer the
loss of app stores, and keeps an app's load off the checks the edge makes on
every request.

### 1.1 The kernel store is pinned by id

The operator creates the kernel store and its model and records both ids in
the ConfigMap `authorization-store` in `kernel-control`, and a copy of it in
`kernel-edge` for the bouncer, which reads ConfigMaps only from its own
namespace. Every enforcement point reads the ids from it and refuses to
start without them (`internal/director/authz/pinned.go`); none looks a store
up by its name. A second store called `gentian`, however it came to exist, is
never asked. The operator looks a store up by name once, to adopt the store
of a cluster that made it before ids were recorded; a recorded store that is
gone is replaced by a new one.

## 2. The authorization service

### 2.1 What a profile declares

`requires.services.authorization`: the model in the OpenFGA DSL, and
optionally `groups: mirror` (§2.5) and `tuples` (§2.6). The field, the Secret
`authorization-<component>` the app receives (`AUTHORIZATION_URL`,
`AUTHORIZATION_KEY`) and the network path to the arbiter's port alone are in
[app-customization.md §1.1](../guides/app-customization.md).

The operator compiles the model when the profile is admitted and refuses a
profile whose model does not compile, so a broken model is a failed install
and never a store without one. Every trust tier may declare it: a store
holds only the declaring component's own relations, and nothing in it is
read by the platform.

### 2.2 The store

A store belongs to one tenant and one component and is named
`<tenant>/<component>`. The arbiter creates it on first sight and records its
id; a store is never looked up by name after that. A model that changes in a
new profile build is written as a new model of the same store, and the
tuples stay: OpenFGA models are immutable and versioned, and the arbiter
always decides against the latest one the operator handed it.

People are named as the platform names them, `user:<realm>/<subject>`, and
groups `group:<realm>/<group name>`, through the one mapping every identifier
crosses into OpenFGA by (`internal/arbiter/naming.go`), so a customer of one
realm and a person of another can never be one user, and an app's tuples and
the platform's agree on who someone is. The arbiter does not enforce the
convention for the tuples an app writes; the model is the app's.

### 2.3 The arbiter

The arbiter (`cmd/arbiter`, `internal/arbiter`) is a kernel service in
`system-authorization` and the only holder of the app instance's credential.
It knows each store by the hash of the key that opens it, from a table the
operator writes: tenant, component, the key's SHA-256 and the compiled model.
It keeps the store and model ids it created in a ConfigMap of its own
namespace.

Its API is OpenFGA's, with the store and the model taken from the key; the
routes and their shapes are in [app-customization.md §1.1](../guides/app-customization.md).
Every call carries `Authorization: Bearer <AUTHORIZATION_KEY>`. A key the
table does not name is `401`; nothing a caller sends can name another store,
and a field the API does not have is `400`. Each key has a rate limit (100
calls a second, burst 200), so no app can slow another's decisions. A key may
not write or delete a tuple whose user or object is of type `group` (`403`):
who is in a group is the realm's to say (§2.5). A refusal is
`{code, message}`.

The arbiter runs from the operator's image with its own command, and the
operator deploys it, so that the arbiter is always the build that matches
the table it reads. The instance's preshared key and its database password
are kernel credentials in the vault (`gentian-os/kernel/authz/openfga-apps`,
`gentian-os/kernel/database/openfga-apps`), seeded once by the operator and
delivered by ExternalSecrets to the instance, the role Job and the arbiter;
no other workload is handed the key
(`internal/controller/authorization_engine.go`).

### 2.4 The platform's path

What the platform writes into stores and reads out of them, no app's key
can: the realm's memberships (§2.5), a store's export and restore, a purge.
The operator calls `/v1/platform/...` on the arbiter with a token of its
ServiceAccount projected for the audience `gentian-arbiter`, and the arbiter
asks the API server whose it is (TokenReview), as the operator's own listener
admits the director. Only the operator's ServiceAccount is admitted: a
component's key is no token the API server vouches for, and a component's
own token for that audience names the component's account.

### 2.5 The realm's groups

`groups: mirror` has the platform keep the tenant realm's group memberships
in the store: every top-level group of the realm, not only the platform's
vocabulary, as `group:<realm>/<group name>#member` of
`user:<realm>/<subject>`, `:` in a name written as `/`. The tenant's own
members group, `gentian:tenant:<t>:members`, is
`group:<realm>/gentian/tenant/<t>/members`. The model must define a type
`group` whose relation `member` users are written with.

The memberships come from the signed state events the membership listener
receives for the kernel's store, after the kernel's projection, which is
unchanged (`internal/membership/events.go`): a person's whole set of groups,
compared with the store and written; a deleted group loses its members. A
realm reaches the stores of the components of its one tenant and no other; a
realm no tenant has, or several share, reaches none. A nested group is not
mirrored, as the kernel's store holds none. An event the stores did not take
is refused, and the listener sends it again.

Keycloak sends memberships as they change and the kernel's projection keeps
no list of them, so a store made after a membership was holds it from that
person's next change on.

### 2.6 Starting tuples

`tuples` are written by the platform when the store is first resolved and
whenever the profile's list changes; one the profile no longer names is
taken out again. The arbiter records which tuples it wrote, so a tuple the
app wrote itself is never touched. They are held to the compiled model at
admission; a store that mirrors groups names no group as an object here.

### 2.7 Lifecycle

| Event | The store |
|---|---|
| a component is installed in a tenant | created on first sight, with the profile's model |
| a new profile build | a new model, tuples kept |
| the component is uninstalled | kept, as every store of a removed app is kept, until the tenant's retained data is purged |
| a tenant export | every tuple and the model, as the artifact `authorization` of the app, read through the platform's path and encrypted by the operator itself |
| a tenant restore | the tuples written back into a fresh store, once |
| a purge | the store deleted, through the platform's path |
| the tenant deleted with its data | every store of the tenant deleted |

## 3. System services on demand

The app authorization instance is one of the system engines the cluster
composes only once a component declares it (`system-authorization`, wanted
when a profile declares `authorization`), beside MariaDB, the cache and
TURN; the operator keeps the set of wanted engines, the Cluster composition
composes each one, a component waits with `SystemEngineNotReady` until its
engine is ready (and, for authorization, the arbiter), and an engine is
never taken away because the last component that needed it went: its data
may be all that is left of a removed app. How the set is kept, how a person
purges an engine and which engines are on every cluster is in
[kernel.md](kernel.md) (`internal/controller/system_engines.go`).

## 4. Who decides what

| Decision | By |
|---|---|
| that a component has a store | its profile |
| the model of the store | its profile, compiled by the operator |
| the tuples | the component, through the arbiter; group memberships the realm, starting tuples the profile |
| which engines run | the profiles installed, and the claim |
| that an engine is removed | a person, by purge |
