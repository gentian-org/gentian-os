# Backup and Recovery

**Status:** Phases 1–5 implemented, Phase 6 partly (scheduling and retention; cluster-level PITR/Velero not started); Phase 7 is the drills, and is now runnable
**Scope:** cluster recovery, credential escrow, self-service tenant export/restore (Admin Console)
**Applies to:** `install.sh`, OpenBao, CNPG, MinIO, Keycloak, tenant namespaces, `gentian-ui`

---

## 1. What the architecture already solves

Most of a Gentian cluster is declarative and reconciled, so most of "restore the
cluster" is **point a new cluster at the same repositories**. That is what the
XR-and-ArgoCD design buys, and it means backup can be scoped to what genuinely
cannot be reconstructed rather than to everything.

Three things cannot be reconstructed. They are the whole problem:

1. The bootstrap material — the derivation salt, the transit unseal key, the
   primary's recovery keys.
2. Stateful workload data — PostgreSQL, MariaDB, MinIO, the Dovecot maildir,
   Keycloak realm content.
3. OpenBao's own storage, for anything not derivable from (1).

Everything else is either in Git or a function of those.

---

## 2. What needs backing up, and what must not

| Layer | Reconstructible from | Back up? |
|---|---|---|
| Config, claims, `AppProfile`s | Git | No — but mirror the remotes |
| The 16 derived credentials | master password + salt | **No**, while `secretMode: derived` |
| The 6 external credentials | the operator's password manager | Yes, off-cluster |
| **Bootstrap material** — salt, transit unseal key, recovery keys | nothing | **Yes. This is the one that gets lost** |
| OpenBao Raft storage | — | Yes, snapshots |
| Secrets materialised by ESO | OpenBao | **No — see below** |
| Data-bound secrets — DKIM keys, app data-encryption secrets | nothing | Yes, inside the encrypted bundle — see §5 |
| Crossplane claims and MRs | Git, then reconcile | No |
| PostgreSQL, MariaDB, MinIO, maildir, Keycloak realms | — | Yes. This is the real payload |
| Redis | it is a cache | No |

**Secrets fall into three classes, with three policies.**

1. **Derived** — a pure function of master password + salt. Never backed up;
   the derivation scheme *is* their backup. This covers more than the sixteen
   kernel credentials: `AppProfile.spec.appSecrets` are HMAC'd from master
   password + tenant + app + name, so an app's own generated passwords also
   reproduce byte-identically. Does not hold under `secretMode: random`, where
   they become class 3.
2. **Projections** — ESO-materialised Kubernetes Secrets. Backing these up is
   actively harmful: a copy of credentials as they were at snapshot time,
   surviving rotation, in an archive nobody watches. Any backup tooling must
   exclude them explicitly; the absence of a backup here is a security
   property, not a gap.
3. **Data-bound** — not derived, held outside the app's own captured data, and
   the data is cryptographically or referentially welded to them. The clearest
   real example is the per-tenant **DKIM private key**, generated once and never
   rotated. (An app that keeps its own encryption secret in a config file on a
   PVC is not in this class — the secret is captured with the volume.) Without
   them the restored data is garbage, so they must travel with the backup —
   envelope-encrypted, declared per app in
   `AppProfile.spec.backup.boundSecrets` (§5) rather than guessed at by the
   platform. Class 1 shrinks this set to a genuine residue, but does not empty
   it, and a restore onto a cluster with a different master password moves the
   whole of class 1 in here.

---

## 3. The recovery kit

One encrypted bundle holding what no amount of reconciliation can rebuild:

```
master password        the root of the derived class
derivation salt        currently stored ONLY in OpenBao — see §8
transit unseal key     without it the transit instance cannot be unsealed
primary recovery keys  without them the primary cannot be unsealed if transit is gone
external credentials   the six in credentials.yaml, or a note of where they live
cluster identity       kernelDomain, deployments repo and branch, stage
```

It is small, changes rarely, and belongs wherever the organisation already keeps
break-glass material. It is the difference between "rebuild from Git" being a
plan and being a hope.

This part exists. `scripts/lib/recovery.sh` implements it, and `install.sh`
exposes it as `--export-recovery-kit` and `--recover` — see §7.

**The whole point of the kit is that derived credentials come back identical.**
Restore it into a fresh cluster before the first install and every database
user, service account and app password derives to the same value it had. Without
it, a rebuild produces a working cluster with entirely different credentials —
which is a migration, not a restore.

---

## 4. Per-tenant restore

This is the case that will actually happen. Whole-cluster loss is rare; "this
customer deleted their files" is routine, and a recovery design that only
handles the rare case will be used for neither.

### What a tenant is made of

| Piece | Where it lives | Restore path |
|---|---|---|
| Namespace, quotas, LimitRange | composed from the `Tenant` claim | Re-apply the claim |
| App workloads | composed from `AppProfile`s | Reconciles |
| OpenBao paths | `gentian-os/tenants/<tenant>/…` | Re-derived, plus data-bound secrets from the bundle |
| **PostgreSQL databases** — `<tenant>_<app>` and `<tenant>_shell` | **inside the shared CNPG cluster** | Logical dump — see below |
| **MariaDB databases** | **inside the shared MariaDB** | Logical dump |
| **Object data** | MinIO, `<tenant>-<app>` buckets | Bucket-level restore |
| **Keycloak realm** — users, groups, clients | the tenant realm | Realm export/import |
| PVC data — app volumes, maildir | the tenant's namespace | Archive per PVC |
| DKIM private key | kernel namespace Secret, generated once | From the bundle |

