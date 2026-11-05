---
audience: operator
type: guide
---

# Recovery Playbook

What to do when something is gone. Three scenarios, in order of severity, each
starting from what you still have rather than from what broke.

`scripts/recovery.sh` does all three. The long form of each is kept below,
because a recovery you cannot perform by hand is one you cannot debug.
The vault has its own two, by hand: restoring it and the transit seal that
unseals it from their daily snapshots (§4), and moving both from file to
Raft storage, which is what makes those snapshots possible (§5).

### What it needs on your machine

`age` and `kubectl` you already have from the install. Two more, depending on
what you are doing:

```bash
# rclone, for reading bundles out of object storage: any S3 store, the
# platform's own or an external destination (https://rclone.org/install/).
sudo apt install rclone        # or: brew install rclone

# to read a QR code back in, and to write one out
sudo apt install zbar-tools qrencode
```

The installer puts `bao` in `~/.local/bin`. If it is not found, that directory
is not on this shell's `PATH` — `~/.profile` adds it only when the directory existed at login, so the
first thing you ever install there is invisible until you say so:

```bash
export PATH="$HOME/.local/bin:$PATH"                      # this shell
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc  # and future ones
```

`jq` is needed for anything that asks the cluster, and `bao` for
`--from-vault`. Each is named at the point it is missing, so you can also just
run the command and see.

---

## Using the script

Five commands:

| | What it does | Writes anything? |
|---|---|---|
| `list` | what backups exist for a workspace | no |
| `inspect` | reads a bundle: is it there, and does your key open it | no |
| `show-key` | prints the backup key and writes its QR code | a PNG |
| `tenant` | restores one workspace from a bundle | **replaces live data** |
| `cluster` | rebuilds the cluster from a recovery kit | **rebuilds everything** |

`inspect`, `show-key` and `tenant` each need an answer to two questions.

### Which backup?

`list` answers that first — every backup, oldest to newest, with how each was
encrypted and where it went:

```bash
scripts/recovery.sh list --tenant corp
```

```
export-2026-09-03-14-32   Ready  2026-09-03T14:37:43Z  recipient   https://sos-ch-dk-2.exo.io/bigbucket
policy-20260904-0300      Ready  2026-09-04T03:04:43Z  recipient   https://sos-ch-dk-2.exo.io/bigbucket
export-2026-09-04-04-54   Ready  2026-09-04T06:49:13Z  passphrase  platform/corp-gentian-backup
```

The mode column tells you which key you will need: `recipient` wants
`--from-vault`, `--key-file` or `--qr`; `passphrase` wants `--passphrase`. With
no cluster left to ask, `--s3-bucket` lists the prefixes in storage instead —
less than the cluster knows, and still enough to restore from.

Then pick **one** of these three ways to name it:

| | Use when |
|---|---|
| `--tenant corp` | the cluster is up. Takes the newest completed backup and says which one it chose. |
| `--tenant corp --export <name>` | you want a specific one — `kubectl get tenantexports -n tenant-corp` lists them. |
| `--s3-endpoint URL --s3-bucket B --s3-prefix P` | there is no cluster to ask. Add `--s3-access-key` / `--s3-secret-key`, or set `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`. |

`show-key` needs none of them — it only deals with the key.

### Which key?

Pick **one**. Everything below ends up as the same thing, so use whichever form
you actually have:

| | Use when |
|---|---|
| `--from-vault` | the cluster is up and escrows the key. **This is the default** if you name no key at all. |
| `--key-file id.txt` | you have the `AGE-SECRET-KEY-…` line in a file — from the recovery kit, say. |
| `--qr photo.png` | you have the printed QR code. Hand it the image; it decodes it. |
| `--passphrase` | the backup was made with a passphrase rather than a key. Prompts for it. |

### The rest

| | |
|---|---|
| `--confirm NAME` | **required by `tenant`**, and must equal `--tenant`. It replaces live data; this is the guard. |
| `--kit PATH` | **required by `cluster`**. The recovery kit to rebuild from. |
| `--apps a,b` | restore only these apps. Default: every app in the bundle. |
| `--dry-run` | print the `TenantRestore` instead of applying it. |

### Put together

```bash
# is last night's backup readable?
scripts/recovery.sh inspect --tenant corp

# same question, from a laptop, with nothing but the bucket and the paper key
scripts/recovery.sh inspect --s3-endpoint https://sos-ch-dk-2.exo.io \
    --s3-bucket bigbucket --s3-prefix policy-20260904-0300 \
    --s3-access-key ... --s3-secret-key ... --qr backup-key.png

# put a workspace back from a specific backup
scripts/recovery.sh tenant --tenant corp --export policy-20260904-0300 \
    --from-vault --confirm corp

# rebuild the cluster itself
scripts/recovery.sh cluster --kit gentian-recovery-kit-ifk-w4h.age
```

For the underlying resources see [commands.md](../reference/commands.md) §11–§15; for the
tenant-facing view see [tenant-backup.md](tenant-backup.md).

---

## What each scenario needs

| | Recovery kit | Backup key | Bundles | Git |
|---|---|---|---|---|
| **1.** Rebuild the cluster | **required** | required for tenant data | required for tenant data | **required** |
| **2.** Restore a tenant, cluster key | no | **required** | **required** | no |
| **3.** Restore a tenant, tenant key | no | tenant's own key or passphrase | **required** | no |

