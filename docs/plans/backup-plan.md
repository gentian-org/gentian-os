# Backup and Recovery

**Status:** Phase 1 (the recovery kit) is implemented; Phases 2–5 are not
**Scope:** cluster recovery, tenant-level restore, credential escrow
**Applies to:** `install.sh`, OpenBao, CNPG, MinIO, tenant namespaces

---

## 1. What the architecture already solves

Most of a Gentian cluster is declarative and reconciled, so most of "restore the
cluster" is **point a new cluster at the same repositories**. That is what the
XR-and-ArgoCD design buys, and it means backup can be scoped to what genuinely
cannot be reconstructed rather than to everything.

Three things cannot be reconstructed. They are the whole problem:

1. The bootstrap material — the derivation salt, the transit unseal key, the
   primary's recovery keys.
2. Stateful workload data — PostgreSQL, MariaDB, MinIO, the Dovecot maildir.
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
| Crossplane claims and MRs | Git, then reconcile | No |
| PostgreSQL, MariaDB, MinIO, maildir | — | Yes. This is the real payload |
| Redis | it is a cache | No |

Two rows carry most of the design.

**The derivation scheme is itself a backup strategy.** Under
`secretMode: derived`, sixteen credentials are a pure function of the master
password and the salt. That turns the credential backup surface from a system
into a sealed envelope. It does not hold under `secretMode: random`, where those
sixteen are unreproducible and must be treated as data.

**Backing up ESO-materialised Secrets is actively harmful.** They are
projections of OpenBao paths. A backup of them is a copy of credentials as they
were at snapshot time, which survives rotation and sits in an archive nobody is
watching. Any Velero configuration must exclude them explicitly; the absence of
a backup here is a security property, not a gap.

---

## 3. The recovery kit

One encrypted bundle holding what no amount of reconciliation can rebuild:

```
master password        the root of the derived class
derivation salt        currently stored ONLY in OpenBao — see §7
transit unseal key     without it the transit instance cannot be unsealed
primary recovery keys  without them the primary cannot be unsealed if transit is gone
external credentials   the six in credentials.yaml, or a note of where they live
cluster identity       kernelDomain, deployments repo and branch, stage
```

It is small, changes rarely, and belongs wherever the organisation already keeps
break-glass material. It is the difference between "rebuild from Git" being a
plan and being a hope.

This part exists. `scripts/lib/recovery.sh` implements it, and `install.sh`
exposes it as `--export-recovery-kit` and `--recover` — see §6.

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
| OpenBao paths | `gentian-os/kernel/tenants/<tenant>/…` | From an OpenBao snapshot, or re-derived |
| **PostgreSQL databases** | **inside the shared CNPG cluster** | Logical dump — see below |
| **MariaDB databases** | **inside the shared MariaDB** | Logical dump |
| **Object data** | MinIO, per-tenant prefixes or buckets | Bucket-level restore |
| PVC data (e.g. maildir) | the tenant's namespace | Velero / CSI snapshot |

Because a tenant's *shape* is composed from its claim, restoring a tenant is
**re-apply the claim, then restore its data**. Nothing about the namespace,
quotas or app set needs backing up at all.

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

### Restore procedure

1. Re-apply the `Tenant` claim if the namespace is gone; wait for it to compose.
2. Restore that tenant's databases from its most recent logical dump.
3. Restore its MinIO prefix.
4. Restore any PVC data from Velero.
5. Let ESO re-materialise the Secrets. Do not restore them.

Step 5 is the one people get wrong: restoring Secrets alongside data reinstates
credentials that may since have been rotated.

---

## 5. Tooling

| Layer | Tool | Why not something else |
|---|---|---|
| PostgreSQL | CNPG `ScheduledBackup` + WAL archiving to object storage | A volume snapshot of a running database is crash-consistent at best. The CNPG operator is installed and provides the `Backup` and `ScheduledBackup` CRDs, so the capability is present — no `barmanObjectStore` is configured on any Cluster, so none of it is in use |
| Per-tenant PostgreSQL | `pg_dump` per tenant database, on a schedule | PITR cannot restore one tenant — §4 |
| MariaDB | `mariabackup`, plus per-database logical dumps | Same reasoning |
| MinIO | Bucket replication to a second endpoint, plus versioning | Versioning covers deletion; replication covers loss of the cluster |
| OpenBao | `bao operator raft snapshot save`, encrypted | Storage is Raft (`kernel/openbao/values.yaml`) |
| Everything else | Velero, with CSI snapshots where the storage class supports them | Namespaced objects and PVCs with no native path — the maildir being the obvious one |

All destinations **outside the cluster**. The cluster is what is being protected
against.

**Nothing above is in use today.** There is no Velero and no restic anywhere in
the tree, no `barmanObjectStore` on any CNPG Cluster, no `ScheduledBackup`, no
OpenBao snapshot job, and no MinIO replication. The CNPG operator ships the
backup CRDs and nothing creates one. This document describes work that has not
started, not work that needs tidying.

---

## 6. Where the installer fits — narrowly

**No `--backup`.** Backup is continuous, scheduled, retained and in-cluster. The
installer is one-shot, and §1 of the configuration plan sets its scope at four
components. A `--backup` flag would also make the installer a dependency of
restore, which is the wrong direction: recovery should need a bundle and a
cluster, not a current checkout.

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

