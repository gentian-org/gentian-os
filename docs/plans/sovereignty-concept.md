# Sovereignty concept: the tenant's data is the tenant's

Gentian OS is open source. This document says what that gives a tenant, how
much of it is built, and what the project commits to keep that way. It
describes Gentian OS as of v0.5; where the code is not there yet, §9 says so.

The promise is security and sovereignty: at any moment a tenant can take all
of their data with them, and at any moment they can have all of it destroyed,
without asking the platform's permission and without depending on anyone's
tooling to read what they took.

Anyone may build on Gentian OS, and a vendor may sell add-ons and services on
top of it. An add-on is not part of this repository: it comes from a catalog
under its own license and brings its own packaging (§5). Nothing in this
document depends on one.

One rule shapes the rest of the document: every lifecycle flow is built from
the same few primitives, so that export, import, restore and deletion agree
about what "all of the data" is. The inventory of what exists today is in §9.

## What is free

Everything in this repository, at any size, for an organisation's own use or
for its customers. [LICENSING.md](../../LICENSING.md) says which license
covers which file; running the software asks nothing under either.

For a tenant's data, that means the following. "Built" is the state on the
branch this document is on; §9 has the detail.

| What | Who does it | State |
|---|---|---|
| The bundle: one format for a tenant's data, encrypted, readable with [age](https://age-encryption.org) and a tar tool (§1) | — | built as schema 1; schema 2 (secrets, mail) is designed |
| Export now, and download the bundle as one file (§4.2) | tenant administrator | built |
| Import a bundle as a new tenant, on this cluster or another (§4.3) | cluster administrator | built |
| Restore a bundle into a tenant that exists | administrator | built, with no console screen |
| Retire a tenant and keep its data | cluster administrator | built |
| Purge: destroy a tenant's data (§4.4) | cluster administrator | built for databases, buckets, volumes, the realm and the backup bucket; the stores listed in §9.3 are still left behind |
| Offboard: export to the tenant's key, hand over, purge, and a signed record of what was destroyed (§4.5) | tenant administrator asks, cluster administrator confirms | not built |

## What the project commits to keep free

Each of these is stated in this repository already; the place is named beside
it. Where a commitment is about something not yet built, the table above says
so.

The commitments cover the whole open platform, not this repository alone:
the desktop and the Admin Console (gentian-ui) and the catalog of
open-source app profiles (gentian-apps) with it. An app in that catalogue
keeps the license of the project it comes from.

1. **The OS is open source, all of it.** The core is MPL-2.0; the resource
   types, the CRDs generated from them and the bundle format are Apache-2.0.
   No code the project writes for the OS is under a source-available or
   network-copyleft license ([licensing.md](licensing.md), Excluded Licenses).
2. **Leaving is part of the core.** Export, import, restore and purge are
   core functions under the core's license, so that a tenant's mobility never
   depends on an add-on ([licensing.md](licensing.md), License Allocation).
3. **The bundle format belongs to the OS and there is one of it.** It is
   specified here, anything that reads or writes a tenant's data in bulk uses
   it, and it is readable with standard tools and no running service (§1,
   §1.2).
4. **A bundle is always encrypted, and the tenant can have it encrypted to a
   key of their own**, which the provider cannot read (§1.4).
5. **An import does not depend on the cluster the bundle came from**, nor on
   its provider's recovery kit (§4.3).
6. **The OS builds, installs and runs with no add-on and no store present.**
   Nothing in it — the APIs, the formats, leaving — depends on one. A default
   install may propose an add-on; it never requires one (§5,
   [licensing.md](licensing.md), [LICENSING.md](../../LICENSING.md)).
7. **The cluster gates nothing on a license.** It holds no entitlement, and
   an install is asked of the person who makes it and of nothing else
   ([architectural-decisions.md](architectural-decisions.md) AD-3, AD-14).

## An aim, not a commitment

A bundle made by v0.5 or later is meant to stay importable by later versions,
if need be with the help of a conversion tool. For bundles older than v0.5
nothing is promised beyond what §7 says of schema 1.

## What is not part of the commitment

