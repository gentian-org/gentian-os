# Installing Gentian OS

Follow these steps in order to get a cluster running. Each one says what to do
and how to tell it worked.

Re-running `./install.sh` is always safe. It reads the cluster to decide what is
already done, so a second run continues rather than restarting.

**The whole install, once you are configured (steps 1–3):**

```bash
./install.sh     # asks what the cluster is, installs everything, writes the recovery kit, then waits
```

It then waits for the things only you can do: move the kit somewhere safe, sign
in at `https://console.<your-domain>/`, and supply the runtime credentials the
console asks for. It sees the sign-in, finishes handover itself, and prints
`Install Complete`.

For flags, troubleshooting and the non-default installs — internal domains,
mirrors, uninstalling — see
[docs/install-reference.md](docs/install-reference.md).

---

## What the installer does

`install.sh` works through a directory of small steps, each named
`<phase>-<number>-<what-it-does>`. The letter is the phase; the number orders
steps within it. Five phases, in this order:

| | Phase | Steps | What it builds |
|---|---|---|---|
| **A** | `control-plane` | A-01 … A-09 | Namespaces, cert-manager, External Secrets, Crossplane, Envoy Gateway, Argo CD, metrics-server, pre-pulled images, cluster issuers |
| **B** | `secrets` | B-01 … B-10 | The kernel bootstrap Applications, the transit seal and OpenBao, Crossplane providers and definitions, the seeded credentials, the deployment signing keys |
| **C** | `platform` | C-01 … C-06 | The Cluster claim, the ApplicationSets, the wildcard certificate, DNS, the credential catalogue, the repository hand-off |
| **D** | `applications` | D-01 … D-04 | The operator and the director, the kernel realm and the platform desktop, OpenBao's OIDC login, the kernel Gateway |
| **E** | `handover` | E-01 … E-03 | The recovery kit, then revoking the installer's own credential |

`./install.sh --explain` prints every step with what it provides and what it
mutates, straight from the step files, so it cannot drift from what runs.

Before any of that there is a **step 0**: the cluster's definition in
`gentian-deployments`. If it exists the installer reads it; if it does not, the
installer asks for every setting — each with its default already in the answer
— writes the files, commits them signed and pushes them. Nothing on the cluster
is touched until that is done. Step 4 below is what that looks like.

Each step reports what it found before it changes anything:

```text
[A-02] cert-manager
     provides: cert-manager controller and its CRDs in the edge namespace
     check: not satisfied  →  applying
     ✓ 34s
```

`check: satisfied → skip` means that step's work is already present, which is
why re-running continues rather than restarting. You can run one phase with
`--phase secrets`, or one step with `--only A-02`.

From phase C onward much of the work is Argo CD and Crossplane converging on
their own, so the installer finishing is not the same as the cluster being
ready — step 6 covers how to tell the difference.

Nor is it the same as the cluster being *yours*: phase E ends by revoking the
credential the installer used, and it will not do that until an administrator
has signed in. Step 5 is that handover, and the cluster holds tenants back
until it is done.

---

## What you need before you start

- **A Kubernetes cluster you are an admin on.** The installer does not create
  one. It runs 100+ pods, so a laptop-sized node pool will be tight.
