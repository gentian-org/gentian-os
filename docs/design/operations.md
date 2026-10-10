# Operations: Backup, DR, Observability, Image Updates

**Companion to:** [architecture.md](../architecture.md),
[deployment.md](../deployment.md)

---

## 1. Backup Strategy

Backup is **per tenant**. A `TenantExport` captures one tenant into one
encrypted bundle in object storage: each app's databases (PostgreSQL and
MariaDB dumps), buckets and volume claims, the tenant's realm, its mailboxes
where the cluster runs its own mail server, and the rights that follow from
nothing else. A `BackupPolicy` says where bundles go, when they are taken and
how long they are kept; the operator turns it into a `TenantExportSchedule`
per tenant. What a bundle carries per kind is in
[data-lifecycle.md](data-lifecycle.md); the reference is §9.

Nothing else is backed up by the platform. There is no continuous archiving of
a database (no WAL archive, no point-in-time recovery), no replication of the
object store, no scheduled vault snapshot and no Velero; a cluster that wants
any of these adds it itself. The kernel's own state is rebuilt, not restored
(§4).

## 2. Tenant-Scoped Restore

Each tenant has its own databases, buckets, realm and namespace, so one tenant
is restored without touching another. A `TenantRestore` in the tenant's
namespace does it from a bundle; `scripts/recovery.sh` and
[recovery-playbook.md](../recovery-playbook.md) are the procedure. Export can
be started from the administration console; a restore is a cluster
administrator's act. Restore is **data-only** — the tenant's shape is re-composed
from its claim, never restored — and quiesces one app at a time, since the
consistency boundary that matters is an app's database plus its bucket plus
its PVC, not the tenant as a whole. What a bundle carries per kind and what no
restore brings back are in [data-lifecycle.md](data-lifecycle.md); the rule a
restore decides by is in §9.4.

## 3. Tenant Migration Between Clusters

Export on the source, import on the target, update DNS:
`kubectl gentian tenants import` declares the tenant from the bundle's
manifest, waits for the operator to provision it, and restores the data into
it ([commands.md](../commands.md) §12a). Credentials are not carried; the
target provisions its own.

### 3.1 Node Flavour Migration

Replacing a cluster's worker nodes with a different flavour — sizing from usable
rather than purchased memory, the drain order the kernel singletons require, and the
CNPG and RWO cases that need manual steps — is covered in
[node-pool-migration.md](../node-pool-migration.md).

## 4. Disaster Recovery

For full-cluster DR ([recovery-playbook.md](../recovery-playbook.md) §1):

1. `./install.sh --recover <kit>` on a fresh cluster, with the same
   deployments repository and cluster id. The recovery kit carries what git
   cannot hold: the master password and salt, the unseal material, the
   repository and registry credentials, the backup key.
2. The install runs as normal: Argo CD, Crossplane and the `Cluster` claim
   bring the kernel back from git.
3. Each tenant is imported from its newest bundle (§3).

With `secretMode: derived` (the default) the kernel's service credentials and
each app's are derived from the master password and salt, so they come back
with their original values; with `secretMode: random` they are generated anew
(`scripts/lib/bootstrap.sh`, `internal/kernel/secrets`), all but an app's own
secrets, which are derived in both modes so that the data a bundle brings
back stays readable ([security.md §6](security.md)). People's and administrators' passwords are never
derived: a realm export carries none, and every member resets theirs after a
restore.

## 5. Observability via the K8s API

Crossplane's MR status model gives uniform observability:

```bash
# Tenant health at a glance
kubectl get tenants
NAME   STATUS   APPS   READY   ADMIN               AGE
demo   Ready    2      2       admin@example.com   30d

# What is installed for a tenant (Component → App claim → helm Release)
kubectl get components,apps -n tenant-demo
kubectl get releases.helm.crossplane.io -n tenant-demo

# The tenant's composite (namespace, quota, policy)
kubectl get xtenant demo

# Integration contract health
kubectl get integrationbindings -n tenant-demo

# Kernel / GitOps (not per-tenant app charts)
kubectl get applications -n kernel-gitops
```

A tenant's apps are observed through the **`Component`**, its **`App` claim**
and the **helm `Release`** in `tenant-{name}`. Argo CD Applications cover the
kernel's services, the claims and the deployments repository's tenant and
catalogue directories, not each tenant app install.

## 6. Metrics

The operator exposes Prometheus metrics (`internal/controller/metrics.go`); the
chart ships a `ServiceMonitor` for them, off unless `metrics.serviceMonitor`
is enabled. The platform installs no Prometheus.

