# Installing Gentian OS

Follow these steps in order to get a cluster running. Each one says what to do
and how to tell it worked.

Re-running `./install.sh` is always safe. It reads the cluster to decide what is
already done, so a second run continues rather than restarting.

**The whole install, once you are configured (steps 1–7):**

```bash
./install.sh     # installs everything, writes the recovery kit, then waits
```

It then waits for the things only you can do: move the kit somewhere safe, sign
in at `https://portal.<your-domain>/login`, and supply the runtime credentials
the portal asks for. It sees the sign-in, finishes handover itself, and prints
`Install Complete`.

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
ready — step 10 covers how to tell the difference.

Nor is it the same as the cluster being *yours*: phase E ends by revoking the
credential the installer used, and it will not do that until an administrator
has signed in. Step 9 is that handover, and the cluster holds tenants back
until it is done.

---

## What you need before you start

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
- `claims/suze.yaml` — Cluster security (incl. Keycloak and OpenFGA).
- `values.yaml` — this cluster's Helm overlay.

`--prepare-deployment` asks for the four settings that decide whether the
install works at all — domain, network mode, certificate issuer and mail — and
writes every other setting into the claim as a comment showing the default that
is in effect. So the file lists the cluster's whole configuration: a value is
either set, or commented with what it defaults to and why.

A field that does nothing in your configuration says so rather than being
absent. On a `tunnel` cluster you get:

```yaml
  networkMode: tunnel
  # nodeIp:                      not used while networkMode is tunnel
```

To change a default, uncomment the line and edit it. The schema rejects a name
it does not know, so a typo fails at `kubectl apply` rather than being silently
ignored.

The settings the file carries:

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

Type them when asked. Each reaches OpenBao once it exists, and a later run
recovers it from there instead of asking again — so a resumed install does not
re-ask. Nothing is written to this machine except a short-lived cache that step
`B-10-seed-secrets` deletes.

**Everything else is supplied after the cluster is up, not now** — you set them when you first sign in, in step 9.

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

One command, start to finish. It works through phases A to E, reporting each
step before acting, and re-running it is safe: every step checks whether its
work is already done and skips if so. Expect it to take a while in B, where
OpenBao is deployed and initialised, and in C and D, where ArgoCD pulls and
syncs the platform's own applications.

It ends in one of two ways:

- **`Gentian OS — Almost There: 1 step left`** — everything is installed and
  handover remains. The summary lists exactly what to do, in order. That is
  step 9 below, and it is the normal ending for a first install.
- **`Gentian OS — Install Complete`** — handover is done too. Nothing is left.

If a step fails, the run stops there and names the command, the step file and
the call stack. Nothing after a failure runs, so the install never reports
success over a broken step. Fix the cause and run `./install.sh` again.

Nothing secret is printed, and there is nothing to copy down: OpenBao's
initialisation material goes to mode-600 files under `~/.gentian`, and the
installer removes them once their contents are safe elsewhere.

Near the end it writes this cluster's **recovery kit** next to the checkout,
asking for a passphrase to encrypt it, and prints the filename. That file is
the one thing you have to look after — step 9.

## 9. Handover

The install pauses here and waits for you. Three things finish it:

1. **Move the recovery kit somewhere safe.** Step 8 wrote it next to the
   checkout and printed the filename. Put it where your break-glass material
   already lives — a password manager, a sealed vault, offline media. Without
   it this cluster cannot be rebuilt as itself.
2. **Sign in to the portal** as the administrator. The installer prints the URL,
   the username and the password while it waits.
3. **Supply the runtime credentials.** Once signed in, open the **Credentials**
   tab and fill in what the cluster is still missing — SMTP relay, any extra app
   repository and its pull secret.

Signing in is what the installer is waiting for: it proves someone other than
the installer can write credentials. The moment it sees that, it revokes its own
credential, deletes the temporary secret files, and prints **`Install Complete`**.

**Do the SMTP relay first.** It is not only about sending mail. Any app whose
profile asks for SMTP reads those credentials from OpenBao, and they are written
there only once the relay exists — so until you supply it, those apps do not
install at all: the tenant stays in `Provisioning` and the app's secret never
syncs. On a cluster with `mail.serviceMode: external` this is the most common
reason a first tenant appears to hang.

Interrupting the wait costs nothing. Sign in whenever you like, then:

```bash
./install.sh --only E-04
```

## 10. Check the status

```bash
./install.sh --status
```

