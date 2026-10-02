# Sovereignty concept: the tenant's data is the tenant's

Two products, two promises. **Gentian OS** promises security and sovereignty:
at any moment a tenant can take all of their data with them, and at any moment
they can have all of it destroyed, without asking the platform's permission and
without depending on anyone's tooling to read what they took. **Gentian Corp**
promises convenience and reliability: the same data looked after on a schedule,
kept off-site, recoverable with one click, and movable in and out of the
workspace products an organisation already uses. The first is free software.
The second is openly available under a different license — free for
organisations under fifty users and not for resale — because convenience is
what people pay for and sovereignty is what they must never have to. That
license is enforced legally, not technically: nothing in the store or the
cluster counts users or gates an install on a subscription. The Corp entries
are listed, anyone installs them, and the terms are the terms.

Everything in this document follows from keeping those two promises distinct,
and from one further rule: every lifecycle flow is built from the same few
primitives, so that export, backup, import, recovery and deletion agree about
what "all of the data" is. The inventory of what exists today is in §9.

## 1. The one object: the bundle

A **bundle** is a self-contained, encrypted snapshot of one tenant. It is the
unit everything else moves: an export produces one, a backup is an export on a
timer, an import consumes one, recovery is an import of the most recent one,
and deletion is the promise that nothing the bundle would have contained
survives. Its format is normative here, in Gentian OS, and every Gentian Corp
tool produces or consumes exactly this format — the converters, the schedulers,
the recovery screen. There is no second format.

### 1.1 Contents (schema 2)

| Part | What | Today (schema 1) |
|---|---|---|
| `manifest.json` | tenant name, the full `TenantSpec` as committed in git, operator version, per-app profile coordinate **and content digest**, chart version, stores, capture times | present; digest missing |
| `identity/realm.json`, `users.ndjson`, `memberships.ndjson` | the Keycloak realm: groups, roles, clients, every user, every membership | present |
| `identity/credentials.ndjson` | password hashes, TOTP secrets, WebAuthn credentials, required actions — **included by default**; an export may say `omitCredentials` | absent |
| `identity/scim/` | the same people and groups as SCIM 2.0 resources (RFC 7643 `User`, `Group`) | absent |
| `apps/<app>/postgres/*.pgc`, `mariadb/*.sql.gz`, `s3/*.tar.gz`, `volumes/*.tar.gz` | each app's stores, as the profile's requirements declare them | present (flat layout) |
| `apps/<app>/canonical/` | the app's data in its **canonical interchange form** where the profile declares one (§6) — files as an object tree, mail as Maildir, contacts as vCard, calendar as iCalendar | absent |
| `secrets/<app>.json` | the OpenBao secrets the tenant's apps own that are *data* rather than platform-issued credentials (API keys the tenant typed, OAuth tokens to third parties) | declared (`boundSecretKeys`), never written |
| `mail/<domain>/` | Maildirs and sieve scripts for the tenant's mail domain; DKIM private key | absent |
| `shell/` | the desktop's own database (preferences, notifications, usage) | present |
| `bundle-info.json` (cleartext) | tenant, schema, encryption mode, recipients, `howToDecrypt` | present |

**Credentials are in the bundle** because they are the tenant's: a restored
workspace is one people can sign in to, and the bundle is encrypted to a key
the tenant chose, so a leaked bundle is a leaked key — which is already true
of the realm. The `omitCredentials` flag is for a tenant who wants a bundle a
provider can hold without that exposure; the console shows it as a checkbox on
the export form, off by default.

**Identity is Keycloak and the portable form is SCIM.** There is no LDAP in
Gentian and none is proposed. A bundle carries the realm export for a faithful
restore and the SCIM projection for everything that is not Gentian — the
converters in §6, and any directory the tenant moves to next.

Not in a bundle, on purpose: caches (Redis), derived files an app regenerates,
and **platform-issued credentials** — the database passwords, S3 keys and
OIDC client secrets the operator mints for an app. Those are the cluster's,
not the tenant's, and an import mints new ones (§4.3).

