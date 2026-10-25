# App authorization and system services on demand

How an app gets an authorization store of its own, and how the system tier
runs only the engines that something declares. Authorization becomes a
service a profile declares, like a database: the platform makes the app a
store with the app's own model, gives the app a key good for that store and
nothing else, and keeps the store with the tenant's data. The engines of the
system tier are composed when the first component declares one and stay
until a person purges them. Decided 2026-10-25. §1 to §4 is the design, §5
the order of work, §6 what stays on the roadmap, §7 what is open.

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
the ConfigMap `authorization-store` in `kernel-control`. Every enforcement
point reads the ids from it and refuses to start without them; none looks a
store up by its name. A second store called `gentian`, however it came to
exist, is never asked.

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
        type company
          relations
            define admin: [user]
            define member: [user] or admin
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

and a NetworkPolicy path from its pods to the arbiter's port and nowhere
else in `system-authorization`.

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
table does not name is `401`; nothing a caller sends can name another store.
Each key has a rate limit (100 calls a second, burst 200), so no app can slow
another's decisions.

### 2.4 Lifecycle

| Event | The store |
|---|---|
| a component is installed in a tenant | created on first sight, with the profile's model |
| a new profile build | a new model, tuples kept |
| the component is uninstalled | kept, as every store of a removed app is kept, until the tenant's retained data is purged |
| a tenant export | every tuple, as the artefact `authorization` of the app |
| a tenant restore | the tuples written back into a fresh store |
| a purge | the store deleted |

## 3. System services on demand

The system tier runs the engines something declares. A component's
`requires.services` names what it needs; the operator keeps, per cluster, the
set of engines that are wanted, and the Cluster composition composes the
namespace and the engine for each entry of that set.

| Engine | Namespace | Wanted when |
|---|---|---|
| PostgreSQL | `system-postgresql` | always: kernel UIs of every tenant use it |
| MariaDB | `system-mariadb` | a component declares `database.engine: mariadb` |
| Redis | `system-cache` | a component declares a cache |
| SeaweedFS | `system-s3` | a component declares object storage |
| App authorization | `system-authorization` | a component declares `authorization` |
| Mail, LLM | `system-mail`, `system-llm` | the claim says so, as now |

The operator writes the set to the cluster-scoped object it owns, the
composition reads it, and the claim stays what git says. An engine is added
the first time a component declares it: the component waits, with a
condition naming the engine, until the engine is ready. An engine is never
taken away because the last component that needed it went: its data may be
all that is left of a removed app. It goes when a person purges it, and the
purge refuses while any component still declares it.

The claim can still say `always` for an engine, for a cluster that wants it
ready before the first install.

## 4. Who decides what

| Decision | By |
|---|---|
| that a component has a store | its profile |
| the model of the store | its profile, compiled by the operator |
| the tuples | the component, through the arbiter |
| which engines run | the profiles installed, and the claim |
| that an engine is removed | a person, by purge |

## 5. Order of work

1. The kernel store pinned by id (§1.1).
2. Engines on demand (§3), MariaDB first, then cache and object storage.
3. `requires.services.authorization`: the type, the CRD, compiling the model
   at admission, the Secret and the NetworkPolicy (§2.1).
4. The app instance and the arbiter in `system-authorization`, composed on
   demand (§1, §2.3).
5. Export, restore and purge of a store (§2.4).

## 6. Roadmap

- Relations across stores: an app model that refers to platform relations
  such as `tenant#admin`, answered by the arbiter from the kernel instance as
  contextual tuples it vouches for.
- A store per tenant shared by several components of one vendor.
- Metrics per store: tuples, calls and refusals, for the cluster
  administrator.

## 7. Open

- Whether the rate limit is a property of the profile's trust tier.