Because a tenant's *shape* is composed from its claim, restoring a tenant is
**re-apply the claim, then restore its data**. Nothing about the namespace,
quotas or app set needs backing up beyond a snapshot of `tenant.yaml`. Data-only
restore is also what keeps Crossplane's `external-name` annotations (§8) out of
play: infrastructure is never restored, only re-converged.

### The constraint that decides the design

**Tenant databases are databases inside shared instances, not per-tenant
instances.** That has a consequence worth stating plainly:

> Point-in-time recovery on the shared CNPG cluster restores **every** tenant to
> that point. It is a cluster-level tool and cannot express "restore tenant B to
> Tuesday while everyone else stays current".

So per-tenant restore needs **per-database logical backups** — a `pg_dump` per
tenant database, and the MariaDB equivalent — *in addition to* cluster-level
PITR. They answer different questions:

| Tool | Answers | Granularity |
|---|---|---|
| CNPG PITR | "the cluster was corrupted at 14:05" | Whole instance, any point in time |
| Per-tenant logical dump | "tenant B needs yesterday back" | One tenant, discrete points |
| MinIO versioning / replication | "these objects were deleted" | Object, continuous |

Both are needed. Choosing one is choosing which outage you can survive.

### The consistency boundary is the app, not the tenant

No mechanism exists for a transactionally consistent snapshot across
independent stores; the only honest way to get one is to pause writes. The
useful observation is *where* consistency matters: an app's database references
its bucket and its PVC — **those must be captured together** or the restored
app is corrupt. Two *different* apps share no transactional state, only loose
OIDC and contract coupling, so skew between them is harmless.

Export therefore quiesces **one app at a time**: pause app A, capture all of
its stores, resume, move on. Each app's write-pause spans only its own dump,
and the manifest records per-app timestamps so the skew is visible rather than
hidden. The realm and the `<tenant>_shell` database are low-write and
internally consistent, and are captured without quiescing anything.

---

## 5. Self-service export and restore (Admin Console)

Tenant admins export their tenant's data as an encrypted bundle and restore
their tenant from one — the routine case of §4, made a product feature.

### Shape

Two namespaced CRs, **`TenantExport`** and **`TenantRestore`**, living in
`tenant-<name>` and reconciled by a gentian-os controller that fans out kernel
Jobs per subsystem and reports per-app status conditions — the same
Job-and-condition machinery the tenant reconciler already uses. Namespacing
keeps each tenant's operations inside its own blast radius, the way `App`
claims are modelled; the access path is the Admin Console BFF, since no
per-tenant kubectl RBAC on `gentianos.io` kinds exists today. One
export-or-restore in flight per tenant, ever.

The subsystem list is **driven by `AppProfile.spec.kernelRequirements`**, the
same enumeration purge uses — never an app name in a reconciler.

### The AppProfile backup contract

`kernelRequirements` says *what stores* an app has; only the app developer
knows *how* to capture them correctly. That knowledge goes in the profile, in
an optional `spec.backup` section with safe defaults (scale down, dump every
declared store, archive every release-owned PVC):

```yaml
backup:
  quiesce:
    mode: command            # none | scaleDown (default) | command
    pre:  ["occ", "maintenance:mode", "--on"]
    post: ["occ", "maintenance:mode", "--off"]
    container: nextcloud
  volumes:
    include: [nextcloud-data]          # default: all release-owned PVCs
    excludePaths: ["appdata_*/preview"]
  boundSecrets: []                     # class-3 secrets (§2), by OpenBao path
  restore:
    post: [["occ", "maintenance:data-fingerprint"]]
    verify: ["occ", "status"]
  consistency: app          # app (default) | perStore
  minRestoreVersion: "27.0"
```

Nextcloud declaring **no** `boundSecrets` is the expected case, and worth
understanding before writing the field for another app. Its `admin_password` is
an `appSecret`, so it re-derives (class 1); its `instanceid`, `secret` and
`passwordsalt` are generated by Nextcloud itself into `config.php`, which sits
on the captured PVC and therefore travels with the data automatically. The
field is for the residue only: a non-derived secret held *outside* the app's own
captured data. This is also why `excludePaths` must never be allowed to exclude
an app's config directory — that would silently drop the key its data is
encrypted with.

Schedule, retention, encryption and destination are deliberately **not** in the
profile — they are platform and tenant policy, and an app developer must not be
able to weaken them. The profile says how to back this app up correctly; the
platform says when, where, and under what key.

### The bundle

A prefix in a per-tenant backup bucket — not one giant archive — so export
streams, restores can be partial, and no tenant-sized scratch space is needed.
Per-subsystem native formats, because native tooling is the only thing
guaranteed to restore across versions:

| Store | Format |
|---|---|
| PostgreSQL | `pg_dump -Fc` per database |
| MariaDB | `mariadb-dump \| zstd` per database |
| MinIO buckets | `mc mirror`, objects as-is |
| Keycloak realm | native JSON export |
| PVCs, maildir | `tar --zstd`, honouring `excludePaths` |

Plus `manifest.json` — tenant spec snapshot, gentian-os and per-app chart
versions, per-app snapshot timestamps, subsystem inventory — and `SHA256SUMS`.
The whole prefix is `age`-encrypted; bound secrets exist only inside it. The
console offers "download as single archive" as a packaging step on top.

**Keycloak, v1 decision:** the Admin API realm export carries users, groups and
clients but not password hashes. Restore re-imports the realm and triggers
password-reset emails. Preserving passwords would require the offline
`kc.sh export` path and a more sensitive bundle — deferred (§10).

### Restore

In-place (the tenant-admin case): preflight against the manifest — refuse a
bundle produced by *newer* app versions than installed (`minRestoreVersion`,
chart versions); older-into-newer just runs app migrations. Then per app:
quiesce → drop and recreate databases from dumps → mirror buckets back →
restore PVCs → run `restore.post` hooks → `restore.verify` → resume. Realm
re-import and bound-secret re-seeding to OpenBao happen before apps resume.