Two different secrets, and confusing them wastes the worst hour of the year:

- **The recovery kit** (`gentian-recovery-kit-<cluster>.age`) holds the master
  password, derivation salt and unseal material. It rebuilds a *cluster*. It
  does not open a backup.
- **The backup key** (`AGE-SECRET-KEY-…`) opens *bundles*. It does not rebuild
  anything. It lives in the kit, in OpenBao at
  `gentian-os/kernel/backup/identity` when the cluster escrows it (the
  default), and on the QR code the kit export prints.
- **The bundle signing key** (`BUNDLE_SIGNING_SEED` in the kit) proves a
  bundle is this cluster's. Rebuilt with the kit and the same deployments
  repository, the cluster keeps it, and restores of its earlier bundles bring
  back the rights granted in their tenants; without it they bring back only
  what was withdrawn.

---

## 1. Cluster Admin — rebuild the whole cluster

**When:** the cluster is gone. Hardware, OpenBao, everything. What survives is
the recovery kit, the `gentian-deployments` repository and the bundles in
object storage.

### Before you start

- A fresh Kubernetes cluster you are admin on.
- `kubectl helm jq yq openssl curl crossplane python3 git age age-keygen` on
  your `PATH`, and the kit's passphrase.
- The same `gentian-deployments` repository and cluster id as the original —
  the rebuild reproduces *that* cluster, not a new one.

### Steps

```bash
scripts/recovery.sh cluster --kit /path/to/gentian-recovery-kit-<cluster>.age
```

which is a wrapper over:

```bash
# 1. Load the kit and install. With `secretMode: derived` (the default) the
#    kernel's service credentials and each app's reproduce their original
#    values from the master password and salt the kit carries. With
#    `secretMode: random` they are generated anew: the kit does not hold
#    them. An app's own secrets come back with each tenant's bundle, in
#    both modes.
./install.sh --recover /path/to/gentian-recovery-kit-<cluster>.age
```

The install runs as normal from there. Everything declarative — namespaces,
Crossplane compositions, Argo CD applications, app charts — comes back from
Git; the kit supplies only what Git cannot hold.

```bash
# 2. Bring each tenant back from its newest bundle: the import declares the
#    tenant from the bundle's manifest, waits for the shells, restores into
#    them. One command per tenant.
kubectl gentian tenants import <tenant>-<export>.gentian --identity-file <key>
```

A tenant this cluster retired keeps its name reserved: its import is refused
until `--reuse-name` approves it, and then it takes back the rights and
memberships the tenant left
([data-lifecycle.md §4](../concepts/data-lifecycle.md)).

A bundle that is still in a bucket the cluster can reach needs no file:

```bash
kubectl gentian tenants import --bucket <tenant>-gentian-backup --prefix <export> \
  --identity-file <key>                      # the platform's own storage
kubectl gentian tenants import --bucket bigbucket --prefix policy-20260904-0300 \
  --endpoint https://sos-ch-dk-2.exo.io --identity-file <key>   # a destination
```

A destination's credentials are the policy's, already in the cluster once
the kernel is back; the Operations Console's Recovery screen does the same
from a list.

### Reading the bundles before the cluster exists

The restore in step 2 needs a cluster to restore *into*, but reading a bundle
does not. With the bucket, its credentials and the key, `inspect` works from a
laptop:

```bash
scripts/recovery.sh inspect \
  --s3-endpoint https://sos-ch-dk-2.exo.io \
  --s3-bucket bigbucket --s3-prefix policy-20260904-0300 \
  --s3-access-key ... --s3-secret-key ... \
  --qr backup-key.png
```

Credentials come from those flags first, then `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, and only then from the cluster — so this answers "is
the data still there and can I open it" while the cluster is still gone. Worth
doing before you start rebuilding, because the answer changes what you do next.

### What does not come back

- **Member passwords.** Keycloak's export carries no hashes. Every member needs
  a reset; `status.passwordResetRequired` says so.
- **Anything written after the last bundle.**
- **The backup key**, if the old cluster escrowed it and the kit predates the
  escrow. Check the kit carries `BACKUP_AGE_IDENTITY` *before* you need it.

---

## 2. Cluster Admin — restore a tenant with the cluster key

**When:** one tenant's data is lost or corrupted, the cluster is healthy, and
the bundle was taken with **Platform key** encryption — every scheduled backup,
and any manual one that did not choose a passphrase.

> A restore **replaces** live data. Everything written since the bundle was
> taken is gone. Apps are paused one at a time while it runs.

### Get the identity

Three places it can be. Add the matching flag to any `recovery.sh` command:

```bash
# in OpenBao, because the cluster escrows it (the default)
scripts/recovery.sh inspect --tenant corp --from-vault

# in the recovery kit — the BACKUP_AGE_IDENTITY line, saved to a file
scripts/recovery.sh inspect --tenant corp --key-file id.txt

