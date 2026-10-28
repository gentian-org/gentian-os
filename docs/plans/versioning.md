# Versioning and updates

How Gentian OS and the apps on it move from one version to the next. The
platform knows which release it runs, every app says which platform it needs,
the cluster administrator decides how each app moves, a move is one guarded
operation that can go back, and one tenant's move never touches another
tenant. Decided 2026-10-19. §1 to §5 is the design, §6 the order of work,
§7 what stays on the roadmap, §8 what is open.

## 1. Three versions

An installed app carries three versions, and each answers one question.

| Version | Names | Answers |
|---|---|---|
| the upstream version | the application itself: Nextcloud 33, Odoo 18 | whether data written by one build can be read by another (`minRestoreVersion`) |
| the profile build | the bundle: its digest, and `spec.version`, bumped on every change to the bundle | what a tenant runs, what the store and every listing show |
| the platform version line | the OS: `0.5`, `0.6`, … | whether a build can run on this cluster |

Compatibility with the OS is a property of the profile build, because the
profile is what the operator reads. The upstream version has nothing to say
about it.

### 1.1 Numbering between releases: the next-dev model

A tree that is not a release never carries a release's bare number. The
commit that cuts a release sets the version to `X.Y.0` and is tagged; the
very next commit on `develop` sets it to `X.(Y+1).0-dev`, and it stays there
until the next release commit. A patch to a released line is made on a
release branch, `release-X.Y`, and tagged `X.Y.Z` there; `develop` never
carries a patch number. This is how Kubernetes, Go and most operator projects
number: the tree always says which release it is heading for, and a build
from it cannot be mistaken for one.

The pre-release suffix is SemVer's. `0.5.0-dev` sorts below `0.5.0` and
above every `0.4.x`. CI appends the distance from the last tag and the
commit, so a chart from `develop` is `0.5.0-dev.37` and an image
`0.5.0-dev.37-g7f373a1`; the `develop-<sha7>` image tag stays as an alias.
The platform version line is the major and minor of that number, and the
version-line constant in the operator is derived from the chart version
rather than kept in step by hand.

The same rule holds for a profile build: `spec.version` on `develop` carries
the `-dev` suffix and is set to the bare number when the catalogue is
released from `main`. The profiles were set to `0.5.1-dev` on 2026-10-19, the
first build of every converted profile under this rule.

## 2. The platform knows its version; a build declares its floor

The `Cluster` claim's status carries the platform version line and the build
the operator runs, written by the operator from its own binary. The director
reads it from there and answers it on the cluster's settings, the
administration console shows it, and every check below compares against it.

A profile declares the minimum platform it needs:

```yaml
spec:
  requires:
    platform: ">=0.5"
```

The director refuses an install whose floor the cluster does not meet and
filters the listing by it, naming the reason: "needs platform 0.5". A profile
without the field is read as `>=0.0`.

No profile declares an upper bound. An author cannot know what a later
platform drops, so the platform carries that half of the contract as a
deprecation rule: a field or behaviour is read for one more version line after
its replacement ships; while it is read, the operator sets a condition
`Deprecated` on every `Component` whose bundle uses it; the release notes of
each line list what the next one removes.

CI holds both sides to it. gentian-apps validates every bundle against the
CRD schema of each version line its floor admits, the schemas vendored per
line. gentian-os fetches the released and the develop catalogue and runs every
bundle through the operator's own bundle check, so a platform change that
breaks a published profile fails the platform's pull request, not a tenant's
install.

## 3. One build object per build

The `ComponentProfile` stays one object per name, the newest build any tenant
installed, because the app's identity -- its group, its routes, its
authorization objects -- is spelled from that name. Beside it the cluster
holds one `ProfileBuild` per build any tenant installed, named by the profile
and a prefix of its digest: the bundle at that digest, verified by the
operator, its profile's spec written on its status for the Composition to
read. A tenant's `Component` pins a build; one pinned to a build that is not
the profile's current one is rendered from its `ProfileBuild`. Two tenants on
two builds of one app share nothing but their companions, which are one
object per name like the profile; a build whose companions differ from the
current build's is held until the tenant moves.

A build stays on the cluster while any `Component` pins it. The director and
the installer write it beside the profile into the catalogue directory, and
a build the cluster holds is installed again without fetching it, which is
what gives rollback (§5).

## 4. Who moves an app

The cluster administrator sets a mode per app, and may override it per
tenant. A tenant administrator cannot move an app unless its mode says so.

| Mode | Who moves the app | To what |
|---|---|---|
| `latest` (**the default**) | the platform | the newest build on the cluster's track that the platform's version line admits, inside the maintenance window |
| `manual` | the cluster administrator | a build they name, for the whole cluster or for one tenant |
| `tenant` | the tenant administrator | a build the store offers; the store's Update action is shown in this mode only |

The default per app is set on the `Cluster` claim's catalogue settings. The
per-tenant override is `Tenant.spec.apps[].update`, written through the
director by the cluster administrator's role only. An add-on has no mode of
its own: it follows its base and moves with it.

An app with a `Customization` record at L3 or above that carries
`upgradeRisk` is in `manual` mode for that tenant unless the cluster
administrator says otherwise. The ladder exists to say what the next upstream
release breaks; `latest` does not walk past it.

### 4.1 The maintenance window and the staged rollout

`latest` moves apps inside a window set on the `Cluster` claim, one for the
whole cluster. Within the window the platform moves one tenant first, waits
for the move to verify (§5), then the rest in batches, and stops with a
condition on the cluster at the first failure. A build the catalogue flags as
a security fix may be applied outside the window.