Into a fresh cluster (the DR case): apply the snapshotted `tenant.yaml`, wait
for `Phase=Ready`, then the same `TenantRestore` — a cluster-admin procedure,
not a console feature.

### Console

A `Backup` tab in the Admin Console. BFF endpoints follow the house pattern:
`_require_admin` + `resolve_admin_tenant` on every route, `record_admin_audit()`
on every mutation, CR access via `CustomObjectsApi`, create-then-poll UX,
download as a streaming response. Restore sits behind a typed-confirmation
dialog — it is the most destructive thing a tenant admin can do.

---

## 6. Tooling

| Layer | Tool | Why not something else |
|---|---|---|
| PostgreSQL | CNPG `ScheduledBackup` + WAL archiving to object storage | A volume snapshot of a running database is crash-consistent at best. The CNPG operator is installed and provides the `Backup` and `ScheduledBackup` CRDs — no `barmanObjectStore` is configured on any Cluster, so none of it is in use |
| Per-tenant PostgreSQL | `pg_dump` per tenant database, via `TenantExport` | PITR cannot restore one tenant — §4 |
| MariaDB | `mariabackup`, plus per-database logical dumps | Same reasoning |
| MinIO | Bucket replication to a second endpoint, plus versioning | Versioning covers deletion; replication covers loss of the cluster |
| Keycloak | Realm export Jobs, per tenant | Realm = tenant; a database-level backup of Keycloak cannot restore one realm |
| OpenBao | `bao operator raft snapshot save`, encrypted | Storage is Raft (`kernel/openbao/values.yaml`) |
| Everything else | Velero, with CSI snapshots where the storage class supports them | Namespaced objects and PVCs with no native path |

All destinations **outside the cluster** for continuous protection; the
cluster is what is being protected against. The self-service bundle (§5) lands
in a MinIO backup bucket first — object-lock/versioning should make it
immutable from inside the cluster — and is downloadable off-cluster.

**Nothing above is in use today.** There is no Velero and no restic anywhere in
the tree, no `barmanObjectStore` on any CNPG Cluster, no `ScheduledBackup`, no
OpenBao snapshot job, and no MinIO replication. This document describes work
that has not started, not work that needs tidying.

---

## 7. Where the installer fits — narrowly

**No `--backup`.** Backup is continuous, scheduled, retained and in-cluster. The
installer is one-shot. A `--backup` flag would also make the installer a
dependency of restore, which is the wrong direction: recovery should need a
bundle and a cluster, not a current checkout.

**`--export-recovery-kit` and `--recover <kit>`, both implemented.** The
bootstrap material is the one thing only the installer holds, and priming a fresh
cluster with it before the first run is what makes derived credentials come back
identical.

```bash
# After an install, while the values are still reachable:
./install.sh --export-recovery-kit kit.age

# Into a fresh cluster, before anything else:
./install.sh --recover kit.age
```

Export reads the environment first, then OpenBao, then the init files the
installer wrote, so it also works against a half-broken cluster. It refuses to
write anything unless both the master password and the salt are present — a kit
missing the salt reproduces nothing and would be worse than no kit at all.

Encryption is `age` when installed and `openssl` otherwise: age authenticates,
so a tampered kit fails to decrypt rather than yielding plausible garbage.
There is no unencrypted path.

| Variable | Effect |
|---|---|
| `GENTIAN_KIT_RECIPIENT` | An age public key. Encrypt to it instead of prompting — the only way to export unattended |
| `GENTIAN_KIT_IDENTITY` | The matching private key file, needed to read a kit written that way |

On import the kit is parsed against a fixed key whitelist rather than sourced —
encrypted is not the same as trusted. Values already in the environment win.

What it does **not** do is restore OpenBao. A fresh instance initialises itself
and issues new unseal material; the keys in the kit belong to the old one and
unseal a restored Raft snapshot, nothing else.

**Backup policy itself should be declarative**, following the pattern already in
use: an `XBackup` claim per cluster and per tenant, composing Velero `Schedule`,
CNPG `ScheduledBackup`, and scheduled `TenantExport` objects. That keeps "what
is backed up and how often" reviewable in Git, rather than a set of CronJobs
nobody reads.

---

## 8. Traps

**The salt lives only in OpenBao — unless a kit was exported.** A disaster that
loses OpenBao's storage also loses the salt, and the master password alone then
reproduces nothing. `--export-recovery-kit` is the fix, which makes *having run
it* the actual dependency. Export after the first install, and again after any
credential that is not derived changes.

**Transit auto-unseal is a two-body problem.** The primary unseals from the
transit instance. Restore the primary without it and the cluster is sealed. The
kit needs the transit key *and* the primary's recovery keys, because either can
be the one that survives.

**Restoring into an existing cluster hits Crossplane's `external-name`
annotations** — the identity of resources already provisioned externally. The
export/restore design (§5) avoids it by restoring data only and letting the
platform re-converge infrastructure; anything that restores provisioned
objects does not.

**A generated secret the data was encrypted with is part of the data.** A
restore that reproduces every byte of an app's files but not the key they were
encrypted with has restored nothing. Two ways to lose it: a non-derived secret
in OpenBao that no profile declared in `boundSecrets`, or an `excludePaths`
pattern that quietly drops the config file holding it. Both fail silently at
backup time and loudly a year later.

**"Export" means tenant data, and only that.** The cross-repo source bundles
that used to sit in `export/` now live in `repo-seeds/` for exactly this
reason. Keep it that way in new code, CRD fields and docs.

**An untested backup is a hypothesis.** The step everyone skips is the only one
that establishes whether any of this works.