# on paper — hand it the photo, it decodes the QR itself
scripts/recovery.sh inspect --tenant corp --qr backup-key.png
```

Nothing is transcribed by hand, which is the point of printing a
machine-readable code.

By hand, the same three:

```bash
# from OpenBao
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > id.txt
# from the kit: the BACKUP_AGE_IDENTITY line
# from the QR: zbarimg --raw photo.png > id.txt
```

Confirm it is the right key before relying on it — its public half must match
what the bundle was encrypted to. `scripts/recovery.sh` prints the public half
for exactly this reason; by hand:

```bash
age-keygen -y id.txt
kubectl get tenantexport <export> -n tenant-<t> -o jsonpath='{.status.encryption.recipients}'
```

### Restore

```bash
scripts/recovery.sh tenant --tenant <t> --export <export> \
                           --from-vault --confirm <t>
```

`--dry-run` prints the `TenantRestore` instead of applying it. By hand:

```bash
kubectl create secret generic backup-identity -n tenant-<t> --from-file=identity=id.txt

kubectl apply -f - <<'YAML'
apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata: {name: restore-<date>, namespace: tenant-<t>}
spec:
  exportRef: <export>          # or bundle: {endpoint, bucket, prefix, region}
  confirmTenant: <t>           # must equal the tenant, or it refuses
  decryption:
    identitySecretRef: {name: backup-identity}
YAML

kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  tenantrestore/restore-<date> -n tenant-<t> --timeout=60m
```

Use `bundle:` instead of `exportRef:` when the `TenantExport` object is gone —
after a rebuild, or when restoring a bundle this cluster did not produce.
`status.bundle` on the original export names the endpoint, bucket, prefix and
credential.

### Afterwards

```bash
kubectl delete secret backup-identity -n tenant-<t>
kubectl get tenantrestore restore-<date> -n tenant-<t> \
  -o jsonpath='{.status.startedAt} -> {.status.completedAt}{"\n"}'
