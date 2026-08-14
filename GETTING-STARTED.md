# Installing Gentian OS

Follow these steps in order to get a cluster running. Each one says what to do
and how to tell it worked.

Re-running `./install.sh` is always safe. It reads the cluster to decide what is
already done, so a second run continues rather than restarting.

For flags, phases, troubleshooting and the non-default installs — internal
domains, mirrors, uninstalling — see
[docs/install-reference.md](docs/install-reference.md).

---

## Before you start

- **A Kubernetes cluster you are an admin on.** The installer does not create
  one. It runs 100+ pods, so a laptop-sized node pool will be tight.
- **These tools on your `PATH`:** `kubectl helm jq yq openssl curl crossplane
  python3 git`. The installer checks and names any that are missing. `bao` (the
  OpenBao CLI) installs itself to `~/.local/bin`.
- **A domain**, for example `platform.example.com`. It does not have to be
  publicly resolvable.
- **A token with read access to `gentian-deployments`.** The installer asks for
  it.

---

## 1. Clone the deployments repository

This cluster's configuration lives here, and the installer reads and writes it
on your machine. Clone it to the default location:

```bash
git clone <your-gentian-deployments-url> ~/.gentian/gentian-deployments
```

To keep it somewhere else, set `GENTIAN_DEPLOYMENTS_PATH` in step 2.

## 2. Write `install.env`

```bash
cp install.env.template install.env
```

Edit it. These are the values that matter for a first install:

| Variable | Set it to |
|---|---|
| `GENTIAN_DEPLOYMENTS_CLUSTER_ID` | This cluster's ID, e.g. `pck-cf2sw4h`. It names the directory under `clusters/` |
| `GENTIAN_DEPLOYMENTS_STAGE` | `dev`, `staging` or `prod` |
| `GENTIAN_DEPLOYMENTS_REPO` / `_BRANCH` | Your deployments repository |
| `GENTIAN_APPS_REPO` / `_BRANCH` | The app catalogue |

Leave the rest at their defaults.

## 3. Generate this cluster's configuration

```bash
./install.sh --prepare-deployment
```

It asks for the kernel domain, then writes
`clusters/<cluster-id>/kernel` into your deployments checkout: three claims,
`values.yaml` and `cluster-settings.env`. It commits nothing, pushes nothing,
and does not contact the cluster.

## 4. Edit what it generated

These files are what the cluster becomes. Read them.

- `claims/cluster.yaml` — the kernel domain, and the cluster's modes.
- `cluster-settings.env` — the exposure and mail model. Set `NETWORK_MODE` to
  `static-ip` if DNS points straight at a node, and set `NODE_IP` with it.
  Leave it `tunnel` if a reverse proxy or tunnel fronts the cluster.
- `values.yaml` — this cluster's Helm overlay.

## 5. Commit and push them

```bash
cd ~/.gentian/gentian-deployments
git add clusters/<cluster-id>
git commit -m "Add cluster <cluster-id>"
git push
```

`install.sh` never writes this directory. If a file is missing it names it and
stops, so a cluster is never configured by a default nobody read.

## 6. Preview the install

```bash
./install.sh --dry-run
```

Runs every check, prints what it would do, and changes nothing.

## 7. Install

```bash
./install.sh
```

It asks for the credentials below, validates each against the system it belongs
to, and stops before touching the cluster if any fail.

| | Required | What it is |
|---|---|---|
| Deployments repository token | yes | Read access to the repository from step 1 |
| Master password | yes | At least 16 characters |
| Chart registry credentials | no | Only for a private chart registry |
| Cloudflare DNS token | no | Only for a wildcard certificate via DNS-01 |

Then it works through the steps, reporting each before acting:

```
[A-05] cert-manager      provides: cert-manager controller and its CRDs
     check: not satisfied  →  applying
     ✓ 34s
```

## 8. Save the OpenBao keys when they appear

Two steps print keys you cannot recover later:

- **`B-02-openbao-transit-init`** — the transit instance's unseal key.
- **`B-04-openbao-init`** — the primary's recovery key and root token.

They are also written to `/tmp/openbao-transit-init.json` and
`/tmp/openbao-init.json`, which do not survive a reboot. **Put them in a
password manager before you continue.** After this the primary auto-unseals on
every restart, which is why these are easy to lose.

## 9. Export the recovery kit

Do this now, while everything is still in the shell that installed it.

```bash
./install.sh --export-recovery-kit kit.age
```

Most of this cluster is reconstructible: configuration comes from Git, and the
derived credentials are a function of the master password and the derivation
salt. The salt, though, is generated during the install and lives only in
OpenBao. Lose OpenBao's storage and the master password on its own reproduces
nothing.

The kit is that gap closed — the salt, the master password, the unseal material
and this cluster's identity, in one encrypted file. Restoring it into a fresh
cluster makes every derived credential come back byte-identical. Without it, a
rebuild gives you a working cluster with entirely different credentials, which
is a migration rather than a restore.

```bash
./install.sh --recover kit.age    # on the fresh cluster, before anything else
```

It is encrypted with `age` where available and `openssl` otherwise; there is no
unencrypted path. Store it wherever your break-glass material already lives —
anyone who can decrypt it holds every derived credential in the cluster.

To export unattended, set `GENTIAN_KIT_RECIPIENT` to an age public key (`age -p`
prompts on the terminal and so cannot run from a job); read such a kit back with
`GENTIAN_KIT_IDENTITY` pointing at the private key.

The kit does **not** back up your data, and it does not restore OpenBao — a
fresh instance issues its own unseal material. It is only the part that nothing
else can rebuild.

## 10. Check it worked

```bash
./install.sh --status
```

Every step reads `satisfied`, except steps that have nothing persistent to check
and steps that do not apply to this cluster — those read `undefined`.
`A-04-prewarm`, `C-03-provider-helm` and `D-02-gateway-wait` are always
`undefined`. `B-04-openbao-init`, `B-09-seed-secrets` and `E-02-tenant-reconcile`
re-run on every pass, because they hold per-run tokens or reconcile continuously.

```bash
make check-credentials
```

Each credential requirement reports satisfied, unset-but-optional, or missing.
This reads External Secrets Operator's sync status, so `missing` means the value
is genuinely absent from OpenBao.

The installer finishing is not the same as the cluster being ready — Crossplane
and ArgoCD keep reconciling after it exits:

```bash
kubectl get managed
kubectl get application,applicationset -n argocd
```

---

## Next steps

- **Provision a tenant** and install apps — [docs/commands.md](docs/commands.md).
- **Configure mail** — [docs/design/mail.md](docs/design/mail.md).
- **Change this cluster's configuration** — edit its claims in
  `gentian-deployments`; the cluster reconciles.
  [docs/deployment.md](docs/deployment.md) explains the layering.
- **Understand the architecture** — [docs/architecture.md](docs/architecture.md)
  and [docs/design/kernel.md](docs/design/kernel.md).
- **Back up the data** — the recovery kit in step 9 covers credentials, not
  databases or object storage.
