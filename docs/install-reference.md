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
| **C** | `platform` | The Cluster claim, ApplicationSets, wildcard certificate, credential catalogue, repository hand-off |
| **D** | `applications` | Operator and director, the kernel hostnames resolving, kernel realm and platform desktop, OpenBao OIDC, kernel Gateway |
| **E** | `handover` | Recovery kit, revoke the bootstrap token; on a single-tenancy cluster, the user tenant after that |

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
the deployments repository. After the handover `B-08`, `B-09`, `B-10` and
`D-04` read `undefined` as well: their checks ask OpenBao, and the installer's
token for it is revoked.

---

## 2. Commands that change nothing

```bash
./install.sh --explain    # every step, what it provides and what it mutates
./install.sh --status     # run every check() against the cluster
./install.sh --dry-run    # run the checks, print the plan, change nothing
./install.sh --validate   # report the configuration, run the pre-flight; changes nothing
```

`--status`, `--dry-run` and `--validate` read the cluster, so it has to be
reachable.
`--validate` checks the step contracts, prints a report of the configuration it
found — the credentials in the environment, the settings of the Cluster claim,
`install.env` — ending in a line `Result:`, and then runs the install's
pre-flight (the tools, the cluster and its Kubernetes version, the operator
image). It asks no question and runs no step's `check()`. With an error in the
report it stops there, exit status 1. A value the install asks for is not an
error: `MASTER_PASSWORD` is listed as `[PENDING]` unless it is in the
environment, the `~/.gentian` cache or the cluster's OpenBao, and so is the
kernel domain before the first install — except in an unattended run
(`GENTIAN_NONINTERACTIVE=1`), which has nobody to ask and needs
`KERNEL_DOMAIN` given.
`--dry-run` and `--validate` do not write the cluster's definition; before the
first install a dry run therefore stops at
`clusters/<cluster-id>/kernel is incomplete`, and a validation says that an
install writes it and goes on to the pre-flight.

None of the four collects a credential. `--dry-run` and `--validate` run the
same preflight as an install except for that: no step's `check()` reads a
credential, so a dry run has everything it needs to print the plan. The
install collects them; the preview does not.

"Changes nothing" is meant of everything an install touches, not only of the
cluster. Under `--dry-run` and `--validate`:

| | An install | A dry run or a validation |
|---|---|---|
| The cluster | applies, patches, deletes | only reads (`kubectl get`, `helm list`, `GET` requests to OpenBao) |
| The deployments checkout | fast-forwards it, completes the cluster's definition, commits what is uncommitted, pushes | reads it as it is. It asks the remote where its branch is (`git ls-remote`) without fetching, and says so if the checkout is behind |
| The signing keys | generates the break-glass key if this cluster records none; refuses when it records one this host does not hold | generates nothing, imports nothing, and does not start `gpg` on a host with no keyring |
| `~/.gentian` | writes `config`, the credential cache, the keyring | writes nothing |
| `~/.local/bin` | fetches the OpenBao CLI when `bao` is missing, and unpacks it only if the archive matches the release's checksum list | fetches nothing; says it would |

Wherever an install would have acted, the run prints a line beginning
`Would`: which uncommitted files it would commit and push, an edit it would
make to the claim, a file it would write. `--dry-run` also applies to
`--uninstall` and `--purge`, where it previews the teardown and asks for no
confirmation. It does not apply to `--verify-only`, `--activate-admin` or
`--export-recovery-kit`, which have no preview: combined with one of them it
is refused, rather than ignored (`… has no preview`). The same holds for
`--validate` with any of the three.

On an install, the break-glass key is found by the id the deployments
repository records for the cluster
(`clusters/<cluster-id>/kernel/signing/keys.env`), not by its name. Only a
first install, which has no record yet, looks it up by name, and only once
the kernel domain is known. A host whose keyring does not hold the recorded
key is refused — see *The recorded signing key is not on this machine* in §9.
A dry run says an install would stop, and goes on with its preview.

This is held by a test that runs the installer in both modes with stand-ins
for `git`, `gpg`, `kubectl`, `helm`, `curl` and `bao` that fail on anything
that would change something, and with a home directory that must be
byte-identical afterwards (`make test-dry-run-changes-nothing`).

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

The deployments repository also holds `profiles/_base.yaml` and
`profiles/<stage>.yaml`, the values every cluster, and every cluster of one
stage, share ([deployment.md](deployment.md) §1). Both must exist, committed
and pushed, even when they set nothing (`{}`): Argo CD reads them as values
files of the platform's own chart and renders nothing when one is missing, so
an install into a repository without them stops at `D-01` with no operator.
Step 0 warns about a missing one and does not write it.

A value exported in the environment `install.sh` is started from beats the
same variable in `install.env`, whichever variable it is, and the run says so
when the two differ (`… is set in the environment; that value is used`). An
exported empty value is not a value. Below that:

A cluster property set in `install.env` beats the claim — the file is loaded
first. The installer warns (`… is set in install.env — it overrides
claims/cluster.yaml`) rather than reversing the precedence, because an operator
who wrote it there meant something, but the claim is where it belongs. The
template lists them at its end: `TENANCY_MODE`, `SECRET_MODE`, `NETWORK_MODE`,
`NODE_IP`, `STORAGE_CLASS`, `PLATFORM`, `MAIL_SERVICE_MODE`, `DNS_PROVIDER`,
`ACME_ENV`, `CERT_ISSUER_MODE`, `LETSENCRYPT_EMAIL`, `LLM_SUPPORT`,
`GPU_ACCELERATION`. An
unattended first run exports them for that run
([GETTING-STARTED.md](../GETTING-STARTED.md), *Installing unattended*).

### What `install.env` holds

The template carries one line per setting; the reasoning is here.

| | |
|---|---|
| `GENTIAN_DEPLOYMENTS_CLUSTER_ID` | The directory name under `clusters/` — **not** the Kubernetes cluster name and not a kubeconfig context. With `_STAGE` it also names the Cluster claim on first bootstrap, so it must be right before the first run: scaffolding pushes the tree it names to a shared repository. |
| `GENTIAN_DEPLOYMENTS_STAGE` | `dev`, `staging` or `prod`. A cluster keeps one stage for life, which is why there is no `<stage>` segment inside its own tree. |
| `GENTIAN_*_AUTH` | `none`, `basic` (username + token) or `bearer` — how the installer authenticates to that repository. The credential itself is prompted for. Deployments defaults to `basic` because a private repository cannot describe its own access; set `none` for a public one. Argo CD reads the deployments repository and `gentian-os` with the same setting — see *How Argo CD gets a repository's credential* below. |
| `GENTIAN_*_REPO` / `_BRANCH` | Point them at a mirror for a forked or air-gapped install; the child ApplicationSets follow. `GENTIAN_DEPLOYMENTS_REPO` has no default: unset, the install asks for it, and an unattended run stops. |
| `GENTIAN_OS_BRANCH` | The ref every in-cluster Application tracks — [deployment.md §4](deployment.md). Unset, it is the branch of the checkout the installer is run from, everywhere it is used: the gentian-os Repository claim step 0 writes, the bootstrap Applications, the default catalogue and the image tag. A release tag has to be named here; with a detached checkout and no value the install stops. The installer runs the image built from the checkout's commit under that name. |
| `GENTIAN_UI_BRANCH` | The branch of `gentian-ui` written to that repository's claim when step 0 scaffolds it. `develop` by default, the branch `gentian-ui` publishes from. |
| `GENTIAN_DEPLOYMENTS_PATH` | The local checkout of the deployments repository. `~/.gentian/gentian-deployments` by default. |
| `CLUSTER_ADMIN_RECOVERY_EMAIL` | Where the platform admin's activation link is mailed at the handover, and the account's recovery address afterwards. Unset, the handover asks; with no address, or while the kernel realm cannot send mail yet, the link is shown once in the terminal. |
| `GENTIAN_HANDOVER_WAIT_SECS` | How long `E-03` waits for the platform admin's sign-in. 1800 by default. |
| `GENTIAN_NONINTERACTIVE` | `1` takes the default for every question and asks for nothing. Then `GENTIAN_DEPLOYMENTS_REPO` has to be set, `KERNEL_DOMAIN` has to be given for the first run, `GENTIAN_KIT_RECIPIENT` has to be set, and the bootstrap credentials come from the environment. |
| `GENTIAN_KIT_RECIPIENT` | An age public key the recovery kit is encrypted to, in place of a passphrase typed at the terminal. |
| `GENTIAN_KIT_INCLUDE_BREAK_GLASS` | `0` keeps the break-glass signing key out of the recovery kit. Then only the install host can sign for the deployments repository when the director cannot. |
| `GENTIAN_NO_CREDENTIAL_CACHE` | `1` keeps typed credentials in the process only; a resumed run asks again. |
| `GENTIAN_MASK_SECRETS` | `1` reads secret fields without showing them. By default what is typed or pasted is shown. |
| `GENTIAN_GPG_HOME` | The keyring of the two deployment signing keys. `~/.gentian/gnupg` by default. |
| `PORTAL_IMAGE_TAG` | The branch whose charts are installed for the platform's own apps. `develop` by default — see *Image tags* below. |
| `GENTIAN_CATALOGUE_URL` | The address of the default catalogue, `gentian`, written to `spec.catalogue.sources` when step 0 scaffolds a new Cluster claim. Unset, the address follows from what is installed: a cluster installed from a release tag or from `main` (`GENTIAN_OS_BRANCH`, or the checkout's branch) gets the released catalogue, `https://gentian-org.github.io/gentian-apps`; one installed from any other branch gets the development catalogue, `https://gentian-org.github.io/gentian-apps/develop`. Set, it is used whatever the ref. Step 0 prints the choice and writes the reason above the address in the claim. A public https address; the director fetches from nothing else ([custom-catalogues.md](custom-catalogues.md)). An existing claim is never rewritten: on an existing cluster the address is changed with `kubectl gentian catalogues` ([custom-catalogues.md §3](custom-catalogues.md)). |
| `GENTIAN_STORE_URL` | The base address of the App Store API, written to `spec.catalogue.storeUrl` when step 0 scaffolds a new Cluster claim. Defaults to `https://store-service.aluvian.io`. It must not be an address a cluster's own App Store app could have — `store.<a domain a cluster is installed under>` — which is why the default is `store-service.…` and not `store.…`; a cluster whose claim names its own App Store host offers no App Store (`store-address-is-own-host`). |
| `TENANCY_MODE`, `SECRET_MODE` | Not for `install.env`. Step 0 asks for both; an unattended first run exports them (`multi` or `single`; `derived` or `random`). They are written to the Cluster claim (`spec.tenancyMode`, `spec.secretMode`), which owns them from then on. See *Tenancy modes* and *What the master password does* below. |
| `GENTIAN_USER_TENANT_WAIT_SECS` | How long `E-04` waits for the user tenant of a single-tenancy cluster to be Ready. 900 by default. |
| `KERNEL_NETWORK_POLICIES` | `true` or unset. **Off by default.** On, step `A-01` gives every kernel namespace the NetworkPolicies of `kernel/security/network-policies` -- ingress is refused unless listed, egress is not restricted ([design/security.md §2.13](design/security.md)) -- and `B-01` tells the operator, which then narrows what tenants and publishing proxies admit from the edge namespace. Anything else removes them. Turn it on after a successful install, and off again if a component then times out reaching another: §9. |
| `STORE_NETWORK_POLICIES` | `true` (default) or `false`. The policies on the shared stores, the kernel's PostgreSQL, the mail servers and the model gateway ([design/security.md §2.7](design/security.md)). |
| `GENTIAN_NO_LICENCE_REPORT` | `1` turns the licence report off, as `--no-licence-report` does; `0` turns it back on. Unset keeps what the cluster has. Off, nothing is sent and the App Store is not offered: no tenant gets the App Store app — [design/operations.md §6.2](design/operations.md). |
| `GENTIAN_LICENCE_REPORT_URL` | Where the licence report goes, instead of the default address in `kernel/bootstrap/chart/values.yaml`. `https` only. |
| `OPENBAO_CLI_VERSION` | Which `bao` to fetch when none is on `PATH`. Defaults to the pin in `versions.yaml`, which is where component versions are declared. The archive is fetched from the OpenBao release for Linux or macOS on x86_64 or arm64, compared with that release's checksum list, and installed to `~/.local/bin` only when it matches; on any other host install `bao` yourself. |
| `GENTIAN_DEFAULT_PROFILES` | The profiles step 0 places in `clusters/<cluster>/catalogue/`, on every install run: https addresses of profiles in a catalogue (`<catalogue>/profiles/<name>.yaml`), comma separated, each optionally pinned as `<address>@sha256:<digest>`. Unset, it is one profile, the Operations Console's, from `https://catalogue.aluvian.io/profiles/operations-console.yaml` (`GENTIAN_STORE_CATALOGUE_URL` replaces the address before `/profiles/`). Set, the value is used as it stands, and set empty places none; `--disable-api-extensions` places none. A profile is written only when it hashes to a stated digest — see *The default profile* below. A local file or an `http` address is refused. Remove a line an earlier install left here. |
| `CROSSPLANE_ACTIVATE_ALL` | `true` installs every resource type of every Crossplane provider, as installs did before; unset, only the types `crossplane/providers/activation.yaml` names exist. The way back if a fresh install waits on a type that list lacks — see *Provider resource types* below. |
| `GENTIAN_OS_IMAGE_TAG` | Leave it unset — see *Image tags* below. |

### How Argo CD gets a repository's credential

Every repository credential reaches Argo CD from OpenBao: the repository's
`Repository` claim composes an ExternalSecret, and External Secrets writes the
Secret `repo-<name>` beside Argo CD. Two repositories are read before that can
happen, and for those the installer bridges the gap with the credential it
collected at the prompt.

| Repository | Why it is read too early |
|---|---|
| `gentian-os` | The bootstrap Applications (`B-01`) read it before OpenBao is initialised. |
| deployments | Its own `Repository` claim is `clusters/<id>/kernel/claims/deployments-repository.yaml` — a file in the repository, delivered by the `gentian-claims-<stage>` Application, which reads that repository. So do the catalogue and the tenants ApplicationSets. |

Where `GENTIAN_<repository>_AUTH` is `basic` or `bearer`:

1. `A-06-argocd` applies a Secret `argocd-repo-creds-bootstrap-<name>` in
   the gitops namespace, labelled `argocd.argoproj.io/secret-type:
   repo-creds`, for the repository's address. With `basic` it carries the
   user name (`x-access-token` when none was given, which is also what the
   vault is seeded with) and the token; with `bearer`, the token alone.
2. `B-08-seed-secrets` writes the same login to
   `gentian-os/kernel/repositories/<name>` in OpenBao.
3. `C-02-appsets` delivers the claims; the `Repository` claim composes
   `repo-<name>`. From the moment that Secret carries a login it is the one
   Argo CD uses: a `repo-creds` Secret is a template Argo CD falls back to
   only for a repository whose own Secret has no login. The two have
   different names and types and never overwrite each other.
4. `C-05-repository-handoff` waits for `repo-<name>` to carry a login and
   then deletes the bridge. If it cannot confirm that within two minutes it
   leaves the bridge in place and says so; `./install.sh --only C-05` tries
   again.

Where the setting is `none`, nothing is registered at any of these steps.

After the hand-off there is one copy of the credential in the cluster, and it
follows the vault: a rotated token is written to OpenBao through the custodian
(or by `./install.sh --only B-08` with the new token), and External Secrets
refreshes `repo-<name>` within the hour. A token rotated while the bridge
still exists — an install that has not reached `C-05` — is noticed by `A-06`,
whose check compares the bridge with the token the run was given and writes
the new one over it; it does so without reinstalling Argo CD.

The token is never printed, and is never an argument of a command: it goes
from the installer's environment into the Secret through a pipe.

### Where app profiles come from

A `ComponentProfile` reaches a cluster when a tenant installs the app: the
director fetches that one profile from a catalogue, checks it against the
digest the install names, and commits it under `clusters/<cluster>/catalogue/`
in the deployments repository. The catalogues are addresses declared on the
Cluster claim and on tenants ([custom-catalogues.md](custom-catalogues.md));
a new claim names the default one.

### The default profile, and the digest it is held to

One profile is placed by the installer rather than by the director: the
Operations Console's, at step 0, before there is a director to ask. It is held to the same rule as
every other — a profile reaches a cluster only at a stated digest:

1. The installer reads `<catalogue>/index.yaml` and takes the digest it lists
   for the entry. A pin in `GENTIAN_DEFAULT_PROFILES`
   (`https://…/profiles/<name>.yaml@sha256:<digest>`) is used instead, and the
   index does not override it.
2. It downloads `<catalogue>/profiles/<name>.yaml` and writes nothing unless
   the bytes hash to that digest.
3. It writes what the director writes for an install of the same bundle, byte
   for byte: `<name>.yaml` as served (the profile and whatever travels with
   it), `<name>.bundle.yaml` carrying the same bytes and the catalogue's
   origin (`cluster/<source>`: the name the Cluster claim gives the catalogue
   at that address, else one made of the address), and both listed in
   `kustomization.yaml`.
4. The digest, who stated it — the index or the pin — and the address go into
   the signed commit that carries the profile.

| What happens | The install |
|---|---|
| The catalogue cannot be reached (no answer, or a 5xx) | goes on without the profile, with a warning. Run the installer again later, or install the profile through the director |
| The bytes do not hash to the digest | **stops** at step 0; nothing of the profile was written |
| The index does not list the entry, or there is no index, and nothing is pinned | **stops**: there is no digest to hold the file to |
| The file is at the right digest and holds what a bundle may not (another kind, a name that is not this profile's) | **stops** |
| An entry is a local file, an `http` address, or not `<catalogue>/profiles/<name>.yaml` | **stops**, and says what to write instead |
| The cluster's definition already holds the profile at that digest | nothing changes; the profile is not fetched |
| It holds **another** build | it is kept and reported with both digests; nothing is fetched or replaced. Move to the new build through the director, or remove the files and run again |
| It holds a copy an earlier installer wrote, with no record | the record is added if the copy hashes to the digest; otherwise the copy is replaced by the verified file, as earlier installers replaced it on every run, and the run says so |

`--dry-run` says which address would be read and which digest would be
required, and asks no catalogue.

The operator compares a profile with its bundle at rollout. For a Component
pinned to a digest the bundle must hash to that digest. The Component the
operator creates by default for the Operations Console names no digest: the
digest it is held to is the one recorded here, that of the bundle beside the
profile. A profile, or anything its bundle brings, that is not what that
bundle says stops the rollout, and the Component says why (`DigestMismatch`,
`DigestUnverifiable`, `BundleRefused`, `CompanionMissing`,
`CompanionMismatch`); what is running is left as it is. A profile replaced
together with its bundle is not noticed: nothing outside the profile states
a digest for a default.

### Provider resource types

Crossplane's providers ship far more resource types than the platform uses:
the vault and Keycloak providers 374 between them, of which the Compositions,
the kernel charts and the operator use 25. Every installed type costs
API-server memory, which is what a managed control plane charges for. So the
installer creates only the ones in use:

- `A-04` installs Crossplane without its default policy that activates every
  type (`provider.defaultActivations: []`).
- `B-05` applies `crossplane/providers/activation.yaml` — one
  `ManagedResourceActivationPolicy` listing the 25 types — before the
  providers, and waits until each of them is an established CRD.
- `provider-http`, which nothing used, is no longer installed, and is removed
  from a cluster that has it.

provider-kubernetes and provider-helm do not declare Crossplane's safe-start
capability; all of their (seven) types are installed whatever the list says.

**This takes effect on a fresh install.** Crossplane never deactivates a type
and leaves an existing default policy in place, so a cluster that was
installed with every type keeps every type; the list is applied there too and
changes nothing.

To see it:

```bash
kubectl get mrap                      # the policies: gentian-platform, and "default" only on an older cluster
kubectl get mrd | grep -c Active      # the types that exist as CRDs
kubectl get mrd | grep -v Active      # the ones that were left off
```

**If a fresh install waits on a type the list lacks** — `B-05` stops and names
it, or a composite stays unready with a composed resource of a kind the API
server does not know — the way back is one line in `install.env`, and a
second run:

```bash
CROSSPLANE_ACTIVATE_ALL=true
```

`A-04` then reinstalls Crossplane with every type activated. It cannot be
taken back on that cluster; report the missing type so the list gets it.

**Adding a resource of a new provider type** — to a Composition here, to one
an app bundle brings, to a kernel chart or to operator code — needs its
`<plural>.<group>` in `crossplane/providers/activation.yaml`.
`make lint-provider-activation` (part of `make lint`) finds every provider
type in use and fails with the exact line to add when one is missing; it
reads the gentian-apps checkout beside this one too, and
`scripts/lint/lint-provider-activation.py --tree <dir>` runs the same check
on any other tree. The plural comes from `crossplane/providers/types/`, the
providers' own type lists at the pinned versions; after moving a provider pin
in `providers.yaml`, run `make refresh-provider-types`.

### Upgrading a cluster that copied its profiles

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
  (`kubectl delete repository gentian-apps -n kernel-provisioning`); it composes
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
new tenant, with one switch on: the user admin may approve the tenant's
public addresses (`spec.perimeter.adminsApprove: true`). Nothing in it admits
the tenant early. The cluster refuses it
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
bring it back; `kubectl gentian tenants create user` does, and that is the
one use `tenants create` has on a single-tenancy cluster). If the tenant is
not Ready within the wait, the install warns, says how to check, and does not
fail; `--status` reports `E-04` as outstanding until it is.

On a single-tenancy cluster these names directly under the cluster's domain
are the platform's own, and a component or app of the user tenant that would
answer on one is refused (`HostReserved` on its status): `id`, `platform`,
`www`, `argocd`, `headlamp`, `llm`, `mail`, `imap`, `mail-egress`, `corp`.
Under either tenancy mode the platform's own address names in a tenant are
refused to apps the same way: `desktop`, `admin`, `store`, `console`,
`platform`, `id`, `auth`, `login`, `signin`, `sign-in`, `sso`, `account`,
`accounts` ([design/routing.md §3.1](design/routing.md)). Both lists are
matched exactly and with everything below a name (`x.admin`); the director
refuses the install with the reason before anything is committed.

The user tenant needs no domain bound: its domain is the cluster's.
`kubectl gentian tenants domain user <domain>` is refused on a
single-tenancy cluster (`422`, nothing is changed): bound to a domain of its
own the tenant would leave the cluster's addresses and could hold no website
at the main address. `tenants domain user --remove` is not refused, for a
tenant that was bound before.

A website at the cluster's main address (`https://<kernel-domain>/` and
`www.`) exists only here. A profile declares the entry (`apex: true`), and
`kubectl gentian exposures approve <app> <entry> --tenant user
--acknowledge-main-address-rule` publishes it; the director refuses that
approval for any tenant but the user tenant of a single-tenancy cluster.