```

Send every member through a password reset from the administration console's
**Members** screen.
The elapsed time is your measured RTO — publish it rather than assuming one.

---

## 3. Tenant Admin — restore a tenant with your own key

**When:** the bundle was encrypted to a key you hold — either a key you minted
in the console, or a passphrase you chose for a manual backup.

> **There is no self-service restore.** The administration console has no restore
> button, and no API behind one. A restore is a cluster-administrator
> operation, so this scenario is a handover, not a procedure you run.

### What to give your cluster administrator

1. **Which backup** — the name from the Backup tab.
2. **Which apps**, if you want only some restored.
3. **The key**, and this is the part only you have:
   - a **passphrase** bundle → the passphrase;
   - an **own-key** bundle → the `AGE-SECRET-KEY-…` identity, from the file the
     console had you download or the QR you printed.

Without it nobody can help you — that is the guarantee you chose, and it holds
against your provider too.

### What they will do with it

Whichever form you hand over — a key file, the QR image itself, or a passphrase
typed at the prompt so it is never written down:

```bash
scripts/recovery.sh tenant --tenant <t> --key-file yours.txt --confirm <t>
scripts/recovery.sh tenant --tenant <t> --qr yours.png     --confirm <t>
scripts/recovery.sh tenant --tenant <t> --passphrase       --confirm <t>
```

Each stages the key as a Secret in your namespace, runs the restore, and
deletes the Secret afterwards — the key is not left on the cluster.

### Expect afterwards

- **Nobody can sign in until passwords are reset.** Backups hold no passwords.
  Plan for it: the data is all there and the workspace looks broken because
  nobody can get in.
- **Everything written after the backup is gone.** If you are restoring because
  of a mistake rather than a loss, ask for a fresh backup first, so the current
  state is recoverable too.

---

## When a restore wedges

An app stuck in `.status.quiesced` is offline. The operator resumes anything it
finds paused on the next reconcile, so deleting the stuck resource is the
recovery — the workload's `gentianos.io/pre-export-replicas` annotation records
what it should be scaled back to.

That depends on the operator still reconciling. Establish that first: a
`TenantExport` carries a finalizer, and deleting it against a stopped
controller hangs rather than resuming anything.

---

## Writing configuration when the director is down

### When this applies

The director is the only identity that may push `gentian-deployments`
(branch protection) and the only signer Argo CD accepts
(`AppProject.spec.sourceIntegrity`). If the director cannot serve — its
pods are down, its issuer is unreachable, its git credential is revoked —
no configuration change can reach the cluster through the normal path.
Most incidents do not need one: a cluster keeps running on the last
synced state. This procedure is for the case where a configuration change
is itself the fix.

### What the recovery kit holds for it

Sealed with the kit's `age` key, alongside the OpenBao recovery material:

- **`break-glass-push`** — a git credential for `gentian-deployments` with
  push rights on `main`, held by no running process. Branch protection
  lists exactly two identities that may push: the director and this one.
- **`break-glass-signing.key`** — a GnuPG key whose id is listed in the
  `sourceIntegrity` policy next to the director's. Argo CD accepts a commit
  signed with either; it refuses everything else.

Both are rotated after every use (step 6).

### Procedure

1. **Declare.** Add yourself to `gentian:platform:break-glass` in the
   kernel realm. The group has no standing members, so the membership
   change is the incident's first audit line, in the Keycloak event log,
   before anything is written.
2. **Open the kit** and extract the two credentials to a workstation that
   will be wiped afterwards. Never onto a cluster node.
3. **Make the change in a clone, signed.** Commit as yourself with the
   break-glass key, and put the incident in the trailer:

       Gentian-Break-Glass: incident=<id> reason=<one line>

   Push to `main`. The director's absence is why this is allowed; the
   trailer is what lets the auditor find it later.
4. **Argo CD syncs** the commit like any other: the signature verifies
   against the break-glass key id. If Argo refuses — the key is not in the
   policy, or the policy was itself the casualty — edit
   `AppProject/gentian` directly with `kubectl` (a kernel object; this is
   what break-glass `kubectl` on `kernel-*` is for) and restore the policy
   as part of the same incident.
5. **Bring the director back.** The operator reconciles the OpenFGA store
   from git, so a change to roles or grants made in step 3 is reflected
   without a second manual step. Group memberships do not come from git: if
   the store itself was lost, they return from Keycloak as each person's
   groups next change (#642).
6. **Rotate and close.** Revoke `break-glass-push` at the git host and issue
   a new one; retire the signing key id from the `sourceIntegrity` policy
   and add the replacement; re-seal both into the kit; remove yourself from
   the break-glass group. The incident is closed when the auditor can
   see, for one incident id: the group change (issuer log), the commit
   (change log), and the rotation.

## What this does not cover

- The **git host** itself being down: nothing can be synced; wait, or
  restore the host from its own backup. Argo CD keeps applying the last
  fetched state.
- **OpenBao** sealed or lost: that is §4. The director
  needs OpenBao only to hand a pull credential to the custodian;
  configuration writes do not.
- A **compromised director**: rotate its push credential and signing key
  first (step 6, before step 3), then proceed. The commits it made are in
  the change log with its key id; the auditor decides which to revert.

## Test

Quarterly, on the test cluster: take the director to zero replicas, run
steps 1–6 for a harmless change (a tenant `displayName`), and confirm the
three audit lines join on the incident id. The test is the proof that the
kit's credentials are current.

## Testing this before you need it

An untested backup is a hypothesis. [commands.md §15](../reference/commands.md) has the
drill to run on a scratch tenant.

The cheapest useful check costs nothing and touches nothing:

```bash
scripts/recovery.sh inspect --tenant <t> --from-vault
```

With no `--export` it takes the newest completed backup and says which one it
chose. Name a specific one with `--export <name>`, or point straight at storage
with `--s3-bucket` when there is no cluster to ask. It reads the bundle's
location from the export's own status, points rclone at the endpoint with the
credential the operator keeps, prints `bundle-info.json` and decrypts the manifest —
writing nothing. By hand:

```bash
rclone cat <remote>:<bucket>/<prefix>/bundle-info.json
rclone cat <remote>:<bucket>/<prefix>/manifest.json.age | age -d -i id.txt
```

(an rclone remote set up as [commands.md](../reference/commands.md) §11 shows, from the
environment, for the platform's own store or an external one).

If the manifest decrypts, the bundle is readable and the key is the right one —
which is most of what a restore needs to be true.

---

## 4. OpenBao: the daily snapshots, and restoring them

**When:** the storage of the vault, of the transit seal that unseals it, or
of both is lost or damaged on a cluster that otherwise runs (a volume is
gone, or it holds data that must go back to an earlier day), and they are on
Raft storage. Under `secretMode: random` this is the only way back to the
credentials drawn at random
([security/design.md §6](../security/design.md)).

The vault (`openbao`, in `kernel-secrets`) is sealed by a key held in the
transit seal (`openbao-transit`, in `kernel-seal`). That key lives nowhere
else: it is not exportable, and the seal is snapshotted for that reason.
Without a snapshot of the seal, no snapshot of the vault could be unsealed
once the seal's volume is lost.

### 4.1 What is taken, and where it is

| | The vault | The transit seal |
|---|---|---|
| **Who** | CronJob `openbao-snapshot` in `kernel-secrets` | CronJob `openbao-transit-snapshot` in `kernel-seal` |
| **Logs in as** | Role `openbao-snapshot` of the vault, composed by the `Cluster` claim | Role `openbao-transit-snapshot` of the seal, written when the seal is set up or moved (`kernel/openbao/transit-snapshot-policy.hcl`) |
| **Token for** | Audience `vault://openbao-snapshot` | Audience `vault://openbao-transit-snapshot` |
| **Where** | `gentian-openbao-snapshots/openbao/openbao-<YYYYMMDD>T<HHMMSS>Z.snap.age` | `gentian-openbao-snapshots/openbao-transit/openbao-transit-<YYYYMMDD>T<HHMMSS>Z.snap.age` |
| **Unsealed after a restore by** | The seal, by itself | `TRANSIT_UNSEAL_KEY` from the recovery kit |

Both jobs are `charts/gentian-os/templates/openbao-snapshots.yaml`, run the
same script and share the following:

| | |
|---|---|
| **When** | Every day at 02:17 UTC; `openbaoSnapshots.schedule` in the cluster's values |
| **Kept** | The newest 14 of each; `openbaoSnapshots.keep`. Older ones are deleted after a new one is written |
| **Bucket** | `gentian-openbao-snapshots` of the platform's object store |
| **Encrypted to** | The backup recipient: `backupRecipients` in the cluster's values when they pin one, else the one the recovery kit wrote to `gentian-os/kernel/backup/recipients`. With neither, the jobs refuse |
| **Opened by** | The backup identity, `BACKUP_AGE_IDENTITY` in the recovery kit, or its QR code |