---

## 9. Implementation plan

Phased so an agent can take one phase end-to-end. Each phase is independently
shippable and names the files it touches. Standing rules: nothing app-specific
in a gentian-os reconciler (`AGENTS.md`), no plaintext secret path, generated
artifacts refreshed with `make gen-all` (CI gates it with `make verify-gen`),
and work lands on `develop` for CI to roll out.

Read §2 (what must never be backed up), §4 (the consistency boundary) and §8
(traps) before starting any phase.

### Phase 1 — Recovery kit — **implemented**

`scripts/lib/recovery.sh`; `install.sh --export-recovery-kit` / `--recover`.
Remaining acceptance (belongs with Phase 7): a fresh-cluster `--recover` run
derives byte-identical credentials; kit-plus-Git rebuild drill.

### Phase 2 — The `spec.backup` contract — **implemented**

**Goal** — teach `AppProfile` how its app must be captured, so every later
phase is profile-driven rather than app-aware.

Shipped as `BackupSpec` in `api/v1alpha1/appprofile_types.go` (with the
`BackupQuiesceMode` / `BackupConsistency` enums in `types.go`), nil-safe
accessors that resolve the defaults in one place, `scripts/validate-backup-spec.py`
in the gentian-apps `validate-profiles` job, §15 of the app-profile guide, and
the first declaration on `nextcloud-base-ce`.

One finding worth carrying into Phase 3: containment of `boundSecrets.openBaoPath`
is enforced by **pattern, not CEL**. Kubernetes bounds the estimated cost of
every CEL rule in a CRD, and a single `self.contains('..')` over an unbounded
string inside an unbounded list put the whole AppProfile schema over budget —
the API server refused to install the CRD at all, which envtest caught and no
amount of unit testing would have. Prefer patterns to CEL on repeated fields,
and if CEL is unavoidable, bound the string and the list.

**Files**

- `api/v1alpha1/appprofile_types.go` — `BackupSpec`, and `Backup *BackupSpec`
  on `AppProfileSpec` as a sibling of `KernelRequirements` (field at `:114`,
  struct at `:512`).
- Generated by `make gen-all`: `api/v1alpha1/zz_generated.deepcopy.go`,
  `config/crd/gentianos.io_appprofiles.yaml`,
  `charts/gentian-os/crds/gentianos.io_appprofiles.yaml`.
- `gentian-apps/scripts/validate-backup-spec.py` (new) plus a step in the
  `validate-profiles` job of `gentian-apps/.github/workflows/apps-ci.yaml`.
- `gentian-apps/docs/app-profile-guide.md` — a new `## 15. Backup and restore
  contract`, and a line in `## 12. Checklist for a new AppProfile`, which is
  the part authors actually read. §13a (`spec.postInstallJob`) is the
  stylistic precedent for documenting a block that runs commands.
- `gentian-apps/profiles/nextcloud/base/base-ce/profile.yaml` — the first real
  declaration. Profiles are directory bundles (`profile.yaml` +
  `kustomization.yaml`); addons are separate profiles under
  `profiles/nextcloud/addons/`.

**Detail**

- Every field `+optional`; `quiesce.mode` and `consistency` get
  `+kubebuilder:validation:Enum`. A nil `Backup` must behave exactly as
  `scaleDown` + every store in `kernelRequirements` + every release-owned PVC.
  That default is the contract for the ~10 profiles that will never set the
  section, so encode it in one place and unit-test it.
- `quiesce.pre/post` are `[]string` argv (no shell) executed in
  `quiesce.container`; `restore.post` is `[][]string`, because ordering matters
  and each entry is a separate exec.
- `boundSecrets[].openBaoPath` is **relative to** `gentian-os/tenants/{tenant}/`
  (`internal/kernel/secrets/paths.go`). Reject absolute paths and `..`, or a
  profile could name another tenant's secrets.
- Most app-internal secrets do **not** belong here. `spec.appSecrets` are
  already HMAC-derived from master password + tenant + app + name, so they
  reproduce byte-identically on the same cluster and on any cluster primed with
  the recovery kit. `boundSecrets` is for the residue: values the platform did
  not derive, and cross-cluster restores where the derivation inputs differ.
- gentian-apps CI has no JSON Schema and never validates a profile against the
  CRD — `kubectl kustomize` renders but does not schema-check, and unknown
  fields are pruned only at apply time. The new script is what makes a typo'd
  hook fail the catalogue PR instead of a tenant's restore.

**Acceptance**

- A profile with no `spec.backup` validates, and the defaults are asserted in a
  Go unit test.
- nextcloud base-ce declares maintenance-mode quiesce and preview excludes, and
  **no** bound secrets — its `admin_password` re-derives and its `config.php`
  rides on the captured PVC. If that profile ends up needing the field, the
  design in §5 is wrong somewhere.
- `make verify-gen` is clean; a `boundSecrets` path escaping the tenant prefix
  fails CI.

### Phase 3 — `TenantExport` — **implemented**

**Goal** — an encrypted, restorable bundle of one tenant, produced by the
operator.

Shipped as `api/v1alpha1/tenantexport_types.go`, the `internal/backup` package
(inventory, capture Jobs, manifest), `internal/controller/tenantexport_*.go`,
and RBAC via kubebuilder markers. purge now reads the shared inventory, so
export and purge can no longer disagree about what a tenant is made of.

Four things went differently from the design above, all worth carrying into
Phase 5:

- **The bundle bucket is not provisioned with the tenant.** Gating
  `StorageReady` on it — as this plan originally said — couples every tenant's
  readiness to backup infrastructure, and broke provisioning for tenants with no
  S3-backed app. Each writer now runs `mc mb --ignore-existing` instead, so the
  bucket appears when first needed and nothing waits on it.
