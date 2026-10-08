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
in at `https://platform.<your-domain>/`, and supply the runtime credentials the
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
| **C** | `platform` | C-01 … C-05 | The Cluster claim, the ApplicationSets, the wildcard certificate, the credential catalogue, the repository hand-off |
| **D** | `applications` | D-01 … D-05 | The operator and the director, the kernel hostnames resolving, the kernel realm and the platform desktop, OpenBao's OIDC login, the kernel Gateway |
| **E** | `handover` | E-01 … E-04 | The recovery kit, then revoking the installer's own credential; on a single-tenancy cluster, the user tenant after that |

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

- **A Kubernetes cluster you are an admin on, version 1.33 or newer.** The
  installer does not create one, and pre-flight refuses an older one. It runs
  100+ pods, so a laptop-sized node pool will be tight.
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
| `gentian-os-repository`, `gentian-ui-repository` | no | Only when the matching `GENTIAN_*_AUTH` in `install.env` is not `none` |
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
| `certificates.acmeEnv` | `production` | Never needed for rebuilds — a purge keeps the issued wildcard in `~/.gentian/certs` and the next install reuses it (only `--purge --cluster-infra` deletes it). `staging` breaks the kernel sign-in, which does not trust its chain |
| `certificates.dnsProvider` | `cloudflare` | The zone is hosted elsewhere |
| `mail.serviceMode` | `external` | You want in-cluster Postfix/Dovecot (`kernel`, needs `static-ip`) |
| `mail.host` | unset | `external` mode: the relay's hostname. Its credentials are a credential, supplied in step 5 — not asked here |
| `platform` | detected from the nodes | Detection is wrong for your provider |
| `storageClass` | the cluster default | The cluster has more than one StorageClass |
| `tenancyMode` | `multi` | Who the cluster is for. `multi`: the platform tenant plus any number of user tenants, each at `desktop.<tenant>.<kernel-domain>`; the install creates none. `single`: the platform tenant plus exactly one user tenant, named `user`, on the cluster's own addresses (`desktop.<kernel-domain>`); the install creates it after the handover. The platform admin signs in at `platform.<kernel-domain>` either way. See step 7. |
| First tenant | none | This cluster's users should have a tenant when the install ends: give its name, and the install creates it (step 7). With exactly one, the cluster's bare domain leads straight to its sign-in |
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
2. **Activate the platform admin's account and sign in.** `admin@<kernel-domain>`
   has no password: you set one through a single-use, expiring activation link,
   which also enrols a second factor unless step 0 switched that off. The
   installer mails the link to a recovery address — `CLUSTER_ADMIN_RECOVERY_EMAIL`
   in `install.env`, or the one it asks you for — or, without one, prints it
   here once. Open it, set the password, then sign in at
   `https://platform.<kernel-domain>/`. Nobody else, the installer included,
   ever knows the password.
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
reason a tenant appears to hang.

The wait is bounded — 30 minutes, `GENTIAN_HANDOVER_WAIT_SECS` to change it —
and interrupting it costs nothing. Sign in whenever you like, then:

```bash
./install.sh --only E-03
```

**On a multi-tenancy cluster the install ends here.** It has created no
tenant for users; that is the platform admin's next step (step 7).

**On a single-tenancy cluster one step follows**, `E-04-user-tenant`, in the
same run and with nothing for you to start. The cluster's one tenant for users
— always named `user` — has been in the deployments repository since step 0
and was refused all along: the cluster admits no tenant but the platform's
until the platform admin has signed in. Now that you have, the installer asks
Argo CD to try it again, waits for the tenant to be Ready (15 minutes,
`GENTIAN_USER_TENANT_WAIT_SECS`), and hands over its administrator's account
the way yours was: an activation link for `user-admin@<kernel-domain>`, mailed
when the tenant's realm can send mail and shown once otherwise. The install
waits for nothing after that and closes by naming both roles:

- the **platform admin**, in charge of the platform:
  `https://platform.<kernel-domain>/`
- the **user admin**, in charge of the users and the user tenant:
  `https://desktop.<kernel-domain>/`

They are two accounts in two realms, and may or may not be the same person.
If the tenant is not Ready within the wait, the install says what it is
waiting for and does not fail: `kubectl get tenant user` shows its state, and
`./install.sh --only E-04` — or `kubectl gentian tenants activate-admin user`,
signed in as the platform admin — issues the link once it is.

## 6. Check the status

```bash
./install.sh --status
```