| Metric | Description |
|---|---|
| `gentianos_tenants_total` | Tenants managed |
| `gentianos_tenant_apps_total` | Requested apps per tenant |
| `gentianos_provisioning_duration_seconds` | Duration of a tenant provisioning pass |
| `gentianos_reconcile_errors_total` | Reconcile errors by controller |
| `gentianos_credentials_age_seconds` | Age of last-provisioned credentials per tenant |
| `gentianos_integration_bindings_status` | Binding state by contract |
| `gentianos_externalsecrets_sync_status` | ExternalSecret sync state per tenant |
| `gentianos_tenant_export_total`, `gentianos_tenant_export_quiesce_duration_seconds` | Exports that ended, and how long an app was paused for one |

Crossplane, External Secrets and Argo CD expose their own upstream metrics.

### 6.1 Usage sampling

The operator runs a leader-elected worker that records each tenant's enforced
ceiling and what is committed under it into that tenant's own `{tenant}_shell`
database, every `usage.sampler.interval` (15 minutes by default). It is what the
administration console's Resources screen and `kubectl gentian resources report` read — see
[resource-plans.md](resource-plans.md).

```bash
# Is it running, and is any tenant being skipped?
kubectl logs -n kernel-control deployment/gentian-os | grep usage-sampler
```

A tenant without a `desktop-database` Secret in its namespace is skipped
and logged; the others are unaffected. One tenant's database being unreachable
never stops the pass, because a single broken tenant must not become a
cluster-wide gap in the billing record.

### 6.2 Licence report

Gentian OS is free under a usage limit that is enforced legally, not
technically. So a cluster says, in the open, what it runs, and nothing on the
cluster is blocked by the answer: the operator reads the HTTP status of the
reply and nothing else.

**When.** A leader-elected worker in the operator sends one report a few
minutes after the operator starts and one every 24 hours after that. A report
that does not arrive is retried with a doubling wait, from one minute up to
one hour. No failure stops anything else.

**Where.** An HTTP `POST` to one `https` address fixed at install time
(`licenceReport.url` in the operator chart). A redirect is not followed.

**What is sent.**

```json
{"version": 1, "sequence": 12, "sentAt": "2026-03-04T05:06:07Z",
 "cluster": {"id": "<cluster id>", "url": "https://<kernel domain>"},
 "tenants": [{"url": "https://<the tenant's host>", "users": 40,
              "apps": [{"coordinate": "<catalogue>/<app>",
                        "digest": "sha256:…", "users": 25,
                        "addons": [{"coordinate": "<catalogue>/<addon>",
                                    "digest": "sha256:…", "users": 7}]}]}],
 "publicKey": "<base64 Ed25519 public key>"}
```

- `tenants[].users` is the number of enabled accounts in the tenant's realm.
- `apps` lists only the apps installed through the App Store: the entries of
  `Tenant.spec.apps` that carry a `digest`. `users` is the number of people
  entitled to the app, the members of its group.
- `addons` lists, under each listed app, the add-ons of it that are pinned to
  a build: the entries of that app's `addonPins` whose add-on is in its
  `addons`. `users` is the number of people entitled to the add-on, the
  members of its own group, counted as an app's are. The key is always sent,
  `[]` for an app with none. An add-on activated with no pin is not listed,
  as an app with no `digest` is not. The director pins an add-on only inside
  an app that is itself pinned, so a pinned add-on always has a listed app to
  appear under. A manifest written before that rule may still hold a pinned
  add-on inside an app with no `digest`; the report still omits it, because
  the app it would be listed under is not in the report and no entry is made
  up for it.
- `coordinate` is `null` for an entry whose install did not record its
  catalogue (`spec.apps[].catalogue`, `addonPins[].catalogue`), and a count
  that could not be had is `null` rather than a smaller number.
- Apps are sorted by `digest`, and an app's add-ons by `digest` and then by
  name, so a cluster whose state has not changed says the same thing again.

`addons` was added without raising `version`: the format does not forbid
members a receiver does not know, and a receiver that reads only the members
it knows reads a report with `addons` exactly as it read one without.

**What is not sent.** No names, e-mail addresses, user ids, group names, tenant
display names or administrators' addresses, and nothing about apps or add-ons
that were not installed at a stated build.

**Signing.** Each cluster has an Ed25519 key pair. The installer writes 32
random bytes to the vault (`gentian-os/kernel/licence-report`, `signing_seed`),
an ExternalSecret hands them to the operator, and the operator derives the pair.
The exact request body is signed: `X-Gentian-Signature: ed25519=<base64>`, with
`X-Gentian-Key-Id` naming the key (the first 16 hex characters of the SHA-256 of
the public key, which is in the body). The seed is never sent or logged. A
cluster without the seed sends nothing and records why.

**Seeing it.** The last report exactly as sent, and what became of the last
attempt, are in the ConfigMap `gentian-licence-report` in the control
namespace. The usher serves the same to whoever holds `can_audit` on the
cluster:

```
GET /v1/clusters/{cluster}/licence-report
{"enabled": true, "url": "https://…",
 "attempt": {"at": "…", "outcome": "accepted", "httpStatus": 202, "nextAt": "…"},
 "report":  {"sequence": 12, "body": "<the request body, byte for byte>",
             "signature": "ed25519=…", "keyId": "…"}}
```