Who approves in the user tenant: the platform admin, a member of the group
`gentian:tenant:user:perimeter`, which the tenant is created with, empty, and
the user admin. The user admin approves because the install writes the user
tenant with that switch on (`spec.perimeter.adminsApprove: true`); every
other tenant, on a multi-tenancy cluster, starts with it off. The second
switch a tenant can be created with, adding catalogues, is off here as
everywhere. Both are the platform admin's to change after the handover:

```bash
kubectl gentian tenants set user --admins-approve-public-addresses=false  # the user admin no longer approves public addresses
kubectl gentian tenants set user --admins-add-catalogues=true             # the user admin adds catalogues
kubectl gentian tenants show user
```

A user tenant that an earlier install wrote keeps its manifest as it is, with
the switch off. One created later with `kubectl gentian tenants create user`
starts as the install's does, with the switch on, unless
`--admins-approve-public-addresses=false` is given.

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
whatever the tag meant at that second. A set value is otherwise used as it
stands, so a line left in `install.env` by an earlier install pins the
cluster to that older build without a warning: remove it.

The model gateway's console is a claim setting, `spec.llm.console.enabled`,
off by default. Off, `llm.<kernel-domain>` has no route and no DNS name and
the edge is not admitted to the gateway; on, platform administrators get the
console there, behind the kernel sign-in, and a tile for it on their desktop.
A claim that does not state it reads as off, so a cluster that had the console
before the setting existed loses it on the next run; the install prints one
line naming the setting whenever the cluster serves models and the console is
off. Changing it needs no run of the install: the route and the gateway's
network rule both follow the commit to the claim ([llms.md](design/llms.md)).