- **These tools on your `PATH`:** `kubectl helm jq yq openssl curl git gpg
  crossplane python3 age age-keygen`. Pre-flight checks and names any that are
  missing, and refuses to start without them. `bao` (the OpenBao CLI) installs
  itself to `~/.local/bin`.

  - `git` and `gpg` are needed before the cluster is touched: step 0 commits
    this cluster's definition to `gentian-deployments`, and every commit to
    that repository is signed — Argo CD refuses unsigned ones. The keys are
    generated for you, in `~/.gentian/gnupg`, never in your own keyring.
  - [`age`](https://age-encryption.org) ships both binaries — `sudo apt install
    age`, `brew install age`, `apk add age`. It encrypts the recovery kit, and it
    **generates this cluster's backup key** — the age key pair every scheduled
    export encrypts to. Nothing else can make one; without it the install would
    finish and every nightly backup would fail with `no age recipients
    configured`, for as long as nobody looked.
- **Optional:** [`qrencode`](https://fukuchi.org/works/qrencode/) — `sudo apt
  install qrencode`, `brew install qrencode`, `apk add qrencode`. The recovery
  kit prints the backup key as text either way; with `qrencode` it also prints
  it as a QR code, which is what you keep on paper. Pre-flight warns if it is
  missing, since the kit is written near the end of a long install.
- **A domain**, for example `platform.example.com`. It does not have to be
  publicly resolvable.
- **A token with write access to `gentian-deployments`.** The installer pushes
  this cluster's definition with it, and the director on the cluster pushes
  every later change — tenants, app installs — with it too. The installer asks
  for it.

---

## 1. Clone the deployments repository

This cluster's configuration lives here, and the installer reads and writes it
on your machine. Clone it to the default location:

```bash
git clone <your-gentian-deployments-url> ~/.gentian/gentian-deployments
```

To keep it somewhere else, set `GENTIAN_DEPLOYMENTS_PATH` in step 2.

The repository needs `profiles/_base.yaml` and `profiles/<stage>.yaml` for the
stage you are about to use. Step 0 warns if either is missing and carries on,
but the cluster's stage-tier policy has no home until they exist.

## 2. Write `install.env`

```bash
cp install.env.template install.env
```

Edit it. These are the values that matter for a first install:

| Variable | Set it to |
|---|---|
| `GENTIAN_DEPLOYMENTS_CLUSTER_ID` | This cluster's ID. It names the directory under `clusters/` and, with the stage, the Cluster claim — get it right before step 4, which pushes the tree it names |
| `GENTIAN_DEPLOYMENTS_STAGE` | `dev`, `staging` or `prod` |
| `GENTIAN_DEPLOYMENTS_REPO` / `_BRANCH` | Your deployments repository |
| `GENTIAN_APPS_REPO` / `_BRANCH` | The app catalogue |

Leave the rest at their defaults. The repository URLs, branches and auth modes
below them are already filled in — they are defaults for a fork or a mirror, not
questions.

**Nothing about the cluster itself is set here.** Its domain, network and
routing modes, certificate issuer, mail mode and storage class live on the
Cluster claim, which the install asks for in step 4. `install.env` says how to
run the install; the claim says what the cluster is.

## 3. Have the credentials ready

The install asks for these, validates each against the system it belongs to, and
stops before touching the cluster if any fail.

| | Required | What it is |
|---|---|---|
| `deployments-repository` | yes | Write access to the repository from step 1 |
| `master-password` | yes | At least 16 characters |
| `infra-chart-registry` | no | Only for a private chart registry |
| `gentian-os-repository`, `gentian-apps-repository`, `gentian-ui-repository` | no | Only when the matching `GENTIAN_*_AUTH` in `install.env` is not `none` |
| Cloudflare API token | under `acme-dns01` | `CF_API_TOKEN` — needed by the default issuer, see below |

Type them when asked. Each reaches OpenBao once it exists, and a later run
recovers it from there instead of asking again — so a resumed install does not
re-ask. Nothing is written to this machine except a short-lived cache that step
`B-08-seed-secrets` deletes.

**Everything else is supplied after the cluster is up, not now** — you set it
when you first sign in, in step 5.

**The Cloudflare token is only optional if this cluster's issuer does not need
it.** Under the default `acme-dns01`, DNS-01 issues every kernel certificate,
not just the wildcard, so an absent or rejected token leaves the cluster with no
working TLS. Choose `acme-http01` (public DNS, port 80 reachable, no wildcards)
or `self-signed` (internal domains) at the issuer question in step 4 if you do
not want to supply one. On a tunnel cluster the same token needs a second
permission for tenant routing — see
[design/routing.md](docs/design/routing.md) §3a for what to grant it.

If a value is rejected, the installer names where it came from and asks for a
replacement rather than aborting.

## 4. Install

```bash
./install.sh
```

One command, start to finish. Before the first phase it runs **step 0**: it
finds no definition for this cluster in `gentian-deployments` and asks for
one. Every question shows its default; Enter takes it.

| Question | Default | Choose otherwise when |
|---|---|---|
| Kernel domain | — | Always asked; there is no default |
| `networkMode` | `tunnel` | DNS points straight at a node — `static-ip`, which then asks for `nodeIp` |
| `certificates.issuerMode` | `acme-dns01` | The domain is not publicly resolvable (`self-signed`), or port 80 is reachable but you have no DNS API token (`acme-http01`) |
| `certificates.acmeEnv` | `staging` on a `dev` stage, else `production` | Names are settled and you want trusted certificates on dev |
| `certificates.dnsProvider` | `cloudflare` | The zone is hosted elsewhere |
| `mail.serviceMode` | `external` | You want in-cluster Postfix/Dovecot (`kernel`, needs `static-ip`) |
| `mail.host` | unset | `external` mode: the relay's hostname. Its credentials are a credential, supplied in step 5 — not asked here |
| `platform` | detected from the nodes | Detection is wrong for your provider |
| `storageClass` | the cluster default | The cluster has more than one StorageClass |
| `tenancyMode` | `multi` | One tenant occupies the whole cluster (`single`) |
| `secretMode` | `derived` | You want independent random secrets rather than ones reproducible from the master password |
| `backup.escrowIdentity` | `true` | The backup key should live in the recovery kit only, never in OpenBao |
| `llm.enabled` | `false` | This cluster serves models; then `llm.gpuAcceleration` is asked too |

With the answers it writes `clusters/<cluster-id>/kernel` into your deployments
checkout:

| File | What it is |
|---|---|
| `claims/cluster.yaml` | Everything that describes this cluster — every setting above, set or commented with the default in effect |
| `claims/suze.yaml` | Cluster security: Keycloak and OpenFGA |
| `claims/deployments-repository.yaml` | Where the director pushes. Without it every write answers 503: no tenant can be created and nobody can be invited |
| `values.yaml` | This cluster's Helm overlay |
| `signing/director.asc`, `signing/break-glass.asc`, `signing/keys.env` | The two public keys Argo CD will accept commits from, and their ids |

Then it **commits and pushes** them, signed with the break-glass key. Two keys
are generated for this cluster in `~/.gentian/gnupg` the first time: the
*director* key, which the director on the cluster signs with, and the
*break-glass* key, which signs what a person writes directly — and before the
cluster exists, that is the only way its first claim can get there at all.
`git log --show-signature` in the deployments repository afterwards says which
commits a person made and which the director made.

Before any of this, step 0 brings your deployments checkout up to date with
its remote — a fast-forward when that is all it takes. If the checkout has
local commits or uncommitted changes that origin does not, it stops and shows
the two ways to reconcile (`pull --rebase` to keep them, `reset --hard` to
discard them); which one is your call. If the push itself fails — no
credential yet — it stops too and prints the command to run by hand: Argo CD
syncs from the repository and not from your checkout, so nothing it syncs —
the claims, later the tenants — reaches the cluster until the push lands.

There is no `claims/infra-data.yaml`. The shared Postgres, MariaDB, Redis and
MinIO are composed with the cluster into the system tier, and the installer
refuses to commit a claim whose kind this checkout does not define — so a
leftover from an older checkout stops step 0 rather than reaching the cluster.

Step 0 done, the install collects the credentials from step 3 and works through
phases A to E, reporting each step before acting. Re-running it is safe: every
step checks whether its work is already done and skips if so, and step 0 asks
nothing on a re-run — the settings are read back from the claim. Expect it to
take a while in B, where OpenBao is deployed and initialised, and in C and D,
where Argo CD pulls and syncs the platform's own applications.

It ends in one of two ways:

- **`Gentian OS — Almost There: 1 step left`** — everything is installed and
  handover remains. The summary lists exactly what to do, in order. That is
  step 5 below, and it is the normal ending for a first install.
- **`Gentian OS — Install Complete`** — handover is done too. Nothing is left.

If a step fails, the run stops there and names the command, the step file and
the call stack. Nothing after a failure runs, so the install never reports
success over a broken step. Fix the cause and run `./install.sh` again.

Nothing secret is printed, and there is nothing to copy down: OpenBao's
initialisation material goes to mode-600 files under `~/.gentian`, and the
installer removes them once their contents are safe elsewhere.

Near the end, `E-02-recovery-kit` writes this cluster's **recovery kit** as
`gentian-recovery-kit-<cluster-id>.age` in the root of this checkout, asking
for a passphrase to encrypt it, and prints the path. That file is the one thing
you have to look after — step 5. By default it carries the break-glass signing
key too, so whoever holds the kit can write to the deployments repository when
the director cannot; set `GENTIAN_KIT_INCLUDE_BREAK_GLASS=0` to keep that key
out of it.

**Want to see the plan before anything runs?** Both of these change nothing —
neither the cluster nor the deployments repository — and report a missing
definition instead of writing one:

```bash
./install.sh --validate      # is this configuration coherent? No cluster needed.
./install.sh --dry-run       # what would the install do to THIS cluster?
```

## 5. Handover

The install pauses here and waits for you. Three things finish it:

1. **Move the recovery kit somewhere safe.** Step 4 wrote it in the checkout
   root and printed the path. Put it where your break-glass material already
   lives — a password manager, a sealed vault, offline media. Without it this
   cluster cannot be rebuilt as itself.
2. **Sign in to the console** as the administrator, at
   `https://console.<kernel-domain>/`. The installer prints the URL, the
   username — `admin@<kernel-domain>` — and the password while it waits.
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

The wait is bounded — 30 minutes, `GENTIAN_HANDOVER_WAIT_SECS` to change it —
and interrupting it costs nothing. Sign in whenever you like, then:

```bash
./install.sh --only E-03
```

## 6. Check the status

```bash
./install.sh --status
```

Every step reads `satisfied`, except steps that do not apply to this cluster —
those read `undefined`, and `undefined` is never a failure. `E-01-tenants`
always reads that way (tenants are created after installation); `B-09` and
`D-03` do so on a cluster without OIDC, `C-03` on one with no DNS provider, and
`B-10` until the signing keys exist in the deployments repository.

```bash
make check-credentials
```

Each credential requirement reports satisfied, unset-but-optional, or missing.
This reads External Secrets Operator's sync status, so `missing` means the value
is genuinely absent from OpenBao.

The installer finishing is not the same as the cluster being ready — Crossplane
and Argo CD keep reconciling after it exits:

```bash
kubectl get managed
kubectl get application,applicationset -n kernel-gitops
```

## 7. Create your first tenant

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

Any name but `default`. A tenant's own `BackupPolicy` has to be named after the
tenant, and `default` is reserved for the cluster-wide one — so a tenant called
`default` could never have a backup policy of its own.

A tenant is authored, then deployed. Two directories, and the difference
matters:

- `definitions/tenants/<name>/tenant.yaml` — what the tenant is *meant to be*.
  Yours to edit.
- `tenants/<name>/` — what Argo CD syncs. Written by the deploy command, and
  written again by the director every time an app is installed from the store.

It asks for a display name, then writes
`clusters/<cluster-id>/definitions/tenants/acme/tenant.yaml`. Nothing is
deployed, committed or applied.

To install apps for the tenant, log in as tenant admin and open the app store,
or see what this cluster offers:

```bash
kubectl gentian apps list
```

You can name apps in the definition, but it is recommended to **add apps with
the tenant admin through the app store**:

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

Set `GENTIAN_NONINTERACTIVE=1` in `install.env`, and supply the bootstrap
credentials through the environment instead of the prompt:

| Credential | Environment variable |
|---|---|
| `deployments-repository` | `GENTIAN_DEPLOYMENTS_GIT_USERNAME`, `GENTIAN_DEPLOYMENTS_GIT_TOKEN` |
| `master-password` | `MASTER_PASSWORD` |
| `infra-chart-registry` | `REGISTRY_USER`, `REGISTRY_PASSWORD` |
| `gentian-os-repository` etc. | `GENTIAN_OS_GIT_USERNAME` / `_TOKEN`, and the same for `APPS` and `UI` |
| Cloudflare API token | `CF_API_TOKEN` |

Step 0's questions take their defaults unattended, and an environment value
answers any of them without a question: `KERNEL_DOMAIN` (no default — must be
set), `NETWORK_MODE`, `NODE_IP`, `CERT_ISSUER_MODE`, `ACME_ENV`,
`DNS_PROVIDER`, `MAIL_SERVICE_MODE`, `EXTERNAL_SMTP_HOST`, `PLATFORM`,
`STORAGE_CLASS`, `TENANCY_MODE`, `SECRET_MODE`, `BACKUP_ESCROW_IDENTITY`,
`LLM_SUPPORT`, `GPU_ACCELERATION`. The handover wait is skipped, so the run
ends at `Almost There` and `--only E-03` finishes it once someone has signed
in.

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
`B-08-seed-secrets` deletes it once OpenBao holds them.

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
./install.sh --only B-08          # a single step
./install.sh --from C-01          # resume from a step
./install.sh --until D-01         # stop after a step
./install.sh --phase secrets      # one phase: control-plane, secrets,
                                  # platform, applications, handover
```

Re-running the whole installer is safe: each step checks the cluster and skips
what is already done, so convergence and update are the same operation.

---

## Next steps

- **Add more tenants** — repeat step 7. Day-to-day operations are in
  [docs/commands.md](docs/commands.md).
- **Configure mail** — [docs/design/mail.md](docs/design/mail.md). Mail between
  users of this cluster works once the kernel mail stack is deployed; mail to and
  from the internet additionally needs port 25 exposed and the MX, SPF, DKIM,
  DMARC and PTR records described in
  [§10 DNS for real mail](docs/design/mail.md#10-dns-for-real-mail) — including
  the Cloudflare rule that MX records must stay DNS-only, never proxied.
- **Change this cluster's configuration** — edit `claims/cluster.yaml` in your
  deployments checkout and run `./install.sh`: step 0 commits the edit signed
  and pushes it, and the cluster reconciles.
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
key, the backup key, the break-glass signing key and this cluster's identity,
in one encrypted file. Restore it into a fresh cluster and every derived
credential comes back byte-identical:

```bash
./install.sh --recover <kit>    # on the fresh cluster, before anything else
```

Without it a rebuild gives you a working cluster with entirely different
credentials, which is a migration rather than a restore. The kit does **not**
back up your data, and it does not restore OpenBao itself — a fresh instance
issues its own unseal material.

It is encrypted with `age`, which pre-flight requires — `openssl` remains only
as a fallback for a kit exported on a machine that somehow lacks it;
there is no unencrypted path. Both ask for a passphrase, so an unattended
install needs `GENTIAN_KIT_RECIPIENT` set to an age public key instead.

To write another one at any time:

```bash
./install.sh --only E-02
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
kubectl get configmap gentian-handover -n kernel-control -o yaml
```

- `writePathProven: "true"` — someone has signed in and the exchange worked.
- `recoveryKitExported: "true"` — a kit has been written.
- `bootstrapCredentialRevoked: "true"` — handover is complete.

### Signing in records nothing

Open the admin console and select the Credentials tab, which performs the same
token exchange as the login.

### The console password

It is derived from the master password, so it is not lost with the terminal
that printed it:

```bash
./install.sh --verify-only
```

### The console password does not work

The password in the install summary is *derived*, not read back from Keycloak.
If `admin@<kernel domain>` is refused, ask the cluster what its inputs are and
derive from those:

```bash
kubectl get secret gentian-os-master-password -n kernel-provisioning \
  -o jsonpath='{.data.password}' | base64 -d > /tmp/mp
kubectl get secret gentian-os-master-password -n kernel-provisioning \
  -o jsonpath='{.data.salt}' | base64 -d > /tmp/salt
printf 'portal-bootstrap:administrator_password' |
  openssl dgst -sha256 -hmac "$(cat /tmp/mp)$(cat /tmp/salt)" | awk '{print $2}'
shred -u /tmp/mp /tmp/salt
```

That is the password Keycloak was given, as long as nothing has re-run the
realm bootstrap with different inputs since. On a cluster whose `secretMode`
is `random` this does not apply — the password is stored, not derived, at
`identity/portal-admin` in OpenBao.

To make the cluster take a new password instead, re-run the login step with
the inputs exported, which rewrites the credential in Keycloak:

```bash
export MASTER_PASSWORD=... DERIVATION_SALT=...
./install.sh --force --only D-02
```

### A commit to the deployments repository is not synced

Argo CD syncs that repository only for commits signed by the director or the
break-glass key — the two ids in `clusters/<cluster-id>/kernel/signing/keys.env`.
A commit made with your own key, or unsigned, sits at the head of the branch
and stops every later sync. Undo it, make the change in your checkout instead,
and run `./install.sh`: step 0 commits it signed with the break-glass key. On
another machine, `./install.sh --recover <kit>` imports that key into
`~/.gentian/gnupg` first.
