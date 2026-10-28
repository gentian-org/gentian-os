# App authorization and system services on demand

How an app gets an authorization store of its own, and how the system tier
runs only the engines that something declares. Authorization becomes a
service a profile declares, like a database: the platform makes the app a
store with the app's own model, gives the app a key good for that store and
nothing else, and keeps the store with the tenant's data. The engines of the
system tier are composed when the first component declares one and stay
until a person purges them. Decided 2026-10-25, with mirrored groups,
starting tuples and the platform's path added the same day. §1 to §4 is the
design, §5 the order of work, §6 what stays on the roadmap, §7 what is open.

## 1. Two OpenFGA instances

The platform decides with OpenFGA in two places, and the two never share a
server.

| | Kernel authorization | App authorization |
|---|---|---|
| Holds | the platform's model: clusters, tenants, apps, groups, contracts | one store per tenant and component, each with the component's own model |
| Runs in | `kernel-authorization` | `system-authorization` |
| Asked by | the enforcement points: bouncer, director, usher, custodian, registrar, the operator | the arbiter, and nothing else |
| Written by | the operator's projection and the membership listener | the arbiter, for the component that holds the store |
| Its data | rebuilt from git and the Cluster claim; nothing to back up | tenant data: exported, restored and purged with the tenant |
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
start without them; none looks a store up by its name. A second store called
`gentian`, however it came to exist, is never asked. The operator looks a
store up by name once, to adopt the store of a cluster that made it before
ids were recorded; a recorded store that is gone is replaced by a new one.

## 2. The authorization service

### 2.1 What a profile declares

```yaml
requires:
  services:
    authorization:
      model: |
        model
          schema 1.1
        type user
        type group
          relations
            define member: [user]
        type company
          relations
            define admin: [user, group#member]
            define member: [user] or admin
      groups: mirror               # optional, §2.5
      tuples:                      # optional, §2.6
        - {user: "group:acme/staff#member", relation: member, object: "company:acme"}
```

`model` is the OpenFGA DSL. The operator compiles it when the profile is
admitted and refuses a profile whose model does not compile, so a broken
model is a failed install and never a store without one. Every trust tier
may declare it: a store holds only the declaring component's own relations,
and nothing in it is read by the platform.

A component that declares it gets the Secret `authorization-<component>` in
its tenant's namespace:

| Key | Value |
|---|---|
| `AUTHORIZATION_URL` | the arbiter, `http://arbiter.system-authorization.svc:8080` |
| `AUTHORIZATION_KEY` | the component's key, made once and kept |

and a NetworkPolicy path from its pods to the arbiter's pods on the
arbiter's port and nowhere else in `system-authorization`: the app instance
listens on the same port number.

### 2.2 The store

A store belongs to one tenant and one component and is named
`<tenant>/<component>`. The arbiter creates it on first sight and records its
id; a store is never looked up by name after that. A model that changes in a
new profile build is written as a new model of the same store, and the
tuples stay: OpenFGA models are immutable and versioned, and the arbiter
always decides against the latest one the operator handed it.

People are named as the platform names them, `user:<realm>/<subject>`, so a
customer of one realm and a person of another can never be one user, and an
app's tuples and the platform's agree on who someone is. The arbiter does not
enforce the convention; the model is the app's.

### 2.3 The arbiter

The arbiter is a kernel service in `system-authorization` and the only holder
of the app instance's credential. It knows each store by the hash of the key
that opens it, from a table the operator writes: tenant, component, the
key's SHA-256 and the compiled model. It keeps the store and model ids it
created in a ConfigMap of its own namespace.

Its API is OpenFGA's, with the store and the model taken from the key:

| Route | Does |
|---|---|
| `POST /v1/check` | `{user, relation, object, contextualTuples?}` → `{allowed}` |
| `POST /v1/batch-check` | up to 50 checks in one call |
| `POST /v1/list-objects` | `{user, relation, type}` → `{objects}` |
| `POST /v1/list-users` | `{object, relation, userFilters}` → `{users}` |
| `POST /v1/read` | tuples matching a partial tuple, paginated |
| `POST /v1/write` | `{writes, deletes}`, at most 100 tuples a call |
| `GET /v1/model` | the model in force, its id and DSL |

Every call carries `Authorization: Bearer <AUTHORIZATION_KEY>`. A key the
table does not name is `401`; nothing a caller sends can name another store,
and a field the API does not have is `400`. Each key has a rate limit (100
calls a second, burst 200), so no app can slow another's decisions. A key
may not write or delete a tuple whose user or object is of type `group`
(`403`): who is in a group is the realm's to say (§2.5).

The shapes, in JSON, every tuple `{user, relation, object}`:

| Route | Request | Response |
|---|---|---|
| check | `{user, relation, object, contextualTuples?}` | `{allowed}` |
| batch-check | `{checks: [check…]}` | `{results: [{allowed, error?}…]}`, in order |
| list-objects | `{user, relation, type, contextualTuples?}` | `{objects: ["type:id"…]}` |
| list-users | `{object, relation, userFilters: [{type, relation?}], contextualTuples?}` | `{users: ["user:x", "group:y#member", "user:*"…]}` |
| read | `{tuple?: {user?, relation?, object?}, pageSize?, continuationToken?}` | `{tuples, continuationToken?}` |
| write | `{writes?, deletes?}` | `{}` |
| model | — | `{id, dsl, model}` |