### 4.2 The outdated-deployments report

There is no floor in `tenant` mode: a tenant may stay where it is. Instead
the operator computes, for every `Component` on the cluster, whether a newer
build the platform admits exists on its track, and what keeps it where it is:
its mode, a `Customization` with `upgradeRisk`, a failed move. A pinned
add-on is listed under its app, at its own build. The usher
serves the report for the cluster and per tenant. The administration console
shows it under the cluster's apps with one action, "move to the newest build",
for all listed or for a selection, whoever introduced the lag.
`kubectl gentian apps outdated` prints the same report.

The OS appears in the same place: the operator reads the release feed, the
tags CI publishes, and reports "a newer release exists" with the address of
its notes on the cluster's status and in the console. Nothing moves the OS
from inside the cluster yet (§7.2).

## 5. A move is one guarded operation

Every move, whatever its mode and whoever asked, is the one director
operation `POST /v1/tenants/{t}/apps/{p}` with a digest:

1. **Refuse** when the cluster does not meet the build's floor, or the bundle
   does not hash to the digest.
2. **Export** the app's data to the cluster's backup destination, as a manual
   export of that app; refuse to go on when the export fails.
3. **Materialise** the new build beside the old one and repoint the
   `Component`.
4. **Verify**: wait for the release, run the profile's `restore.verify`
   command. On failure repoint the `Component` to the build it came from,
   which is still on the cluster, and record the reason on the `Component`.
5. **Record** who or what moved it: the policy, or a named person.

`rollbackLimit` leaves the default composition; the move's own rollback
replaces it and does not uninstall a release at revision 1.

A move that was rolled back, or did not start because its export failed,
stays where it ended until it is asked for again. Asking again is installing
the pinned build once more, or "move to the newest build" (`apps
move-outdated`), which takes in every such move: the director writes a fresh
`movedAt` and `movedBy` on the entry, with the pin unchanged, and the operator
starts the move from step 2. Who may ask is who may move the app (§4). The
platform's `latest` mover never asks again by itself, so a build that keeps
failing is not tried over and over; the report lists it as rolled back.

## 6. Order of work

Five pieces, ordered by what reads what. Each is one pull request series and
leaves the cluster working at its end.

| | Piece | Delivers | Depends on |
|---|---|---|---|
| 1 | **The version the cluster knows** (§2) | version line and build on the `Cluster` status; `spec.requires.platform`; the director's refusal and filtered listing; `spec.version` bump enforced in gentian-apps' CI; schema conformance in both CIs | the released catalogue moving again: the catalogue build on gentian-apps' `main`, every profile on `main` on a released chart, enforced by `validate-chart-refs.py` |
| 2 | **Discovery and the report** (§4.2) | the outdated-deployments report in the usher, the console's screen and action, `kubectl gentian apps outdated`; the OS release feed on the cluster status | 1 |
| 3 | **The guarded move** (§5) | export before, verify after, rollback to the previous build, the record of who moved it; the catalogue's snapshot per publish | 1, and the per-build objects of 4 for the rollback |
| 4 | **Per-build profiles, modes, staged rollout** (§3, §4) | one `ComponentProfile` per build and the pointer; the three modes and the per-tenant override; `manual` for customized apps; the window and the staged `latest`; the store's Update action gated by mode | 1; 3 for the rollout's verification |
| 5 | **The OS re-run converges** | every installer step compares the pinned version with the running one, Argo CD's, metrics-server's, each provider package's, each bootstrap Application's against its template; Image Updater pinned or removed; then Argo CD takes over the bootstrap Applications and the Crossplane definitions, so one sync moves every definition with the binary that reads it | 1 |

3 and 4 are built together: the per-build objects come first inside 4, then
the move of 3, then modes and the rollout.

## 7. Roadmap

Named so that §6 is not mistaken for the whole. None is scheduled.

### 7.1 Migration machinery

A second CRD version with conversion, or a stated storage-version policy;
versioned migrations for the operator's own stores in place of sweeps inside
reconcilers; an upgrade hook on the profile beside the backup hooks, for an
app whose start does not migrate its data by itself.

### 7.2 The OS moves itself, and a floor for tenants

The OS on a track, moved from inside the cluster in the window with the same
staged rollout as apps; an automatic lane for patch releases and security
fixes, approval for a new version line. Once the report of §4.2 has been read
for a while: a minimum build in `tenant` mode that a tenant cannot stay below.

### 7.3 The platform below the OS

A Kubernetes version policy beyond MicroK8s; node operating system updates;
the kernel's own Postgres with a pinned image and more than one instance, so
that neither the CNPG operator's version nor a restart decides what Keycloak
runs on.

### 7.4 Supply chain

Signed releases and a signed catalogue index; the mirror for an air-gapped
install; image scanning that gates a release.

### 7.5 Upgrade and install tests

A nightly matrix installs each app on each supported version line, N and
N-1, signs in through the gateway, runs `restore.verify`, exports and
restores. One upgrade case: apps installed on N, platform moved to N+1, apps
still healthy, then each moved to its new build. CI writes the result into
the catalogue index as `testedOn` per build and line; the single "one tag
across three repositories" snapshot becomes a matrix that stays true while
the catalogue moves between platform releases.

## 8. Open

What the build of §6 left open is on the roadmap,
[roadmap.md §2.34](../roadmap.md), so this plan is not read as a backlog.