Encryption is `age` when installed and `openssl` otherwise, with the difference
stated on the way past: age authenticates, so a tampered kit fails to decrypt
rather than yielding plausible garbage. There is no unencrypted path.

| Variable | Effect |
|---|---|
| `GENTIAN_KIT_RECIPIENT` | An age public key. Encrypt to it instead of prompting — the only way to export unattended, since `age -p` reads its passphrase from the terminal |
| `GENTIAN_KIT_IDENTITY` | The matching private key file, needed to read a kit written that way |

On import the kit is parsed against a fixed key whitelist rather than sourced.
Encrypted is not the same as trusted, and sourcing it would let a key name in the
file decide what the installer sets. Values already in the environment win, so an
explicit override on the command line is not silently undone by the kit.

What it does **not** do is restore OpenBao. A fresh instance initialises itself
and issues new unseal material; the keys in the kit belong to the old one and
unseal a restored Raft snapshot, nothing else. Restoring that snapshot is a
separate operation, and §4 still applies for data.

**Backup policy itself should be declarative**, following the pattern already in
use: an `XBackup` claim per cluster and per tenant, composing Velero `Schedule`
and CNPG `ScheduledBackup` objects. That keeps "what is backed up and how often"
reviewable in Git and expressible per tenant, rather than a set of CronJobs
nobody reads.

---

## 7. Traps

**The salt lives only in OpenBao — unless a kit was exported.** A disaster that
loses OpenBao's storage also loses the salt, and the master password alone then
reproduces nothing. `--export-recovery-kit` is the fix, which makes *having run
it* the actual dependency: the command exists, but a kit that was never exported
protects nothing. Export after the first install, and again after any credential
that is not derived changes.

**Transit auto-unseal is a two-body problem.** The primary unseals from the
transit instance. Restore the primary without it and the cluster is sealed. The
kit needs the transit key *and* the primary's recovery keys, because either can
be the one that survives.

**Restoring into an existing cluster hits Crossplane's `external-name`
annotations** — the identity of resources already provisioned externally.
Rebuild-from-scratch avoids this entirely; partial restore does not, and it is
the reason to prefer the former.

**An untested backup is a hypothesis.** The step everyone skips is the only one
that establishes whether any of this works.

---

## 8. Implementation

Ordered by value per unit of effort.

### Phase 1 — Recovery kit — **implemented**

`install.sh --export-recovery-kit` writes an encrypted bundle;
`--recover <kit>` primes a fresh cluster from it. See §6.

**Acceptance**
- ✅ The kit contains the salt, and the salt is therefore recoverable without OpenBao.
- ✅ The kit is encrypted at rest and the tool refuses to write it unencrypted — there is no plaintext path.
- ✅ A tampered kit is rejected rather than partially loaded (age kits; openssl kits are unauthenticated by construction).
- ⬜ A cluster installed with `--recover` derives credentials byte-identical to the original. **Verified only by round-tripping the kit itself; the end-to-end claim needs a fresh-cluster run.**
- ⬜ Losing the cluster and holding only the kit is sufficient to rebuild from Git. Same — this is a drill, not a unit test, and belongs with Phase 5.

### Phase 2 — Database backups

CNPG `ScheduledBackup` with WAL archiving, plus per-tenant logical dumps.

**Acceptance**
- PITR restores the shared cluster to an arbitrary point.
- A single tenant's databases restore without affecting any other tenant.
- Dump schedule and retention are declared in Git, not in a CronJob nobody reviews.

### Phase 3 — Object and volume backups

MinIO replication and versioning; Velero for the residue.

**Acceptance**
- ESO-managed Secrets are excluded, asserted by inspecting a backup's contents.
- A deleted object is recoverable without a cluster-level restore.
- Every PVC with no native backup path is covered.

### Phase 4 — Declarative policy

`XBackup` claim composing the schedules above.

**Acceptance**
- Adding a tenant produces its backup schedule with no new Git object beyond the claim.
- Backup policy is visible with `kubectl get xbackup`.

### Phase 5 — Restore drills

A scheduled exercise restoring into a scratch cluster.

**Acceptance**
- A full rebuild from kit plus backups is performed and timed at least once.
- A per-tenant restore is performed against a cluster with more than one tenant,
  and the other tenants are verified unaffected.
- The measured times become the stated RTO, rather than an assumed one.

---

## 9. Open questions

| Question | Notes |
|---|---|
| RPO and RTO per layer | Nothing below can be sized without these. Config is RPO=0; tenant data is probably minutes; credentials are hours. They should be stated before tools are chosen |
| Kit custody | Encryption is settled — age, falling back to openssl. Who holds the kit, where, and how a restore is authorised remains open, and is a policy question rather than a technical one |
| When to re-export | The kit is only as current as its last export. Nothing prompts for one today; a reminder on credential rotation, or an unattended scheduled export using `GENTIAN_KIT_RECIPIENT`, would both work |
| Shared versus per-tenant database instances | Per-tenant CNPG clusters would make per-tenant PITR possible and remove the need for logical dumps, at a cost in footprint. Worth costing before Phase 2 hardens the current shape |
| Tenant restore and OpenBao | Whether a tenant's paths are restored from a snapshot or re-derived. Re-derivation is cleaner but only works for the derived class |
| Backup of the deployments repository | It is a Git remote, so it is somebody's backup already — but whose, and is that stated anywhere? |
| Air-gapped restore | The recovery kit assumes the mirror in `versions.yaml` is still reachable. A restore during an outage of that mirror is a different exercise |