Every step reads `satisfied`, except steps that do not apply to this cluster —
those read `undefined`, and `undefined` is never a failure. `E-01-tenants`
always reads that way (it only acts on an uninstall), and `E-04-user-tenant`
does on a multi-tenancy cluster, where the install creates no tenant; `B-09` and
`D-04` do so on a cluster without OIDC, `C-03` on one with no DNS provider, and
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

## 7. Tenants

A cluster's users live in a tenant of their own. The platform tenant, which
every cluster has, holds the platform admin and the platform's own components
and nothing else: it takes no apps, and the director says so if you try. How
many tenants for users a cluster has is its **tenancy mode**, which step 0
asked for (`tenancyMode` on the Cluster claim).

| | Multi-tenancy (`multi`, the default) | Single-tenancy (`single`) |
|---|---|---|
| Tenants for users | any number; you create them | exactly one, named `user`; the install creates it (step 5) |
| Its desktop | `https://desktop.<tenant>.<kernel-domain>/` | `https://desktop.<kernel-domain>/` |
| Its admin console and apps | `admin.<tenant>.<kernel-domain>`, `<app>.<tenant>.<kernel-domain>` | `admin.<kernel-domain>`, `<app>.<kernel-domain>` |
| In charge of it | a **tenant admin**, `admin@<tenant>.<kernel-domain>` | the **user admin**, `user-admin@<kernel-domain>` |
| `https://<kernel-domain>/` and `www.` | a page that asks for an e-mail address and sends each person to their tenant | the user tenant's desktop |
| `desktop.<kernel-domain>` | leads to that page | the user tenant's desktop |
| The **platform admin** | `admin@<kernel-domain>` at `https://platform.<kernel-domain>/`; admin console at `admin.platform.<kernel-domain>` | the same |

**On a single-tenancy cluster your website can live at the main address.**
Install an app that offers a public website for it, then publish that surface
for the main address (the user admin approves it, like every public surface).
`https://<kernel-domain>/` and `www.` then show the website. Sign-in is at
`https://desktop.<kernel-domain>/`, and `https://<kernel-domain>/sign-in`
always leads there.

**On a single-tenancy cluster there is nothing to create.** A second tenant
is refused, with a message that names the mode — in the console, by the CLI
and by the cluster itself. The user admin installs apps for the tenant `user`
exactly as described below for any tenant. A few host names are the
platform's own there (`id`, `platform`, `www`, `argocd`, `headlamp`, `llm`,
`mail`, `imap`, `mail-egress`, `corp`): an app that would answer on one of
them is refused, and says so on its status.

**On a multi-tenancy cluster** the platform admin creates tenants through the
director — in the admin console, or with the `gentian` CLI, which is a
command-line client of the same director. Either way the director checks that
you may, commits the tenant to the deployments repository as you, and records
it.

**In the console:** **Tenants** → *Bring a tenant on*. Give it a name (any name
but `default`, which is reserved for the cluster-wide backup policy), a display
name, and optionally the administrator's recovery email; leave *second factor*
on unless you have a reason not to. After it is committed the screen waits for
the tenant to be provisioned and then hands the administrator account over:
the activation link is mailed to the recovery address, or shown once if you
gave none. **Activate administrator** on any tenant issues a new link later —
which is also how an administrator who lost access gets back in.

**With the CLI:** install it once, on whichever machine you administer clusters
from, and sign in:

```bash
make install-plugin          # kubectl-gentian + gtnctl into ~/.local/bin
kubectl gentian login        # a code to confirm in the browser, as admin@<kernel-domain>
```

It talks to the director of the current kubectl context, so it needs no
configuration of its own. Re-run `make install-plugin` after pulling — the CLI
is a copy, not a link — and `gtnctl version` shows which copy answers.

```bash
kubectl gentian tenants create acme --display-name "ACME AG"   # --no-mfa to skip the second factor
kubectl gentian tenants activate-admin acme --recovery-email owner@acme.example
kubectl get tenant acme -w
```

`activate-admin` waits for the tenant to be provisioned, then mails the link,
or prints it once when you give no `--recovery-email`.