### 1.2 Self-contained

A bundle is readable with [age](https://age-encryption.org) and a tar tool.
`bundle-info.json` says how. Nothing in it points at a URL that has to be
alive; the one dependency a bundle keeps is on the **catalogue entries** it
names — an import needs the profiles at the pinned digests, or newer ones that
declare a migration from them. That dependency is stated in the manifest
rather than hidden, and it is the catalogue's job (AD-3, AD-14) to serve old
digests for as long as a supported version can import them.

### 1.3 Three containers, one bundle

The set of artefacts is the bundle. It may be held as

- an **S3 prefix** (today's storage form: streamable, per-artefact, resumable),
- a **directory** on disk,
- a **single file** `<tenant>-<export>.gentian` — a tar of the directory.

One package (`internal/backup/bundle`) reads and writes all three, and every
flow in this document takes "a bundle" without caring which. The single file
is what a browser download produces and what an import upload accepts.

### 1.4 Encrypted, always, to a key the tenant chooses

Every artefact is an age file. The recipient is one of:

| Mode | Who can open it | Default for |
|---|---|---|
| **tenant key** — an X25519 identity minted in the console, shown once, escrowable at the tenant's choice | the tenant | manual export |
| **passphrase** | whoever holds the phrase | manual export, on request |
| **platform key** — the cluster's backup recipients | the tenant and the operator | scheduled backup (Gentian Corp) |

This is what exists (`BackupKeyChoice`), with one change: a manual export
defaults to the tenant's own key, not the platform's. Sovereignty means the
default bundle is one the provider cannot read.

## 2. The primitives

Five operations, each implemented once in Gentian OS, from which every flow in
§3 is built — the free ones and the paid ones alike.

| # | Primitive | Where it lives | Does |
|---|---|---|---|
| P1 | **Declare** | director, `gitops.CreateTenant` | write a tenant manifest to `gentian-deployments` as a signed commit; Argo CD and the operator do the rest |
| P2 | **Inventory** | operator, `internal/backup/inventory.go` | from a tenant's live apps and their profiles, list every store the tenant owns: databases, buckets, volumes, secrets, realm, mail, shell |
| P3 | **Capture** | operator, `TenantExport` | quiesce one app at a time, copy each inventoried store into bundle artefacts, encrypt, write manifest last |
| P4 | **Load** | operator, `TenantRestore` | the inverse of P3 into a tenant that exists: pause, load, run the profile's restore hooks, resume; realm and shell last |
| P5 | **Purge** | operator, `reconcileDelete` under `deletionPolicy: Delete` | destroy every inventoried store — the same list P2 produced for capture |

The rule that keeps the flows honest: **P3 and P5 iterate the same inventory.**
A store that export captures is a store that purge destroys, and a store that
purge destroys is one export would have carried. Today they diverge (§9.3);
closing that is the first piece of work.

## 3. The flows

| Flow | Product | Who | Composition | Today |
|---|---|---|---|---|
| **Create** | OS | cluster admin | P1 | built |
| **Export** | OS | tenant admin, one click | P3 → stage in the cluster's bucket → stream to the browser as a `.gentian` file → delete the stage after `ttlSeconds` | P3 built; download missing |
| **Import** | OS | cluster admin | read `manifest.json` → P1 with the bundle's `TenantSpec` → wait for Ready → P4 | missing |
| **Restore** | OS | cluster admin | P4 into the existing tenant from a bundle the admin supplies | built, admin-only, no console |
| **Retire** | OS | cluster admin | remove the manifest; data follows `deletionPolicy` (Retain) | built |
| **Purge** | OS | cluster admin | set `deletionPolicy: Delete`, wait for the cluster to hold it, remove the manifest → P5 | built 2026-10-02; P5 incomplete |
| **Offboard** | OS | tenant admin asks, cluster admin confirms | Export to the tenant's key → hand over → Purge → signed deletion record | missing; the sovereign exit |
| **Backup** | Corp | tenant admin sets it, nobody runs it | P3 on a schedule → a destination → retention | built in OS today; moves (§5) |
| **Remote backup** | Corp | cluster admin | Backup to an external destination, keys escrowed | partly built in OS today; moves (§5) |
| **Recovery** | Corp | cluster or tenant admin, one click | pick a bundle → Import (tenant gone) or Restore (tenant present) → verified | missing |
| **Ingest** | Corp | tenant admin | converter reads M365 / Google Workspace → writes a bundle → Import | missing |
| **Egress** | Corp | tenant admin | Export → converter pushes the bundle to M365 / Google Workspace | missing |

Import is Create plus Restore; Recovery is Import or Restore chosen by whether
the tenant exists; Backup is Export on a timer; Offboard is Export plus Purge;
Ingest and Egress are a converter on either side of Import and Export. No flow
introduces machinery the others lack, which is the DRY the title asks for: not
one binary, but one inventory, one bundle, one manifest path, and the paid
flows calling the free primitives through the director like any other client.

## 4. What has to change in Gentian OS for the flows to hold

### 4.1 The bundle becomes complete (schema 2)

- **Identity.** Keep the realm export. Add `credentials.ndjson` so a restored
  workspace is one people can sign in to, and the SCIM projection so a bundle's
  people are readable by anything that speaks SCIM.
- **Secrets.** Walk the tenant's OpenBao tree the way purge already does
  (`SidecarNames`, `purgeOpenBaoSecrets`) and capture the keys the profile
  marks as tenant data. A profile declares which of its secrets are data
  (`spec.backup.boundSecrets`, already in the CRD) — platform-issued
  credentials are never in that list.
- **Mail.** The tenant's domain is a store like any other: Maildirs, sieve,
  DKIM key. Captured by a `mail` unit; destroyed by purge.
- **Digests.** The manifest pins each profile's content digest, so an import
  can refuse a catalogue that no longer serves what the data was written by.
- **Layout.** Per-app directories, so a bundle is readable by a person and so
  §6's canonical forms have a home beside the raw stores.

### 4.2 Export lands in the browser

A director route streams a bundle as one `.gentian` file:
`GET /v1/tenants/{t}/backups/{name}/download`. The export is staged in the
cluster's own bucket as today, the stream is a tar of the prefix, and the stage
is removed when `ttlSeconds` expires. The console's Backup tab gets a
**Download** button per bundle and an **Export now** that defaults to "to my
computer, encrypted to my key", with the `omitCredentials` checkbox beside the
key choice.

The download is authenticated and goes through the director, never a presigned
URL: the director is the enforcement point for the tenant's data (AD-1), and a
URL that works without a session is a session nobody can revoke.

### 4.3 Import is Create from a manifest, then Restore

`POST /v1/clusters/{c}/tenants/import` takes a bundle (uploaded, or an S3
reference the cluster can reach) and the decryption material. The director:

1. reads `manifest.json` and refuses a tenant name the cluster already has;
2. **declares** the tenant from the bundle's `TenantSpec` — the same commit
   `CreateTenant` makes, with the apps list the bundle carried;
3. waits for the operator to report Ready, which provisions the realm,
   databases, buckets and apps as empty shells;
4. creates a `TenantRestore` against the bundle.

The tenant is "up and running again" when step 4 reports Ready. Nothing in it
is new machinery; the one new piece is the director reading a manifest instead
of a form.

**Import does not depend on the source cluster.** Platform-issued credentials
are re-minted by the importing cluster, and where an app has cached one in its
own store the profile's restore hook rewrites it. [open-items.md](open-items.md)
records the alternative — the recovery kit as the cluster's identity, so that
a new server importing it *becomes* the old cluster — and it is not taken:
it would bind a tenant's portability to its provider's kit, which is the
dependency this document exists to remove.

### 4.4 Purge destroys the inventory

Purge iterates P2. Today it does not (§9.3): PostgreSQL databases and roles,
OpenFGA tuples, the kernel-realm broker and client, the LiteLLM team, MinIO
users, Redis keys, Maildirs and tenant-export bundles all survive. Each becomes
a cleanup unit keyed off the same inventory entry export captures from.

Purge deletes everything, **including the tenant's backup bucket**, unless the
request sets `keepBundles` — the offboarding case, where the tenant was handed
a copy and wants the cluster's own gone but a provider is keeping one under
contract. The purge dialog in the console and `tenants retire --purge` in the
CLI both expose the flag, off by default.

Purge ends by writing a **deletion record**: tenant, who asked, request id,
the inventory destroyed, signed by the director (work-packages WP-9
`records.deletion`). A tenant leaving can be given that record; it is the proof
the promise was kept.

### 4.5 Offboarding is a flow, not a procedure

One console action for the cluster administrator, on a tenant's request:
export to a key the tenant supplies → hand the bundle over → purge with
`keepBundles: false` → deletion record. Each step is one of the above; the flow
only sequences them and refuses to purge before the export is Ready.

## 5. What is Gentian OS and what is Gentian Corp

Everything a tenant needs to **own** their data is in gentian-os. Everything
that makes owning it **convenient and reliable** is Gentian Corp's: delivered
as catalogue entries under its own license, bringing its own profiles, its own
controller and its own AppProject as every add-on does. The OS is written by
the company that sells the Corp layer, and it says so: the free console
promotes the paid features and builds its default workflows around them. What
it never does is make them mandatory — every promise in §1 holds with nothing
but Gentian OS installed.

| | Gentian OS (FOSS) | Gentian Corp |
|---|---|---|
| Bundle format, schema, encryption | normative here | consumes and produces |
| Export now, to the browser, to the tenant's key | ✓ | |
| Delete now: retire, purge, offboard, deletion record | ✓ | |
| Import a bundle; restore into an existing tenant | ✓, cluster admin | |
| Canonical interchange forms per app (§6) | ✓, the contract | converters target it |
| Scheduled backups | | ✓ |
| External destinations, key escrow, retention across tenants | | ✓ |
| Recovery on a click, restore drills with proof, DR | | ✓ |
| Converters: M365 / Google Workspace ⇄ bundle | | ✓ |

**What moves.** `TenantExportSchedule`, `BackupPolicy` and their controllers
exist in gentian-os today and are the scheduling layer; they move to the
Operations Console's component (§5.1). What stays is the primitive they
schedule (`TenantExport`) and the director's routes that write a committed
backup policy for a tenant — the director is the only writer of git (AD-2),
so a Corp console sets a policy the same way the Admin Console sets anything:
by asking the director. A cluster without the Corp component installed holds
the policy file and nothing acts on it; that is the free tier, precisely.

**The 2026-09-22 backup split** recorded in work-packages WP-9 drew the line
one step further towards gentian-os: it kept "local backup" — scheduling to
the cluster's own storage — and `BackupPolicy` on the free side, and moved
only remote targets, escrow, cross-tenant retention, drills, DR and migration
to a GTC component. This document supersedes it: the line is at *scheduling*,
not at *where the bundle goes*. WP-9's entry is updated to point here.

### 5.1 The Operations Console

A Gentian Corp app, built from the same template as the Admin Console and
installed the same way — a component per tenant from its own profile
(AD-10), with its platform-tenant instance carrying the cluster-scope
screens. Where the Admin Console is the place a tenant *controls* its
workspace, the Operations Console is the place it is *looked after*:

| Screen | Scope | Does |
|---|---|---|
| **Backups** | tenant | schedule (cron, apps, key, retention), destination (cluster storage, external S3), last and next run, each bundle's status and size, verification state |
| **Recovery** | tenant and cluster | pick a bundle, see what it holds (manifest, apps, people, capture time), restore into the tenant or re-import a tenant that is gone — one click, behind the typed-name confirmation the purge dialog uses |
| **Drills** | cluster | periodic restore into a scratch tenant, diffed against the source, reported as proof that the backups are restorable |
| **Destinations and keys** | cluster | external endpoints, credentials, escrowed identities, which tenants may override the cluster policy |
| **Ingest** | tenant | connect M365 or Google Workspace, choose what to bring (people, mail, files, calendars, contacts), produce a bundle, import it |
| **Egress** | tenant | export a bundle and push it to M365 or Google Workspace |
| **Disaster recovery** | cluster | the whole cluster's tenants from the newest bundles at an external destination, in order |

Its backend talks to the director with the signed-in person's token, as the
Admin Console does; what it needs that the director does not yet offer
(scheduling state, drill results, converter progress) its own controller
holds, in its own CRDs, shipped with its profile. It never bypasses the
director for a write to git, and it never reads tenant data except through a
bundle the person was allowed to open.

**The Admin Console promotes it.** Its Backup tab is built around the
Operations Console being there: with it installed, the tab shows the schedule,
the last run and a link into Recovery beside the one-click export; without it,
the same places show what scheduled backups, recovery and drills would give
this tenant and an **Install** that opens the store entry — free under fifty
users, so for most tenants the install is the whole decision. The same pattern
applies wherever a free flow has a paid continuation: the purge dialog mentions
that a tenant with scheduled backups keeps its bundles off-site; the import
screen mentions that Ingest does the same from M365 or Google Workspace. The
free path is always there and always works; the paid path is the default the
screens lead to.

### 5.2 Shipping a controller from the catalogue

The Operations Console is two catalogue entries, because it is two kinds of
thing:

| Entry | Class (AD-4) | Installed by | Holds |
|---|---|---|---|
| `operations` | `service` | the platform administrator, once per cluster, into `system-operations` | the CRDs (`BackupSchedule`, `BackupDestination`, `RestoreDrill`, `ConverterRun`), the controller, its ClusterRole |
| `operations-console` | `app` | each tenant, from the store | the console: frontend, BFF, tile |

The console **requires the service** the way an app requires a database —
`requires.services: [operations]` — and AD-5 does the rest: a tenant that
installs the console on a cluster without the service is told the platform
owes it something, and the install waits until the administrator provides it.
No new mechanism; a service that happens to carry CRDs is still a service.

What is new is that a catalogue entry extends the Kubernetes API. That is a
privilege, and it goes through the one approval path privileges have
([target-component-structure.md](target-component-structure.md) §4.3):

```yaml
spec:
  classes: [service]
  requires:
    privileges:
      apiExtensions:
        - name: backup-kinds
          group: operations.gentian.example
          kinds: [BackupSchedule, BackupDestination, RestoreDrill, ConverterRun]
          reason: "Schedules and drills are declared per tenant and reconciled cluster-wide."
      clusterRoles:
        - name: run-exports
          rules: [...]   # create TenantExport/TenantRestore, read Tenants
          reason: "The scheduler creates the exports the OS defines."
```

| kind | scope | approver | relation |
|---|---|---|---|
| **API extension** — the component installs CRDs and runs a controller that reconciles them | cluster: a new kind every namespace can hold | security officer | `cluster#can_approve` |

Rules that come with the kind, each preventing something specific:

- **The CRDs are the component's and travel with it.** They are rendered from
  the profile's package like any other object, in the component's own API
  group, never in `gentianos.io`; uninstalling the service removes the
  controller and leaves the CRDs and their objects in place until a
  cluster administrator deletes them, which is the Helm `crds/` behaviour and
  what every operator framework settled on, because a CRD removed takes every
  object of its kind with it.
- **The controller's reach is the ClusterRole it asked for**, granted
  alongside — it can create `TenantExport` and `TenantRestore` because those
  are the OS's own kinds, and it can read `Tenant`; it cannot write git, it
  cannot touch a tenant's data except through an export the OS captures, and
  it holds no credential the director would honour.
- **A tenant's objects of the new kinds live in `tenant-<t>`** and are written
  by the director from the console's requests, like every other declaration.
  The tenant's own RBAC never gets to write them directly, because then the
  controller would be a second writer beside git.
- **One extension per group.** Two profiles asking for kinds in the same group
  are refused at admission; the second would otherwise fight the first over
  the CRD.

Precedent, so none of this is invented: OLM's `ClusterServiceVersion`
declares the CRDs a package owns and the permissions its controller needs, and
an `InstallPlan` is approved by a cluster administrator before anything is
applied — the request/grant split above. Crossplane packages ship CRDs plus a
controller, are installed cluster-wide by an administrator, and declare
dependencies on other packages — the service/app split above. Helm installs a
chart's `crds/` once and never upgrades or deletes them — the ownership rule
above. Rancher distinguishes cluster-level apps from project-level apps on
exactly the line the two entries sit on. Argo CD's `AppProject` whitelists
which cluster-scoped kinds a project may apply; ours admits everything today,
and the grant is where the whitelist would otherwise have to be, because the
grant has an approver and a reason and a whitelist has neither.

### 5.3 How the Operations Console stores backups

Scheduled backups are not a nightly `.gentian` bundle: a bundle repeats every
byte, and a tenant with 200 GB of files cannot afford one a day. Nor are they a
monthly full plus daily diffs, the classic grandfather-father-son scheme — a
restore then replays a chain, one damaged diff breaks every restore after it,
and the chain grows until the next full. The industry moved past both to
**content-addressed, deduplicated, encrypted repositories** (restic, Kopia,
Borg; Veeam's and Rubrik's immutable repositories are the commercial form),
and that is what the Operations Console uses.

- **A repository per tenant**, in a bucket with **S3 Object Lock** in
  compliance mode. Data is split into chunks stored once under their content
  hash; **every snapshot is logically a full backup** — a restore reads one
  snapshot and no chain — while **physically only new chunks are written**, so
  a daily snapshot costs the day's changes. Chunks are immutable, which is what
  Object Lock wants: new objects are locked for the retention period (35 days
  by default), and pruning removes only chunks whose lock has expired. An
  attacker holding the cluster's credentials can write, and cannot delete or
  overwrite anything inside the window.
- **Databases by their own point-in-time machinery**, not dumps: CNPG WAL
  archiving and MariaDB incremental backups into the same locked bucket, which
  gives recovery to any minute at almost no daily cost. The quiesce logic the
  OS already has stays for the object and volume stores where consistency
  needs it.
- **A full `.gentian` bundle monthly, the last two kept, at a second
  destination** — another provider or site. Not as the base of a chain, but as
  the 3-2-1 off-site copy and the sovereign escape hatch that needs no
  repository software to read. It is the same bundle the tenant downloads with
  one click, so recovery onto a cold cluster is "import the newest bundle",
  the flow §4.3 defines.
- **Verification is not optional.** Deduplication's one real risk is a damaged
  shared chunk silently reaching many snapshots, so the repository is checked
  with full data reads on a schedule, and the restore drills (§5.1) restore a
  real snapshot into a scratch tenant and diff it.
- **The key is the other half of the defence.** A locked bucket protects
  nothing if the key can be deleted, so the repository key is the tenant's or
  escrowed outside the cluster, never only a Secret the cluster holds.
- **Snapshots are bundles.** The repository's snapshot is schema 2 in a fourth
  container (§1.3): the same artefacts, the same manifest, chunked instead of
  tarred. The OS's `bundle` package does not need to read it — exporting a
  snapshot as a `.gentian` file is the Operations Console's job — but the
  contents are identical, so a drill, a recovery and a download all restore
  the same thing.

Loss bounds that follow: an operational mistake costs up to one snapshot
interval; ransomware with cluster access costs nothing inside the retention
window; losing the cluster and its provider together costs up to a month, from
the off-site bundle. Whether the engine is restic or Kopia as a library or the
same principle implemented over the OS's capture units is a build decision for
the Operations Console, not for this document.

### 5.4 Installed by default

A vanilla installation comes with the App Store and the Operations Console,
because the default workflows are built around them (§5). Both arrive through
surfaces the OS already has, so the OS still installs and runs without them:

- **The store.** Step 0's scaffold of the Cluster claim sets `catalogue.storeUrl`
  and lists the Gentian catalogue source (`access: entitled`) rather than
  commenting them out. The App Store tile is a link component the desktop
  shows; what the cluster may install from the source is still the store's
  signed say, per tenant (AD-3, AD-14).
- **The Operations Console.** Its two entries (§5.2) are `defaultForTenants`
  apps and a service the installer declares on the Cluster claim's default
  components. The `apiExtensions` grant the service needs is written into the
  scaffold with the installing administrator as approver — the scaffold commit
  is theirs, under the break-glass key, so the grant has the person, the time
  and the reason AD-5 asks for. A cluster administrator who wants it gone
  removes the grant, and the service with it, by one commit.
- **The switch.** `./install.sh --disable-api-extensions`
  (`GENTIAN_DISABLE_API_EXTENSIONS=1`) leaves the grant and the service out
  of the scaffold. The console app, requiring the service, then waits and
  says why, and the Admin Console promotes it exactly as it would on any
  cluster without it. Nothing else changes: export, import, purge and the
  bundle are the OS's and need no extension.

## 6. Converters, and why apps need a canonical form

A bundle's app data is store-shaped: a Nextcloud bundle is a Postgres dump and
an object tree keyed the way Nextcloud keys them. A converter from Google Drive
cannot usefully produce that. What it can produce is **files in folders,
contacts as vCard, calendars as iCalendar, mail as Maildir** — the
interchange forms every workspace product can emit and ingest. These are
standard formats, never a Gentian schema, so the exit door stays open even for
a tenant who never installs the converters.

A `ComponentProfile` may therefore declare, under `spec.backup`:

```yaml
backup:
  canonical:
    files:    { export: <hook>, import: <hook> }   # object tree with paths
    contacts: { export: <hook>, import: <hook> }   # vCard 4.0
    calendar: { export: <hook>, import: <hook> }   # iCalendar
    mail:     { export: <hook>, import: <hook> }   # Maildir
```

Export runs the hooks and writes `apps/<app>/canonical/`; import, finding
canonical data and no raw stores for an app, runs the import hooks after the
app is provisioned. A converter therefore writes a bundle containing a manifest
(tenant name, people as SCIM, the apps to install) and canonical data only, and
Import brings it up like any other bundle. The reverse converter reads
canonical data and the SCIM people and pushes them through the Graph or
Workspace APIs. The hooks are each app's own and ship with its profile.

## 7. Order of work

Gentian OS first, because it is the contract everything else is built on.

**M1 (landed 2026-10-02) — the structures, before the content.** Creating
and deleting a tenant follow §2–§4: purge drops Postgres databases and
roles, MinIO users and the backup bucket (the OpenFGA, kernel-realm, LiteLLM,
Redis and mail units of §9.3 are still open); `keepBundles` reaches the
operator, the console and the CLI; the Operations Console exists as its own
app with the backup screens moved out of the Admin Console, talking to the
director's existing backup routes; the Admin Console keeps export and gains
the promotion; the installer learns the switch.

**M2a (landed 2026-10-02) — the round trip.** Download (§4.2) and Import
(§4.3) exist end to end: director routes, operator verbs, the console's
Download link and Import card, `kubectl gentian tenants import`. Step 0
materialises the default profiles into `clusters/<id>/catalogue/`
(`GENTIAN_DEFAULT_PROFILES`, §5.4). The Corp side publishes its chart and
builds its catalogue source.

**M2b — still to do**, in this order: inventory parity for the remaining
units; the scheduling controllers and the `operations` service with its
`apiExtensions` grant; schema 2 (credentials, SCIM, secrets, mail, digests);
the deletion record and the offboard flow; canonical forms.

1. **Inventory parity** (P2 = P3 = P5): mail unit, OpenBao data secrets,
   Postgres roles, OpenFGA tuples, kernel-realm artefacts, LiteLLM, MinIO
   users, Redis keys, backup bucket — each added to the inventory and to both
   capture and purge. `keepBundles` on purge, in console and CLI. Deletion
   record.
2. **Schema 2**: per-app layout, digests in the manifest, credentials
   (with `omitCredentials`) and SCIM in the identity part, single-file
   container, `bundle` package with three backends. Schema 1 bundles stay
   readable.
3. **Download**: director route, console buttons, tenant-key default,
   `omitCredentials` checkbox.
4. **Import**: director route, manifest-driven Create, chained Restore;
   `kubectl gentian tenants import <file>`. Recovery playbook rewritten
   around it (it still names `tenants deploy`, a command that no longer
   exists).
5. **Offboard** flow and console action.
6. **Canonical forms**: CRD field, hooks in the first profiles (Nextcloud
   files, contacts, calendar; mail).
7. **Scheduling moves out**: `TenantExportSchedule`, `BackupPolicy` and
   their controllers leave gentian-os for the Operations Console's component;
   the Admin Console's Backup tab keeps export and download and promotes the
   Operations Console for the rest.

Gentian Corp — the Operations Console with backups, destinations, recovery,
drills, then ingest and egress — starts after 2, 6 and 7, since those are its
contract and its code.

## 8. Open questions

- Whether `apiExtensions` grants should carry an expiry like the other
  privilege kinds. A CRD that expires is a cluster that loses objects; the
  grant probably has no `expiresAt`, and §4.3 of the component structure
  should say so for this kind.

## 9. What exists today

### 9.1 Built

- CRDs `TenantExport`, `TenantExportSchedule`, `BackupPolicy`, `TenantRestore`
  with `confirmTenant`, per-app quiesce, encryption modes recipient/passphrase,
  destinations policy/platform/custom.
- Bundles as age-encrypted artefacts under an S3 prefix, `manifest.json.age`
  last, cleartext `bundle-info.json` with the decrypt command.
- Capture of Postgres, MariaDB, S3 buckets, volumes, the Keycloak realm
  (users, groups, memberships — no credentials) and the shell database.
- Restore into an existing tenant, admin-only, realm by partial import with
  `passwordResetRequired`.
- Console: Backup tab with key choice (platform / new / existing /
  passphrase), destination choice, schedule; cluster and tenant
  `BackupPolicy` through the director.
- Retire and, since 2026-10-02, purge through the director, behind a typed
  name in console and CLI.

### 9.2 Not built

Recovery as a one-click flow, offboarding, deletion record, SCIM projection,
credentials in the bundle, mail and secrets capture, canonical forms,
converters, the `operations` service and the controllers' move. Built since
the first draft: download, import (upload, inspect, declare from the
manifest, restore), the Operations Console as an app, `keepBundles`, the
default-profile materialisation.

### 9.3 Where capture and purge disagree

| Store | Captured | Purged |
|---|---|---|
| Postgres databases | ✓ | ✓ (since M1: databases and roles dropped by a Job) |
| MariaDB | ✓ | ✓ |
| S3 buckets | ✓ | ✓ (since M1: users and policies go with the bucket) |
| Volumes | ✓ | ✓ with the namespace |
| Keycloak tenant realm | ✓ | ✓ |
| Kernel realm: broker, client, tenant admin | — | ✗ |
| OpenBao tenant tree | ✗ | ✓ |
| OpenFGA tuples | — | ✗ |
| Mail | ✗ | ✗ (domain routing yes; Maildirs no) |
| Redis | — | ✗ (ACL user yes; keys no) |
| LiteLLM team and keys | — | ✗ |
| Director records | — | retention only |
| Backup bucket | — | ✓ (since M1, unless `keepBundles`) |