Each job logs in with a token issued for its own ServiceAccount and its
instance's audience alone. Each role may read `sys/storage/raft/snapshot` and
nothing else: no default policy, five minutes to live. The snapshot passes
from the instance through `age` in a pipe, so the plaintext never touches a
disk and never leaves the pod. The operator makes the bucket and, for each
job, a key that may read, write and list that job's own prefix (`openbao/`
or `openbao-transit/`) and nothing else, so neither job can read or delete
the other's snapshots. It keeps them in the vault at
`gentian-os/kernel/openbao-snapshots/openbao` and
`gentian-os/kernel/openbao-snapshots/openbao-transit`, and points both at
`openbaoSnapshots.bucket` whenever that changes. Beside its snapshots each
job leaves an empty object, `storage.raft` or `storage.file`, saying what it
found.

An instance on file storage is not snapshotted: its job logs `NOT ON RAFT`
and exits, and §5 moves it to Raft.

**A snapshot of the vault holds its own key.** Where the cluster escrows the
backup identity (`backup.escrowIdentity`, on by default), its private half
is in the vault at `gentian-os/kernel/backup/identity`, and so inside every
snapshot of the vault. That copy is of no use for opening the snapshot,
which is why the recovery kit's copy is the one that counts: keep the kit
where the cluster is not. The kit's `TRANSIT_UNSEAL_KEY` is likewise what
opens a restored seal; the seal's own copy, the Secret
`openbao-transit-unseal` in `kernel-seal`, goes with the cluster.

The snapshots are on the cluster's own object store. A copy somewhere else,
for the loss of the whole cluster, is yours to make: each object is
encrypted, and can be copied as it is.

### 4.2 Is there a recent one

The operator reads the bucket every ten minutes and writes what it finds on
the `Cluster` claim's status, beside the platform's version: the vault's in
`status.openbaoSnapshots`, the seal's in `status.transitSnapshots`, the same
fields in both:

```bash
kubectl get clusters.gentianos.io -n kernel-provisioning \
  -o jsonpath='{.items[0].status.openbaoSnapshots}{"\n"}{.items[0].status.transitSnapshots}{"\n"}'
```

| `state` | Means |
|---|---|
| `Current` | The newest snapshot is younger than `staleAfter` (30 hours) |
| `Stale` | It is older. The last Job of the instance's CronJob says why |
| `Missing` | There is none in the bucket |
| `NotRaft` | The instance is on file storage and none is taken (§5) |
| `Unknown` | The bucket could not be read; `message` says why |
| `Disabled` | The seal only: `openbaoSnapshots.transit.enabled` is false and none is taken |

Beside it: `instance` (`openbao` or `openbao-transit`), `storage`, `newest`
and `newestAt`, `ageSeconds`, `count`, `bucket`, `staleAfter`, `message` and
`checkedAt`. The vault's storage is asked of the vault; the seal's, which the
operator does not reach, is what its job last found. The same as metrics,
per `instance`: `gentianos_openbao_snapshot_state{instance,state}` and
`gentianos_openbao_snapshot_age_seconds{instance}`
([operations.md §6](../concepts/operations.md)).

The Operations Console reads `status.openbaoSnapshots` and
`status.transitSnapshots` of the `Cluster` claim: `state` of each for the
signal, anything but `Current` in either a warning, with that entry's
`message`, `newestAt` and `ageSeconds` beside it. Both are needed for a
restore, so the cluster's snapshot state is the worse of the two.
`checkedAt` older than half an hour means the operator has stopped writing
them.

The jobs' own word, and a snapshot taken now:

```bash
kubectl get jobs -n kernel-secrets -l app.kubernetes.io/component=openbao-snapshot
kubectl get jobs -n kernel-seal -l app.kubernetes.io/component=openbao-snapshot
kubectl logs -n kernel-secrets job/<the newest>
kubectl create job --from=cronjob/openbao-snapshot openbao-snapshot-now -n kernel-secrets
kubectl create job --from=cronjob/openbao-transit-snapshot openbao-transit-snapshot-now -n kernel-seal
```

### 4.3 Restore

**The seal first, then the vault.** The vault unseals only through the seal
that holds its key. Where only the vault's volume is lost, the seal is
running with that key and steps 1 to 3 are skipped. Where the seal's is lost,
it comes back from its snapshot first, whether or not the vault's is lost as
well. Restore the two from snapshots taken on the same day: a vault never
needs a newer seal than the one it was sealed with, and the seal's key does
not change after it is set up, so any seal snapshot taken after the seal was
set up unseals any vault snapshot.

You need: the recovery kit (its `BACKUP_AGE_IDENTITY` and
`TRANSIT_UNSEAL_KEY`, and `OPENBAO_RECOVERY_KEYS` and `OPENBAO_ROOT_TOKEN`),
`bao`, `age`, `rclone`, `jq` and `kubectl`.

> A restore **replaces** the whole instance. Every secret written to the
> vault after its snapshot is gone: under `derived` the operator writes the
> same values again; under `random`, a credential made since is lost with it.

