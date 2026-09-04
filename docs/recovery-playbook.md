# Recovery Playbook

What to do when something is gone. Three scenarios, in order of severity, each
starting from what you still have rather than from what broke.

`scripts/recovery.sh` does all three. The long form of each is kept below,
because a recovery you cannot perform by hand is one you cannot debug.

```bash
scripts/recovery.sh cluster  --kit gentian-recovery-kit-<cluster>.age
scripts/recovery.sh tenant   --tenant corp --qr backup-key.png --confirm corp
scripts/recovery.sh inspect  --tenant corp            # reads, changes nothing
```

The key can arrive however you have it — `--key-file`, `--qr` (a photo of the
printed code), `--from-vault`, or `--passphrase`. See `--help`.

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

Three sources, in order of convenience:

```bash
# a. OpenBao, when the cluster escrows it (spec.backup.escrowIdentity, default true)
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > id.txt

# b. The recovery kit — BACKUP_AGE_IDENTITY
# c. The printed QR code — scan it, paste the AGE-SECRET-KEY- line into id.txt
```

Confirm it is the right key before relying on it: its public half must match
what the bundle was encrypted to.

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

The key goes into a Secret in your namespace, and is deleted afterwards:

```bash
# passphrase bundle
kubectl create secret generic restore-key -n tenant-<t> --from-literal=passphrase='…'
# own-key bundle
kubectl create secret generic restore-key -n tenant-<t> --from-file=identity=id.txt
```

...referenced as `passphraseSecretRef` or `identitySecretRef` in the
`TenantRestore` from scenario 2.

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

It resolves the bundle from the export's own status, aliases the endpoint with
the credential the operator keeps, prints `bundle-info.json` and decrypts the
manifest — writing nothing. By hand:

```bash
mc cat <alias>/<bucket>/<prefix>/bundle-info.json
mc cat <alias>/<bucket>/<prefix>/manifest.json.age | age -d -i id.txt
```

If the manifest decrypts, the bundle is readable and the key is the right one —
which is most of what a restore needs to be true.