- **Scheduled backups.** Taking exports on a schedule, keeping them somewhere
  other than the cluster and deciding how long they are kept are not part of
  the OS. They are an add-on's work (§5), built on the export the OS
  provides.
- **A store.** The App Store app on a cluster shows the data of a store run
  outside it. A cluster with no store installs apps by command (AD-14). A
  cluster that has turned its usage statistics report off has no App Store app
  ([operations.md](../design/operations.md) §6.2); nothing else changes.
- **Software the OS installs that is not the project's.** The system services
  and the catalog's apps keep their own licenses
  ([LICENSING.md](../../LICENSING.md)).

## 1. The one object: the bundle

A **bundle** is a self-contained, encrypted snapshot of one tenant. It is the
unit everything else moves: an export produces one, a backup is an export on a
timer, an import consumes one, recovery is an import of the most recent one,
and deletion is the promise that nothing the bundle would have contained
survives. Its format is normative here, in Gentian OS, and anything built on
top of the OS produces or consumes exactly this format. There is no second
format.

### 1.1 Contents (schema 2)

| Part | What | Today (schema 1) |
|---|---|---|
| `manifest.json` | tenant name, the full `TenantSpec` as committed in git, operator version, per-app profile coordinate **and content digest**, chart version, stores, capture times | present; digest missing |
| `identity/realm.json`, `users.ndjson`, `memberships.ndjson` | the Keycloak realm: groups, roles, clients, every user, every membership | present |
| `apps/<app>/postgres/*.pgc`, `mariadb/*.sql.gz`, `s3/*.tar.gz`, `volumes/*.tar.gz` | each app's stores, as the profile's requirements declare them | present (flat layout) |
| `secrets/<app>.json` | the OpenBao secrets the tenant's apps own that are *data* rather than platform-issued credentials (API keys the tenant typed, OAuth tokens to third parties) | declared (`boundSecretKeys`), never written |
| `mail/<domain>/` | Maildirs and sieve scripts for the tenant's mail domain; DKIM private key | absent |
| `shell/` | the desktop's own database (preferences, notifications, usage) | present |
| `bundle-info.json` (cleartext) | tenant, schema, encryption mode, recipients, `howToDecrypt` | present |

Not in a bundle, on purpose: caches (Redis), derived files an app regenerates,
and **platform-issued credentials** — the database passwords, S3 keys and
OIDC client secrets the operator mints for an app. Those are the cluster's,
not the tenant's, and an import mints new ones (§4.3).

People's sign-in credentials are not in a bundle either: a restored person
gets a password reset.

### 1.2 Self-contained