The models the gateway offers are the claim's, and changing them needs no run
of the install: the cluster declares the credential of a provider the claim
names, and it appears in the administration console once the claim is applied.
`spec.llm.providers` names external OpenAI-compatible
endpoints and their models, `spec.llm.instances` the models a cluster with
GPUs serves itself:

```yaml
spec:
  llm:
    enabled: true
    providers:
      - name: infomaniak
        apiBase: https://api.infomaniak.com/2/ai/<product-id>/openai/v1
        apiKeyProperty: infomaniak_api_key   # always <name>_api_key: the provider's own token
        models:
          - name: gemma-4-31b                # offered as infomaniak/gemma-4-31b
            model: google/gemma-4-31B-it     # the id the provider expects
            maxTokens: 8192
    gpuAcceleration: true                    # instances are read only with this
    instances:
      - name: qwen
        modelId: Qwen/Qwen2.5-7B-Instruct    # offered as qwen-qwen2.5-7b-instruct
```

Commit the claim: Argo CD hands it to the gateway's chart, the chart writes
the gateway's configuration file from it, and the gateway's pods are replaced
to read it. A provider's token is entered in the administration console,
under the credential `llm-provider-<name>`; the gateway restarts by itself
when it arrives or changes, and until then the provider's models are listed
and answer an authentication error. The gateway takes models from nowhere
else, so one that was added at its console on an earlier version is no longer
offered. An instance is offered and **not started**: nothing in the platform
runs the vLLM server behind it yet ([llms.md §5 and §6](design/llms.md), which
also say how to check what is served).

