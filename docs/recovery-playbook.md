# Recovery Playbook

What to do when something is gone. Three scenarios, in order of severity, each
starting from what you still have rather than from what broke.

`scripts/recovery.sh` does all three. The long form of each is kept below,
because a recovery you cannot perform by hand is one you cannot debug.

### What it needs on your machine

`age` and `kubectl` you already have from the install. Two more, depending on
what you are doing:

```bash
# mc — the MinIO client, for reading bundles out of object storage
curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc -o ~/.local/bin/mc
chmod +x ~/.local/bin/mc          # macOS: brew install minio/stable/mc

# to read a QR code back in, and to write one out
sudo apt install zbar-tools qrencode
```

`jq` is needed for anything that asks the cluster, and `bao` for
`--from-vault`. Each is named at the point it is missing, so you can also just
run the command and see.

---

## Using the script

Four commands:

| | What it does | Writes anything? |
|---|---|---|
| `inspect` | reads a bundle: is it there, and does your key open it | no |
| `show-key` | prints the backup key and writes its QR code | a PNG |
| `tenant` | restores one workspace from a bundle | **replaces live data** |
| `cluster` | rebuilds the cluster from a recovery kit | **rebuilds everything** |

`inspect`, `show-key` and `tenant` each need an answer to two questions.

### Which backup?

Pick **one** of these three ways to say it:

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

For the underlying resources see [commands.md](commands.md) §11–§15; for the
tenant-facing view see [tenant-backup-guide.md](tenant-backup-guide.md).

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
# 1. Load the kit and install. Derived credentials reproduce their original
#    values from the master password and salt the kit carries.
./install.sh --recover /path/to/gentian-recovery-kit-<cluster>.age
```

The install runs as normal from there. Everything declarative — namespaces,
Crossplane compositions, Argo CD applications, app charts — comes back from
Git; the kit supplies only what Git cannot hold.

```bash
# 2. Bring the tenants back as empty shells.
kubectl gentian tenants deploy <tenant>
```

```bash
# 3. Put each tenant's data back — scenario 2 below, once per tenant.
```

### Reading the bundles before the cluster exists

The restore in step 3 needs a cluster to restore *into*, but reading a bundle
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

Send every member through a password reset from **Admin Console → Members**.
The elapsed time is your measured RTO — publish it rather than assuming one.

---

## 3. Tenant Admin — restore a tenant with your own key

**When:** the bundle was encrypted to a key you hold — either a key you minted
in the console, or a passphrase you chose for a manual backup.

> **There is no self-service restore.** The Admin Console has no restore
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

## Testing this before you need it

An untested backup is a hypothesis. [commands.md §15](commands.md) has the
drill to run on a scratch tenant.

The cheapest useful check costs nothing and touches nothing:

```bash
scripts/recovery.sh inspect --tenant <t> --from-vault
```

With no `--export` it takes the newest completed backup and says which one it
chose. Name a specific one with `--export <name>`, or point straight at storage
with `--s3-bucket` when there is no cluster to ask. It reads the bundle's
location from the export's own status, aliases the endpoint with the credential
the operator keeps, prints `bundle-info.json` and decrypts the manifest —
writing nothing. By hand:

```bash
mc cat <alias>/<bucket>/<prefix>/bundle-info.json
mc cat <alias>/<bucket>/<prefix>/manifest.json.age | age -d -i id.txt
```

If the manifest decrypts, the bundle is readable and the key is the right one —
which is most of what a restore needs to be true.
