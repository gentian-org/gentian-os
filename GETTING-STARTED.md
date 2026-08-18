# Installing Gentian OS

Follow these steps in order to get a cluster running. Each one says what to do
and how to tell it worked.

Re-running `./install.sh` is always safe. It reads the cluster to decide what is
already done, so a second run continues rather than restarting.

For flags, troubleshooting and the non-default installs — internal domains,
mirrors, uninstalling — see
[docs/install-reference.md](docs/install-reference.md).

---

## What the installer does

`install.sh` works through a directory of small steps, each named
`<phase>-<number>-<what-it-does>`. The letter is the phase; the number orders
steps within it. Five phases, in this order:

| | Phase | What it builds |
|---|---|---|
| **A** | `control-plane` | Crossplane, namespaces, cert-manager, External Secrets, ArgoCD |
| **B** | `secrets` | OpenBao, the Cluster claim, the credentials seeded into it |
| **C** | `platform` | Certificates, the root ApplicationSet, admission control, the catalogue |
| **D** | `applications` | The operator, mail, LLM serving, portal login, app profiles |
| **E** | `handover` | Tenants, per-tenant reconcile, revoking the installer's own token |

Each step reports what it found before it changes anything:

```text
[A-05] cert-manager
     provides: cert-manager controller and its CRDs
     check: not satisfied  →  applying
     ✓ 34s
```

`check: satisfied → skip` means that step's work is already present, which is
why re-running continues rather than restarting. You can run one phase with
`--phase secrets`, or one step with `--only A-05`.

From phase C onward much of the work is ArgoCD and Crossplane converging on
their own, so the installer finishing is not the same as the cluster being
ready — step 11 covers how to tell the difference.

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

It asks for the kernel domain, then writes `clusters/<cluster-id>/kernel` into
your deployments checkout: `claims/cluster.yaml`, `claims/infra-data.yaml`,
`claims/suze.yaml` and `values.yaml`. It commits nothing, pushes nothing, and
does not contact the cluster.

## 4. Edit what it generated

These files are what the cluster becomes. Read them.

- `claims/cluster.yaml` — everything that describes this cluster.
- `claims/infra-data.yaml` — the shared Postgres, MariaDB, Redis and MinIO.
- `claims/suze.yaml` — Keycloak and OpenFGA.
- `values.yaml` — this cluster's Helm overlay.

The scaffold writes only `kernelDomain`, because everything else has a default
that is right for most clusters. Set a field when your cluster differs; the
schema rejects a name it does not know, so a typo fails at `kubectl apply`
rather than being silently ignored.

The ones worth a decision:

| Field | Default | Set it when |
|---|---|---|
| `networkMode` | `tunnel` | DNS points straight at a node — then use `static-ip` **and set `nodeIp`** |
| `nodeIp` | — | Required by `static-ip`. Apps can also reference it as `${NODE_IP}` |
| `certificates.issuerMode` | `acme-dns01` | The domain is not publicly resolvable — see §8 |
| `mail.serviceMode` | `external` | You want in-cluster Postfix/Dovecot instead of a relay (`kernel`, needs `static-ip`) |
| `mail.host` | — | `external` mode: the relay's address. Its credentials are a credential, not a field |
| `storageClass` | cluster default | The cluster has more than one StorageClass |
| `tenancyMode` | `multi` | One tenant occupies the whole cluster (`single`) |
| `secretMode` | `derived` | You want independent random secrets rather than ones reproducible from the master password |
| `llm.enabled` | `false` | This cluster serves models |

`routingMode` is `gateway` and that is the only supported value.

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

Runs every step's check against the cluster and prints what it would do. It
collects no credentials and changes nothing, so run it before you have gathered
a single secret — it is how you find out what the install intends.

## 7. Have the credentials ready

The install asks for these, validates each against the system it belongs to, and
stops before touching the cluster if any fail.

| | Required | What it is |
|---|---|---|
| `deployments-repository` | yes | Read access to the repository from step 1 |
| `master-password` | yes | At least 16 characters |
| `infra-chart-registry` | no | Only for a private chart registry |
| `acme-dns-cloudflare` | no | Needed by `issuerMode: acme-dns01`, the default — see below |

Each one reaches OpenBao once it exists, at `B-09-seed-secrets`, and a later run
recovers it from there instead of asking again.

Between being typed and reaching OpenBao there is a window where an install can
fail with nothing to recover from, so validated answers are cached at
`~/.gentian/bootstrap-credentials.env` (0600, in a 0700 directory) and
`B-09-seed-secrets` deletes the cache once OpenBao holds them. Set
`GENTIAN_NO_CREDENTIAL_CACHE=1` to opt out and retype on every resumed run.

**There is no secrets file.** For an unattended run, export the credentials
instead:

| Credential | Environment variable |
|---|---|
| `deployments-repository` | `GENTIAN_DEPLOYMENTS_GIT_USERNAME`, `GENTIAN_DEPLOYMENTS_GIT_TOKEN` |
| `master-password` | `MASTER_PASSWORD` |
| `infra-chart-registry` | `REGISTRY_USER`, `REGISTRY_PASSWORD` |
| `acme-dns-cloudflare` | `CF_API_TOKEN` |

The installer reads the environment, then the cache, then OpenBao, and prompts
for whatever is still missing. A plaintext file of secrets beside the installer
was a fourth source that nothing rotated and nothing audited, so it was removed
rather than kept working.

**Everything else is supplied after the cluster is up, not now.** The SMTP relay
and the ArgoCD webhook secret are `phase: runtime`: they belong to the
credential manager on the cluster, and step 12 is where they get set. Only the
four above block a bootstrap.

**The Cloudflare token is only optional if this cluster's issuer does not need
it.** Under the default `acme-dns01`, DNS-01 issues every kernel certificate,
not just the wildcard, so an absent or rejected token leaves the cluster with no
working TLS. Set `certificates.issuerMode` on the Cluster claim to `acme-http01`
(public DNS, port 80 reachable, no wildcards) or `self-signed` (internal
domains) if you do not want to supply one.

If a value is rejected, the installer names where it came from and asks for a
replacement rather than aborting.

## 8. Install

```bash
./install.sh
```

It works through phases A to E, reporting each step before acting. Expect it to
take a while in B, where OpenBao is deployed and initialised, and in C and D,
where ArgoCD pulls and syncs the platform's own applications.

## 9. Save the OpenBao keys when they appear

Two steps print keys you cannot recover later:

- **`B-02-openbao-transit-init`** — the transit instance's unseal key.
- **`B-04-openbao-init`** — the primary's recovery key and root token.

They are also written to `/tmp/openbao-transit-init.json` and
`/tmp/openbao-init.json`, which do not survive a reboot. **Put them in a
password manager before you continue.** After this the primary auto-unseals on
every restart, which is why these are easy to lose.

## 10. Export the recovery kit

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

## 11. Check it worked

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

## 12. Supply the runtime credentials

The four from step 6 got the cluster up. The rest are supplied now, through the
credential manager the cluster runs — the SMTP relay, the ArgoCD webhook secret,
and anything a claim you add later declares.

`make check-credentials` lists what is outstanding. A claim waiting on one says
so itself, and resumes on its own when the value arrives; nothing has to be
re-run.

The manager serves `/v1/credentials` from the operator. It validates a value
against its own endpoint before storing it, so a bad token is a rejection rather
than a stalled provisioning run an hour later. It writes with **your** Keycloak
identity, not a service token, so the audit device records who set what. It
never returns a stored value — existence, who set it and when, and the last
validation result, and nothing more.

Lost credentials are rotated, not recovered.

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