The model gateway's image (`llm.enabled`) is not a setting. It is one chart
value, named by tag and digest, and `make lint` (`lint-image-pins`) fails on
any image under `kernel/`, `charts/` or `crossplane/` tagged `latest`.

Nothing advances the pin on its own. `./install.sh --only B-01`
does, which is how a cluster following a branch takes a newer build; if the
commit has not been published yet, the preflight says so rather than leaving
the cluster on an older image. The tag the installer resolved is written into
the operator's Argo CD Application as an inline value, which Argo CD ranks
above `image.tag` in `clusters/<cluster>/kernel/values.yaml`; pre-flight
checks that both tags exist and says when they differ. `PORTAL_IMAGE_TAG`
names the branch whose charts are installed for the platform's own apps (the
desktop, the administration console, the sign-in page, the App Store app).

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

The custodian holds no OpenBao token of its own: it exchanges the caller's
Keycloak token for a short-lived one, so the write carries a human identity into
the audit device. It has no endpoint that returns a value.

### What the master password does

It depends on the cluster's `secretMode`, a field on the `Cluster` claim:

| `secretMode` | Generated credentials are | Reproducible on a rebuild? |
|---|---|---|
| `derived` (default) | Computed from the master password **and** a per-cluster salt | Yes — with **both** |
| `random` | Drawn at random once, then stored in OpenBao | **No.** The master password leads to none of them |