- **`quiesce.mode: command` falls back to scaling down.** Executing a command
  inside a running pod needs a client the reconciler does not have. The data
  guarantee is unchanged — no writes during the capture — but the app goes
  offline instead of showing a maintenance page, and the manifest records the
  mode actually used rather than the one requested. Wiring exec is the natural
  first task of Phase 5, which needs it anyway for `restore.post`.
- **Realm capture is three calls, not one.** Keycloak's `partial-export` returns
  no users at all, so the Job also pages `/users` and collects per-user group
  membership. A realm restored from `partial-export` alone is a correctly
  configured workspace with nobody in it.
- **Dumps stage on disk before upload.** Streaming into `mc pipe` needs the dump
  tool and `mc` in one image, which the platform does not have. The scratch
  volume is bounded (`exportScratchLimit`), so an oversized tenant fails its
  capture rather than filling a node — but a tenant larger than that bound
  cannot be exported until a combined image or a streaming path exists.

**Encryption is implemented, in both custody models** — the §10 question is
settled by supporting both rather than choosing:

| `spec.encryption.mode` | Who can decrypt | For |
|---|---|---|
| `recipient` (default) | whoever holds an identity for the configured recipients — escrowed off-cluster with the recovery kit | scheduled and unattended exports, and disaster recovery |
| `recipient` + own `recipients` | only the requester | an admin who wants a bundle the platform cannot read |
| `passphrase` | only the passphrase holder | admin-triggered exports, where the platform should retain nothing |

The middle row is a standing arrangement, not only a per-export one:
`BackupPolicy.spec.encryption.recipients` names the keys a tenant's scheduled
bundles are encrypted to, and the operator carries them onto the managed
`TenantExportSchedule`. Stated recipients **replace** the cluster's rather than
adding to them — appending would leave the platform able to read a bundle
somebody asked to be readable only by them — so a tenant that wants both keeps
support's help by listing the platform's key alongside its own. Only tenant
scope may state one: the cluster's recipients are pinned in git and written by
the installer, which is what makes "the key a bundle is encrypted to is the key
the repository says it is" checkable.

Both produce an ordinary age file, so a bundle opens with `age -d` or
`age -d -i <identity>` and needs no Gentian tooling — which matters most in the
situation a backup exists for. `status.encryption.platformReadable` states
plainly whether any platform-held key still opens a given bundle, because
support reading that field will otherwise promise a recovery they cannot
perform.

Three things fell out of building it:

- **There is no unencrypted path, and no fallback.** An export that cannot be
  protected fails. A cluster with no `backupRecipients` configured cannot take
  an export unless the requester supplies a key or a passphrase — deliberately,
  because the alternative is writing an entire tenant to object storage in the
  clear.
- **`age -p` cannot be scripted**, which `--export-recovery-kit` already knew
  (§7). It reads from the terminal and refuses a pipe, so passphrase mode gives
  it a pty via `script(1)`. age still does the cryptography; the output is a
  standard scrypt-stanza age file.
- **Bucket capture had to stop being a mirror.** Artefacts are encrypted
  individually, so `mc mirror` would have landed bucket objects in the bundle as
  plaintext — the largest part of most tenants' data, readable, inside a bundle
  whose whole premise is that it is not. Buckets are now staged and archived
  like volumes, at the cost of scratch space.

`bundle-info.json` is the single unencrypted file: tenant, export name,
timestamp, mode, recipients, and the literal command to decrypt the rest. The
manifest is encrypted with everything else, since it carries the tenant's spec
and app inventory.

**Still not done:** `boundSecrets` are declared and validated but not yet
fetched into the bundle.

**Files**

- `api/v1alpha1/tenantexport_types.go` (new) — namespaced,
  `+kubebuilder:object:root=true`, `+kubebuilder:subresource:status`, and
  `SchemeBuilder.Register` at the bottom (pattern:
  `api/v1alpha1/appgrant_types.go:121`). Spec: `apps []string` (empty = all
  installed), `ttl`. Status: phase, per-app conditions, `bundle{bucket,prefix}`,
  per-app snapshot timestamps. For the phase enum, `IntegrationBindingState`
  (`api/v1alpha1/types.go:109`, `Pending`/`Ready`/`Failed`) is the existing
  precedent for a one-shot CRD.
- `internal/backup/` (new package): `inventory.go`, `jobs.go`, `manifest.go`,
  `bundle.go`.
- `internal/controller/tenantexport_controller.go` (new), registered in
  `cmd/main.go` after the existing reconcilers (~`:216`).
- `internal/applifecycle/purge.go` — switch to the shared inventory.
- `internal/controller/storage_reconciler.go` — per-tenant backup bucket.
- `internal/authz/keycloak_client.go` — realm export.
- `internal/controller/metrics.go` — `gentianos_tenant_export_*`, following the
  existing `gentianos_*` naming.
- RBAC: `+kubebuilder:rbac` markers on the new controller (markers live beside
  the code in `./internal/...`), then `make gen-all`.
  `charts/gentian-os/templates/clusterrole.yaml` is generated — never edit it.

**Detail**

- **Extract the inventory first — it is the highest-value step in the phase.**
  Everything needed to enumerate a tenant's stores already exists in
  `internal/applifecycle/purge.go` (`profileKernelReqs:91`, `purgePVCs:550`,
  `pvcBelongsToApp:642`) and `internal/applifecycle/names.go`, but those are
  unexported, and `internal/controller` already carries near-duplicates
  (`databaseName`, `roleUserName`, `s3BucketName` in the `*_reconciler.go`
  files). Move one copy into `internal/backup/inventory.go` and have both
  callers use it, or export will drift from purge the first time a naming rule
  changes — and a store nobody enumerates is a store nobody backs up.
