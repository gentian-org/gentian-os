# Operations: Backup, DR, Observability, Image Updates

**Companion to:** [architecture.md](../architecture.md),
[deployment.md](../deployment.md)

---

## 1. Backup Strategy

Backup is **per subsystem** — each kernel component uses the
industry-standard tool for its data type, orchestrated centrally.

| Data type | Tool | Scope | Method |
|---|---|---|---|
| PostgreSQL databases | **pgBackRest** or CloudNativePG built-in | Per-database (tenant-scoped restores) | WAL archiving + base backups to S3 |
| MariaDB databases | **Mariabackup** or MariaDB Operator backup CRD | Per-database | Full + incremental to S3 |
| S3 / MinIO buckets | **MinIO replication** or **Restic** | Per-bucket (tenant-scoped) | Cross-site replication or snapshot to external S3 |
| Dovecot mailboxes | **dsync** or **Restic** | Per-domain (`/var/mail/{domain}/`) | Filesystem-level backup or Dovecot-native sync |
| Keycloak realms | **Keycloak realm export** (JSON) | Per-realm (tenant-scoped) | Scheduled export to S3, versioned |
| OpenBao secrets | **OpenBao snapshots** (`bao operator raft snapshot`) | Full vault | Raft snapshots to S3, encrypted |
| Kubernetes resources | **Velero** | Per-namespace (tenant-scoped) | CRD state, ConfigMaps, Secrets (encrypted) |

**Velero** serves as the cross-cutting backup orchestrator for
Kubernetes-native resources (CRDs, ConfigMaps, namespace metadata)
and triggers pre/post-backup hooks coordinating with the
application-specific tools.

## 2. Tenant-Scoped Restore

The per-tenant isolation model (separate databases, buckets, realms,
namespaces) enables **single-tenant restore** without touching others.
Restoring tenant `demo` means:

1. Restore PostgreSQL databases matching `demo_*` from pgBackRest.
2. Restore MinIO buckets matching `demo-*`.
3. Restore Dovecot mailboxes for the tenant mail domain.
4. Re-import the Keycloak realm `demo` from JSON export.
5. Restore namespace `tenant-demo` via Velero.

This sequence is being automated by the namespaced `TenantExport` /
`TenantRestore` CRs, which also back self-service export and restore in the
Admin Console. Restore is **data-only** — the tenant's shape is re-composed
from its claim, never restored — and quiesces one app at a time, since the
consistency boundary that matters is an app's database plus its bucket plus
its PVC, not the tenant as a whole. What a bundle carries per kind, the rule a
restore decides by and what no restore brings back are in §9.

## 3. Tenant Migration Between Clusters

Same backup/restore pattern: backup on source, restore on target,
update DNS. The key requirement is that OpenBao secrets are either
migrated or re-provisioned. Re-provisioning is the natural path —
applying the Tenant CR on the target cluster triggers the full
Crossplane Composition, which picks up the existing data from the
restored databases and buckets.

### 3.1 Node Flavour Migration

Replacing a cluster's worker nodes with a different flavour — sizing from usable
rather than purchased memory, the drain order the kernel singletons require, and the
CNPG and RWO cases that need manual steps — is covered in
[node-pool-migration.md](../node-pool-migration.md).

## 4. Disaster Recovery

For full-cluster DR, recovery follows the deployment layers:

1. Bootstrap: install ArgoCD + Crossplane (one-shot script).
2. Apply the `Cluster` XR — Crossplane provisions kernel
   infrastructure from declared state.
3. Restore data: OpenBao snapshots, database backups, S3 replication.
4. Apply Tenant CRs — the operator and Crossplane re-provision tenant
   resources; apps pick up the restored data.

GitOps ensures the desired state of all workloads is recoverable from
Git; only stateful data requires backup restoration. The deterministic
secret-derivation model (see [security.md](security.md)) means kernel
credentials can be regenerated from the master password alone if
OpenBao itself is unrecoverable.

## 5. Observability via the K8s API

Crossplane's MR status model gives uniform observability:

```bash
# Tenant health at a glance
kubectl get tenants
NAME          STATUS         APPS   READY   MAIL         AGE
demo          Ready          2      2/2     selfhosted   30d

# Tenant app installs (Crossplane claims → helm Releases)
kubectl get apps -n tenant-demo
kubectl get releases.helm.crossplane.io -n tenant-demo

# Optional: Crossplane composite for namespace/policy (if XTenant is used)
kubectl get xtenant demo 2>/dev/null || true

# Integration contract health
kubectl get integrationbindings -n tenant-demo

# Kernel / GitOps (not per-tenant app charts)
kubectl get applications -n argocd
```