This covers the kernel's credentials, which the installer makes, and each
app's, which the operator makes when the app is installed. Two kernel
credentials are computed from the master password under `random` as well:
the key Keycloak's event listener signs with, and the kernel realm's own
mail login on a cluster that runs its own mail server
([security.md §6.3](design/security.md)). An app's own secrets follow the
mode, and travel in the tenant's backup with the data that was written with
them ([security.md §6.4](design/security.md)). The installer reads
the claim; the operator reads the same field from the `gentian-cluster-config`
ConfigMap the `Cluster` Composition writes.

Choose the mode at the first install. Changing it on a running cluster
converts nothing: every credential that exists stays as it is, and only
credentials made afterwards follow the new mode. Do not change a `random`
cluster back to `derived`: the next installer run writes derived keys for the
model gateway over the ones its database was created with.

The salt is generated at first install and stored in OpenBao beside the password.

What you have to keep, by mode:

| `secretMode` | Keep | Why |
|---|---|---|
| `derived` | The recovery kit, and its passphrase somewhere else | It holds the master password and the salt, which reproduce every generated credential; the OpenBao recovery key; the backup key; and the break-glass signing key |
| `random` | The recovery kit, **and** snapshots of OpenBao that you take yourself | The kit holds the same things, and none of the credentials drawn at random; only OpenBao has those |

