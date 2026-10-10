# Scheduled backups leave the OS: what is there, and the order of the move

[sovereignty-concept.md](sovereignty-concept.md) describes v0.5: the OS takes
an export when it is asked to, and taking exports on a schedule is an
add-on's work. The code has not followed. This document lists what in this
repository implements scheduled backups today, what stays, what an add-on
needs from the OS to do the scheduling itself, and an order of work with what
breaks at each step.

It is a plan for a decision. Nothing in it has been changed in the code.

## 1. What implements scheduled backups today

### 1.1 Resource kinds

| Kind | File | What it is |
|---|---|---|
| `TenantExportSchedule` (namespaced, in `tenant-<t>`) | `api/v1alpha1/tenantexportschedule_types.go` | a cron expression, the apps to include, the key, and a retention; its status carries the last and next run |
| `BackupPolicy` (cluster-scoped; `default` for the cluster, the tenant's name for a tenant) | `api/v1alpha1/backuppolicy_types.go` | four things in one object: a **destination** (endpoint, region, bucket), the **recipients** bundles are encrypted to, a **schedule**, and a **retention** |
| `TenantBackupPolicy` | CRD files only: `charts/gentian-os/crds/`, `config/crd/`, `internal/schemacheck/definitions/crds/` | no Go type and no controller; a leftover from before the policy became one kind |

The generated CRDs of the first two are in the same three directories, and
the operator's ClusterRole (`charts/gentian-os/templates/clusterrole.yaml`)
names all three kinds.

### 1.2 Controllers, in the operator (`cmd/main.go` registers both)

| Controller | File | Does |
|---|---|---|
| `TenantExportScheduleReconciler` | `internal/controller/tenantexportschedule_controller.go` | creates a `TenantExport` when the cron is due; deletes finished exports beyond the retention (`internal/backup/retention.go`); records the last success |
| `BackupPolicyReconciler` | `internal/controller/backuppolicy_controller.go` | resolves the cluster's policy with a tenant's; writes one managed `TenantExportSchedule` per tenant from it; for a destination outside the cluster, creates the ExternalSecrets that fetch its keys from the vault (`tenants/<t>/backup/destination`) and reports whether they are there |

Shared code: `internal/backup/policy.go` (`ResolveEffective`,
`ApplyExportDestination`, the names of the destination's credential and
Secret).

### 1.3 Routes

| Server | Routes | File |
|---|---|---|
| director (writes) | `PUT` and `DELETE /v1/tenants/{t}/backup-policy`, `PUT /v1/clusters/{c}/backup-policy` — each a commit of a `BackupPolicy` file to the deployments repository | `internal/director/api/api.go`, `resources.go`, `internal/director/gitops/backup.go` |
| usher (reads) | `GET /v1/tenants/{t}/backup-policy`, `GET /v1/tenants/{t}/backup-schedules`, `GET /v1/clusters/{c}/backup-policy`, `GET /v1/clusters/{c}/backup-schedules` | `internal/usher/state.go` |
| operator (answers the usher) | `GET /v1/tenants/{tenant}/backup-policy`, `/backup-schedules`, `GET /v1/backup-policy`, `/v1/backup-schedules` | `internal/applifecycle/http_backups.go`, `backups.go` |

### 1.4 Everything else

- **Vault policy.** The cluster composition lets External Secrets read
  `tenants/+/backup/*` (`crossplane/compositions/cluster-default.yaml`), and
  `scripts/lint/lint-eso-readable-paths.py` holds it to that. Five render
  fixtures under `crossplane/tests/unit/render/` carry the same lines.
- **Command line.** `kubectl gentian` has no schedule or policy command. A
  person sets either with `kubectl apply`, as
  [commands.md](../commands.md) §13 and §14 show.
- **Consoles.** The Admin Console (gentian-ui) calls only the routes that
  stay: list, read, download, start and delete a backup. The policy and
  schedule routes are called by a console outside the open platform.
- **Tests.** `internal/controller/tenantexportschedule_test.go`,
  `backuppolicy_encryption_test.go`, `backuppolicy_naming_test.go`,
  `backuppolicy_watch_test.go`; `internal/backup/policy_test.go`,
  `retention_test.go`; parts of `internal/director/api/resources_test.go`,
  `internal/director/gitops/changes_test.go` and `definitions_test.go`,
  `internal/usher/state_test.go`, `internal/applifecycle/http_test.go`.
- **Documents that describe it as the OS's.**
  [commands.md](../commands.md) §13 and §14,
  [tenant-backup-guide.md](../tenant-backup-guide.md),
  [design/operations.md](../design/operations.md) §1,
  [architecture.md](../architecture.md), [folder-structure.md](../folder-structure.md),
  [design/data-lifecycle.md](../design/data-lifecycle.md),
  `GETTING-STARTED.md`, [recovery-playbook.md](../recovery-playbook.md),
  [roadmap.md](../roadmap.md) (two items written against `BackupPolicy`),
  [releases/v0.4.md](../releases/v0.4.md) (history; it stays as written).

## 2. What stays in the OS

The primitives of the concept, and what they need:

| Stays | Where |
|---|---|
| `TenantExport` and its controller: capture, encryption, the bundle | `internal/controller/tenantexport_controller.go`, `internal/backup/` |
| `TenantRestore` and import | `internal/controller/`, `internal/director/api/import.go` |
| Start, list, read, download and delete a backup | director `POST …/actions/backup`, `…/actions/delete-backup`, `GET …/backups/{name}/download`; usher `GET …/backups`, `…/backups/{name}` |
| Retire and purge, with the tenant's backup bucket | `internal/backup/teardown.go`, `internal/controller/storage_reconciler.go` |
| The cluster's backup key and the tenant's backup bucket | the Cluster claim's `backup` section; `internal/backup/inventory.go` |
| The retention arithmetic, if the OS keeps "delete this backup" only | `internal/backup/retention.go` goes with the schedule; `delete-backup` stays |

## 3. The one thing that does not split cleanly

`BackupPolicy` is not only a schedule. **An export a person asks for reads
it too.** `TenantExportReconciler` fetches the cluster's policy and the
tenant's, merges them (`ResolveEffective`), and takes from the result where
the bundle goes and who can open it. `spec.destination.mode: policy` is the
default of a `TenantExport`.

So removing `BackupPolicy` changes one-off exports, which are the OS's:

| What the policy gives an export today | Without the policy |
|---|---|
| a destination outside the cluster, with its keys | only the cluster's own storage (`platform`), or what the one export states (`custom`) |
| recipients other than the cluster's key | the cluster's key, or what the one export states |
| whether a tenant may override the cluster | nothing to override |

This needs a decision before any code moves. Three ways to make it:

1. **The export keeps no standing arrangement.** `policy` is removed as a
   destination mode; an export goes to the cluster's storage or to what it
   states itself. Simplest, and it matches the concept's line (*scheduling*,
   and keeping copies elsewhere, are the add-on's). A cluster administrator
   who sends every manual export to external storage today loses that.
2. **The OS keeps a small kind for the standing destination and key**, with
   no schedule and no retention in it; the add-on's kind refers to it. The
   export controller changes little. The OS keeps the vault path, the
   ExternalSecrets and the credential check of §1.2.
3. **The add-on states destination and key on every export it creates**
   (`custom`), and holds the standing arrangement in its own kind. An
   export can already carry keys for itself alone (the `transient`
   credential source), so the OS needs little new; the add-on's controller
   must then hold a destination's keys itself to hand them over on each run
   (§4).

## 4. What an add-on needs from the OS

To run schedules itself, an add-on brings a controller and kinds of its own.
The concept's §5.2 describes how; none of it is built.

| Need | State |
|---|---|
| **Install CRDs and a controller from a catalogue entry** — the `apiExtensions` privilege kind, with its approver | designed in the concept §5.2; no code. A profile bundle may carry companion objects today, and what the director admits among them decides whether a CRD can travel that way |
| **A ClusterRole granted with the entry** — create `TenantExport`, read `Tenant`, delete finished exports | designed (§5.2); the grant path for cluster roles is the privilege mechanism of [target-component-structure.md](target-component-structure.md) §4.3, of which only the allowlist exists |
| **A way to write its own objects through the director** — a tenant's schedule is declared state, and the director is the only writer of git | not there. The director's routes are written per kind (`backup-policy`); there is no route that commits an object of a kind the OS does not know |
| **A way to read its own objects' state for a console** | not there. The usher relays fixed paths to the operator; an add-on's console would ask the add-on's own backend instead, which then needs its own authorization check against the cluster's graph |
| **A destination's keys for an export** (only under §3 option 3) | an export takes keys of its own (`transient`); where the add-on keeps a destination's keys between runs, and who may read them, is not designed |
| **Run on a cluster as a service**, in a namespace of its own | the `service` class exists (AD-4) |

## 5. Order of work for v0.5, and what breaks at each step

Each step leaves the tree building and a cluster working. Steps 1 to 3 add;
nothing breaks until step 5.

| # | Step | What breaks |
|---|---|---|
| 0 | **Decide §3**, and decide whether a cluster that upgrades keeps its schedules (step 6). | nothing |
| 1 | **Make the export independent of the policy**, as §3 decided. Under option 1: remove the `policy` destination mode and default to `platform`. | exports that relied on the policy for an external destination or for recipients go to the cluster's storage under the cluster's key. A `TenantExport` in git or in a script that says `mode: policy` is refused by the schema |
| 2 | **Build the `apiExtensions` privilege kind** and the grant for a cluster role (concept §5.2): schema, the director's admission of a bundle that carries CRDs, the approval route, the operator applying the grant. | nothing existing. This is new, security-relevant surface: a catalogue entry that extends the cluster's API |
| 3 | **Give an add-on a way to declare its objects through the director** (§4, third row), and decide where its console reads state from. | nothing existing |
| 4 | **The add-on ships its controller and kinds** in a group of its own, creating `TenantExport`s. Outside this repository; listed because step 5 waits on it. | nothing here |
| 5 | **Remove from the OS**: the two controllers and their registration; the two kinds and the leftover `TenantBackupPolicy` CRD, from `api/`, the three CRD directories and the ClusterRole; `internal/backup/policy.go` as far as step 1 left it; the director's three policy routes and `gitops/backup.go`; the usher's and the operator's policy and schedule reads; the vault policy lines and their lint and fixtures, unless §3 option 2 keeps them; the tests of §1.4. | a cluster with no add-on stops taking scheduled backups — the intended state. A console that calls the removed routes gets 404 until it calls the add-on's. `make verify` fails until the generated files, the RBAC table and the render fixtures are regenerated in the same change |
| 6 | **Clusters that already have policies.** A `BackupPolicy` file in a deployments repository names a kind that no longer exists, so Argo CD's sync of that directory fails on it. Either the upgrade removes the files, or a one-time conversion writes the add-on's objects from them. Removing the CRDs deletes every `BackupPolicy` and `TenantExportSchedule` on the cluster; bundles already taken are untouched, because they belong to their `TenantExport`s. | scheduled backups stop on upgrade unless the add-on is installed first and the conversion has run |
| 7 | **Documents**: the list in §1.4, rewritten so that the OS's guides describe export, download, import and restore, and say that scheduling is an add-on's. | nothing |

## 6. The riskiest step

**Step 6, on a cluster that upgrades.** Everything before it is additive or
is caught by the build. Step 6 is where a cluster that takes nightly backups
today silently takes none tomorrow: the kinds go, their objects go with
them, nothing fails loudly, and the first sign is a missing bundle on the day
one is needed. Whatever is decided, the upgrade has to say what it did — a
cluster that had a schedule and now has none must report it where its
administrator looks.

Second to it is **step 2**: it is the first time a catalogue entry may add
kinds to the cluster's API and receive a cluster-wide role, and it decides
what every later add-on can ask for.

## 7. Open

- §3: which of the three options.
- Whether v0.5 supports upgrading a cluster that has policies, or only new
  installs.
- Whether "delete finished exports beyond a count" stays available to the OS
  in some form — a tenant that exports by hand accumulates bundles in the
  cluster's storage with nothing to remove them but `delete-backup`.
- Whether the cluster administrator's external destination for manual
  exports (§3, option 1) is a loss the owner accepts.
