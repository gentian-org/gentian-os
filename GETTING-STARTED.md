# Installing Gentian OS

This walks through installing a Gentian OS cluster from nothing. It assumes you
have a Kubernetes cluster and admin access to it; it does not assume you have
read any of the design documents.

If something goes wrong partway through, **re-running the installer is the
correct response**. It reads the cluster to decide what is already done, so a
second run continues rather than restarting.

---

## 1. What you need

**A Kubernetes cluster you are an admin on.** The installer does not create one.
Anything from a single-node k3s to a managed cloud cluster works; it needs to
run 100+ pods, so a laptop-sized node pool will be tight.

**These CLI tools on the machine you run the installer from:**

```
kubectl  helm  jq  yq  openssl  curl  bao  crossplane  python3  git
```

`bao` (the OpenBao CLI) installs itself to `~/.local/bin` if missing. The rest
must be on your `PATH` before you start — the installer checks and tells you
which are absent rather than failing partway.

**A domain.** Every kernel service is reachable under a single DNS suffix, for
example `platform.example.com`. It does **not** have to be publicly resolvable —
see [Installing on an internal domain](#7-installing-on-an-internal-domain).

**Two Git repositories**, or access to them:

- `gentian-deployments` — holds this cluster's configuration. You need a token
  with read access; the installer asks for it.
- `gentian-apps` — the app catalogue. Public unless you are running your own.

---

## 2. How the installer is put together

`install.sh` does not install anything itself. It is a **driver** over a
directory of steps:

```
scripts/steps/A-01-crossplane.sh
scripts/steps/A-08-eso.sh
scripts/steps/B-07-cluster-xr.sh
…
```

Each step is a self-contained file you can read top to bottom. It declares what
it needs and what it produces in a header, and implements up to three verbs:

| Verb | What it does |
|---|---|
| `check()` | Read-only. Is this already done? |
| `apply()` | Do it |
| `destroy()` | Undo it |

`check()` is why re-running works. Each step asks the *cluster* whether its work
is already present, so there is no state file on your machine to get out of sync
and nothing to clean up after a failed run.

It also means one program covers three directions:

```bash
./install.sh              # install, or continue where a previous run stopped
./install.sh --update     # the same thing; converging IS updating
./install.sh --uninstall  # the same steps in reverse, calling destroy()
```

---

## 3. Configuration: three places, and only three

Everything you supply belongs to exactly one of these.

### `install.env` — where things come from

The only non-secret file the installer reads from your machine. Copy the
template and edit it:

```bash
cp install.env.template install.env
```

| Variable | Meaning |
|---|---|
| `GENTIAN_DEPLOYMENTS_REPO` / `_BRANCH` | Where this cluster's configuration lives |
| `GENTIAN_DEPLOYMENTS_CLUSTER` | Which directory under `clusters/` is this cluster |
| `GENTIAN_DEPLOYMENTS_STAGE` | `dev`, `staging` or `prod` — selects a profile |
| `GENTIAN_APPS_REPO` / `_BRANCH` | The app catalogue |
| `GENTIAN_OS_REPO` / `_BRANCH` | Where the platform itself comes from |
| `GENTIAN_OS_IMAGE_REPOSITORY` | Where the platform's images come from |

The last two only matter for a mirrored or air-gapped install — see
[Installing from a mirror](#8-installing-from-a-mirror). Leave them at their
defaults otherwise.

### The deployments repository — what this cluster is

Everything non-secret that varies per cluster lives in Git and is reconciled:
the domain, sizing, which apps each tenant gets. The installer scaffolds this
directory on a first run, so you do not have to create it by hand.

### Credentials — prompted, then written to OpenBao

**Nothing is written to your machine.** The installer asks for what it needs,
checks each value against the system it is for, and stores it in OpenBao. If you
re-run later it recovers them from OpenBao rather than asking again.

It asks for four things, two of which are optional:

| | Required | What it is |
|---|---|---|
| Deployments repository token | yes | Read access to the repository above |
| Master password | yes | At least 16 characters. Under the default `secretMode`, kernel credentials are derived from it — see below |
| Chart registry credentials | no | Only if you pull infrastructure charts from a private registry |
| Cloudflare DNS token | no | Only if you want a wildcard TLS certificate via DNS-01 |

**What the master password actually does** depends on the cluster's
`secretMode`, which is a field on the `Cluster` claim:

| `secretMode` | Kernel credentials are | Reproducible on a rebuild? |
|---|---|---|
| `derived` (default) | `HMAC-SHA256` of the master password **and** a per-cluster salt | Yes — with **both** |
| `random` | Generated once by `openssl rand`, then stored | **No.** The master password reproduces nothing; it only guards the paths |

The salt is generated at first install, not chosen, and stored in OpenBao beside
the password itself. That has a consequence worth planning for:

> Under `derived`, reproducing a cluster's credentials needs the master password
> **and** the salt. The salt lives only in OpenBao — so a disaster that loses
> OpenBao's storage also loses the salt, and the master password alone will not
> reproduce anything. If reproducibility is part of your recovery plan, back up
> `gentian-os/kernel/internal/master-password` (both keys), not just the
> password you typed.

Under `random` there is nothing to reproduce; recovery means restoring OpenBao.

---

## 4. Look before you leap

Three commands that change nothing. Run them in this order.

```bash
./install.sh --explain
```

Prints every step, what it provides and what it changes. Needs no cluster
connection at all — this is the sequence, from the steps themselves, so it
cannot drift from what will actually run.

```bash
./install.sh --status
```

Runs every step's `check()` against your cluster and reports what is already
present. On a fresh cluster everything reads `missing`; that is what you expect.

```bash
./install.sh --dry-run
```

Runs the checks, prints what it would do, and applies nothing. Verified to make
no cluster changes: the self-heal hooks and the repository scaffolding are
skipped, not merely unreported.

---

## 5. Install

```bash
./install.sh
```

It will ask for the credentials in §3, validate each one against the system it
belongs to, and stop before touching the cluster if any fail. A wrong token
costs you a retype, not a half-built cluster.

Then it works through the steps, reporting each one before acting:

```
[04] cert-manager        provides: cert-manager controller and its CRDs
     check: not satisfied  →  applying
     ✓ 34s

[07] eso                 provides: External Secrets Operator
     check: satisfied  →  skip
```

### Save the OpenBao keys when they appear

Two steps print keys you cannot recover later.

- **`B-02-openbao-transit-init`** prints the transit instance's unseal key.
- **`B-04-openbao-init`** prints the primary's recovery key and root token.

They are also written to `/tmp/openbao-transit-init.json` and
`/tmp/openbao-init.json`, which are **not** durable — `/tmp` is cleared on
reboot. **Put them in a password manager before you continue.**

After this the primary OpenBao auto-unseals from the transit instance on every
restart. You will not need these keys in normal operation, which is exactly why
they are easy to lose.

### If a step fails

The failure names the step and the file to open. Fix the cause and re-run:

```bash
./install.sh                     # continues; completed steps are skipped
./install.sh --from 16           # or resume from a specific point
./install.sh --only 07           # or re-run one step
./install.sh --phase secrets     # or one phase
```

Steps are numbered `<phase letter>-<NN>`:

| | Phase | What it does |
|---|---|---|
| **A** | `control-plane` | Crossplane, namespaces, cert-manager, ESO, ArgoCD |
| **B** | `secrets` | OpenBao, the Cluster claim, credential seeding |
| **C** | `platform` | Wildcard cert, root ApplicationSet, admission, catalogue |
| **D** | `applications` | Operator, mail, LLM serving, portal, app profiles |
| **E** | `handover` | Tenants, per-tenant reconcile, revoke the bootstrap token |

The number orders steps *within* a phase, so `--from B-03` means "from there to
the end" and `--phase C` runs one stage.

---

## 6. Check it worked

```bash
./install.sh --status
```

Everything should read `satisfied` except the steps that report their state
differently by design — `A-04-prewarm`, `C-03-provider-helm` and `D-02-gateway-wait`
have nothing persistent to check, and `B-04-openbao-init`, `B-09-seed-secrets` and
`E-02-tenant-reconcile` re-run on every pass because they hold per-run tokens or
reconcile continuously.

Then check that the credentials the cluster declares are actually present:

```bash
make check-credentials
```

Each requirement reports satisfied, unset-but-optional, or missing. This reads
External Secrets Operator's sync status, so a `missing` here means the value is
genuinely absent from OpenBao — not that something failed to start.

---

## 7. Export the recovery kit

Do this now, while everything is still in the shell that installed it.

```bash
./install.sh --export-recovery-kit kit.age
```

Most of this cluster is reconstructible: configuration comes from Git, and the
sixteen derived credentials are a function of the master password and the
derivation salt. The salt, though, is generated during the install and lives only
in OpenBao. Lose OpenBao's storage and the master password on its own reproduces
nothing.

The kit is that gap closed — the salt, the master password, the unseal material
and this cluster's identity, in one encrypted file. Restoring it into a fresh
cluster with `--recover` makes every derived credential come back byte-identical.
Without it, a rebuild gives you a working cluster with entirely different
credentials, which is a migration rather than a restore.

```bash
./install.sh --recover kit.age    # on the fresh cluster, before anything else
```

It is encrypted with `age` where available and `openssl` otherwise; there is no
unencrypted path. Store it wherever your break-glass material already lives —
anyone who can decrypt it holds every derived credential in the cluster.

To export unattended, set `GENTIAN_KIT_RECIPIENT` to an age public key
(`age -p` prompts on the terminal and so cannot run from a job); read such a kit
back with `GENTIAN_KIT_IDENTITY` pointing at the private key.

The kit does **not** back up your data, and it does not restore OpenBao — a fresh
instance issues its own unseal material. It is only the part that nothing else
can rebuild.

---

## 8. Installing on an internal domain

If your domain is not publicly resolvable, Let's Encrypt cannot reach it and no
certificate will ever be issued. The symptom is unhelpful: every Gateway sits at
`ResolvedRefs=False` complaining about a missing Secret, which says nothing
about DNS.

Tell the cluster to use its own certificate authority instead. In
`clusters/<cluster>/kernel/claims/cluster.yaml`:

```yaml
spec:
  kernelDomain: platform.internal
  certificates:
    issuerMode: self-signed
```

Issuance is then offline and instant. The certificates are **not** publicly
trusted, so anything validating a kernel hostname from outside the cluster needs
the root CA in its trust store:

```bash
kubectl get secret gentian-root-ca-tls -n cert-manager \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > gentian-root-ca.crt
```

The other modes are `acme-dns01` (the default; public DNS, wildcards),
`acme-http01` (public DNS, no wildcards) and `private-ca` (you supply the CA).
Selecting a mode the cluster cannot satisfy fails with a message naming the
mode — it never silently falls back to a public issuer.

---

## 9. Installing from a mirror

For an air-gapped or forked install, point the platform at your own copies in
`install.env`:

```bash
GENTIAN_OS_REPO=https://git.internal/gentian-os
GENTIAN_OS_IMAGE_REPOSITORY=registry.internal/gentian-os
GENTIAN_DEPLOYMENTS_REPO=https://git.internal/gentian-deployments
```

Both the Git origin and the image registry are redirected, including for every
child ApplicationSet the platform creates.

`versions.yaml` in this repository is the inventory of everything else the
install pulls — Crossplane, cert-manager, External Secrets Operator, ArgoCD,
Envoy Gateway and the OpenBao CLI, each with its pinned version and source.
That file is what to mirror.

---

## 10. Day-2 credentials

Once the cluster is running, credentials are supplied through the on-cluster
credential manager rather than the installer. It validates a value against the
system it is for before storing it, and it never displays a stored value —
metadata only: whether it exists, who set it and when.

Lost credentials are rotated, not recovered.

The installer's own bootstrap token is revoked by the last step once an OIDC
write path is configured. Until then that token is the only way to write a
credential, so the step refuses to revoke it and says so.

---

## 11. Uninstalling

Uninstall is the same steps in reverse:

```bash
./install.sh --uninstall --dry-run   # see the order first
./install.sh --uninstall
./install.sh --uninstall --skip 33   # keep tenant workloads and their Git manifests
```

OpenBao KV data survives an uninstall, so reinstalling onto the same cluster
recovers the credentials rather than re-prompting.

---

## 12. When something is wrong

**Start here.** It tells you which step's expectation is unmet:

```bash
./install.sh --status
```

**A step failed and you want to see what it does.** Every step is one readable
file; the failure names it:

```bash
less scripts/steps/B-07-cluster-xr.sh
```

**A resource is not becoming Ready.** Most of the cluster is reconciled by
Crossplane and ArgoCD after the installer finishes, so the installer completing
is not the same as the cluster being ready:

```bash
kubectl get managed                          # every Crossplane managed resource
kubectl get application,applicationset -n argocd
kubectl describe cluster.gentianos.io -n crossplane-system
```

**Something depends on a credential that is not there.** A claim waiting on a
credential says which one:

```bash
kubectl get repository.gentianos.io -A -o custom-columns=\
NAME:.metadata.name,SATISFIED:.status.credentialSatisfied,WHY:.status.credentialMessage
```

**A credential looks present but its probe says otherwise.** Check the field
names, not just the path — a value stored under the right path with the wrong
key reads as absent:

```bash
bao kv get -mount=secret gentian-os/kernel/mail/postfix
grep -A8 'name: smtp-relay' credentials.yaml
```

**You want to see what the pre-driver installer did.** It is in git history,
not in the working tree:

```bash
git show d7bab42:install.sh      # the single-script installer
git show d7bab42:uninstall.sh
git show d7bab42:update.sh
```

They are not kept as files and are **not** a fallback: they call functions that
no longer exist, so they fail within seconds of starting. Reading them is
useful; running them is not.

---

## Next steps

- **Provision a tenant** and install apps — see [docs/commands.md](docs/commands.md).
- **Configure mail** — see [docs/design/mail.md](docs/design/mail.md).
- **Understand the architecture** — see [docs/architecture.md](docs/architecture.md)
  and [docs/design/kernel.md](docs/design/kernel.md).
- **Change this cluster's configuration** — edit its claims in
  `gentian-deployments`; the cluster reconciles. See
  [docs/deployment.md](docs/deployment.md) for the layering.

- **Back up the data** — the recovery kit in §7 covers credentials, not
  databases or object storage.