> Under `derived`, reproducing a cluster's credentials needs the master password
> **and** the salt. The salt lives only in OpenBao, so a disaster that loses
> OpenBao's storage also loses it, and the master password alone reproduces
> nothing. `./install.sh --export-recovery-kit` captures both, plus the unseal
> material and the cluster's identity, in one encrypted file — see *The
> recovery kit* in [GETTING-STARTED.md](../GETTING-STARTED.md).

Under `random` there is nothing to reproduce; recovery means restoring OpenBao.
The platform does not snapshot OpenBao, and the recovery kit holds no
generated credential; a tenant's backup holds its apps' own secrets and no
other. So that snapshot is yours to take.
A tenant's backup, restore and import are the same in both modes. What
differs is the loss of OpenBao's storage on a cluster that keeps running:
`derived` writes the same values again, `random` writes new ones that the
running databases, buckets and sign-in clients were not created with.

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
an `ExternalSecret`. The operator can read it too: its policy,
`operator-write`, reads and writes all of `gentian-os/*`, this path included,
so whoever takes the operator's identity at OpenBao holds the key. Escrow
therefore means "a cluster administrator and the operator can read it, and
nothing that goes through External Secrets can". A bundle holds a tenant's
data and its apps' own secrets, so the escrowed key opens both to a platform
administrator, whose policy is otherwise refused every tenant's paths.
`make test-policy-openbao` asserts against a real OpenBao that `cluster-admin`
reads the path and `eso-read` does not.

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

Choose it at step 0's issuer question on a first install. Every later run reads
the mode back from the claim file, so it decides which credentials are asked
for (a DNS credential only under `acme-dns01`) and which issuers step `A-09`
creates before the Cluster object exists; once that object exists (`C-01`),
`A-09` reads the mode from it.

Issuance is then offline and instant. The certificates are not publicly trusted,
so anything validating a kernel hostname from outside the cluster needs the root
CA in its trust store:

```bash
kubectl get secret gentian-root-ca-tls -n kernel-edge \
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
GENTIAN_OS_AUTH=basic                 # when the mirror asks for a login
GENTIAN_DEPLOYMENTS_REPO=https://git.internal/gentian-deployments
```

The Git origin is redirected, including for every child ApplicationSet the
platform creates. The operator's image is not: the cluster pulls
`ghcr.io/gentian-org/gentian-os` whatever `install.env` says, and the
pre-flight looks the tag up there. Nor are the infrastructure charts: there is
no setting that redirects them, and the installer asks for no chart registry
credential.

App profiles are not part of this: they are fetched from a catalogue, which is
a public https address (`GENTIAN_CATALOGUE_URL` for the default one). A
catalogue on a private network is refused, so a cluster that cannot reach a
public catalogue has profiles committed into `clusters/<cluster>/catalogue/` by
hand. (Step 0's own default profile is fetched from a public https catalogue
too, and is skipped with a warning when that cannot be reached — §4.)

`versions.yaml` is the inventory of everything else the install pulls —
Crossplane, cert-manager, External Secrets Operator, ArgoCD, Envoy Gateway and
the OpenBao CLI, each with its pinned version and source. That file is what to
mirror.

---

## 7. Day-2 credentials

On a running cluster, credentials are supplied through the custodian — the
**Credentials** tab of the administration console — rather than the installer.
It validates a value against the system it is for before storing it, and never
displays a stored value — metadata only: whether it exists, who set it, and
when.

Lost credentials are rotated, not recovered.

The installer's bootstrap token is revoked by step `E-03` once a recovery kit
has been exported (`E-02`) and a platform administrator's sign-in has proven
that somebody else can write a credential. Until then that token is the only
way to write one, so the step refuses to revoke it and says so.

---

## 8. Uninstalling

Uninstall is the same steps in reverse:

```bash
./install.sh --uninstall --dry-run    # see the order first
./install.sh --uninstall
./install.sh --uninstall --skip E-01  # keep tenant workloads and their Git manifests
./install.sh --purge                  # the same, plus OpenBao and infra volumes and local state
./install.sh --purge --cluster-infra  # the same, plus CNPG, Reloader, external-dns, cert-manager, their CRDs,
                                      # this cluster's published DNS records and the kept wildcard certificate
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

**The recorded signing key is not on this machine.** The installer signs what
it writes to the deployments repository with the cluster's break-glass key,
and `clusters/<cluster-id>/kernel/signing/keys.env` records which key that is.
When it records one and this host's keyring (`~/.gentian/gnupg`) does not hold
it, the install stops before it writes anything — no file in the checkout, no
key, no object in a cluster — and says what is recorded and what is missing.
It used to generate another key and replace the recorded id. A first install,
which records nothing yet, generates its key as before; other keys in the
keyring change nothing either way. The ways forward:

```bash
./install.sh --recover <kit>             # the kit carries the key (unless exported with GENTIAN_KIT_INCLUDE_BREAK_GLASS=0)
# or run the installer on the machine that holds the key, or copy its ~/.gentian/gnupg here
./install.sh --rotate-break-glass-key    # only when the key is lost for good
```

`--rotate-break-glass-key` generates a new key in place of the recorded one.
It asks a person to type the old key's id at a terminal; without a terminal,
or with `GENTIAN_NONINTERACTIVE=1`, it is refused, and no variable confirms it
on their behalf. It is also refused when the recorded key is present or when
nothing is recorded. Before it asks, it prints what follows from a new key:
the commit that names it is signed with it, and Argo CD on a running cluster
refuses the repository from that commit on until the AppProject `gentian`
(`spec.sourceIntegrity`, rendered by `B-01` from `keys.env`) and the ConfigMap
`argocd-gpg-keys-cm` (filled by `B-10` from `break-glass.asc`) name the new
id. On a cluster that is already installed `B-01` reports itself satisfied, so
apply both with `./install.sh --only B-01,B-10 --force`; then export a new
recovery kit, because the old one carries a key the cluster no longer trusts.

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

**Turning the kernel's network rules on, and what to do when a component
then cannot reach another.** With `KERNEL_NETWORK_POLICIES=true` every kernel
namespace refuses a connection that its NetworkPolicy does not list
([design/security.md §2.13](design/security.md)). The rules are off by
default because no cluster had run them when they were written. On a cluster
whose install has succeeded:

```bash
# in install.env
KERNEL_NETWORK_POLICIES=true

./install.sh --only A-01,B-01
kubectl get networkpolicy -A -l gentianos.io/kernel-network-policy=true   # twelve
```

`A-01` applies the rules; `B-01` passes the switch to the operator, which
restarts and narrows the tenants' side. Then check, in this order: a page
behind sign-in opens (the desktop); the administration console lists people
and saves a setting; `kubectl get externalsecret -A` shows none failing;
`kubectl get managed` and `kubectl get application -n kernel-gitops` settle
as before; a new app installs. A fresh install with the switch on is the
last check, and the one that says it can become the default.

A caller the list lacks shows as a timeout, never as a refusal with a
reason: a pod that waits for its configuration, `context deadline exceeded`
or `i/o timeout` in a controller's log, an Application that stays
`Progressing`. To rule the rules out, remove them -- this takes effect at
once and nothing puts them back until `A-01` runs again:

```bash
kubectl delete networkpolicy -A -l gentianos.io/kernel-network-policy=true
```

To keep them off, set `KERNEL_NETWORK_POLICIES=false` (or remove the line)
and run `./install.sh --only A-01,B-01` again. If the trouble went away, the
list is missing a caller: add it to
`internal/kernel/kernelnet/inventory.yaml`, run
`make gen-kernel-network-policies`, and turn the switch back on. Where to
look first, by symptom:

| Symptom | The flow to check |
|---|---|
| Every signed-in page answers 403 or 500 | the Envoy proxies to the bouncer (`kernel-edge`, 9001), the bouncer to OpenFGA (`kernel-authorization`, 8080) |
| No page answers at all; the Envoy pods are not Ready | the Envoy proxies to Envoy Gateway (`kernel-edge`, 18000) |
| The operator's pod restarts until OpenFGA answers | the operator to OpenFGA (`kernel-authorization`, 8080) |
| ExternalSecrets stay `SecretSyncedError` | External Secrets to the vault (`kernel-secrets`, 8200) |
| The vault stays sealed after a restart | the vault to its seal (`kernel-seal`, 8200) |
| Realm or client Jobs time out; `Realm` objects not Ready | the Jobs and provider-keycloak to Keycloak (`kernel-authentication`, 8080) |
| The desktop or the administration console shows errors after sign-in | the tenant's namespace to the director, usher, custodian, registrar (`kernel-control`, 8080, 9444, 9445) |
| A tenant's apps answer 503 from the Gateway while their pods are Ready | the Envoy proxies into the tenant's namespace: the operator's `tenant-isolation` policy then admits pods labelled `app.kubernetes.io/name=envoy` in `kernel-edge` only. It follows the switch through `B-01`, not through the `kubectl delete` above |
| A published page (the bare domain's sign-in form) answers 503 | the Envoy proxies to the publishing proxy in the tenant's DMZ: its `<name>-ingress` policy, which follows the switch the same way |
| "failed calling webhook" on any apply | not these rules: every webhook port admits any source. Check the webhook's pod |