1. **Fetch and decrypt the snapshots.** With the rclone remote `gentian:`
   for the platform's object store that
   [commands.md](../reference/commands.md) §11 sets up, and the kit's backup
   identity, the `AGE-SECRET-KEY-…` line saved to `id.txt` (or
   `zbarimg --raw backup-key.png > id.txt`):

   ```bash
   rclone lsf gentian:gentian-openbao-snapshots/openbao-transit/ | sort | tail -3
   rclone lsf gentian:gentian-openbao-snapshots/openbao/ | sort | tail -3
   rclone copyto gentian:gentian-openbao-snapshots/openbao-transit/<name> transit.age
   rclone copyto gentian:gentian-openbao-snapshots/openbao/<name> vault.age
   age -d -i id.txt -o transit.snap transit.age
   age -d -i id.txt -o vault.snap vault.age
   ```

   Each `.snap` is a whole instance, readable by anyone who can unseal it.
   Keep them on an encrypted disk and delete them when done.

2. **The seal: bring up an empty one and restore it.** Where its volume is
   lost, the StatefulSet makes a new one once the pod and the claim are
   gone; the seal starts uninitialized, on Raft. Initialize it with one share
   of one, which gives a key and a root token that live only until the
   restore, unseal it with that key, and restore with `-force`, because the
   snapshot's keys are the old seal's (`sys/storage/raft/snapshot-force`).
   The seal answers on plain HTTP inside the cluster:

   ```bash
   kubectl delete pvc data-openbao-transit-0 -n kernel-seal --wait=false   # a damaged volume; not when it is gone already
   kubectl delete pod openbao-transit-0 -n kernel-seal
   kubectl port-forward -n kernel-seal svc/openbao-transit 8210:8200 &
   export BAO_ADDR=http://localhost:8210
   bao operator init -key-shares=1 -key-threshold=1 -format=json > transit-temporary.json
   bao operator unseal "$(jq -r '.unseal_keys_b64[0]' transit-temporary.json)"
   BAO_TOKEN="$(jq -r .root_token transit-temporary.json)" \
     bao operator raft snapshot restore -force transit.snap
   rm -f transit-temporary.json transit.snap
   ```

3. **The seal: unseal it with the kit's key.** After the restore the seal is
   sealed under its restored configuration, one share of one, and only the
   old seal's key opens it: `TRANSIT_UNSEAL_KEY`, typed at the prompt.

   ```bash
   bao operator unseal          # paste TRANSIT_UNSEAL_KEY
   bao status                   # Sealed false, Storage Type raft
   ```

   The seal unseals itself on every restart from the Secret
   `openbao-transit-unseal` in `kernel-seal`. If that Secret was lost too,
   make it again from the kit, through a file rather than the command line,
   and delete the file:

   ```bash
   kubectl create secret generic openbao-transit-unseal -n kernel-seal \
     --from-file=unseal-key=transit-unseal-key.txt --dry-run=client -o yaml | kubectl apply -f -
   ```

   The vault authenticates to the seal with the token in the Secret
   `openbao-transit-token` (in `kernel-seal` and `kernel-secrets`), which the
   restored seal knows again. Only if that Secret was lost too, or the
   snapshot predates the token, does the seal need a new one: `bao operator
   generate-root` with `TRANSIT_UNSEAL_KEY`, then
   `./install.sh --only B-02`, which asks for that root token, mints a token
   for the `transit-autounseal` policy and copies it to both namespaces.

4. **The vault: unseal it.** Where the vault's volume is intact, restart it
   (`kubectl delete pod openbao-0 -n kernel-secrets`) and it unseals itself
   through the restored seal; continue at step 8.

5. **The vault: bring up an empty one.** Where its volume is lost, the
   StatefulSet makes a new one once the pod and the claim are gone; the
   vault starts uninitialized, on Raft. Initialize it; with the transit seal
   it unseals itself and answers with recovery keys and a root token that
   live only until step 6:

   ```bash
   kubectl delete pvc data-openbao-0 -n kernel-secrets --wait=false   # a damaged volume; not when it is gone already
   kubectl delete pod openbao-0 -n kernel-secrets
   kubectl port-forward -n kernel-secrets svc/openbao 8200:8200 &
   kubectl get secret openbao-tls -n kernel-secrets -o jsonpath='{.data.ca\.crt}' | base64 -d > openbao-ca.crt
   export BAO_ADDR=https://localhost:8200 BAO_CACERT=openbao-ca.crt
   bao operator init -recovery-shares=1 -recovery-threshold=1 -format=json > init-temporary.json
   ```

6. **The vault: restore** with that temporary root token, `-force` because
   the snapshot's keys are the old vault's:

   ```bash
   BAO_TOKEN="$(jq -r .root_token init-temporary.json)" \
     bao operator raft snapshot restore -force vault.snap
   rm -f init-temporary.json vault.snap
   ```

   The restored vault is sealed with the same transit key and unseals itself
   through the seal; `bao status` says `Sealed false`. If it says `true`, the
   seal is sealed or unreachable: step 3.

7. **Get back in.** The tokens are the old vault's again. The kit's
   `OPENBAO_ROOT_TOKEN` works if it was not revoked
   (`bao token lookup`); otherwise make a root token from the kit's
   recovery key with `bao operator generate-root -init`, then
   `bao operator generate-root -nonce=<nonce>`, which asks for the key, and
   `bao operator generate-root -decode=<encoded> -otp=<otp>`. A cluster
   administrator signs in as usual (`bao login -method=oidc`) and needs
   neither.