- Resolve the **effective** app set through `internal/catalogue`:
  `Tenant.spec.apps[].profile` *and* `.addons`, each its own `AppProfile` with
  its own `kernelRequirements`. Add the portal shell database
  (`internal/controller/portal_shell_database.go` — `<tenant>_shell`, Secret
  `portal-shell-<tenant>`) and, when `mail.mode: selfhosted`, the maildir and
  the DKIM key (Secret `dkim-<tenant>` in `platform-kernel`, key `tls.key`).
- **Do not use `runKernelJob` (`purge.go:228`)** — it blocks with a five-minute
  deadline and a real dump will exceed it. Use the reconciler pattern: create
  the Job, return, and poll with `waitForProvisioningJob(ctx, tenantName,
  jobName) (bool, error)` (`internal/controller/kernel_job_wait.go:66`),
  requeueing through `requeueForPendingApps`
  (`internal/controller/provisioning_requeue.go:77`). Note that
  `waitForProvisioningJob` **deletes a failed Job so it gets recreated** —
  correct for idempotent provisioning, wrong here. Bound the attempts in
  `status` and fail the export rather than retrying forever.
- Build Jobs with a helper modelled on
  `kernelDeleteJob(ns, name, tenant, app, image, container, script, env)`
  (`purge.go:291`). They run in `meta.KernelNamespace` (`platform-kernel`) and
  carry `meta.TenantLabel` / `gentianos.io/app` / `meta.ManagedByLabel`. Use
  `meta.ProvisioningJobTTLSeconds` and set a `backoffLimit`; the purge builder
  hardcodes its own TTL and sets no backoff, so do not copy it verbatim.
- Images and credentials already exist: `kernel.PostgresProvisionerImage()`
  (`internal/kernel/images.go`) with Secret `postgres-admin`
  (`PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`, assembled by `secretEnv`);
  `kernel.MariaDBProvisionerImage()` with `mariadb-admin`; `minio/mc` with
  `minio-admin`; `kernel.KeycloakProvisionerImage()` with `keycloak-admin`.
  The CNPG endpoint is `<cnpgClusterName>-rw.platform-kernel.svc.cluster.local:5432`
  (`CNPG_CLUSTER_NAME`, default `postgres`), and `pg_dump` must match the CNPG
  major version. Never read an ESO-materialised app Secret, and never mount the
  master password into a capture Job.
- Keycloak realm export does not exist anywhere yet. Add
  `POST /admin/realms/{realm}/partial-export` to `KeycloakAdminClient`
  (`internal/authz/keycloak_client.go`); its admin credentials come from
  `loadKeycloakAdmin` (`internal/controller/app_privilege_reconciler.go:226`).
- Backup bucket: add `backupBucketName(tenant)` beside `s3BucketName`
  (`storage_reconciler.go:238`) and a `makeBackupBucketJob` reusing
  `minioContainer` / `minioSetupScript` unchanged — they are already
  bucket-parameterised. Call it from `ensureStorage` **outside** the
  `collectStorageApps` loop so every tenant gets one, and exclude it from the
  app inventory, or the next export backs up the backups.
- `manifest.json`: schema version, tenant name and spec snapshot, gentian-os
  version, per app `{profile, chart version, stores captured, quiesce
  start/end}`, bound-secret key names (never values), and checksums. This is
  what makes a restore debuggable two years later.
- Quiesce/resume must be **crash-safe**. An app left scaled to zero across an
  operator restart is an outage. Record the quiesced app in `status`, make
  resume idempotent, and run it on the failure path and on controller startup.
- Mutual exclusion: refuse to start while another `TenantExport`/`TenantRestore`
  in the namespace is non-terminal, and surface that as a condition rather than
  an error loop. Conditions go through `setCondition`
  (`internal/controller/tenant_controller.go:928`).
- Tenant admins have **no kubectl RBAC on `gentianos.io` kinds today** — there
  is no per-tenant `Role` anywhere in the repo; access is mediated by the
  console BFF, Keycloak groups and OpenFGA. Keep it that way: grant the portal
  ServiceAccount in Phase 4 and leave direct RBAC out of scope. If it is ever
  wanted, the natural home is a `Role`/`RoleBinding` beside the LimitRange and
  ResourceQuota in `crossplane/compositions/tenant-default.yaml`.

**Acceptance**

- Exporting a two-app tenant produces a bundle whose manifest covers every
  store implied by `kernelRequirements` plus addons, with no app name anywhere
  under `internal/`.
- Each app's pause spans only its own capture — the manifest timestamps prove
  it — and no app is left quiesced after a forced controller restart mid-export.
- The bundle contains no ESO Secret and no derived credential, and does contain
  the declared bound secrets.
- A second concurrent export is rejected via status, not by crashing.
- A capture Job that keeps failing fails the export instead of looping forever.

### Phase 4 — Console: export — **implemented, except in-browser download**

**Goal** — a tenant admin produces and downloads a bundle without kubectl.

Shipped in `gentian-ui`: `services/k8s_backup.py`, the `/admin/backups`
endpoints, `admin/BackupSection.tsx` behind a Backup tab, and the RBAC to go
with it. Documented in `docs/commands.md` §11.

The encryption choice is put in front of the admin rather than defaulted
silently, because it decides whether anyone but them can ever read the bundle:
**Platform key** (support can help restore) or **My passphrase** (nobody else
can, ever). A passphrase never enters the `TenantExport` spec — the BFF writes
it to a Secret in the tenant namespace and the export references that, so it
stays out of etcd-as-spec-data and out of `kubectl get -o yaml`. A create that
fails afterwards deletes the Secret rather than leaving a passphrase with no
export attached to it.

