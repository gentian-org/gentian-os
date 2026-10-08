# Install reference

Everything about installing that is not the first-install walkthrough. For that,
see [GETTING-STARTED.md](../GETTING-STARTED.md).

---

## 1. How the installer is put together

`install.sh` installs nothing itself. It is a driver over a directory of steps:

```text
scripts/steps/A-01-namespaces.sh
scripts/steps/A-04-crossplane.sh
scripts/steps/C-01-cluster-claim.sh
…
```

Each step is a self-contained file, readable top to bottom. It declares what it
requires and provides in a header and implements up to three verbs:

| Verb | What it does |
|---|---|
| `check()` | Read-only. Is this already done? |
| `apply()` | Do it |
| `destroy()` | Undo it |

`check()` is why re-running works: each step asks the *cluster* whether its work
is present, so there is no state file on your machine to fall out of sync.

One program therefore covers three directions:

```bash
./install.sh              # install, or continue where a previous run stopped
./install.sh --update     # the same thing; converging IS updating
./install.sh --uninstall  # the same steps in reverse, calling destroy()
```

### Step numbering and phases

Steps are numbered `<phase letter>-<NN>`. The number orders steps within a
phase, so a new step only ever affects its own phase.

| | Phase | What it does |
|---|---|---|
| **A** | `control-plane` | Namespaces, cert-manager, ESO, Crossplane, Envoy Gateway, Argo CD, metrics-server, image pre-warm, cluster issuers |
| **B** | `secrets` | Kernel bootstrap Applications, transit seal and OpenBao, Crossplane providers and definitions, credential seeding, deployment signing keys |
| **C** | `platform` | The Cluster claim, ApplicationSets, wildcard certificate, DNS, credential catalogue, repository hand-off |
| **D** | `applications` | Operator and director, kernel realm and platform desktop, OpenBao OIDC, kernel Gateway |
| **E** | `handover` | Recovery kit, revoke the bootstrap token |

Before phase A there is a step 0: the cluster's definition in
`gentian-deployments`. When it is absent the forward run asks for every setting
with its default, writes the files, commits them signed with the break-glass key
and pushes them, then continues. When it is present, the same step commits any
edit found in the checkout — so changing the claim and running `./install.sh`
is the whole of a reconfigure.

### Reading a `--status` verdict

| Verdict | Meaning |
|---|---|
| `satisfied` | The step's `provides:` is present on the cluster |
| `missing` | It is not; `apply()` has work to do |
| `undefined` | The step has nothing persistent to check, or does not apply to this cluster — a feature that is switched off, or no install-time artefact |

`undefined` is never a failure. `E-01-tenants` always reads that way — it acts
only on an uninstall — and `E-04-user-tenant` does on a multi-tenancy cluster,
where the install creates no tenant; `B-09` and `D-04` do on a cluster without
OIDC, `C-03` on one with no DNS provider, `B-10` until the signing keys are in
the deployments repository.

---

## 2. Commands that change nothing

```bash
./install.sh --explain    # every step, what it provides and what it mutates
./install.sh --status     # run every check() against the cluster
./install.sh --dry-run    # run the checks, print the plan, apply nothing
```

None of the three collects a credential. `--dry-run` runs the same preflight as
an install except for that: it applies nothing, and no step's `check()` reads a
credential, so it has everything it needs to print the plan. The install
collects them; the preview does not.

`--explain` needs no cluster connection. It reads the step headers, so it cannot
drift from what will actually run.

---

## 3. Running part of the install

```bash
./install.sh --from B-03            # from there to the end
./install.sh --only A-07            # one step
./install.sh --only A-07,A-08       # a named subset
./install.sh --skip A-08            # everything but that
./install.sh --until D-01           # stop after a step
./install.sh --phase secrets        # one phase
```

A failure names the step and its file. Fix the cause and re-run — completed
steps are skipped.

```bash
less scripts/steps/C-01-cluster-claim.sh
```

---

## 4. Configuration surfaces

Everything you supply belongs to one of three places.