Tenant apps are observed via **`App` claim status** and **helm `Release`
MRs** in `tenant-{name}`. ArgoCD Applications cover kernel services and
catalogue sync (`gentian-appprofiles`), not each tenant app install.

## 6. Metrics

Prometheus metrics exposed by Crossplane and the kernel:

| Metric | Source | Description |
|---|---|---|
| `crossplane_resource_total{kind="XTenant"}` | Crossplane | Total tenants |
| `crossplane_reconcile_duration_seconds` | Crossplane | Reconcile latency per claim kind |
| `crossplane_reconcile_errors_total` | Crossplane | Failed reconciliations |
| `crossplane_resource_ready_status` | Crossplane | Ready conditions per MR |
| `externalsecrets_sync_calls_total` | ESO | OpenBao → K8s Secret sync health |
| `argocd_app_health_status` | ArgoCD | Kernel Application health |
| `gentian_os_credentials_age_seconds` | Custom | Age of oldest credential per tenant |
| `gentian_os_integration_bindings_status` | Custom | Binding health by contract |

### 6.1 Usage sampling

The operator runs a leader-elected worker that records each tenant's enforced
ceiling and what is committed under it into that tenant's own `{tenant}_shell`
database, every `usage.sampler.interval` (15 minutes by default). It is what the
Admin Console's Resources tab and `kubectl gentian resources report` read — see
[resource-plans.md](resource-plans.md).

```bash
# Is it running, and is any tenant being skipped?
kubectl logs -n gentian-system deployment/gentian-os | grep usage-sampler
```

A tenant without a `portal-shell-<tenant>` Secret in `platform-kernel` is skipped
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
`"appStore": {"available": false, "reason": "licence-report-disabled"}` and
no App Store tile is shown; the installer also leaves `catalogue.storeUrl`
out of a new Cluster claim. Everything else runs as before, and apps are
installed by command.

**For whoever receives it.** The request above is also written down as a
machine-readable format,
[licence-report.openapi.yaml](../plans/artefacts/licence-report.openapi.yaml).
It describes what the operator sends and changes nothing about it.

## 7. Image Updates via ArgoCD Image Updater

### 7.1 Philosophy

The kernel is a **singular shared service**: one kernel version per
cluster, all tenants on the same version. This is intentional — kernel
upgrades are platform-wide and atomic, unlike app upgrades which can
roll per-tenant.

App upgrades are catalogue-wide: bumping an `AppProfile`'s chart
version propagates to every tenant referencing the profile via
Crossplane helm `Release` reconciliation (and operator-driven
`App` claim updates when `spec.apps` changes).

### 7.2 Per-Environment Update Policies

Cluster-to-stage mapping and promotion workflows (simplified dev→prod and
fortified dev→staging→prod) are documented in
[deployment.md](../deployment.md).

A per-environment `ImageUpdater` CR watches the registry and updates
the kernel `Application` whenever a new image is published:

| Environment | Policy | Target tag |
|---|---|---|
| **dev** | Aggressive — track latest develop builds | `latest`, `develop`, `newest-build` |
| **staging** | Track release candidates | `semver:v*-rc.*` |
| **prod** | Conservative — only released semver | `semver:v1.x.x` |

```yaml
apiVersion: argocd-image-updater.argoproj.io/v1alpha1
kind: ImageUpdater
metadata:
  name: gentian-os-kernel-prod
  namespace: argocd
spec:
  applicationRefs:
    - namePattern: "prod-kernel-os"
      images:
        - imageName: ghcr.io/gentian-org/gentian-os
          policy: semver:v1.*
          tagsMatchRegex: '^v[0-9]+\.[0-9]+\.[0-9]+$'
          ignoreTagsRegex: '^.*-(rc|alpha|beta)\..*$'
  updateMethod:
    method: argocd          # patch Application params, no Git commit
  webhook:
    enabled: true            # immediate update on registry push
```

The full flow takes 30–60 seconds from image push to running new
pods.

### 7.3 Tenant Impact

Tenants are not parameterised by kernel version — when the kernel
updates, all tenants automatically use the new kernel. This is the
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

- **Stateful Argo Apps** (OpenBao, Crossplane) deploy with `prune: false` and `finalizers: []` —
  prevents Argo from ever deleting them if their manifests are
  temporarily missing from Git. Self-healing remains on for value
  drift.
- **OpenBao paths** managed by Crossplane use
  `managementPolicies: [Observe, Create]` — never overwrite live
  credentials. See [security.md](security.md).
- **Plaintext-secret admission policy** rejects any `Release` MR that
  literally embeds a secret value instead of referencing one.