Every step reads `satisfied`, except steps that have nothing persistent to check
and steps that do not apply to this cluster — those read `undefined`.
`A-04-prewarm`, `C-03-provider-helm` and `D-02-gateway-wait` are always
`undefined`. `B-04-openbao-init`, `B-10-seed-secrets` and `E-02-litellm-reconcile`
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

## 11. Create your first tenant

The commands below use the `gentian` CLI. Install it once, on whichever machine
you administer clusters from:

```bash
make install-plugin      # kubectl-gentian + gtnctl into ~/.local/bin
```

The installer does not do this for you, and `--uninstall` does not remove it:
one CLI serves every cluster you manage. Remove it with `make uninstall-plugin`.
Neither needs `sudo` — both write to `~/.local/bin`.

Re-run `make install-plugin` after pulling. The CLI is a copy, not a link, so it
does not follow the checkout, and `./install.sh --status` warns when the copy on
your PATH is not the one in your tree. To see which copy answers:

```bash
gtnctl version           # version, fingerprint, and any other copy on PATH
```

Scaffold the definition:

```bash
./install.sh --prepare-tenant acme
```

A tenant is authored, then deployed. Two directories, and the difference
matters:

- `definitions/tenants/<name>/tenant.yaml` — what the tenant is *meant to be*. Yours to
  edit.
- `tenants/<name>/` — what Argo CD syncs. Written by the deploy command, and
  written again by the operator every time an app is installed from the store.


It asks for a display name, then writes
`clusters/<cluster-id>/definitions/tenants/acme/tenant.yaml`. Nothing is deployed,
committed or applied.

To install apps for the tenant, log in as tenant admin and open the app store or:

```bash
kubectl gentian apps list          # what this cluster offers
```

and then if you want, add the apps of interest to the tenant profile but it is recommended to **add apps with the tenant admin through the app store.**

```yaml
  apps:
  - profile: nextcloud-base-ce
    addons:
    - nextcloud-calendar-ce
```

Quotas and mail are not in the definition. They come from this cluster's shared
`definitions/components/tenant-defaults` component, so every tenant is sized the
same way — override in the definition only what this tenant needs differently.

Then deploy it:

```bash
kubectl gentian tenants deploy acme
```

That copies the definition into `clusters/<cluster-id>/tenants/acme/`, creates
the defaults component if this is the cluster's first tenant, commits and
pushes. Argo CD creates the Tenant:

```bash
kubectl get tenant acme -w
```

To remove one, `kubectl gentian tenants undeploy acme` — not `kubectl delete`.
The directory is what the cluster reconciles towards, so deleting the object
just brings it back.

---

## Advanced install options

None of this is needed for a normal install. Skip it unless one of the headings
is the problem you have.

### Installing unattended

Set `GENTIAN_NONINTERACTIVE=1` in `install.env`, and supply the four bootstrap
credentials through the environment instead of the prompt:

| Credential | Environment variable |
|---|---|
| `deployments-repository` | `GENTIAN_DEPLOYMENTS_GIT_USERNAME`, `GENTIAN_DEPLOYMENTS_GIT_TOKEN` |
| `master-password` | `MASTER_PASSWORD` |
| `infra-chart-registry` | `REGISTRY_USER`, `REGISTRY_PASSWORD` |
| `acme-dns-cloudflare` | `CF_API_TOKEN` |

The installer reads the environment first, then its cache, then OpenBao, and
prompts for whatever is still missing — so a partly-supplied environment still
works interactively.

**There is no secrets file.** A plaintext file of secrets beside the installer
was a fourth source that nothing rotated and nothing audited, so it was removed
rather than kept working.

### The bootstrap credential cache

Between being typed and reaching OpenBao there is a window where an install can
fail with nothing to recover from. Validated answers are cached at
`~/.gentian/bootstrap-credentials.env` — 0600, in a 0700 directory — and
`B-10-seed-secrets` deletes it once OpenBao holds them.

To keep credentials in the process only, and retype on every resumed run:

```bash
GENTIAN_NO_CREDENTIAL_CACHE=1 ./install.sh
```

Put it in `install.env` to make that permanent for the machine. That file is
read before any credential is collected, so anything set there applies to the
whole run.

### Exporting a recovery kit from a job

`age -p` prompts on the terminal, so an unattended export needs a public key
instead, and reading the kit back needs the matching private key:

```bash
GENTIAN_KIT_RECIPIENT=age1... ./install.sh --export-recovery-kit kit.age
GENTIAN_KIT_IDENTITY=~/.age/key.txt ./install.sh --recover kit.age
```

### Running one step, or stopping early