A bundle is readable with [age](https://age-encryption.org) and a tar tool.
`bundle-info.json` says how. Nothing in it points at a URL that has to be
alive; the one dependency a bundle keeps is on the **catalogue entries** it
names — an import needs the profiles at the pinned digests, or newer ones that
declare a migration from them. That dependency is stated in the manifest
rather than hidden, and it is the catalog's job (AD-3, AD-14) to serve old
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
| **platform key** — the cluster's backup recipients | the tenant and the operator | manual export, scheduled export |
| **existing recipient** — a public key whose private half the tenant holds | the tenant | — |
| **passphrase** | whoever holds the phrase | — |

The console offers these three, starts on the platform key and makes no key
itself.

## 2. The primitives

Five operations, each implemented once in Gentian OS, from which every flow in
§3 is built, and which anything built on top calls as well.

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

| Flow | Who | Composition | Today |
|---|---|---|---|
| **Create** | cluster admin | P1 | built |
| **Export** | tenant admin, one click | P3 → stage in the cluster's bucket → stream to the browser as a `.gentian` file → delete the stage after `ttlSeconds` | built |
| **Import** | cluster admin | read `manifest.json` → P1 with the bundle's `TenantSpec` → wait for Ready → P4 | built |
| **Restore** | administrator | P4 into the existing tenant from a bundle the admin supplies | built, no console |
| **Retire** | cluster admin | remove the manifest; data follows `deletionPolicy` (Retain) | built |
| **Purge** | cluster admin | set `deletionPolicy: Delete`, wait for the cluster to hold it, remove the manifest → P5 | built 2026-10-02; P5 incomplete (§9.3) |
| **Offboard** | tenant admin asks, cluster admin confirms | Export to the tenant's key → hand over → Purge → signed deletion record | missing; the sovereign exit |

Import is Create plus Restore, and Offboard is Export plus Purge. No flow
introduces machinery the others lack: one inventory, one bundle, one manifest
path.

Anything further is a composition of these and is not a flow of the OS: a
backup is an Export on a timer, a recovery is an Import or a Restore of the
newest bundle, and a migration from or to another product is a converter on
either side of Import and Export. Whoever builds such a thing calls the
OS primitives through the director like any other client (§5).

## 4. What has to change in Gentian OS for the flows to hold

### 4.1 The bundle becomes complete (schema 2)

- **Secrets.** Walk the tenant's OpenBao tree the way purge already does
  (`SidecarNames`, `purgeOpenBaoSecrets`) and capture the keys the profile
  marks as tenant data. A profile declares which of its secrets are data
  (`spec.backup.boundSecrets`, already in the CRD) — platform-issued
  credentials are never in that list.
- **Mail.** The tenant's domain is a store like any other: Maildirs, sieve,
  DKIM key. Captured by a `mail` unit; destroyed by purge.
- **Digests.** The manifest pins each profile's content digest, so an import
  can refuse a catalog that no longer serves what the data was written by.
- **Layout.** Per-app directories, so a bundle is readable by a person.

### 4.2 Export lands in the browser

A director route streams a bundle as one `.gentian` file:
`GET /v1/tenants/{t}/backups/{name}/download`. The export is staged in the
cluster's own bucket as today, the stream is a tar of the prefix, and the stage
is removed when `ttlSeconds` expires. The console's Export tab gets a
**Download** button per bundle and an **Export now** with the key choice of
§1.4.

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
own store the profile's restore hook rewrites it. The alternative — the recovery kit as the cluster's identity, so that
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
the inventory destroyed, signed by the director (`records.deletion`). A tenant leaving can be given that record; it is the proof
the promise was kept.

### 4.5 Offboarding is a flow, not a procedure

One console action for the cluster administrator, on a tenant's request:
export to a key the tenant supplies → hand the bundle over → purge with
`keepBundles: false` → deletion record. Each step is one of the above; the flow
only sequences them and refuses to purge before the export is Ready.

## 5. Add-ons, and where the OS ends

Everything a tenant needs to **own** their data is in gentian-os, and every
promise in §1 holds with nothing but Gentian OS installed.

What goes beyond that — looking after the data on a schedule, keeping copies
elsewhere, moving data in from other products — can be built on top by
anyone, and a vendor may sell it. Such an add-on is delivered as catalogue
entries under its own license, and it brings its own packaging as every
add-on does: its profiles, its controller if it has one, and its own
AppProject. What its license asks of whoever installs it is stated with the
add-on, not here, and the cluster gates no install on it.

An add-on uses the OS the way every other client does:

- It calls the primitives of §2 through the director, with the signed-in
  person's token. It never writes to git itself — the director is the only
  writer (AD-2) — and it reads a tenant's data only through a bundle the
  person was allowed to open.
- It produces and consumes the bundle of §1 and no other format.

**Scheduling is not the OS's.** The OS takes an export when it is asked to
(`TenantExport`). When exports are taken without anyone asking, where else
they are kept and when old ones are removed is an add-on's to decide, with a
controller and resource kinds of its own (§5.2). The line is at *scheduling*,
not at *where the bundle goes*: a cluster with no such add-on takes no export
on its own, and every export a person asks for works as before.

### 5.1 What the Admin Console says about add-ons

Where an OS flow has a continuation in an add-on, the Admin Console says so:
with the add-on installed it links to it, and without it the same place says
what the add-on would do and offers to install it. The OS path is always
there and always works.

### 5.3 What a default install proposes

A default install proposes a store and one add-on. Both arrive through
surfaces the OS already has, and the OS installs and runs without them.

- **The store.** Step 0's scaffold of the Cluster claim sets
  `catalogue.storeUrl` and lists one catalog source. The App Store app is a
  platform UI on the cluster that shows the store's data and installs through
  the director (AD-3). Every tenant can install from the source; nothing the
  store says decides what a tenant may install, and the cluster does no
  license gating (AD-3, AD-14). The addresses are the installer's defaults
  and can be changed or left out ([install-reference.md](../install-reference.md)).
- **One add-on's profile.** Step 0 places the profile of the Operations
  Console, an add-on from the store's catalogue, among the cluster's
  profiles, held to a digest like any install (AD-14,
  `GENTIAN_DEFAULT_PROFILES`). It is not part of this repository and carries
  its own license.
- **The switch.** `./install.sh --disable-api-extensions`
  (`GENTIAN_DISABLE_API_EXTENSIONS=1`) places no default profile. Nothing
  else changes: export, import, purge and the bundle are the OS's and need no
  extension. The switch is also to leave out the API-extension grant of §5.2
  once that exists; today the scaffold only records the choice.

## 7. Order of work

**M1 (landed 2026-10-02) — the structures, before the content.** Creating
and deleting a tenant follow §2–§4: purge drops Postgres databases and
roles, MinIO users and the backup bucket (the OpenFGA, kernel-realm, LiteLLM,
Redis and mail units of §9.3 are still open); `keepBundles` reaches the
operator, the console and the CLI; the schedule, policy and destination
screens left the Admin Console, which keeps export; the installer learns the
switch (§5.3).

**M2a (landed 2026-10-02) — the round trip.** Download (§4.2) and Import
(§4.3) exist end to end: director routes, operator verbs, the console's
Download link and Import card, `kubectl gentian tenants import`. Step 0
materialises the default profiles into `clusters/<id>/catalogue/`
(`GENTIAN_DEFAULT_PROFILES`, §5.3).

**M2b — still to do**, in this order:

1. **Inventory parity** (P2 = P3 = P5): mail unit, OpenBao data secrets,
   OpenFGA tuples, kernel-realm artefacts, LiteLLM, Redis keys — each added
   to the inventory and to both capture and purge. Deletion record.
2. **Schema 2**: per-app layout, digests in the manifest, single-file
   container, `bundle` package with three backends. Schema 1 bundles stay
   readable.
3. **Offboard** flow and console action.
4. **Scheduling leaves the tree** (§9.2): the order of that work, and what
   breaks at each step, is in [#608](https://github.com/gentian-org/gentian-os/issues/608).

## 8. Open questions

- Whether `apiExtensions` grants should carry an expiry like the other
  privilege kinds. A CRD that expires is a cluster that loses objects; the
  grant probably has no `expiresAt`, and §4.3 of the component structure
  should say so for this kind.

## 9. What exists today

### 9.1 Built

- CRDs `TenantExport` and `TenantRestore` with `confirmTenant`, per-app
  quiesce, encryption modes recipient/passphrase, destinations
  policy/platform/custom.
- Bundles as age-encrypted artefacts under an S3 prefix, `manifest.json.age`
  last, cleartext `bundle-info.json` with the decrypt command.
- Capture of Postgres, MariaDB, S3 buckets, volumes, the Keycloak realm
  (users, groups, memberships — no credentials) and the shell database.
- Restore into an existing tenant by an administrator, realm by partial import
  with `passwordResetRequired`.
- Console: Export tab with key choice (platform / existing /
  passphrase) and download.
- Retire and, since 2026-10-02, purge through the director, behind a typed
  name in console and CLI.

### 9.2 Not built, and what is still in the tree

Offboarding, the deletion record, mail and secrets capture, and the
API-extension privilege kind.

**Scheduled backups are still in this repository.** The body of this
document describes v0.5, in which they are not part of the OS. The code has
not followed yet: the kinds `TenantExportSchedule` and `BackupPolicy`, their
two controllers in the operator, and the director's and the usher's
backup-policy and backup-schedule routes are all still here and working. A
cluster built from this tree takes scheduled backups with no add-on
installed. Whether and when that move starts is open ([#608](https://github.com/gentian-org/gentian-os/issues/608)).

Built since the first draft: download, import (upload, inspect, declare from the manifest,
restore), `keepBundles`, the default-profile materialisation.

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