8. **Check that every service reads its store again.**

   ```bash
   bao status                                          # Storage Type raft, Sealed false
   kubectl get clustersecretstore openbao              # Valid, Ready
   kubectl get externalsecrets -A | grep -v SecretSynced
   kubectl annotate externalsecrets -A --all force-sync="$(date +%s)" --overwrite
   kubectl rollout restart deploy -n kernel-control    # the operator, director and custodian log in again
   kubectl get managed | grep vault | grep -v 'True.*True'   # provider-vault's resources, Synced and Ready
   kubectl create job --from=cronjob/openbao-transit-snapshot openbao-transit-snapshot-now -n kernel-seal
   kubectl create job --from=cronjob/openbao-snapshot openbao-snapshot-now -n kernel-secrets
   ```

   The restore is done when no `ExternalSecret` is left unsynced, the
   operator logs no OpenBao error, the vault's resources of the `Cluster`
   claim are `Synced`, and the next snapshots are written
   (`state` is `Current` in both `status.openbaoSnapshots` and
   `status.transitSnapshots`).

---

## 5. OpenBao: moving the vault and its seal from file to Raft storage

**When:** `status.openbaoSnapshots.state` or `status.transitSnapshots.state`
says `NotRaft`. A new cluster's claim states `openbao.storage: raft`, which
puts both instances on Raft; a claim written before the setting says
nothing, which is `file`, and that cluster keeps file storage for both until
they are moved by hand. This is not done by the installer.

### 5.1 How a cluster on file storage behaves until then

It runs as it did. The installer renders each instance on the storage the
claim states, and it keeps the backend a running instance has unless that
instance's volume says its data was moved (`scripts/lib/openbao.sh`,
`openbao_storage_for_render`): a claim changed to `raft` on an instance that
was not moved renders `file` and says so, because Raft started on a volume
of file data is a new, empty instance. Each instance is decided by its own
volume. The snapshot jobs log `NOT ON RAFT` every day, and the operator
reports `NotRaft`.

### 5.2 Before you start

- A recovery kit exported from this cluster that carries
  `BACKUP_AGE_IDENTITY`, `TRANSIT_UNSEAL_KEY` and `OPENBAO_RECOVERY_KEYS`.
- A few minutes of the vault being down. Running apps keep the Secrets they
  have; what reads the vault meanwhile (External Secrets, the operator, the
  custodian) retries.
- `kubectl` on the cluster, `bao`, a checkout of the release the cluster
  runs, and the deployments repository.

The seal is moved first, then the vault, each the same way. The vault must
be stopped while the seal is down, because it reaches the seal only to
unseal; it is stopped for its own move anyway.

### 5.3 Steps

1. **Keep Argo CD from starting either again**, and stop both, the vault
   first:

   ```bash
   for app in openbao openbao-transit; do
     kubectl patch application "${app}" -n kernel-gitops --type merge \
       -p '{"spec":{"syncPolicy":{"automated":null}}}'
   done
   kubectl scale statefulset openbao -n kernel-secrets --replicas=0
   kubectl wait --for=delete pod/openbao-0 -n kernel-secrets --timeout=5m
   kubectl scale statefulset openbao-transit -n kernel-seal --replicas=0
   kubectl wait --for=delete pod/openbao-transit-0 -n kernel-seal --timeout=5m
   ```

2. **Move the data** of each, in a pod that mounts its volume, with its own
   image and user. `bao operator migrate` copies the file backend into Raft
   in a scratch directory; the result is copied beside the file data, which
   stays as it is. Run this once with the seal's values and once with the
   vault's:

   | | `NS` | `STS` | `PVC` | `NODE` | `CLUSTER_ADDR` |
   |---|---|---|---|---|---|
   | The seal | `kernel-seal` | `openbao-transit` | `data-openbao-transit-0` | `openbao-transit-0` | `https://openbao-transit-0.openbao-transit-internal:8201` |
   | The vault | `kernel-secrets` | `openbao` | `data-openbao-0` | `openbao-0` | `https://openbao-0.openbao-internal:8201` |

   ```bash
   IMAGE="$(kubectl get statefulset "${STS}" -n "${NS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="openbao")].image}')"
   kubectl apply -f - <<YAML
   apiVersion: v1
   kind: Pod
   metadata: {name: openbao-migrate, namespace: ${NS}}
   spec:
     restartPolicy: Never
     securityContext:
       runAsNonRoot: true
       runAsUser: 100
       runAsGroup: 1000
       fsGroup: 1000
       seccompProfile: {type: RuntimeDefault}
     containers:
       - name: migrate
         image: ${IMAGE}
         command: ["/bin/sh", "-ec"]
         args:
           - |
             mkdir -p /scratch/raft
             printf '%s\n' \
               'storage_source "file" {' \
               '  path = "/openbao/data"' \
               '}' \
               'storage_destination "raft" {' \
               '  path    = "/scratch/raft"' \
               '  node_id = "${NODE}"' \
               '}' \
               'cluster_addr = "${CLUSTER_ADDR}"' > /scratch/migrate.hcl
             bao operator migrate -config=/scratch/migrate.hcl
             cp -a /scratch/raft/. /openbao/data/
             ls -la /openbao/data
         securityContext:
           allowPrivilegeEscalation: false
           capabilities: {drop: ["ALL"]}
         volumeMounts:
           - {name: data, mountPath: /openbao/data}
           - {name: scratch, mountPath: /scratch}
     volumes:
       - name: data
         persistentVolumeClaim: {claimName: ${PVC}}
       - name: scratch
         emptyDir: {sizeLimit: 2Gi}
   YAML
   kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/openbao-migrate -n "${NS}" --timeout=10m
   kubectl logs -n "${NS}" openbao-migrate    # "Success! All of the keys have been migrated.", vault.db and raft/ listed
   kubectl delete pod openbao-migrate -n "${NS}"
   ```

   The node id and cluster address are the ones the instance starts with:
   its pod's name, and the chart's `BAO_CLUSTER_ADDR`.