`outcome` is `accepted`, `failed` (sent, and refused or not delivered; see
`reason`, `httpStatus`, `error`), `not-sent` (`reason` is
`signing-key-absent`, `signing-key-invalid` or `inventory-unavailable`) or
`sending`. A cluster that does not report answers `{"enabled": false}`.

**Turning it off.** `./install.sh --no-licence-report`, or
`GENTIAN_NO_LICENCE_REPORT=1` in `install.env`; a later run that says neither
leaves it off. The operator chart's own default is off with no address, so a
cluster installed without the installer reports nowhere. Off, nothing is sent,
ever, and no signing key is created.

**What turning it off costs.** The cluster has no App Store app. A store
serves a tenant only when the reports it has received list that tenant's
address ([store-contract.md](store-contract.md) §6.2), so the usher's tiles
answer (`GET /v1/tenants/{tenant}/tiles`) carries
`"appStore": {"available": false, "reason": "licence-report-disabled"}`, the
operator places the App Store app on no tenant and removes it where it was,
and no App Store tile is shown; the installer also leaves
`catalogue.storeUrl` out of a new Cluster claim. (A cluster that reports and
names no store has none either, and answers `"reason": "no-store-configured"`; one whose claim names its own App Store app's address answers `"reason": "store-address-is-own-host"`.) Everything else runs as before, and apps are
installed by command.

**For whoever receives it.** The request above is also written down as a
machine-readable format,
[licence-report.openapi.yaml](../plans/artefacts/licence-report.openapi.yaml).
It describes what the operator sends and changes nothing about it.

## 7. Image Updates via ArgoCD Image Updater

### 7.1 Philosophy

The kernel is a **singular shared service**: one kernel version per
cluster, all tenants on the same version. This is intentional — kernel
upgrades are platform-wide and atomic.

An app is not upgraded with the kernel. A tenant's install names the build of
the profile it runs (`spec.apps[].digest`); the `ComponentProfile` is
committed to the deployments repository at that digest and delivered by
Argo CD ([custom-catalogues.md](../custom-catalogues.md)).

### 7.2 Per-Environment Update Policies

Cluster-to-stage mapping and promotion workflows (simplified dev→prod and
fortified dev→staging→prod) are documented in
[deployment.md](../deployment.md).

Nothing rolls the kernel forward on its own. The `gentian-os` Application
(in `kernel-gitops`) follows the ref the cluster tracks, and the operator's
image is pinned to the build of the installer's checkout: a branch resolves
to that commit's image, a release tag to its version.
`./install.sh --only B-01` advances the pin.

Argo CD Image Updater is installed (step A-06), but the platform writes no
`ImageUpdater` resource, so it updates nothing. A cluster that wants an image
to roll on its own adds one in `kernel-gitops`, naming the Application and the
tags it may take.

### 7.3 Tenant Impact

Tenants are not parameterised by kernel version — when the kernel
is updated, all tenants use the new kernel. This is the
intended behaviour: cluster admins manage kernel versions, tenant
admins do not.

### 7.4 Upgrades and definitions

A cluster holds two kinds of resource definition for this API group, and they
reach it by different roads:

| Definition | In the repository | Reaches a cluster by |
|---|---|---|
| The operator's own CRDs (`Tenant`, `Component`, `ComponentProfile`, …) | `charts/gentian-os/crds/` | The Argo CD sync of the `gentian-os` Application — the same sync that rolls the operator and the director, CRDs first. |
| The kinds Crossplane generates (`App`/`XApp`, `XTenant`, `Cluster`, `Repository`, …) and the Compositions | `crossplane/xrds/`, `crossplane/compositions/` | Installer step B-06 only, from the checkout on the install host: `./install.sh --only B-06`. |

An API server prunes what it does not know. When a definition on the cluster is
older than the software — Argo CD following a branch that moved back under a
pinned image, a plain `helm upgrade` (which never touches `crds/`), a new image
rolled without re-running B-06 — a field the software writes is dropped on
write and nothing fails. So both binaries carry the definitions they were built
with (`internal/schemacheck`, copied by `make manifests`), and the operator
compares them with what the cluster serves: at start, then every five minutes,
and every thirty seconds while something is wrong. For a Crossplane kind it
reads the CRD Crossplane generated, claim and composite, because that is what
the API server prunes against. It reads each definition by name and may read
no other.

**What a person sees** when a definition is older than the software:

- The operator's log names the kind, the fields that would be dropped and the
  remedy, and a Warning Event `DefinitionsOutdated` is recorded on the
  operator's pod.
- Every object a held reconciler would have reconciled carries the condition
  `DefinitionsCurrent=False`, reason `DefinitionsOutdated`, with the same
  message. An outdated chart CRD holds every reconciler of the operator's own
  kinds; an outdated Crossplane kind holds the reconciler that writes it —
  `XTenant` the tenant's, `App` the component's. The operator keeps running.
- The director answers `503` to a write that sets one of those fields, naming
  the definition and the remedy, and commits nothing. Reads, removals and
  writes that do not touch the field are served. If the director cannot learn
  the state at all — the operator is unreachable, has not checked yet, or is
  mid-rollout on another build — it refuses every write that sets a field and
  says the state could not be confirmed.
- `GET /v1/definitions` on the operator's listener returns the whole finding:
  each kind, its state, the missing fields and the remedy. The director's and
  the usher's identities are admitted to it.

**What to do.** For a chart CRD, sync the `gentian-os` Application at the
revision the image was built from. For a Crossplane kind, re-run B-06 from a
current checkout. Nothing has to be restarted: the next check lets the held
reconcilers go, the condition comes off, and the director serves the writes it
refused.

A definition that is absent rather than old — Crossplane generates a CRD a
little after its XRD is applied — is reported as `DefinitionsNotReady` and
waited for; it holds only the reconciler that writes that kind. On a fresh
install this does not arise in the normal order: B-06 applies the XRDs and
waits for them to be established, and the operator arrives at D-01.

Delivering the XRDs and Compositions through Argo CD as well, so that one sync
moved every definition together, is a possible later step. It is not built.

### 7.5 Upgrading Envoy Gateway on an existing cluster

A fresh install needs nothing from this section. A cluster that already runs
an earlier Envoy Gateway is brought forward by re-running step A-05
(`./install.sh --only A-05`) from a checkout of the new release, and then
letting Argo CD roll the operator and the bouncer. In that order:

1. **Kubernetes 1.33 or newer.** Pre-flight checks it.
2. **CRDs first.** A-05 applies the pinned chart's CRDs — Envoy Gateway's own
   and the Gateway API's — server-side before it upgrades the chart. Helm
   never upgrades CRDs, and a controller on older definitions has the fields
   it does not know dropped from every policy. The chart then installs an
   admission policy that refuses Gateway API CRDs older than v1.5, so going
   back means deleting that policy first.
3. **The proxies are rebuilt.** The new controller replaces the Envoy pods
   (new image, changed pod template). With two replicas this is a rolling
   replacement; connections are cut once.
4. **The order at the front door.** A-05 re-applies the edge `EnvoyProxy`
   with `filterOrder`. Until the new operator has rewritten the session
   policies and the new bouncer is running, sign-in fails closed: an old
   bouncer behind the new order, or a new one in front of old policies,
   refuses.
5. **Everyone signs in again.** Expect session cookies written by the
   earlier version not to be accepted: they were not encrypted, and the way
   the cookies are sealed changed in between. Keycloak sessions survive, so
   this is one silent redirect per host, not a password prompt.
6. **The zone clients' post-logout addresses** change to each host's front
   page; `./install.sh --only B-06` delivers the Composition.

## 8. Safety Guards

- **The vault's Argo CD Applications** (`openbao`, `openbao-transit`) deploy
  with `prune: false` and `finalizers: []`, so Argo CD never deletes them when
  their manifests are temporarily missing from git. The Applications of the
  claims and of the catalogue directory also sync without pruning.
- **OpenBao paths** managed by Crossplane use
  `managementPolicies: [Observe, Create]` — never overwrite live
  credentials. See [security.md](security.md).
- **Backups are not verified by the platform.** Nothing runs a restore or
  checks a bundle on a schedule, and nothing alerts. `scripts/recovery.sh
  inspect` reads a bundle and its key without writing anything, and
  [commands.md](../commands.md) §15 is the restore drill. A schedule records
  its last success so that it can be watched.

## 9. Data Lifecycle: Reference

What creating, backing up, restoring, importing, uninstalling, purging,
retiring and deleting do with each kind of thing a tenant and an app own is in
**[data-lifecycle.md](data-lifecycle.md)**: one table, the order, what a
bundle does not hold, how each act fails, and the known gaps. Read that first.
This section is the reference under it: detail that page leaves out, and
nothing that page already says.

The inventory and the order are in [`internal/backup`](../../internal/backup/):
`inventory.go` names each thing (`InventoryOf`, and `AppVolumes` for whose a
volume claim is), `teardown.go` lists the kinds in the order provisioning
makes them (`AppKinds`), says what every act does with each and how each is
found once its app is uninstalled, builds the one Job that destroys each
store, and holds the one question that says which databases are an app's.
`record.go` is the record provisioning keeps of what it made. Teardown is
provisioning's order reversed (`TeardownOrder`); nothing else defines an
order. A test fails when a kind is added without saying what export and each
teardown do with it, when something uninstalling keeps does not say how it is
found afterwards, or when a store a profile can declare is left out of the
record.

### 9.1 Per kind: what makes it, and the fine print

In provisioning order.

| Kind | Made by | Detail |
| --- | --- | --- |
| Provisioning records | every provisioning Job, and the operator's labelled Secrets, from the first step on | Jobs, their pods and labelled Secrets; found by the tenant's and the app's labels. A teardown's own Jobs are records too, which is why they go last. |
| Stored credentials | the tenant reconciler's seeder before each store; the app Composition for generated secrets | Vault paths `…/apps/<app>` and `…/apps/<app>-<extension>`. A tenant's deletion removes them with the tenant's whole vault subtree. |
| Access group and memberships | the tenant's identity Job, from `Tenant.spec.apps` | Carried inside the realm export; a purge destroys it in the tenant's own realm. |
| Sign-in scope | the tenant's identity Job, from the OIDC pack the app's profile names | The client scope with its mappers. Kept at an uninstall: taking it away there would need the uninstall to talk to the identity provider, and an uninstall is a commit nobody waits on. A purge destroys it unless an installed app names the same scope or it is one of Keycloak's own. |
| Database and role | a role Job and a CloudNativePG Database, or a MariaDB setup Job | A bundle holds a dump of the provisioned database and of every other database that is the app's (§9.3). A restore replaces the provisioned one; each other one is created if missing and replaced, under the provisioned name of the tenant restored into when that is not the tenant the bundle was taken of (`<source>_x` becomes `<target>_x`; on PostgreSQL any other name `y` becomes `<target>_y`); one that is the app's now and that the bundle does not hold is left, and named. A PostgreSQL database owned by another role is never replaced: the restore fails first. |
| Object storage | the bucket Job | A restore makes the bucket, its user and its policy with the code install uses, then writes the objects, removing objects the bundle does not hold. |
| Cache user | the ACL Job | A restored cache is stale, so none is carried. |
| Model key | the tenant reconciler, at the model gateway, where the cluster serves models | A credential, registered again at install. A tenant's deletion removes the keys, then the tenant's team. |
| Sign-in client | the app Composition, or the identity Job | With its client role, its default-scope assignments and the group's mapping to the role. Keycloak removes what is part of the client with it. |
| Workloads | the Helm release the app Composition or the component reconciler writes | The manifest records the chart version, the digest and the releases; a restore checks the installed build against them (§9.4). |
| Files | the app's chart, as volume claims of its release | An archive per claim that is the app's (`AppVolumes`), unpacked onto the claim of the same name. At a tenant's deletion they go with the namespace. |
| Materialised profile | committed by the director under `clusters/<cluster>/catalogue/` at the digest the install named, applied by Argo CD | Kept by every act: a purge of the app reads it to know what the app owns. |
| Bundle companions | applied with the profile, from the same file | The Composition, OIDC pack catalog, ConfigMaps and customization records a profile's bundle brings. Kept by every act. |

So the teardown order is: files, workloads, sign-in client, model key, cache,
object storage, database, sign-in scope, access group, stored credentials,
provisioning records. A purge of an app runs the steps of that list that
destroy (the workloads and the client went with the uninstall). A tenant's
deletion removes the workloads first — its Components, and with each the App
claim and the release — waits for them, then runs the store steps in the same
order for every app the tenant ever had, with the same Jobs and scripts, then
the realm; then it removes the namespace and waits until the API server no
longer has it, removes the vault subtree and, last, the records.

The last two rows are not a tenant's. A profile and its companions belong to
the cluster, are shared by every tenant that installs the app, and no act on
a tenant or an app removes them; neither does a newer build that stops
bringing a companion. They leave by one deliberate act of the cluster's
administrator, one object at a time: the operator lists what no bundle owns
any more and the profiles no tenant uses or retains data for, and removes an
entry of that list on request
([custom-catalogues.md](../custom-catalogues.md) §6).

### 9.2 What a tenant has that is no app's

Several of these lie outside what a deletion sweeps by default — outside the
tenant's namespace, realm and vault subtree, and without its label. The
inventory lists them (`TenantOwned`) so that each says what removes it. What
each act does with them is in [data-lifecycle.md](data-lifecycle.md) §2; the
fine print:

- **The namespace.** With `Retain` the Components and the operator's quota,
  limits and network policy are removed from it. With `Delete` the deletion
  waits until the namespace is gone.
- **The realm.** Never deleted or disabled when it is the kernel realm, which
  a tenant only adopts.
- **The client a realm signed in to the kernel realm as** (`broker-<realm>`),
  and the mapper on it. It is no longer made ([iam.md §1.7](iam.md)); one
  left from before is removed from the kernel realm by the Job that deletes
  the realm.
- **The vault subtree.** An operator with no vault that was not told to run
  without one (`GENTIAN_WITHOUT_VAULT=true`) fails the deletion here.
- **Mail.** The DNS records are `DNSEndpoint mail-<tenant>`; a deletion also
  removes the DKIM key and the SMTP credentials.
- **The edge.** A route that cannot be removed fails the deletion.

### 9.3 Finding what an uninstalled app left

An uninstalled app has left the tenant's manifest and all its stores are still
there. Provisioning therefore writes down what it makes: one ConfigMap per
tenant in the provisioning namespace (`tenant-<name>-provisioned-stores`), one
entry per app with the database engine and names, the bucket, the cache user
and the model key, written before the Jobs that make them are handed over and
never reduced by provisioning. A purge takes a kind off the record once it has
destroyed it. The deletion of a tenant, a purge and the read of what
uninstalled apps hold (`GET /apps/retained`) read it, and so does an export,
which captures what an uninstalled app left (§9.4); the read answers
`present` or `absent` for a bucket, a cache user and a MariaDB database from
it without running anything, and `unknown` only when the record could not be
read. A PostgreSQL database is also found through the CloudNativePG Database
object; files, credentials and the access group by the claims, vault paths and
group names themselves.

Which databases are an app's is one rule that export, restore and purge
share. The provisioned database is. On PostgreSQL so is every other database
the app's role owns — a role creates databases only when its profile asks
(`allowDynamicDatabaseCreation`), owns what it creates, and the server records
the owner.

On MariaDB a database has no owner and one server holds every tenant's, so the
rule is by name, and the same names are all the app's user may touch. An app's
databases are its provisioned database `<database>` and every database named
`<database>_…` — the provisioned name, an underscore, anything. `demo_crm_reports`
is `demo_crm`'s; `demo_crm2` and `demoXcrm` are not: the grant and the query
escape the underscores, which MariaDB otherwise reads as "any one character",
and compare byte for byte. The user of an app whose profile sets
`allowDynamicDatabaseCreation` is granted all privileges on those names; every
other app's user on its provisioned database alone; none is granted anything
on the server (`*.*`). The rule and the grants are stated once, in
`internal/backup/mariadb.go`.

Hyphens in a tenant's or an app's name become underscores in a database's, so
one app's provisioned database can be named under another's prefix:
`demo_crm_extra` is app `crm-extra` of tenant `demo`, and app `extra` of a
tenant `demo-crm`. Two things keep the rule exact there. A database under an
app's prefix that another account holds rights on is that account's: export,
restore and purge leave it alone and say so. And provisioning refuses, under a
lock on the server, to make the overlap where a grant would span it — an app
whose database another account's rights already reach, and an app that asks to
create databases while another account's database lies under its prefix. The
setup Job fails with the reason and the app is not provisioned.

**Clusters provisioned before this rule.** The setup Job is run again when its
script changes, and a run leaves the user with the grants above and nothing
else: a user that held `ALL PRIVILEGES ON *.* … WITH GRANT OPTION` (every app
with `allowDynamicDatabaseCreation` on MariaDB did), or a grant on its
unescaped database name, has everything revoked and is granted again. Two
things it cannot put right. A database such an app created under a name
outside its prefix is no longer reachable by the app, and is in no export and
no purge: rename it under the prefix or drop it by hand. And an account the
app created for itself while it could (`CREATE USER`, `GRANT`) is not the
platform's to find: compare `mysql.global_priv` with the tenants' apps. Until
every app's Job has run again, an old unescaped grant of one app can make the
Job of another fail with "within the rights of another account"; it passes
once the first has run.

### 9.4 Restore

What a restore puts back is what the bundle says it holds. The operator reads
the bundle's manifest with the restore's own key and makes a plan once, before
anything is changed; the plan is in the restore's status.

1. Only an app the manifest lists is touched. An app the tenant has and the
   bundle does not is left exactly as it is.
2. An app is restored whole or not at all. It is not restored when it is not
   installed; when its ComponentProfile is not on the cluster; when the build
   installed is older than the one that wrote the data, or cannot be compared
   with it (unless `spec.skipVersionCheck`); when the bundle holds a store of a
   kind or engine the installed app does not have; or when it holds a volume
   claim that is not one of the app's here. A newer build installed is
   restored: an app upgrades older data when it starts.
3. Every app not restored is named with the reason in `status.notRestored`,
   and the restore ends `Ready` with `status.complete: false` and the reason
   `PartiallyRestored`. Nothing a bundle holds is dropped without saying so.
4. With `spec.apps`, only the apps named are considered and each has to be
   restorable; otherwise the restore is refused before anything is changed.

Each artefact is fetched from the path the manifest gives and loaded into the
store of that kind the installed app has, by the inventory's names — which
differ from the bundle's whenever the tenant's name or prefixes do, as when a
bundle is imported under another name.

**The bundle format.** The manifest (`manifest.json`, encrypted like every
artefact) carries `schemaVersion`. Format 1 named each app and the kinds
captured, nothing else. **Format 2**, written since the restore went by the
manifest, adds per app one `stores` entry per artefact — `kind` (`postgres`,
`postgresOwned`, `mariadb`, `mariadbOwned`, `s3`, `volume`), `name` (what it was captured
from), `path` (where in the bundle), and for a volume the Helm `release` it
recorded — and the app's `digest`, `databaseEngine` and `releases`; the
tenant-wide captures are no longer listed among the apps. Fields were added
and none renamed. A format 1 bundle still restores: the names are derived from
the tenant the manifest records and the volume claims are the ones the apps
have now, and the result says `nameDerivation: derived`. **Format 3** adds
what a format 2 bundle did not hold, and again renames nothing: `retained` on
an app that was uninstalled with its data kept when the bundle was taken, and
on each of its volumes the `claim` it was captured from (size, access modes,
and the labels and annotations that say whose it is), from which the claim is
made again where it is not; `mailboxes` (`kind: mailboxes`, `name` the mail
domain, `path`); and `rights`, the entries of the rights store that follow
from nothing else, as `granted` and `withdrawn` lists of `user`, `relation`,
`object`, with the `cluster` they were read on. A format 2 bundle has none of
them and restores as before. A manifest of a format newer than the platform
reads is refused. A `postgresOwned` artefact is
a tar.gz holding `INDEX`, the database names one per line, and
`<line number from 0>.pgc`, each one's custom-format dump. A `mailboxes`
artefact is a tar.gz holding `INDEX`, the mailboxes one per line by the part
of the address before the @, and `<line number from 0>/`, each one's mail as a
Maildir++ tree written by `doveadm backup`: every folder and message with its
flags, UID and GUID.

**Uninstalled apps.** An app that was uninstalled with its data kept is
captured after the installed ones, by the same units, from the stores the
cluster still holds for it: the ones on the record of what was provisioned,
a PostgreSQL database the cluster keeps a record of, and the volume claims
that are the app's. Nothing is paused. A restore puts the data back into the
stores the tenant still holds for the app and installs nothing; where the
tenant holds none of it, it was purged, and the app is named in
`status.notRestored`. Only a restore with `spec.intoNewTenant`, which an
import sets, makes the stores: the record first, then the database with its
role and vault record (or the MariaDB setup Job), the bucket with its user,
and each volume claim from what the bundle recorded, on the cluster's default
storage class.

**Mailboxes.** On a cluster that runs its own mail server
(`mail.serviceMode: system`) a tenant's mailboxes are the directories below
its mail domain's on the server's volume. They are copied by `doveadm
backup`, in the mail server's own image, beside the volume and on the node
the server holds it on: the server's own synchronisation takes the server's
locks, where an archive of a Maildir that is being written to can miss a
message that is moving between `new/` and `cur/`. A restore runs `doveadm
sync` one way, from the bundle into the mailbox: what the bundle holds and the
mailbox lacks is added, and nothing is removed. A mail domain that is the
cluster's own (the user tenant of a single-tenancy cluster) is shared with
the cluster's administrators; its mailboxes are neither copied nor destroyed,
and `status.notIncluded` says so. On a cluster without a mail server there is
no unit and nothing is said.

The archived mailboxes of removed people ([mail.md §5c](mail.md)) are in the
same artefact, under `archived/` with an `INDEX` of their own, copied the
same way from `/var/mail/.archive/<domain>/`. The manifest names them
(`archivedMailboxes`: archive, address, when, by whom, size). A restore
synchronises each into `/var/mail/.archive/<target domain>/<archive>` and
writes a `MailboxRemoval` for it that says it was restored, so that it is
listed and can be deleted; the operator runs no Job for such a record. A
tenant's deletion removes `/var/mail/.archive/<domain>` with the domain's
mailboxes, and then the tenant's records.

**Rights.** The operator reads the tenant's entries in the rights store and
writes into the manifest the ones its projection would not write, and the
defaults the store no longer holds; no Job reads the store. A restore writes
them after the realm is back and after the projection has attached the
tenant. Into a tenant made new (`spec.intoNewTenant`, or a bundle of a tenant
of another name or cluster) the granted ones are not written and are named in
`status.rights.notBrought` and in the notes; the withdrawn ones are withdrawn,
under the new tenant's names.

**What no restore brings back** is the checklist in
[data-lifecycle.md](data-lifecycle.md) §5, and is said on every result
(`status.notes`). The detail behind it:

- **Stored credentials.** A profile's `spec.backup.boundSecrets` are not
  carried either, and an export of such an app says so.
- **Declared state.** App grants are declared in git and come from there;
  integration bindings are derived from the installed apps and their
  profiles; authorization tuples are projections, but for the ones a bundle
  carries (above). On the same cluster none of the three is lost. A tenant
  imported into another cluster has its bindings and tuples made again and
  its app grants to set again.

An uploaded bundle (the import bucket, `gentian-imports`) is removed when a
restore of it has run to its end, restored or failed. A restore refused before
it changed anything leaves it, so the request can be made again; an upload that
is never restored stays until it is removed by hand.

### 9.4a Where each step of a backup and a restore runs

A step runs in the namespace where the credential it works with already is.
No administrator credential of a database or of the identity provider is
copied anywhere.

| Step | Namespace | Reads there |
| --- | --- | --- |
| PostgreSQL dump and load, an app's and the desktop's | `system-postgresql` | `postgres-admin` |
| The desktop's database of the tenant that adopts the kernel realm | `kernel-data` | `kernel-postgres-role-portal-shell`: the database's own role, not an administrator |
| Mailbox copy and load | `system-mail` | the mail server's volume |
| MariaDB dump and load | `system-mariadb` | `mariadb-admin` |
| Realm export and import | the identity namespace (`kernel-authentication`) | `keycloak-admin` |
| Bucket archive and load, the manifest, a bundle's removal | `system-s3` | `minio-admin`, and the bundle's own credential and key |
| Volume archive and load | the tenant's namespace | the claim |

What a step needs besides is the credential the bundle is reached with and
the key it is encrypted or opened with. Those exist beside the object store.
For every other namespace a run has a step in, the operator stages one Secret
holding both (`tx-<tenant>-<run>-run-creds`, `…-rcreds` for a restore) and
removes it when the run ends: completed, failed or deleted. For a bundle on
the platform's own storage the staged credential is the object store's
administrator's, as it always was for volume steps.

The names of a run's Jobs and Secrets carry the tenant's name: those
namespaces are shared, and a run's name is unique in its tenant only. A Job
or Secret found under a run's name that another run made is never used.

The object store admits these pods by their label
(`gentianos.io/component=tenant-export`) from those namespaces
([security.md](security.md) §2.5). A test builds every step with the
reconcilers' own code and fails when a pod reads a Secret that is not in the
namespace it runs in (`TestEveryUnitFindsWhatItsPodReadsWhereItRuns`).

### 9.5 Failing loudly

What a person sees when an act fails is in
[data-lifecycle.md](data-lifecycle.md) §6. Underneath: a destroy script ends
in success only when what it was asked to remove is verifiably gone. A purge
of an app stops at the first step that fails and answers with it
([store-contract.md](store-contract.md) §8). A tenant's deletion is not waited
for by anybody: a cleanup Job that fails, a vault or a model gateway that does
not answer, an edge route or a provisioning Job that cannot be removed, is a
reconcile error; the Tenant stays `Terminating`, and the failed step is run
again on the next pass. The Tenant is gone only when its namespace is.

The scripts of a backup and a restore are held to the same rule by the same
kind of test (`TestNoUnitScriptDiscardsAFailure`): each stops at the first
command that fails and discards no failure. The realm import goes on to the
end, names every person and membership it could not put back, and then
fails. A step whose pod does not start is counted like one that failed, and a
backup or a restore gives up after `exportMaxAttempts` of them, in every
stage, starting the paused app again. A `TenantRestore` holds a finalizer:
deleting one that runs stops its Jobs and starts the app again.

### 9.6 Known limits

The gaps that matter to a person running the platform are in
[data-lifecycle.md](data-lifecycle.md) §7. The technical limits behind them:

- **Cache keys.** The cache is one shared instance and an app's user may touch
  every key, so the keys an app wrote cannot be told from another's. A purge
  and a tenant's deletion remove the user and leave the keys.
- **`spec.isolation.namespace`.** Export, restore, purge and the retained read
  take a tenant's namespace from the Tenant, as provisioning does. The rest of
  the platform does not: an export or a restore finds its tenant by stripping
  `tenant-` from its namespace, the app Composition derives the tenant's name
  the same way, and the director never sets the field. A tenant placed in a
  namespace of another name is not supported end to end; an export or restore
  created for one is refused.
- **MariaDB databases an app creates for itself** are the app's by their name
  (§9.3). `mariadbOwned` was added to format 2 without a new format number: a
  platform from before it refuses an app whose bundle holds one ("an artefact
  of kind … which this platform does not know how to restore") and restores
  nothing of that app. Each database is dumped at one instant of its own; an
  export is consistent across an app's databases only because the app is
  paused. A database whose name has a control character in it fails the
  export. The read of what uninstalled apps hold reports the database kind as
  one — present or absent, from the record — and does not list the databases.
- **The MariaDB client is the server's image.** The Jobs that provision, dump
  and load run the image the MariaDB chart pins. A newer `mariadb-dump` writes
  dumps that server refuses to load; the two are bumped together.
- **Files at a tenant's deletion** go with the namespace, after the stores,
  not before them.
- **Renamed databases.** A database an app made for itself is restored under
  another name when the bundle is of another tenant (§9.1). The app's own
  settings may still name the old one.
- **The realm of a bundle of another tenant.** Groups named
  `gentian:tenant:<old>:…` are renamed to the new tenant's, with their
  members; the bundle's clients, client roles and the old realm's default
  role are not imported.