**In-browser download is deliberately not implemented.** Serving one needs
either MinIO credentials in the portal — which today has no S3 access at all,
and giving it the admin credentials that can read every tenant's bundles is a
poor trade for a convenience — or a new operator endpoint minting presigned
URLs. A tenant-sized bundle is also not a browser download. The console shows
the bundle location and points at `bundle-info.json`, which is unencrypted and
names the exact decrypt command. Presigned, time-limited URLs minted by the
operator are the right shape when this is picked up; it belongs with Phase 6,
alongside retention.

**Acceptance, as met:** an admin starts an export, watches per-app progress and
pause windows, and sees whether the result is platform-readable; every creation
is audited as `backup.created`; a cross-tenant request 403s in
`resolve_admin_tenant` before reaching the cluster; polling stops when nothing
can change on its own.

**Files**

- `gentian-ui/backend/app/services/k8s_backup.py` (new) — CR access, modelled
  on `services/k8s_authorization.py` (namespaced `CustomObjectsApi` calls,
  `tenant_namespace()`). Note there is no `create_namespaced_custom_object`
  caller in the codebase yet; `replace_platform_security_policy` is the closest
  create example.
- `gentian-ui/backend/app/api/routes/admin.py` — the endpoints.
- `gentian-ui/chart/templates/rbac.yaml` — the portal's ClusterRole is
  **hand-written** (unlike the operator's); add `tenantexports` here, and
  `tenantrestores` in Phase 5.
- `gentian-ui/frontend/src/admin/BackupSection.tsx` (new), plus
  `AdminConsole.tsx` (the `AdminTab` union at `:18`, a nav button, a ternary
  arm — there is no tab registry), `frontend/src/api/admin.ts`,
  `frontend/src/admin/admin.css`.
- `docs/commands.md` — a section beside §5 "Tenant App Store".

**Detail**

- Endpoints: `POST /admin/backups`, `GET /admin/backups`,
  `GET /admin/backups/{name}` (conditions → progress), and
  `GET /admin/backups/{name}/download`.
- House shape: `_require_admin(user, settings)` then
  `resolve_admin_tenant(user, settings, tenant)` as the first two lines of the
  handler — they are plain functions, not FastAPI dependencies — with `tenant`
  from `Depends(admin_tenant_query)`. Mutations call `record_admin_audit(...)`
  with dotted actions (`backup.created`, `backup.downloaded`,
  `restore.started`).
- The download copies `export_audit_events` (`admin.py:1154`) server-side —
  `StreamingResponse` plus `Content-Disposition` — and `downloadAuditExport`
  (`frontend/src/api/admin.ts:285`) client-side, which bypasses `apiFetch` for a
  raw `fetch` + `blob()` + synthetic `<a download>`. Unlike the audit export, a
  bundle download **should** be audited.
- Streaming the bundle through the BFF is the simple path; a presigned MinIO URL
  avoids holding the connection open for a large tenant. If the bundle ends up
  served by an operator HTTP endpoint instead, follow the token-forwarding proxy
  in `backend/app/api/routes/credentials.py` (`_forward`,
  `credential_manager_url`) — not the applifecycle API, which authenticates on
  an actor header alone.
- Polling: no admin section polls today (the only `refetchInterval` in the app
  is the notification inbox). Use TanStack Query with a function-form
  `refetchInterval` that returns `false` on a terminal phase.

**Acceptance**

- A tenant admin starts an export, watches per-app progress, and downloads the
  bundle; every action appears in the Audit tab.
- A cross-tenant request 403s in `resolve_admin_tenant` and never reaches the
  cluster.
- Polling stops when the export reaches a terminal phase.

### Phase 5 — `TenantRestore` (in place) — **implemented**

**Goal** — put a bundle back into a live tenant.

Shipped as `api/v1alpha1/tenantrestore_types.go`, `internal/backup/restore_jobs.go`
and `internal/controller/tenantrestore_controller.go`, with the drill procedure
in `docs/commands.md` §12 and §14.

Three decisions worth carrying forward:

- **`confirmTenant` is a name, not a boolean.** `force: true` is something a
  person sets once and copies forever; a name has to be looked up and matches
  only the tenant in front of them.
- **Exec is now wired**, so `quiesce.mode: command` runs the profile's real
  maintenance hooks instead of falling back to scaling, and `restore.post` /
  `restore.verify` run at all. It still falls back when there is no ready pod
  or no execer — the data guarantee is identical either way, and refusing to
  back up an app because its pod is unhealthy would be the wrong trade.
- **The identity is asked for at restore time.** A recipient bundle needs the
  age identity that lives off-cluster with the recovery kit, so a restore is
  where an operator demonstrates they still have it. Keeping a copy on the
  cluster would defeat the escrow.

Restores replace rather than merge — `pg_restore --clean`, `DROP DATABASE`,
`mc mirror --remove` — because anything the bundle does not contain has no
business surviving a restore that claims to return the app to that point.
Volumes are the exception: `excludePaths` mean the archive is not always a
complete picture, so wiping the target would turn a documented omission into
data loss.

**Files** — `api/v1alpha1/tenantrestore_types.go`,
`internal/controller/tenantrestore_controller.go`, the same `internal/backup/`
package in reverse, console wiring as in Phase 4, a `tenants restore` verb in
`scripts/kubectl-gentian` for the cluster-admin path, and
`docs/design/operations.md` §2.

**Detail**

- Spec: a bundle reference (bucket/prefix, or a `TenantExport` name), an
  optional `apps` subset, and an explicit confirmation field. Status mirrors
  export.
- Preflight, before touching anything: manifest app set versus installed apps;
  refusal of a bundle produced by newer app versions (`minRestoreVersion` plus
  the manifest's chart versions); `SHA256SUMS` verified; decryption confirmed.
- Per app: quiesce → drop and recreate databases from the dumps → mirror the
  buckets back → restore PVCs → `restore.post` hooks → `restore.verify` →
  resume. Realm re-import and bound-secret re-seeding into OpenBao happen
  before any app resumes.
- **Take the app lifecycle lock.** `internal/applifecycle/service.go:81`
  `lockApp(tenant, profile)` exists for exactly this hazard, and its doc comment
  describes it: a restore racing an install or purge of the same app. A restore
  that ignores the lock can write into a database being dropped.
- Keycloak: `partialImport`, alongside the export call added in Phase 3. Per the
  v1 decision passwords are not preserved — trigger reset emails, and say so in
  the console *before* the operation starts, not after.
- Console: a typed-confirmation dialog naming the tenant; audited.
- Document restore-into-fresh (apply the snapshotted `tenant.yaml`, wait for
  `Phase=Ready`, then the same CR) as a cluster-admin procedure — same Jobs, no
  new mechanism.

**Acceptance**

- Export → mutate → restore round-trips a two-app tenant, `restore.verify`
  passes, and other tenants are demonstrably untouched.
- A bundle newer than the installed apps is refused in preflight, before any app
  is quiesced.
- Export and restore cannot overlap, and neither runs concurrently with an
  install or purge of the same app.

### Phase 6 — Continuous protection — **scheduling done, cluster-level not started**

**Goal** — protection that does not depend on someone clicking Export.

`TenantExportSchedule` ships: a cron expression in UTC, `suspend`, `keepLast`
retention, and `status.lastSuccessfulTime` — the field to alert on, because a
schedule that fires nightly and never succeeds looks healthy by every other
measure. A new schedule does not fire immediately (that would pause a tenant's
apps as a side effect of writing YAML), a window missed by more than an hour is
skipped rather than caught up, and retention never deletes a running export,
whose paused apps would be stranded.

One bug worth recording: `robfig/cron` evaluates an expression in the *location
of the timestamp it is given*, so a `lastScheduleTime` read back in a local zone
shifted every firing by that zone's UTC offset. The schedule field promises UTC;
it now converts explicitly on both sides. A test caught it, not review.

**Still not started, and genuinely separate infrastructure:** CNPG
`ScheduledBackup` with WAL archiving, MinIO versioning and replication, Velero,
and object-lock on the backup bucket. Those protect against losing the *cluster*
rather than losing a tenant's data, and none of them can be configured
meaningfully without the cluster in front of you.

**Detail**

- CNPG `ScheduledBackup` + WAL archiving to object storage for cluster-level
  PITR. The operator already ships the CRDs; no `barmanObjectStore` is
  configured on any Cluster today.
- Scheduled `TenantExport`s: either a schedule field reconciled by an in-process
  ticker — `internal/controller/metering_job.go`'s `MeteringWorker` is the
  existing `mgr.Add` precedent — or the `XBackup` claim of §7. Prefer the claim
  if backup policy should be reviewable in Git per tenant.
- Retention GC for bundles, modelled on `kernel/manifests/job-gc/`, the only
  CronJob in the tree.
- MinIO versioning and replication, plus object-lock on the backup bucket so a
  compromised cluster cannot erase its own backups.
- Velero (or CSI snapshots) for the residue, with ESO-managed Secrets excluded
  explicitly.

**Acceptance**

- PITR restores the shared cluster to an arbitrary point.
- A tenant's schedule exists with no Git object beyond its claim.
- A deleted object is recoverable without a cluster-level restore.
- A Velero backup's contents verifiably exclude ESO Secrets.
- Expired bundles are collected, and retention is stated per cluster.

### Phase 7 — Restore drills

**Goal** — turn the hypothesis into a measured RTO.

A scheduled exercise against a scratch cluster: a full rebuild from recovery kit
plus backups, timed end to end; and a per-tenant restore on a cluster with more
than one tenant, with the others verified unaffected. This is also where
Phase 1's two open acceptance items finally close — that `--recover` derives
byte-identical credentials on a fresh cluster, and that kit-plus-Git is
sufficient to rebuild.

**Acceptance**

- Both drills are performed and timed at least once, and the measured times are
  published as the RTO rather than an assumed one.
- Every drill failure becomes a fix in an earlier phase, not a note in this
  document.

---

## 10. Open questions

| Question | Notes |
|---|---|
| RPO and RTO per layer | Nothing above can be sized without these. Config is RPO=0; tenant data is probably minutes; credentials are hours |
| Kit custody | Who holds the kit, where, and how a restore is authorised — policy, not technology |
| When to re-export the kit | Nothing prompts for one today; a reminder on credential rotation or an unattended scheduled export would both work |
| Who holds the recovery-kit identity | Settled in code: both custody models are supported per export (§9, Phase 3). What remains is operational — the identity matching `backupRecipients` must live off-cluster with the kit, and nothing yet enforces or checks that |
| Preserving Keycloak passwords | v1 resets them on restore. The alternative is offline `kc.sh export` — includes hashes, whole-realm, and makes the bundle markedly more sensitive |
| Export size and quota | Whether the backup bucket counts against tenant quota, and whether exports have a size cap |
| Shared versus per-tenant database instances | Per-tenant CNPG clusters would make per-tenant PITR possible, at a cost in footprint. Worth costing before Phase 6 hardens the current shape |
| Backup of the deployments repository | It is a Git remote, so it is somebody's backup already — but whose, and is that stated anywhere? |
| Air-gapped restore | The recovery kit assumes the mirror in `versions.yaml` is still reachable. A restore during an outage of that mirror is a different exercise |