Apps are installed by the tenant's administrator from the App Store, or from
here. The App Store is a tile on the tenant's desktop, shown to the people
who may install apps there, at `store.<the tenant's domain>`. A tenant has it
while the cluster reports its licences and its Cluster claim names a store
(`spec.catalogue.storeUrl`), both of which an installation does by default;
the platform tenant never has it. The store named there is the store's API,
`https://store-service.aluvian.io` by default, and must not be an address a
cluster's own App Store app could have (`store.<a domain a cluster is
installed under>`).

```bash
kubectl gentian apps list --tenant acme --available           # what the tenant's catalogues offer
kubectl gentian apps install nextcloud-base-ce --tenant acme  # the build the catalogue lists, pinned by digest
kubectl gentian apps list --tenant acme                       # what the tenant has
```

`install` takes the entry from a catalogue the tenant sees and pins it to the
digest that catalogue lists; see [docs/commands.md](docs/commands.md) §6 for
`--from` and `--digest`. A new cluster has one catalogue, `gentian`, for every
tenant: the released one when the cluster is installed from a release, the
development one (`…/gentian-apps/develop`) when it is installed from a branch
([docs/custom-catalogues.md](docs/custom-catalogues.md) §3). To offer your own apps, publish a catalogue and add it for the cluster
or for one tenant:

```bash
kubectl gentian catalogues list
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant acme
```

[docs/custom-catalogues.md](docs/custom-catalogues.md) explains how to build
one, step by step.

To remove a tenant, `kubectl gentian tenants retire acme` (or **Retire** in the
console) — not `kubectl delete`: git is what the cluster reconciles towards, so
deleting the object just brings it back. Retire keeps its data; add `--purge`
(or choose **Purge** in the console) to delete it.

A tenant's data is the tenant's. **Export** in the console (or a scheduled
backup through the Operations Console) produces one encrypted bundle, which
**Download** hands over as a single `.gentian` file. The way back is one
command, on this cluster or any other:

```bash
kubectl gentian tenants import acme-export-20261001.gentian --identity-file acme-backup-key.txt
```

It declares the tenant from the bundle's own manifest, waits for the operator
to provision it, restores the data, and says when people can sign in again
(members need a password reset until bundles carry credentials; activate the
administrator with `tenants activate-admin`).

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
ends at `Almost There` and `--only E-03` finishes it once the platform admin
has signed in. On a single-tenancy cluster (`TENANCY_MODE=single`) the user
tenant is created after that sign-in and not before: `./install.sh --only
E-04`, or a plain re-run, asks Argo CD to sync it, waits for it, and issues
the user admin's activation link — shown in its output, or mailed when the
tenant's realm can send mail. `kubectl gentian tenants activate-admin user
[--recovery-email <address>]`, signed in as the platform admin, issues one
from anywhere.

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

### Turning the licence report off

By default the cluster tells a report address once a day what it runs: its
tenants, how many accounts each has, and the apps installed through the App
Store — no personal data. `./install.sh --no-licence-report` (or
`GENTIAN_NO_LICENCE_REPORT=1` in `install.env`) turns that off; nothing is then
sent, and the App Store is not offered: no tenant has the App Store tile. What is sent, and where to read the
last report, is in [docs/design/operations.md §6.2](docs/design/operations.md).

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
There is no exception for the install's own: on a single-tenancy cluster the
user tenant's manifest is committed in step 0 and the cluster refuses it like
any other until the platform admin has signed in, which is why `E-04` comes
after the handover and has to ask Argo CD to try the manifest again.

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

### The administrator has no password

Neither the platform admin nor any tenant admin (or the user admin of a
single-tenancy cluster) is given one.
Each account is created without a password and handed to its holder through a
single-use, expiring link that sets one (and enrols a second factor, unless
switched off) — the same way every member is invited. The installer and the
director never know it, so there is nothing to print, derive or recover.

Lost access, an expired link, or a link nobody received, for a tenant's
administrator:

```bash
kubectl gentian tenants activate-admin acme         # a tenant administrator
```

or **Tenants → Activate administrator** in the console. Each call issues a new
link; earlier ones still expire on their own.

The platform admin's own account is not handed over that way. The CLI
and the console ask the registrar, and the registrar changes nothing about a
member of the platform administrators' group, whoever asks: it answers 403.
That account's link comes from the install host:

```bash
./install.sh --activate-admin    # a new link for admin@<kernel-domain>, from the installer's own credential
```

which is also the way back in when no administrator can sign in at all.

### A commit to the deployments repository is not synced

Argo CD syncs that repository only for commits signed by the director or the
break-glass key — the two ids in `clusters/<cluster-id>/kernel/signing/keys.env`.
A commit made with your own key, or unsigned, sits at the head of the branch
and stops every later sync. Undo it, make the change in your checkout instead,
and run `./install.sh`: step 0 commits it signed with the break-glass key. On
another machine, `./install.sh --recover <kit>` imports that key into
`~/.gentian/gnupg` first.
