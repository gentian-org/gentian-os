---
audience: operator
type: guide
---

# Deployment Environments and Promotion

**Companion to:** [architecture.md](../overview/architecture.md), [operations.md](../concepts/operations.md)

This guide describes how Gentian OS clusters are configured, how a new
cluster gets bootstrapped, and how releases promote from development to
production. For first-time bootstrap steps see
[GETTING-STARTED.md](../../GETTING-STARTED.md); for day-2 commands see
[commands.md](../reference/commands.md).

**Design principle:** `gentian-os` is agnostic to app, tenant, stage, and
cluster — it ships defaults and generic templates only. Every value that
actually varies (which cluster, which stage, which domain) lives in
[`gentian-deployments`](https://github.com/gentian-org/gentian-deployments),
split by how widely it's shared, never duplicated across files that could
drift from each other.

---

## 1. Layered configuration

Four layers, each holding only what the layer below it can't know:

| Layer | Lives in | Scope | Holds |
| --- | --- | --- | --- |
| 1. Chart defaults | `gentian-os/charts/gentian-os/values.yaml` | every cluster, every deployer | a default for every key |
| 2a. Cross-stage shared | `<deployments>/profiles/_base.yaml` | every cluster of this deployment | what is the same in all stages but not something every deployer wants |
| 2b. Stage profile | `<deployments>/profiles/<stage>.yaml` | every cluster of that stage | genuine stage deltas only: log level, ACME issuer |
| 3. Cluster overlay | `<deployments>/clusters/<cluster>/kernel/values.yaml` | one cluster | genuine deltas only |
| 4. Cluster definition | `<deployments>/clusters/<cluster>/kernel/claims/cluster.yaml` | one cluster | what the cluster is: `kernelDomain`, tenancy mode, network mode, mail, models, catalogs |

Argo CD merges the Helm values of the platform's own chart in that order
(1 → 2a → 2b → 3), and over them the values the installer renders into the
`gentian-os` Application itself: the image tag (§4) and the namespace layout.
Both profile files must exist, even empty
([reference/install.md](../reference/install.md) §4). Layer 4 isn't a Helm value
at all — it's the Crossplane `Cluster` claim. Its schema
(`crossplane/xrds/cluster.yaml`) requires only `kernelDomain`; every other
field has a default, and step 0 of the installer writes each setting it asked
for.

**When in doubt where a value belongs:** would a different organization
deploying gentian-os from scratch want this value too? If yes, chart default.
If it is the same across your stages but particular to your deployment,
`_base.yaml`. If it varies by stage, `profiles/<stage>.yaml`.

**`kernelDomain` is authored on the claim** (Layer 4). `install.sh` reads it
from there. Layer 3's `values.yaml` repeats it for the platform's chart,
because a running process needs it as an environment variable at start and
cannot read a file in git: one copy that exists for a structural reason.

**A cluster has exactly one stage, fixed at bootstrap.** The stage selects
which `profiles/<stage>.yaml` the cluster reads and is half of the claim's
name (`<cluster>-<stage>`). To move a cluster to a different stage, bootstrap
a fresh cluster.

Directory layout:

```text
<deployments repository>/
  profiles/
    _base.yaml                    # Layer 2a
    dev.yaml                      # Layer 2b
    prod.yaml
  clusters/
    <cluster>/
      kernel/
        values.yaml                # Layer 3
        claims/
          cluster.yaml             # Layer 4
          deployments-repository.yaml   # this repository, and the director's push credential
          gentian-os-repository.yaml    # the repositories Argo CD reads
          gentian-ui-repository.yaml
        signing/                   # public halves of the keys commits are signed with
      catalogue/                   # profiles installed on this cluster, as committed by the director
      tenants/<tenant>/            # tenant.yaml + kustomization.yaml; platform/ from the first install
```

What is **not** in `kernel/`: any Argo CD `Application`. The bootstrap
Applications are the same on every cluster apart from a few values, so they
are templates in `gentian-os` (`kernel/bootstrap/chart`, `kernel/appsets`),
rendered and applied by the installer. See §3.1.

There is no `<stage>` suffix or subdirectory inside a cluster's tree:
`clusters/<cluster>/` already means one stage.

---

## 2. Three repositories

| Repository | Role | Typical ref |
| --- | --- | --- |
| **gentian-os** | The platform's chart, Crossplane definitions, kernel manifests, installer | a branch (dev) · a release tag `v*` (prod) |
| **gentian-ui** | The charts of the desktop, the consoles and the sign-in page | a branch or tag |
| **deployments repository** | Stage profiles, per-cluster claims and values, the materialized catalog, tenant manifests | `main` |

A catalog such as `gentian-apps` is not among them: a cluster reads no
catalog's git repository. A profile arrives one at a time through the
director, from the https address a catalog is published at
([custom-catalogs.md](custom-catalogs.md)).

Environment separation lives in **directory paths and layered values files**
inside the deployments repository, not in separate branches. Secrets never go
in git — the installer prompts for them and OpenBao holds them (see
[security/design.md](../security/design.md)).

**Who writes the deployments repository.** The installer, at step 0 of a run,
signed with the break-glass key; after that the director, for every tenant,
install and setting, signed with its own key and committed in the name of
the person who asked. Argo CD syncs only commits signed by one of the two.

---

## 3. Bootstrapping a new cluster

### Control plane sizing

Provision a control plane with **at least 8 GB of memory**. On managed
Kubernetes, check the tier's control-plane specification before creating the
cluster — a 4 GB control plane does not run a Gentian kernel.

API server memory tracks the number of objects in etcd and the number of
cluster-wide watches, and the kernel is heavy on both. ArgoCD watches every
resource type to compute drift; Crossplane, cert-manager, Envoy Gateway,
ExternalSecrets, CNPG, Gateway API and `gentianos.io` each register CRDs that
carry their own watch caches. Tenant provisioning adds Secrets, Jobs and Events
on top.

An undersized control plane is OOM-killed, and the failure is easy to
misread: the API endpoint stops accepting connections entirely — refusing TCP
while still answering ICMP — while every workload keeps serving traffic, because
nodes need no API server to keep running what is already scheduled. Reaching a
cluster's own edge but not its API is the signature.

Measure what a running cluster actually holds:

```bash
kubectl get --raw /metrics | grep apiserver_storage_objects | sort -t= -k2 -rn | head -30
kubectl get events -A --no-headers | wc -l
```

Keep the count down by narrowing ArgoCD's tracked resource types, and by
confirming that TTLs on finished Jobs are firing — Events and completed Jobs
accumulate faster than anything else the kernel creates.

### What `install.sh` does

Two different jobs. The steps, flags and configuration are in
[reference/install.md](../reference/install.md); this is the part that concerns
the deployments repository.

**A. Writing the cluster's definition** — `scaffold_cluster_deployment()`,
step 0 of the forward run. Given `GENTIAN_DEPLOYMENTS_CLUSTER_ID` and
`GENTIAN_DEPLOYMENTS_STAGE` in `install.env`, and every cluster setting from
the prompt — each asked with its default:

1. For each of the files of `clusters/<cluster>/kernel/` listed in §1, and
   for `tenants/platform/` (and `tenants/user/` on a single-tenancy cluster):
   write it **only if it doesn't already exist**. Per file — re-running never
   overwrites one a person has since edited.
2. Publish the two signing keys' public halves and ids under `signing/`,
   generating the keys in `~/.gentian/gnupg` if this host has none.
3. Place the default profile under `catalogue/`, held to its digest.
4. Commit what changed, signed with the break-glass key, and push it. A claim
   whose kind no XRD in this checkout defines is refused rather than
   committed.

The commit is the installer's because Argo CD syncs from the repository, not
from the checkout: a file left uncommitted is applied by nothing, and
`deployments-repository.yaml` is what gives the director its push credential.
A later edit to the claim is committed the same way, by the next
`./install.sh` run.

No cluster is contacted before this runs, and `--validate` and `--dry-run`
never run it.

**B. Bootstrapping the cluster.** Install Crossplane and Argo CD, then render
and `kubectl apply` the bootstrap Applications from `kernel/bootstrap/chart`
(step `B-01`, and `D-01` for the platform's own chart): the `gentian`
AppProject, the kernel Applications, the `gentian-os` Application — the
platform's chart with the values layered per §1 — and the root of the
ApplicationSets in `kernel/appsets/`, among them `gentian-claims`,
`gentian-catalogue` and `gentian-tenants`, which read the three directories
of §1.

They are rendered by `helm template` with the cluster, stage, repositories,
refs and image tag as values, and neither the rendered output nor a
per-cluster variant is committed anywhere. Helm, rather than `envsubst`,
because these manifests carry Argo CD's multi-source `$values` references,
which Helm passes through untouched.

Installing Crossplane and Argo CD and applying the first Applications is the
one imperative step every GitOps system has: something has to install the
agent that pulls from git. From there Argo CD reconciles the rest.
`install.sh` is run again to change a cluster setting (step 0 commits the
edited claim), to take a newer build (§4), or to repair.

### 3.1 What belongs in a cluster's `kernel/` — and what doesn't

| Kind | Example | Where it goes | Scaffolded? |
| --- | --- | --- | --- |
| **Cluster data** — unique per cluster | the claims, the `values.yaml` overlay, the signing keys' public halves | `clusters/<cluster>/kernel/` | Yes, by step 0 |
| **Bootstrap Applications** — the same on every cluster | the `gentian-os` Application, the ApplicationSets | Nowhere in the deployments repository: templates in `gentian-os`, rendered and applied by `install.sh` | No |
| **Tenants and their apps** | a tenant's manifest; Nextcloud for it | `clusters/<cluster>/tenants/<tenant>/` and `clusters/<cluster>/catalogue/`, written by the director | No — created through the director |
| **Add-ons** — something a deployer runs beside the platform | an organization's own service | Not in the platform's tree at all (below) | No |

The test for the first two rows: **does this cluster need its own copy of
the data, or just its own values in an otherwise identical template?**
`kernelDomain` is data and goes in `claims/cluster.yaml`. The Application
that references it is the same everywhere, so it stays a template: change it
once in `gentian-os`, and every cluster picks it up on its next
`./install.sh`.

**An add-on is self-contained, and gentian-os does not know it exists.** There
is deliberately no register-your-add-on hook, and nothing in gentian-os syncs
an add-on's manifests. Everything an add-on needs is a plain Argo CD object it
creates for itself:

| It needs | It ships |
| --- | --- |
| Permission to sync from its own repo | Its own `AppProject`, naming its own `sourceRepos` and destination namespace. It must not borrow the `gentian` project — that one covers the platform's repositories only. |
| Its private repo readable by Argo CD | Its own `repository` Secret in `kernel-gitops`, created by its installer. |
| Image tags followed | Its own `ImageUpdater` resource, if it wants one. |

Consequently a cluster can install, upgrade and run gentian-os with no add-on
present, and an add-on can be removed by deleting its `Application`,
`AppProject` and namespace — with nothing left behind in the platform.

Something a *tenant* uses is not an add-on: it is an app, installed from a
catalog through the director (`kubectl gentian apps install <app> --tenant
<name>`).

---

## 4. Image update policies per stage

The Cluster claim declares what a cluster runs, under `spec.release`
([versioning.md §2](../concepts/versioning.md)): a release tag the cluster
runs exactly, or a branch it follows build by build. The operator applies
it: every Application on the gentian-os repository, its own image, the
gentian-ui charts and the catalog move together, and `status.release` says
what was applied and what the release index offers. Nothing else in the
cluster moves an image: `argocd-image-updater` is installed with Argo CD, and
the platform ships no `ImageUpdater` resource for itself. The detail is in
[reference/install.md](../reference/install.md) §4, "Image tags".

| Stage | Follows | By |
| --- | --- | --- |
| **dev** | a branch | `release.track: <branch>`; the cluster takes each build once CI has published it |
| **prod** | a release | `release.version: vX.Y.Z`, moved by `kubectl gentian release set` or, inside the window, by `autoUpgrade` (§5.4) |

The installer writes the first value from `GENTIAN_OS_BRANCH` when it
scaffolds the claim, and from then on renders what the claim says: a run
whose `install.env` names another ref stops and says so.

### Which tag a cluster runs

**The stage name is not a tag.** CI (`.github/workflows/ci.yaml`) publishes:

| Tag | Published when | Mutable? | Use for |
| --- | --- | --- | --- |
| `1.2.3` | a `v1.2.3` git tag is pushed | no, by convention | **prod** |
| `1.2` | same | yes — moves with each patch | nothing to pin |
| `develop`, `main` | every build of that branch | yes | nothing to pin |
| `abc1234` | every commit, any branch | no | ambiguous across branches |
| `develop-abc1234` | every commit on that branch | no | what the installer pins a branch to |

There is no `prod`, `staging` or `latest`, and none should be added. The
installer's preflight checks that the tag it is about to render exists, and
that `image.tag` in `clusters/<cluster>/kernel/values.yaml` does, before
anything is deployed.

The tag the installer renders is set on the Application itself, over the
values files, so it is what the cluster runs. To hold a cluster to an exact
build regardless of the installer, set `image.digest` in the cluster's
`values.yaml`: the chart prefers a digest over any tag.

### Why not a `prod` or `latest` tag

A stage-named or `latest` tag is a *mutable pointer*: the same string resolves
to different content over time. In production that costs more than it saves.

- **You cannot tell what is running.** `image.tag: prod` in Git names a
  pointer, not a build. Answering "what version is in production" needs a
  registry lookup that is only true until the next push.
- **Rollback stops being a revert.** Reverting the commit gives you the same
  mutable tag, which still resolves to the bad image. You have to re-point the
  tag — a registry operation nobody reviews and Git never records.
- **Replicas drift.** A pod rescheduled after a re-push pulls the new content
  while its siblings keep the old, so one Deployment runs two versions with no
  indication that it does.
- **Nobody decided.** A mutable tag moves production whenever someone pushes.

The practice here is the opposite: **immutable, content-addressable
references.** In order of strength:

- **A digest**, in `clusters/<cluster>/kernel/values.yaml`. It cannot be
  re-pointed at different content by anyone, and the chart prefers
  `image.digest` over any tag:

  ```yaml
  image:
    digest: "sha256:…"
  ```

- **An immutable tag**, which is what the installer renders: `1.2.3` for
  `GENTIAN_OS_BRANCH=v1.2.3`, `<branch>-<short-sha>` for a branch. Set
  `GENTIAN_OS_IMAGE_TAG` only to run something else.

`GENTIAN_OS_BRANCH` (`install.env`) is the ref the first install renders,
and what the scaffold writes into the claim's `spec.release`: a tag becomes
`version`, a branch `track`. It decides which gentian-os a cluster
**runs**, which need not be the one you are installing **from**. For a
release tag, every in-cluster Application on the gentian-os repository — the
kernel ApplicationSets, the operator Application and the bootstrap
Applications — follows the tag. For a branch the installer renders the
commit of the checkout it runs from, the commit the operator image is built
from; the operator then follows the branch tip, build by build, as CI
publishes each.

Left unset it is the branch of the checkout `install.sh` runs from, in every
place the installer uses it — an observation, not a guess. Where there is no branch to read, which is exactly
what `git checkout v0.4.0` leaves behind, the installer stops and asks rather
than answering `develop` for a cluster somebody meant to pin. **Pinning a
cluster to a release means naming the tag**, not checking it out. Once the
claim exists it is the claim that names it (§5.4).

---

## 5. Simplified flow (dev + prod, no staging)

Best for small teams and first releases: one fast dev cluster and one
production cluster. Validation happens on dev; production receives only
tagged releases.

### 5.1 Cluster mapping

| Cluster | Stage | Purpose |
| --- | --- | --- |
| Homelab / lab (`test`) | `dev` | Daily integration, experimental tenants |
| Cloud / customer-facing (`prod-1`) | `prod` | Live workloads |

Example `install.env` per machine:

```bash
# Homelab
GENTIAN_DEPLOYMENTS_CLUSTER_ID=test
GENTIAN_DEPLOYMENTS_STAGE=dev
GENTIAN_DEPLOYMENTS_BRANCH=main
GENTIAN_OS_BRANCH=develop

# Cloud production
GENTIAN_DEPLOYMENTS_CLUSTER_ID=prod-1
GENTIAN_DEPLOYMENTS_STAGE=prod
GENTIAN_DEPLOYMENTS_BRANCH=main
GENTIAN_OS_BRANCH=v1.2.3
```

The domain and the other cluster settings are not in `install.env`: step 0
asks for them and writes them to `claims/cluster.yaml` (§3), which is
authoritative from then on. A cluster property set in `install.env` overrides
the claim, so leave it out after the first run.

### 5.2 Promotion diagram

```mermaid
flowchart TD
    FeatureBranches["feature branches"]
    Develop["develop (next release, -dev)"]
    ImageDev["ghcr.io/.../gentian-os:develop-<sha>"]
    DevCluster["dev cluster"]
    ReleaseBranch["release/vX.Y.Z (X.Y.Z)"]
    Main["main + tag vX.Y.Z"]
    ImageProd["ghcr.io/.../gentian-os:X.Y.Z + GitHub Release"]
    ProdCluster["prod cluster"]

    FeatureBranches -->|"pull request"| Develop
    Develop -->|"CI"| ImageDev
    Develop -->|"./install.sh"| DevCluster
    Develop -->|"version commit"| ReleaseBranch
    ReleaseBranch -->|"pull request, tag"| Main
    ReleaseBranch -->|"next-dev commit, pull request"| Develop
    Main -->|"CI on the tag"| ImageProd
    ImageProd -->|"GENTIAN_OS_BRANCH=vX.Y.Z, ./install.sh"| ProdCluster
```

### 5.3 Tenant workflow

1. Sign in: `kubectl gentian login`.
2. Create the tenant: `kubectl gentian tenants create <name>`. The director
   checks that you may, and commits
   `clusters/<cluster>/tenants/<name>/tenant.yaml`.
3. Argo CD's `gentian-tenants` ApplicationSet applies the Tenant and the
   operator provisions it.
4. Hand the tenant to its administrator:
   `kubectl gentian tenants activate-admin <name>` issues a single-use
   activation link.

Tenants are not promoted between clusters by copying manifests: each
cluster's tenants are created through that cluster's director. A tenant's
data moves between clusters as an export bundle
(`kubectl gentian tenants import`,
[data-lifecycle.md](../concepts/data-lifecycle.md)).

### 5.4 Release runbook

A platform release is one tag `vX.Y.Z` on gentian-os, gentian-ui and
gentian-apps: the platform, the user interfaces and the catalog that were
tested together. Cutting it is a repository operation, done per repository
in the order below; rolling it onto a cluster is a separate one, done per
cluster and repeatable. The release issue carries this list as its
checklist, and every step is a command or a pull request, so that a bot can
run it.

**What the flow rests on**

- `develop` is where work lands. `main` is what is released and receives
  nothing but release merges (a patch release's included, below): Dependabot,
  governance and tooling changes target `develop`. Every release merge is
  followed by a merge of `main` back into `develop`, by pull request, since
  the merge commit exists on `main` alone; so `main` is always an ancestor
  of `develop`, and a release merge never conflicts.
- A tree that is not a release never carries a release's bare number (§5.5).
  The bare number is written on a release branch and reaches `main` from
  there; `develop` carries the next release's number with `-dev` and never names a tag that does
  not exist.
- CI builds the release from the tag. The tag run refuses a tag whose name
  is not the chart's version, publishes the images, and creates the GitHub
  Release from `docs/releases/vX.Y.md` with the pull requests since the
  previous tag listed under it.
- A cluster takes the release by naming the tag in `install.env`; nothing
  on a cluster is pinned by hand.

**Preconditions**

1. CI is green on `develop` in all three repositories. The tag rebuilds the
   same tree, so a red `develop` is a red release.
2. A fresh install of `develop`'s tip on a throwaway cluster passed, with
   tenants imported from bundles of the previous release.
3. `main` is an ancestor of `develop` in each repository:
   `git merge-base --is-ancestor origin/main origin/develop`. Where it is
   not, merge `main` into `develop` first, as a pull request. A merge that
   keeps `develop`'s tree (`-s ours`) is right only when every commit on
   `main` is superseded; the merge message says so, commit by commit.
4. Dependabot pull requests are merged or closed, and the milestone holds
   nothing but the release issue.
5. The release notes, `docs/releases/vX.Y.md`, are on `develop`: what the
   release contains and what an operator must do differently after it,
   with the commit count against the previous release in the header. A
   patch release adds a section to its line's file.

**1. gentian-apps**

The catalog is released first, because a cluster installed from a
gentian-os tag reads the released catalog
(`https://gentian-org.github.io/gentian-apps/`), and the released catalog
is what `main` publishes. Its own rules are in that repository's
`docs/releasing.md`: a release pull request from `develop` into `main`
takes every profile from its `-dev` version to the bare number, every chart
pin from `<version>-develop.<sha7>` to the plain version and every image
from `develop-<sha7>` to its `IMAGE_TAG`; CI refuses a development name on
a pull request into `main`. Merging it publishes the root catalog. Tag
`vX.Y.Z` on that `main` commit; nothing is built from the tag. Then merge
`main` into `develop` by pull request: the bare numbers come to `develop`,
and the next change to a bundle bumps its profile to `X.Y.(Z+1)-dev.1`.

**2. gentian-ui**

Merge `develop` into `main` by pull request and tag `vX.Y.Z` on `main`.
The tag runs the three workflows, which publish the desktop, the
administration console and the concierge images and their charts under the
tag: `<chart base>-vX.Y.Z`, the chart's own version in its `Chart.yaml`
followed by the tag. A cluster that pins the tag reads nothing from
`develop`. Wait for the three runs before rolling a cluster, and merge
`main` into `develop` by pull request.

**3. gentian-os**

1. Branch `release/vX.Y.Z` off `develop`. One `chore(release): vX.Y.Z`
   commit sets `version` and `appVersion` in `charts/gentian-os/Chart.yaml`
   to `X.Y.Z` and runs `make gen-platform-version`, and commits everything
   it rewrites: `internal/platformversion/line.go`, the Cluster composition
   and the render fixtures under `crossplane/tests/unit/render/`
   (`make verify-gen` fails while they disagree with the chart). This is
   load-bearing: the chart's Deployments render
   `image.tag | default .Chart.AppVersion`, so `appVersion` is the image a
   cluster pulls where no tag is set, and it must be what CI publishes
   (`docker/metadata-action` strips the `v`, so tag `vX.Y.Z` becomes image
   `:X.Y.Z`). The same target points the composition's infra charts
   (`charts/infra/packages`) at the tag `vX.Y.Z`, so the release installs
   its own charts and not `develop`'s.
2. Open the pull request `release/vX.Y.Z` → `main`. Its body is the
   release notes' summary. Merge it once CI is green.
3. Tag the merge commit on `main`, annotated, with a message that
   summarizes the notes, and push the tag:

   ```bash
   git fetch origin main
   git tag -a vX.Y.Z -m "Gentian OS vX.Y.Z: ..." origin/main
   git push origin vX.Y.Z
   ```

   The tag run checks that the tag names the chart's version, publishes
   `ghcr.io/gentian-org/gentian-os:X.Y.Z` and `:X.Y` with the event
   listener and storage-tools images, and creates the GitHub Release from
   `docs/releases/vX.Y.md`. Nothing downstream can adopt the release until
   that run is green.
4. On the same branch merge `main` (`git merge origin/main`: the release
   merge commit, whose tree the branch already has) and add the next-dev
   commit, `chore: <next>-dev`, which sets the chart to the number of the
   release that comes next with `-dev` (`X.Y.(Z+1)-dev` for a patch, `X.(Y+1).0-dev`
   for a minor)
   and runs `make gen-platform-version` again (which points the infra
   charts back at `develop`). Merge the branch into `develop` by pull
   request; `main` is an ancestor of `develop` again. The branch may be
   deleted once both pull requests are merged.
5. Close the release issue and the milestone.

**Roll it onto a cluster**

A cluster takes a release through its Cluster claim, not through the
installer. One command, as the cluster's administrator:

```bash
kubectl gentian release set vX.Y.Z                      # this release, moved by a person from here on
kubectl gentian release set vX.Y.Z --auto-upgrade patch # ... and every later patch of the line by itself
```

The director writes `spec.release` and points the `gentian` catalog source
at the released catalog, in one signed commit. The operator reads the claim,
looks the release up in the index, moves every Application on the gentian-os
repository to the tag, its own image to `X.Y.Z` and the gentian-ui charts to
the ones the index names for the release, and writes `status.release`.
`kubectl gentian release show` follows it; the move is done when the state
is `Applied` and the operator's build is the release:

```bash
kubectl gentian release show
kubectl -n kernel-control get deploy gentian-os \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Under `autoUpgrade: patch` the director commits the next patch of the line
by itself inside the maintenance window (`spec.catalogue.updates.window`);
under `minor` the next minor too, where the index says it is applied in
place. A release the mode does not take is `pending` on the status and in
the console, for a person, who is told once by a console notice and by
mail. A release whose notes say the cluster layout
changed lists no line under `upgradesFrom` and is never taken by itself: it
is a fresh install and a tenant import, as the notes' upgrade section says.

A cluster installed before the claim carried `spec.release` gets the field
on its next `./install.sh` run, naming what that run renders; after that
the claim is where the release is declared, and `GENTIAN_OS_BRANCH` in
`install.env` may go.

**Additionally, when the target is a new production cluster**

- Run the install with `GENTIAN_DEPLOYMENTS_STAGE=prod`; make sure
  `profiles/prod.yaml` exists in the deployments repository first.
- Create the tenants afterwards (§5.3).

**A patch release**

A fix for a released line is released from `main`, not from `develop`:
branch `release/vX.Y.(Z+1)` off the tag, cherry-pick the fixes from
`develop` (where they land first), add the version commit and follow steps
2 and 3 above. Then merge `main` into `develop` by pull request, so `main`
stays an ancestor of `develop`; `develop`'s own number is unchanged. The
notes are a new section in `docs/releases/vX.Y.md`.

### 5.5 Release numbering

A tree that is not a release never carries a release's bare number. The
release branch's version commit sets `version` in
`charts/gentian-os/Chart.yaml` to `X.Y.Z` and is what `main` is tagged at;
the next-dev commit sets the number of the release that comes next, with
`-dev`: `X.Y.(Z+1)-dev` while the line is still being built out (a patch is
the next release), `X.(Y+1).0-dev` once the next release is a minor; and
`develop` stays there until that release. The tree always says which release it is heading for, and a
build from it cannot be mistaken for one. The pre-release suffix is SemVer's:
`0.5.0-dev` sorts below `0.5.0` and above every `0.4.x`.

The platform version line is the major and minor of that number. The
constant the operator and the director compare profiles' floors and backup
bundles against is generated from the chart
(`make gen-platform-version`, `scripts/gen/gen-platform-version.py` →
`internal/platformversion/line.go`), so the chart and the binary cannot
disagree ([versioning.md §2](../concepts/versioning.md)). The same target
writes the git ref the Cluster composition reads the infra charts at:
`develop` on any number with a suffix, the tag `vX.Y.Z` on a bare one.

A profile build follows the same rule: `spec.version` on the apps
repository's `develop` carries the `-dev` suffix with a counter per change
(`0.5.1-dev.4`), and is set to the bare number when the catalog is released
from `main`. The apps repository's CI holds every profile to it.

---

## 6. Fortified flow (dev + staging + prod)

Use when you need a production-like dress rehearsal on cloud infrastructure
before customer-facing rollout. Staging should run on **prod-class**
infrastructure (same storage class, DNS, TLS, and network model as
production), not on a homelab.

### 6.1 Cluster mapping

| Cluster | Stage | Purpose |
| --- | --- | --- |
| Homelab (`test`) | `dev` | Fast feedback, LE staging certs, tunnel or lab network |
| Cloud (`staging-1`) | `staging` | Pre-production validation on real infra |
| Cloud (`prod-1`) | `prod` | Live workloads |

Because one cluster runs one kernel stage for life (§1), staging and prod
on the **same** cloud cluster require either:

- **Sequential cutover** — bootstrap a new cluster identity with
  `GENTIAN_DEPLOYMENTS_STAGE=staging`, validate, then bootstrap a fresh one
  with `prod` and cut traffic over; or
- **Two cloud clusters** — one for `staging`, one for `prod` (preferred at
  scale — no cutover needed, and matches §1's "don't mutate stage in place"
  rule exactly).

### 6.2 Promotion diagram

```mermaid
flowchart TD
    FeatureBranches["feature branches"]
    Develop["develop"]
    ImageDev[":develop"]
    DevCluster["homelab / dev (a branch)"]
    StagingCluster["cloud / staging (release candidates)"]
    ProdCluster["cloud / prod (releases)"]
    
    FeatureBranches --> Develop
    Develop -->|"CI"| ImageDev
    Develop --> DevCluster
    DevCluster -->|"tag vX.Y.Z-rc.N"| StagingCluster
    StagingCluster -->|"smoke + integration tests<br>tag vX.Y.Z"| ProdCluster
```

### 6.3 Config promotion

**Code (gentian-os):**

1. `develop` → dev (`./install.sh --only B-01`).
2. Tag `vX.Y.Z-rc.N` on the release branch → roll it onto staging.
3. After validation, merge and tag `vX.Y.Z` → roll it onto prod.

An RC is cut by the §5.4 runbook from the release branch before it is
merged, with its own version commit (`X.Y.Z-rc.N`), so the staging cluster
pulls the RC image rather than the last stable one. A number with a suffix
reads the infra charts from `develop` (§5.5), so an RC proves the platform
image and the catalog, not the pinned charts.

**Stage policy (`gentian-deployments/profiles/`):** edit `staging.yaml` or
`prod.yaml` directly — since it's shared by every cluster of that tier,
a one-line change (e.g. flipping `metrics.serviceMonitor.enabled`) applies
everywhere that tier runs without touching any cluster's overlay. Argo CD
syncs only signed commits (§2), so such an edit is signed with the
break-glass key.

**Tenants:** created on each cluster through its director (§5.3); nothing is
copied between clusters' trees.

**Apps:** a tenant's install is pinned to the digest of the profile's bundle,
so a new build of an app reaches a tenant when it is installed again at that
build, per cluster ([custom-catalogs.md](custom-catalogs.md)).

### 6.4 Staging configuration

A staging tier needs a `profiles/staging.yaml` in the deployments repository
before the install, with the tier policy you want (ACME issuer, log level);
the installer warns when the stage's profile is missing and does not write
it. Step 0 then scaffolds the staging cluster's own
`clusters/<cluster>/kernel/` like any other.

---

## 7. What belongs in Git vs locally

| Location | Committed? | Contents |
| --- | --- | --- |
| `<deployments>/profiles/` | Yes | Stage-tier values, shared across clusters of that tier |
| `<deployments>/clusters/<cluster>/kernel/claims/` | Yes | Crossplane claims — what the cluster is |
| `<deployments>/clusters/<cluster>/kernel/` (rest) | Yes | The `values.yaml` overlay and the signing keys' public halves — **not** bootstrap Applications, see §3.1 |
| `<deployments>/clusters/<cluster>/catalogue/` | Yes, by the director | The profiles installed on this cluster |
| `<deployments>/clusters/<cluster>/tenants/` | Yes, by the director | Tenant manifests |
| `install.env` | No (per machine) | `GENTIAN_DEPLOYMENTS_*`, `GENTIAN_OS_BRANCH`, repository addresses |
| Credentials | **Never** | Master password, registry, deployments token, DNS token: prompted for by the installer (or read from the environment unattended) and stored in OpenBao. There is no secrets file |

All deployment configuration for every cluster and stage can live on the
`main` branch of one deployments repository.

---

## 8. Day-2 operations

| Task | Command / action |
| --- | --- |
| List tenants | `kubectl gentian tenants list` |
| Create a tenant | `kubectl gentian tenants create <name>` |
| Install an app on a tenant | `kubectl gentian apps install <app> --tenant <name>` |
| Change a cluster setting | edit `claims/cluster.yaml`, then `./install.sh` (step 0 commits it signed) |
| Take a newer build of the platform | `./install.sh --only B-01` |
| Monitor GitOps sync | `kubectl get applications -n kernel-gitops` |

Kernel upgrades are **cluster-wide**: when the platform's image changes, all
tenants on that cluster run on the new version. See
[operations.md](../concepts/operations.md) §7.

Day-2 changes go through the director, which checks the person and records
who asked; the full command list is in [commands.md](../reference/commands.md).

---

## 9. Related documents

| Topic | Document |
| --- | --- |
| First-time install | [GETTING-STARTED.md](../../GETTING-STARTED.md) |
| Installer steps, flags, configuration surfaces | [reference/install.md](../reference/install.md) |
| System architecture | [architecture.md](../overview/architecture.md) |
| Upgrades | [operations.md](../concepts/operations.md) §7 |
| Secrets and TLS | [security/design.md](../security/design.md) |
| Multi-tenancy and DNS | [multi-tenancy.md](../concepts/multi-tenancy.md) |
| kubectl reference | [commands.md](../reference/commands.md) |