3. **Say on each volume that it holds Raft data.** The installer renders
   Raft on an existing instance only when its volume says so:

   ```bash
   kubectl annotate pvc data-openbao-transit-0 -n kernel-seal gentianos.io/openbao-storage=raft --overwrite
   kubectl annotate pvc data-openbao-0 -n kernel-secrets gentianos.io/openbao-storage=raft --overwrite
   ```

4. **Change the claim** in the deployments repository,
   `clusters/<cluster>/kernel/claims/cluster.yaml`, and commit and push it
   as every change there is made:

   ```yaml
   spec:
     openbao:
       storage: raft
   ```

5. **Render both on Raft.** From the checkout of the release the cluster
   runs, the bootstrap step alone. It writes the `openbao` and
   `openbao-transit` Applications with Raft storage and their automated sync
   again; Argo CD syncs the configuration and starts both. The seal unseals
   itself from `openbao-transit-unseal`, the vault through the seal:

   ```bash
   ./install.sh --only B-01
   ```

6. **Give the seal its snapshot role.** A seal set up before this has none,
   and its snapshot job is refused until it has. It is written with a root
   token of the seal, made from the kit's `TRANSIT_UNSEAL_KEY` and revoked
   afterwards:

   ```bash
   kubectl port-forward -n kernel-seal svc/openbao-transit 8210:8200 &
   export BAO_ADDR=http://localhost:8210
   bao operator generate-root -generate-otp > otp.txt
   bao operator generate-root -init -otp="$(cat otp.txt)"     # prints the nonce
   bao operator generate-root -nonce=<nonce>                   # paste TRANSIT_UNSEAL_KEY; prints the encoded token
   export BAO_TOKEN="$(bao operator generate-root -decode=<encoded> -otp="$(cat otp.txt)")"
   bao auth list | grep -q '^kubernetes/' || bao auth enable kubernetes
   bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
   bao policy write openbao-transit-snapshot kernel/openbao/transit-snapshot-policy.hcl
   bao write auth/kubernetes/role/openbao-transit-snapshot \
     bound_service_account_names=openbao-transit-snapshot \
     bound_service_account_namespaces=kernel-seal \
     audience=vault://openbao-transit-snapshot \
     token_policies=openbao-transit-snapshot token_no_default_policy=true \
     token_ttl=300 token_max_ttl=300
   bao token revoke -self
   unset BAO_TOKEN; rm -f otp.txt
   ```

   These are the same writes `scripts/bootstrap/init-openbao-transit.sh`
   makes when it sets up a new seal.

7. **Check:**

   ```bash
   kubectl exec -n kernel-seal openbao-transit-0 -- bao status                # Storage Type raft, Sealed false
   kubectl exec -n kernel-secrets openbao-0 -c openbao -- bao status           # Storage Type raft, Sealed false
   kubectl get externalsecrets -A | grep -v SecretSynced
   kubectl create job --from=cronjob/openbao-transit-snapshot openbao-transit-snapshot-now -n kernel-seal
   kubectl create job --from=cronjob/openbao-snapshot openbao-snapshot-now -n kernel-secrets
   ```

   The move is done when the first snapshot of each is written and
   `state` is `Current` in both `status.openbaoSnapshots` and
   `status.transitSnapshots` (§4.2).

### 5.4 Rollback

The file data of each instance is still on its volume, as it was at step 2.
Going back loses whatever was written to it on Raft since. For both, or for
the one that is to go back:

```bash
kubectl patch application openbao -n kernel-gitops --type merge \
  -p '{"spec":{"syncPolicy":{"automated":null}}}'
kubectl scale statefulset openbao -n kernel-secrets --replicas=0
kubectl annotate pvc data-openbao-0 -n kernel-secrets gentianos.io/openbao-storage=file --overwrite
# the same for openbao-transit in kernel-seal, the vault stopped first
# the claim back to openbao.storage: file, committed and pushed
./install.sh --only B-01
```

The claim's one setting covers both. Rolled back for one instance alone, it
says `file` while the other's volume says `raft`, and the installer keeps
that one on Raft and says so on every run.

Once an instance has run on Raft and its first snapshot is written, the
file backend's directories on its volume (everything in `/openbao/data` but
`vault.db` and `raft/`) are the rollback and nothing else; they can be
removed with a pod like the one in step 2, the instance stopped.