| Surface | Carrier | Answers |
|---|---|---|
| Repository pointer | `install.env` on the installing machine | Where does this cluster's configuration come from? |
| Declarative configuration | YAML in `gentian-deployments` | What is this cluster, its tenants, and their apps? |
| Credentials | Prompted, written to OpenBao | What secrets does it need that cannot be derived? |

`install.env` is the only non-secret file the installer reads from local disk,
and the deployments checkout is the only place it writes: step 0 scaffolds the
cluster's definition there when it is absent, commits it signed with the
break-glass key, and pushes it.
[docs/deployment.md](deployment.md) covers the layering inside
`gentian-deployments`.

A cluster property set in `install.env` beats the claim, silently — the file is
loaded first. The installer reports it rather than reversing the precedence,
because an operator who wrote it there meant something, but the claim is where
it belongs.

### What `install.env` holds

The template carries one line per setting; the reasoning is here.

| | |
|---|---|
| `GENTIAN_DEPLOYMENTS_CLUSTER_ID` | The directory name under `clusters/` — **not** the Kubernetes cluster name and not a kubeconfig context. With `_STAGE` it also names the Cluster claim on first bootstrap, so it must be right before the first run: scaffolding pushes the tree it names to a shared repository. |
| `GENTIAN_DEPLOYMENTS_STAGE` | `dev`, `staging` or `prod`. A cluster keeps one stage for life, which is why there is no `<stage>` segment inside its own tree. |
| `GENTIAN_*_AUTH` | `none`, `basic` (username + token) or `bearer` — how the installer authenticates to that repository. The credential itself is prompted for. Deployments defaults to `basic` because a private repository cannot describe its own access; set `none` for a public one. |
| `GENTIAN_*_REPO` / `_BRANCH` | Point them at a mirror for a forked or air-gapped install; the child ApplicationSets follow. |
| `GENTIAN_OS_BRANCH` | The ref every in-cluster Application tracks — [deployment.md §4](deployment.md). |
| `GENTIAN_CATALOGUE_URL` | The address of the default catalogue, `gentian`, written to `spec.catalogue.sources` when step 0 scaffolds a new Cluster claim. Unset, the address follows from what is installed: a cluster installed from a release tag or from `main` (`GENTIAN_OS_BRANCH`, or the checkout's branch) gets the released catalogue, `https://gentian-org.github.io/gentian-apps`; one installed from any other branch gets the development catalogue, `https://gentian-org.github.io/gentian-apps/develop`. Set, it is used whatever the ref. Step 0 prints the choice and writes the reason above the address in the claim. A public https address; the director fetches from nothing else ([custom-catalogues.md](custom-catalogues.md)). An existing claim is never rewritten: on an existing cluster the address is changed with `kubectl gentian catalogues` ([custom-catalogues.md §3](custom-catalogues.md)). |
| `GENTIAN_STORE_URL` | The base address of the App Store API, written to `spec.catalogue.storeUrl` when step 0 scaffolds a new Cluster claim. Defaults to `https://store-service.aluvian.io`. It must not be an address a cluster's own App Store app could have — `store.<a domain a cluster is installed under>` — which is why the default is `store-service.…` and not `store.…`; a cluster whose claim names its own App Store host offers no App Store (`store-address-is-own-host`). |
| `TENANCY_MODE` | `multi` (default) or `single`, for an unattended first run; step 0 asks otherwise. It is written to the Cluster claim (`spec.tenancyMode`), which owns it from then on. See *Tenancy modes* below. |
| `GENTIAN_USER_TENANT_WAIT_SECS` | How long `E-04` waits for the user tenant of a single-tenancy cluster to be Ready. 900 by default. |
| `INSTALL_CLUSTER_INFRA` | `0` when cert-manager, CloudNativePG and Reloader are managed elsewhere on this cluster. |
| `GENTIAN_NO_LICENCE_REPORT` | `1` turns the licence report off, as `--no-licence-report` does; `0` turns it back on. Unset keeps what the cluster has. Off, nothing is sent and the App Store is not offered: no tenant gets the App Store app — [design/operations.md §6.2](design/operations.md). |
| `GENTIAN_LICENCE_REPORT_URL` | Where the licence report goes, instead of the default address in `kernel/bootstrap/chart/values.yaml`. `https` only. |
| `OPENBAO_CLI_VERSION` | Which `bao` to fetch when none is on `PATH`. Defaults to the pin in `versions.yaml`, which is where component versions are declared. |
| `INFRA_CHART_REPO` / `_PRIVATE` | Where the infrastructure charts come from, and whether that registry needs a credential. Install-time rather than cluster state: it decides what the installer does before a cluster exists. |

### Where app profiles come from, and upgrading a cluster that copied them

A `ComponentProfile` reaches a cluster when a tenant installs the app: the
director fetches that one profile from a catalogue, checks it against the
digest the install names, and commits it under `clusters/<cluster>/catalogue/`
in the deployments repository. The catalogues are addresses declared on the
Cluster claim and on tenants ([custom-catalogues.md](custom-catalogues.md));
a new claim names the default one.

Earlier installs worked differently: step 0 scaffolded a `Repository` claim for
the gentian-apps git repository (`type: git`, `role: apps`), and an
ApplicationSet `catalogue-gentian-apps` copied every profile of that repository
into the cluster and followed its branch. That is retired. The installer no
longer writes the claim, and `GENTIAN_APPS_REPO`, `GENTIAN_APPS_BRANCH`,
`GENTIAN_APPS_AUTH` and the `gentian-apps-repository` credential are gone with
it; an `install.env` that still sets them is read without effect.

**On a fresh install nothing needs doing.** On a cluster installed before:

- The `catalogue-<name>` ApplicationSet is removed as soon as the cluster runs
  the new Compositions, and Argo CD prunes the `catalogue-<profile>`
  Applications it generated and **the ComponentProfiles those own**. Profiles
  installed through the director are not affected for long: they are in
  `clusters/<cluster>/catalogue/` and the `gentian-catalogue` Application
  applies them again.
- An app that was installed by name from a copied profile is left without its
  profile. Its workloads keep running; its Component reports `ProfileMissing`
  and nothing is rolled out for it. Install it again with its coordinate and
  digest, which fetches the profile and pins the install:
  `kubectl gentian apps install <app> --tenant <tenant>`. Do this before
  purging an uninstalled app's data as well: a purge is refused without the
  profile.
- An add-on that was switched on by name is the same: the app it is switched
  on in reports `AddonProfileMissing` until the add-on is given with its
  coordinate and digest, or switched off.
- The ApplicationSet synced a profile's whole directory, not only the profile:
  a `composition.yaml`, an `oidc-catalog.yaml` and `customizations/` beside it
  were applied too, and pruning removes those with the profiles. A catalogue
  serves profiles only. An app whose profile names a Composition of its own
  (`spec.package.composition`) or relies on an OIDC pack shipped beside it
  needs those objects applied separately before it is installed again.
- Step 0 removes `claims/gentian-apps-repository.yaml` from the deployments
  checkout. Claims are synced without pruning, so the `Repository` object stays
  on the cluster until it is deleted there
  (`kubectl delete repository gentian-apps -n crossplane-system`); it composes
  nothing but an Argo CD repository entry in the meantime.
- A tenant's own `Repository` of `type: git` and `role: apps` copies nothing any
  more either, and the director refuses to declare a new one. Publish those
  profiles as a catalogue instead and add it for the tenant.

### Tenancy modes, addresses, and who is in charge

A cluster has one of two tenancy modes
([design/multi-tenancy.md §3](design/multi-tenancy.md)). The platform tenant
is in both and is never counted.

| | `multi` | `single` |
|---|---|---|
| Tenants for users | any number, created after the install by the platform admin | exactly one, named `user`, created by the install after the handover; any other is refused |
| **Platform admin** — in charge of the platform | `https://platform.<kernel-domain>/` as `admin@<kernel-domain>`, kernel realm; admin console `admin.platform.<kernel-domain>` | the same |
| In charge of a tenant and its users | a **tenant admin** per tenant: `https://desktop.<tenant>.<kernel-domain>/` as `admin@<tenant>.<kernel-domain>`, the tenant's realm | the **user admin**: `https://desktop.<kernel-domain>/` as `user-admin@<kernel-domain>`, realm `user` |
| A tenant's admin console and apps | `admin.<tenant>.<kernel-domain>`, `<app>.<tenant>.<kernel-domain>` | `admin.<kernel-domain>`, `<app>.<kernel-domain>` |
| `https://<kernel-domain>/`, `www.` | the page that asks for an e-mail address | the user tenant's desktop |
| `desktop.<kernel-domain>` | redirects to the bare domain | the user tenant's desktop |
| Identity provider | `id.<kernel-domain>` | the same |
| Account handed over by | platform admin: the handover's activation link, `./install.sh --activate-admin` for a new one. Tenant admin: the admin console, or `kubectl gentian tenants activate-admin <tenant>` | platform admin: the same. User admin: `E-04`'s activation link, `kubectl gentian tenants activate-admin user` for a new one |

**How the user tenant of a single-tenancy cluster comes to exist.** Step 0
scaffolds `clusters/<cluster-id>/tenants/user` beside the platform tenant's
and commits both, signed. The manifest is the one the director writes for any
new tenant; nothing in it admits the tenant early. The cluster refuses it
until the platform admin has signed in once (the handover gate), so through
the whole install the Argo CD Application `tenant-user` fails and, after ten
retries over about a quarter of an hour, stops retrying. `E-03` ends the
handover as on any cluster. `E-04-user-tenant` then asks Argo CD for a new
sync, waits for `Tenant/user` to be Ready, and issues the user admin's
activation link. It uses the kubeconfig and the `keycloak-admin` Secret, as
`--activate-admin` does, and nothing the handover revoked.

Step 0 writes no user tenant when the definition already holds one, when it
holds another tenant for users (a single-tenancy cluster carries exactly one,
and which stays is yours to decide), or when `tenants/user` was there before
and was removed (a retired tenant keeps its data, and the install does not
bring it back; `kubectl gentian tenants create user` does). If the tenant is
not Ready within the wait, the install warns, says how to check, and does not
fail; `--status` reports `E-04` as outstanding until it is.

On a single-tenancy cluster these names directly under the cluster's domain
are the platform's own, and a component or app of the user tenant that would
answer on one is refused (`HostReserved` on its status): `id`, `platform`,
`www`, `argocd`, `headlamp`, `llm`, `mail`, `imap`, `mail-egress`, `corp`.
Under either tenancy mode the platform's own address names in a tenant
(`desktop`, `admin`, `store`, and the names a sign-in page would have) are
refused to apps the same way ([design/routing.md §3.1](design/routing.md)).

The platform tenant's admin console is two labels under the cluster's domain,
so it has a wildcard certificate of its own, `*.platform.<kernel-domain>`,
issued by the DNS-01 issuer every tenant's wildcard is issued by.

### Image tags

The installer pulls published images and builds none. CI publishes a moving tag
per branch (`develop`), the version for a release tag (`v1.2.3` → `1.2.3`), and
`<branch>-<short-sha>` per commit.

`GENTIAN_OS_IMAGE_TAG` is resolved from the checkout unless set: a branch
resolves to `<branch>-<short-sha>` of the commit you are installing from, so
the manifests Argo CD syncs and the binary the kubelet pulls come from one
commit. A release tag resolves to its version. Set it only to run something
else — and a value that names a moving tag is warned about, because a
Deployment on one never rolls by itself and a pod that restarts picks up
whatever the tag meant at that second.

Nothing advances the pin on its own. `./install.sh --only B-01`
does, which is how a cluster following a branch takes a newer build; if the
commit has not been published yet, the preflight says so rather than leaving
the cluster on an older image. `PORTAL_IMAGE_TAG` follows gentian-ui's tags the
same way. A cluster pins its own tags in
`clusters/<cluster>/kernel/values.yaml`.

### The director's write-back

The director on the cluster is the only thing that writes to
`gentian-deployments` after the install — tenants, app installs and uninstalls
are commits it makes, signed with its own key. Its push credential is the
`deployments-repository` credential the installer asks for, projected by the
composition of `claims/deployments-repository.yaml`; without that claim every
write answers 503. Argo CD accepts commits from the director's key and the
break-glass key only, as listed in `clusters/<cluster>/kernel/signing/keys.env`.

### Which credentials the installer handles

`credentials.yaml` gives every requirement a `phase`, and the phase decides who
asks for it:

| `phase` | Asked by | Why |
|---|---|---|
| `bootstrap` | The installer, at the prompt | The cluster does not exist yet, so nothing on it can |
| `runtime` | The custodian, once the cluster runs | It can validate, record who set it, and gate the claims that need it |

The bootstrap set is deliberately small and every member is validatable with
`curl` or `openssl` alone. A credential needing an SDK or a signing algorithm is
`runtime` by that fact, and the shell never sees it — which is what stops
credential logic accumulating in the installer.

The manager holds no OpenBao token of its own: it exchanges the caller's
Keycloak token for a short-lived one, so the write carries a human identity into
the audit device. It has no endpoint that returns a value.

### What the master password does

It depends on the cluster's `secretMode`, a field on the `Cluster` claim:

| `secretMode` | Kernel credentials are | Reproducible on a rebuild? |
|---|---|---|
| `derived` (default) | `HMAC-SHA256` of the master password **and** a per-cluster salt | Yes — with **both** |
| `random` | Generated once by `openssl rand`, then stored | **No.** The master password only guards the paths |

The salt is generated at first install and stored in OpenBao beside the password.

> Under `derived`, reproducing a cluster's credentials needs the master password
> **and** the salt. The salt lives only in OpenBao, so a disaster that loses
> OpenBao's storage also loses it, and the master password alone reproduces
> nothing. `./install.sh --export-recovery-kit` captures both, plus the unseal
> material and the cluster's identity, in one encrypted file — see step 8 of
> [GETTING-STARTED.md](../GETTING-STARTED.md).

Under `random` there is nothing to reproduce; recovery means restoring OpenBao.

### Where the backup key lives

Scheduled backups are encrypted to the cluster's age key. Encryption needs only
the public half, so that is all the operator ever holds — the private half is
what opens a bundle, and where it lives is a choice.

`./install.sh --export-recovery-kit` generates the pair on a cluster that has
none: the public half goes to OpenBao at `gentian-os/kernel/backup/recipients`,
and the private half into the kit. It never regenerates, because a second pair
would orphan every bundle written to the first.

`spec.backup.escrowIdentity` in the cluster claim decides whether the private
half is *also* stored in OpenBao, at `gentian-os/kernel/backup/identity`:

| | Restoring needs | The risk you are taking |
|---|---|---|
| `true` (default) | OpenBao credentials, or the kit | whoever reaches OpenBao as a cluster administrator gets the bundles *and* the key that opens them |
| `false` | the recovery kit | losing every copy of the kit loses every backup, with no recourse |

On by default, because the likelier disaster is a lost recovery kit rather than
a stolen cluster, and a backup nobody can open is not a backup.

Off is the stronger position against tampering and theft: nothing the cluster
holds can open a bundle, so an attacker who takes the cluster gets ciphertext
and a public key — which is what an attacker who already has the bucket has.
Choose it when the cluster is the likelier loss and you are certain of your kit
custody, because it makes the first kit irreplaceable.

Escrow is read by the `cluster-admin` policy and explicitly denied to `eso-read`,
so the key cannot be turned into a Kubernetes Secret by anything that can write
an `ExternalSecret`. It means "a cluster administrator can read it", not "the
cluster can read it"; `make test-policy-openbao` asserts both halves against a
real OpenBao.

It also has a second effect worth knowing: with escrow on, a later
`--export-recovery-kit` reads the identity back and the new kit carries it. With
escrow off, the first kit is irreplaceable, and every kit written afterwards is
missing the one value that cannot be regenerated.

Only the literal `false` turns escrow off. The XRD defaults the field, so a
current cluster always states it; an empty answer means the resource could not
be read at all, and the kit export says which way it defaulted rather than
leaving it to be inferred.

To restore from an escrowed key:

```bash
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > identity.txt
age -d -i identity.txt manifest.json.age > manifest.json
```

Turning escrow on for a cluster whose key predates it stores nothing by itself —
the key is only ever written at generation. Supply it once from the kit:

```bash
bao kv put -mount=secret gentian-os/kernel/backup/identity identity=@identity.txt
```

---

## 5. Installing on an internal domain

A domain that is not publicly resolvable cannot be reached by Let's Encrypt, and
no certificate is ever issued. The symptom is unhelpful: every Gateway sits at
`ResolvedRefs=False` complaining about a missing Secret, which says nothing
about DNS.

Use the cluster's own certificate authority instead. In
`clusters/<cluster>/kernel/claims/cluster.yaml`:

```yaml
spec:
  kernelDomain: platform.internal
  certificates:
    issuerMode: self-signed
```

Issuance is then offline and instant. The certificates are not publicly trusted,
so anything validating a kernel hostname from outside the cluster needs the root
CA in its trust store:

```bash
kubectl get secret gentian-root-ca-tls -n cert-manager \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > gentian-root-ca.crt
```

The other modes are `acme-dns01` (the default; public DNS, wildcards),
`acme-http01` (public DNS, no wildcards) and `private-ca` (you supply the CA).
Selecting a mode the cluster cannot satisfy fails with a message naming the
mode; it never falls back to a public issuer.

---

## 6. Installing from a mirror

For an air-gapped or forked install, point the platform at your own copies in
`install.env`:

```bash
GENTIAN_OS_REPO=https://git.internal/gentian-os
GENTIAN_OS_IMAGE_REPOSITORY=registry.internal/gentian-os
GENTIAN_DEPLOYMENTS_REPO=https://git.internal/gentian-deployments
```

Both the Git origin and the image registry are redirected, including for every
child ApplicationSet the platform creates.

App profiles are not part of this: they are fetched from a catalogue, which is
a public https address (`GENTIAN_CATALOGUE_URL` for the default one). A
catalogue on a private network is refused, so a cluster that cannot reach a
public catalogue has profiles committed into `clusters/<cluster>/catalogue/` by
hand, as step 0 does for `GENTIAN_DEFAULT_PROFILES`.

`versions.yaml` is the inventory of everything else the install pulls —
Crossplane, cert-manager, External Secrets Operator, ArgoCD, Envoy Gateway and
the OpenBao CLI, each with its pinned version and source. That file is what to
mirror.

---

## 7. Day-2 credentials

On a running cluster, credentials are supplied through the on-cluster credential
manager rather than the installer. It validates a value against the system it is
for before storing it, and never displays a stored value — metadata only:
whether it exists, who set it, and when.

Lost credentials are rotated, not recovered.

The installer's bootstrap token is revoked by the last step once an OIDC write
path is configured. Until then that token is the only way to write a credential,
so the step refuses to revoke it and says so.

---

## 8. Uninstalling

Uninstall is the same steps in reverse:

```bash
./install.sh --uninstall --dry-run    # see the order first
./install.sh --uninstall
./install.sh --uninstall --skip E-01  # keep tenant workloads and their Git manifests
./install.sh --purge                  # the same, plus OpenBao and infra volumes and local state
./install.sh --purge --cluster-infra  # the same, plus CNPG, Reloader and their CRDs
```

OpenBao KV data survives an uninstall, so reinstalling onto the same cluster
recovers the credentials rather than re-prompting. A purge removes it; the
recovery kit is then the only way to rebuild the cluster as itself. Neither
touches the deployments repository or the signing keys in `~/.gentian/gnupg`.

---

## 9. When something is wrong

**Start here.** It names the step whose expectation is unmet:

```bash
./install.sh --status
```

**A resource is not becoming Ready.** Most of the cluster is reconciled by
Crossplane and ArgoCD after the installer finishes:

```bash
kubectl get managed
kubectl get application,applicationset -n kernel-gitops
kubectl describe cluster.gentianos.io -n kernel-provisioning
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