```bash
./install.sh --explain            # what each step does, in order
./install.sh --status             # which steps this cluster has already satisfied
./install.sh --only B-10          # a single step
./install.sh --from C-01          # resume from a step
./install.sh --phase secrets      # one phase: control-plane, secrets,
                                  # platform, applications, handover
```

Re-running the whole installer is safe: each step checks the cluster and skips
what is already done, so convergence and update are the same operation.

---

## Next steps

- **Add more tenants** — repeat step 11. Day-to-day operations are in
  [docs/commands.md](docs/commands.md).
- **Configure mail** — [docs/design/mail.md](docs/design/mail.md). Mail between
  users of this cluster works once the kernel mail stack is deployed; mail to and
  from the internet additionally needs port 25 exposed and the MX, SPF, DKIM,
  DMARC and PTR records described in
  [§10 DNS for real mail](docs/design/mail.md#10-dns-for-real-mail) — including
  the Cloudflare rule that MX records must stay DNS-only, never proxied.
- **Change this cluster's configuration** — edit its claims in
  `gentian-deployments`; the cluster reconciles.
  [docs/deployment.md](docs/deployment.md) explains the layering.
- **Understand the architecture** — [docs/architecture.md](docs/architecture.md)
  and [docs/design/kernel.md](docs/design/kernel.md).
- **Back up the data** — the recovery kit covers credentials, not
  databases or object storage.

## Notes

Things worth knowing, and what to do when something looks wrong.

### The recovery kit

Most of a cluster is reconstructible: configuration comes from Git, and every
derived credential is a function of the master password and the derivation
salt. The salt is generated during the install and lives only in the cluster.
Lose it and the same master password reproduces nothing.

The kit closes that gap — the salt, the master password, the OpenBao recovery
key and this cluster's identity, in one encrypted file. Restore it into a
fresh cluster and every derived credential comes back byte-identical:

```bash
./install.sh --recover <kit>    # on the fresh cluster, before anything else
```

Without it a rebuild gives you a working cluster with entirely different
credentials, which is a migration rather than a restore. The kit does **not**
back up your data, and it does not restore OpenBao itself — a fresh instance
issues its own unseal material.

It is encrypted with `age` when that is installed and `openssl` otherwise;
there is no unencrypted path. Both ask for a passphrase, so an unattended
install needs `GENTIAN_KIT_RECIPIENT` set to an age public key instead.

To write another one at any time:

```bash
./install.sh --only E-03
```

### Why handover is gated

The installer's credential can write every secret in the cluster. Revoking it
before anyone else has demonstrably written one would leave a cluster nobody
can supply a credential to; revoking it with no recovery kit would mean that
if the login path later breaks, there is nothing to fall back on. Either gap
alone makes the recovery "re-initialise OpenBao from scratch", so the
revocation waits for both. Until it happens, creating tenants is held back.

Where a cluster stands:

```bash
kubectl get configmap gentian-handover -n gentian-system -o yaml
```

- `writePathProven: "true"` — someone has signed in and the exchange worked.
- `recoveryKitExported: "true"` — a kit has been written.
- `bootstrapCredentialRevoked: "true"` — handover is complete.

### Signing in records nothing

Open the Admin Console and select the Credentials tab, which performs the same
token exchange as the login.

### The portal password

It is derived from the master password, so it is not lost with the terminal
that printed it:

```bash
./install.sh --verify-only
```

### The portal password does not work

The password in the install summary is *derived*, not read back from Keycloak.
If `administrator@<kernel domain>` is refused, ask the cluster what its inputs
are and derive from those:

```bash
kubectl get secret gentian-os-master-password -n crossplane-system \
  -o jsonpath='{.data.password}' | base64 -d > /tmp/mp
kubectl get secret gentian-os-master-password -n crossplane-system \
  -o jsonpath='{.data.salt}' | base64 -d > /tmp/salt
printf 'portal-bootstrap:administrator_password' |
  openssl dgst -sha256 -hmac "$(cat /tmp/mp)$(cat /tmp/salt)" | awk '{print $2}'
shred -u /tmp/mp /tmp/salt
```

That is the password Keycloak was given, as long as nothing has re-run the
portal bootstrap with different inputs since. On a cluster whose
`secretMode` is `random` this does not apply — the password is stored, not
derived, at `identity/portal-admin` in OpenBao.

To make the cluster take a new password instead, re-run the portal step with
the inputs exported, which rewrites the credential in Keycloak:

```bash
export MASTER_PASSWORD=... DERIVATION_SALT=...
./install.sh --force --only D-05
```