- **Backup verification** runs daily: pgBackRest verify, MinIO
  replication lag check, OpenBao snapshot integrity check. Failures
  alert on the platform team's PagerDuty.

## 9. One Inventory, One Order

Five acts work on what an app of a tenant owns: provisioning makes it, export
and backup copy it, uninstalling leaves it, a purge of the app destroys it,
and deleting the tenant destroys it for every app at once. They read one
inventory and follow one order, both in
[`internal/backup`](../../internal/backup/): `inventory.go` names each thing
(`InventoryOf`, and `AppVolumes` for whose a volume claim is), `teardown.go`
lists the kinds in the order provisioning makes them (`AppKinds`), says what
every act does with each and how each is found once its app is uninstalled,
builds the one Job that destroys each store, and holds the one question that
says which databases are an app's. `record.go` is the record provisioning
keeps of what it made. Teardown is provisioning's order reversed
(`TeardownOrder`); nothing else defines an order. A test fails when a kind is
added without saying what export and each teardown do with it, when something
uninstalling keeps does not say how it is found afterwards, or when a store a
profile can declare is left out of the record.

### 9.1 Per kind

In provisioning order. "Restore" is what a restore puts back: exactly what was
carried, for the apps the bundle's manifest lists (§9.4).

| Kind | Install creates | Export / backup carries | Restore puts back | Uninstall | App purge | Tenant delete (`deletionPolicy: Delete`) |
| --- | --- | --- | --- | --- | --- | --- |
| Provisioning records (Jobs, labelled Secrets) | written from the first step on | nothing: not data | nothing | kept | destroyed, last | destroyed, last |
| Stored credentials (vault `…/apps/<app>`, `…/apps/<app>-<extension>`) | seeded before each store; generated secrets by the app Composition | nothing: a bundle holds no stored credential | nothing: the tenant restored into has its own, seeded when it was provisioned; what a person entered has to be entered again | kept | destroyed | destroyed, with the tenant's whole vault subtree |
| Access group and memberships | the tenant's identity Job | carried, in the realm export | put back, with the realm | kept | destroyed, in the tenant's realm | destroyed, with the realm |
| Sign-in scope (the client scope the app's OIDC pack describes, with its mappers) | the tenant's identity Job | nothing: configuration, made again at install | nothing | kept | destroyed, unless an installed app names the same scope or it is one of Keycloak's own | destroyed, with the realm |
| Database and role | role Job and CloudNativePG Database, or MariaDB setup Job | a dump of the provisioned database, and of every other database that is the app's: on PostgreSQL the ones its role owns, on MariaDB the ones named `<database>_…` | the provisioned database replaced; each other database of the app's created if missing and replaced — on MariaDB under the provisioned name of the tenant restored into; one that is the app's now and that the bundle does not hold is left, and named | kept | destroyed, with every other database that is the app's, and the role or user | destroyed, likewise |
| Object storage (bucket, user, policy) | bucket Job | the bucket's objects | the bucket, its user and its policy made by the code install uses, then the objects | kept | destroyed | destroyed, and the tenant's backup bucket unless bundles are kept |
| Cache user | ACL Job | nothing: a restored cache is stale | nothing | kept | removed; keys are not | removed; keys are not |
| Model key (at the model gateway, where the cluster serves models) | the tenant reconciler | nothing: a credential, registered again at install | nothing | kept | removed | removed, then the tenant's team |
| Sign-in client, with its client role, default-scope assignments and the group's mapping to the role | the app Composition, or the identity Job | carried, in the realm export | put back, with the realm | removed (Keycloak removes what is part of the client with it) | — | destroyed, with the realm |
| Workloads (the Helm release) | the app Composition or the component reconciler | nothing: re-made from the profile; the manifest records the build | nothing; the installed build is checked against the manifest's | removed | — | removed, first |
| Files (the release's volume claims) | the app's chart | an archive per claim that is the app's (`AppVolumes`) | each archive unpacked onto the claim of the same name | kept | destroyed | destroyed, with the namespace |
| Materialised profile (the `ComponentProfile`, committed under `clusters/<cluster>/catalogue/`) | committed by the director at the digest the install named, applied by Argo CD | nothing: the manifest records the build | nothing; the tenant restored into installs from its own catalogues | kept: a purge of the app reads it to know what the app owns | kept | kept: it is the cluster's and no tenant's |
| Bundle companions (the Composition, OIDC pack catalog, ConfigMaps and customization records a profile's bundle brings) | applied with the profile, from the same file | nothing | nothing | kept | kept | kept |

So the teardown order is: files, workloads, sign-in client, model key, cache,
object storage, database, sign-in scope, access group, stored credentials,
provisioning records. A purge of an app runs the steps of that list that
destroy (the workloads and the client went with the uninstall). A tenant's
deletion removes the workloads first — its Components, and with each the App
claim and the release — waits for them, then runs the store steps in the same
order for every app the tenant ever had, with the same Jobs and scripts, then
the realm; then it removes the namespace and waits until the API server no
longer has it, removes the vault subtree and, last, the records.

With `deletionPolicy: Retain` a tenant's deletion keeps every kind and removes
the workloads.

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
inventory lists them (`TenantOwned`) so that each says what removes it.

| What | Export | Retain | Delete |
| --- | --- | --- | --- |
| The namespace, with every workload and volume in it | volumes per app; workloads not | kept; Components and the operator's quota, limits and network policy removed | deleted, and the deletion waits until it is gone |
| The realm | carried: configuration, people, memberships; no passwords | disabled | deleted; never when it is the kernel realm, which a tenant only adopts |
| The client the realm signs in to the kernel realm as (`broker-<realm>`), and the mapper on it | not carried | kept | removed from the kernel realm, by the Job that deletes the realm |
| The vault subtree | not carried | kept | deleted; an operator with no vault that was not told to run without one (`GENTIAN_WITHOUT_VAULT=true`) fails here |
| The team at the model gateway | not carried | kept | removed, after the apps' keys |
| Mail routing, submission and IMAP credentials, mail DNS records (`DNSEndpoint mail-<tenant>`) | not carried | removed | removed, with the DKIM key and SMTP credentials |
| The edge: gateway, routes, wildcard certificate, edge routes, DNS records | not carried | removed | removed; a route that cannot be removed fails the deletion |
| The backup bucket | it is where exports go | kept | destroyed, unless the tenant keeps its bundles |
| The record of what was provisioned | not carried | kept | deleted, last |

### 9.3 Finding what an uninstalled app left

An uninstalled app has left the tenant's manifest and all its stores are still
there. Provisioning therefore writes down what it makes: one ConfigMap per
tenant in the provisioning namespace (`tenant-<name>-provisioned-stores`), one
entry per app with the database engine and names, the bucket, the cache user
and the model key, written before the Jobs that make them are handed over and
never reduced by provisioning. A purge takes a kind off the record once it has
destroyed it. The deletion of a tenant, a purge and the read of what
uninstalled apps hold (`GET /apps/retained`) read it; the read answers
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
have now, and the result says `nameDerivation: derived`. A manifest of a
format newer than the platform reads is refused. A `postgresOwned` artefact is
a tar.gz holding `INDEX`, the database names one per line, and
`<line number from 0>.pgc`, each one's custom-format dump.

**What no restore brings back**, said on every result (`status.notes`):

- **Stored credentials.** A bundle holds none, on purpose: it would put every
  password of a tenant in a file that leaves the cluster. What the platform
  seeds was made for the tenant restored into when it was provisioned, and a
  restore changes none of it. Credentials a person entered — a repository's
  password, an SMTP relay's, an API key — did not come back and have to be
  entered again. A profile's `spec.backup.boundSecrets` are not carried
  either, and an export of such an app says so.
- **Data sealed with a generated secret** can be read only where that secret
  is the same: on the cluster the bundle was taken on, or one built from its
  recovery kit.
- **Passwords.** Members come back without them and are sent a reset.
- **Mail, the cache, and declared state.** Mailboxes are not in a bundle. App
  grants are declared in git and come from there; integration bindings are
  derived from the installed apps and their profiles; authorization tuples are
  projections. On the same cluster none of the three is lost. A tenant
  imported into another cluster has its bindings and tuples made again and its
  app grants to set again.

An uploaded bundle (the import bucket, `gentian-imports`) is removed when a
restore of it has run to its end, restored or failed. A restore refused before
it changed anything leaves it, so the request can be made again; an upload that
is never restored stays until it is removed by hand.

### 9.5 Failing loudly

Both teardowns fail loudly. A destroy script ends in success only when what
it was asked to remove is verifiably gone. A purge of an app stops at the first
step that fails and answers with it
([store-contract.md](store-contract.md) §8). A tenant's deletion is not waited
for by anybody: a cleanup Job that fails, a vault or a model gateway that does
not answer, an edge route or a provisioning Job that cannot be removed, is a
reconcile error; the Tenant stays `Terminating`, and the failed step is run
again on the next pass, so it resumes where it stopped and does not move past
a store it could not destroy. The Tenant is gone only when its namespace is.

### 9.6 Known limits

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