A refusal is `{code, message}`. The arbiter runs once, from the operator's
image with its own command; the operator deploys it, so that the arbiter is
always the build that matches the table it reads. The instance's preshared
key and its database password are kernel credentials in the vault
(`gentian-os/kernel/authz/openfga-apps`, `gentian-os/kernel/database/openfga-apps`),
seeded once by the operator and delivered by ExternalSecrets to the
instance, the role Job and the arbiter; no other workload is handed the key.

### 2.4 The platform's path

What the platform writes into stores and reads out of them, no app's key
can: the realm's memberships (§2.5), a store's export and restore, a purge.
The operator calls `/v1/platform/...` on the arbiter with a token of its
ServiceAccount projected for the audience `gentian-arbiter`, and the arbiter
asks the API server whose it is (TokenReview), as the operator's own
listener admits the director. Only the operator's ServiceAccount is
admitted: a component's key is no token the API server vouches for, and a
component's own token for that audience names the component's account.

### 2.5 The realm's groups

`groups: mirror` has the platform keep the tenant realm's group memberships
in the store: every top-level group of the realm, not only the platform's
vocabulary, as `group:<realm>/<group name>#member` of
`user:<realm>/<subject>`, ':' in a name written as '/'. The tenant's own
members group, `gentian:tenant:<t>:members`, is
`group:<realm>/gentian/tenant/<t>/members`. The model must define a type
`group` whose relation `member` users are written with.

The memberships come from the signed state events the membership listener
receives for the kernel's store, after the kernel's projection, which is
unchanged: a person's whole set of groups, compared with the store and
written; a deleted group loses its members. A realm reaches the stores of
the components of its one tenant and no other; a realm no tenant has, or
several share, reaches none. A nested group is not mirrored, as the kernel's
store holds none. An event the stores did not take is refused, and the
listener sends it again.

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
| a tenant export | every tuple and the model, as the artefact `authorization` of the app (bundle format 5), read through the platform's path and encrypted by the operator itself |
| a tenant restore | the tuples written back into a fresh store, once |
| a purge | the store deleted, through the platform's path |
| the tenant deleted with its data | every store of the tenant deleted |

## 3. System services on demand

The system tier runs the engines something declares. A component's
`requires.services` names what it needs; the operator keeps, per cluster, the
set of engines that are wanted, and the Cluster composition composes the
engine for each entry of that set.

| Engine | Namespace | Wanted when |
|---|---|---|
| PostgreSQL | `system-postgresql` | always: kernel UIs of every tenant use it |
| MariaDB | `system-mariadb` (always there) | a component declares `database.engine: mariadb` |
| Redis | `system-cache` (always there) | a component declares a cache |
| SeaweedFS | `system-s3` | always: every backup, bundle and export is written to it |
| App authorization | `system-authorization` | a component declares `authorization` |
| TURN | `system-turn-dmz` | a component declares `turn` ([networking.md §9](networking.md)) |
| Mail, LLM | `system-mail`, `system-llm` | the claim says so, as now |

The set is the ConfigMap `system-engines` in `kernel-control`, one key per
engine, `wanted` or `purge`; the composition reads it through
function-extra-resources, and the claim stays what git says. What is on
demand is the engine's Release. The namespaces of MariaDB and Redis stay:
the 08-data-plane ApplicationSet syncs each engine's values, credential and
NetworkPolicy into its namespace, and an Argo CD Application whose
namespace is missing fails and is not retried against the same commit.
`system-authorization` holds nothing Argo CD syncs and comes with its
engine.

An engine is added the first time a component declares it: the component
waits, with the condition `SystemEngineNotReady` naming the engine, until
the engine's Releases are ready (and, for authorization, the arbiter). An
engine is never taken away because the last component that needed it went:
its data may be all that is left of a removed app. The composition keeps
every engine it has composed whatever the set says, unless the set says
`purge`. A person asks for a purge by writing `purge.<engine>` into the set;
the operator refuses while any component declares the engine or a tenant's
record of provisioned stores names data on it, and says so in an Event;
otherwise it marks the engine `purge` and removes its volumes once its
Release is gone.

The claim can still say an engine is always there
(`systemServices.always`), for a cluster that wants it ready before the
first install.

## 4. Who decides what

| Decision | By |
|---|---|
| that a component has a store | its profile |
| the model of the store | its profile, compiled by the operator |
| the tuples | the component, through the arbiter; group memberships the realm, starting tuples the profile |
| which engines run | the profiles installed, and the claim |
| that an engine is removed | a person, by purge |

## 5. Order of work

1. The kernel store pinned by id (§1.1).
2. Engines on demand (§3), MariaDB first, then cache and object storage.
3. `requires.services.authorization`: the type, the CRD, compiling the model
   at admission, the Secret and the NetworkPolicy (§2.1).
4. The app instance and the arbiter in `system-authorization`, composed on
   demand (§1, §2.3).
5. Export, restore and purge of a store (§2.7), through the platform's path
   (§2.4).
6. The platform's path to the arbiter (§2.4).
7. Mirrored groups (§2.5), and the refusal of group tuples from app keys.
8. Starting tuples (§2.6).

## 6. Roadmap

- Relations across stores: an app model that refers to platform relations
  such as `tenant#admin`, answered by the arbiter from the kernel instance as
  contextual tuples it vouches for.
- A store per tenant shared by several components of one vendor.
- Metrics per store: tuples, calls and refusals, for the cluster
  administrator.

## 7. Open

- Whether the rate limit is a property of the profile's trust tier.
