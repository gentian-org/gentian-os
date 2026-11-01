---
audience: customer, architect
type: concept
---

# Sovereignty: the tenant's data is the tenant's

Gentian OS is open source, and what that gives a tenant is this: at any
moment a tenant can take all of their data with them, and at any moment they
can have all of it destroyed, without asking the platform's permission and
without depending on anyone's tooling to read what they took.

Anyone may build on Gentian OS, and a vendor may sell add-ons and services on
top of it. An add-on is not part of this repository: it comes from a catalog
under its own license and brings its own packaging (§5). Nothing in this
document depends on one.

## 1. What a tenant owns

A tenant is one organization's workspace. Its data is:

- the data of its apps: databases, files on volumes, buckets, each app's own
  secrets (the values the platform generated for it and its data is written
  with) and each app's authorization store;
- the data of apps it uninstalled with their data kept;
- its people, groups and roles: the realm;
- its mailboxes, and the archived mailboxes of people who were removed, where
  the cluster runs its own mail server;
- the desktop's database: preferences and notices;
- the access rights that follow from nothing else: a right granted beyond the
  defaults, a default withdrawn.

Not the tenant's, and so in no bundle: caches; the credentials the platform
mints for an app to reach its database, bucket, cache or sign-in client,
which an import mints anew; and people's passwords, which a restored person
sets again. [data-lifecycle.md](data-lifecycle.md) §2 is the inventory, kind
by kind, with what each act does to each.

## 2. The commitments

Each is stated in this repository; the place is named beside it. They cover
the whole open platform: the desktop and the administration console
(gentian-ui) and the catalog of open-source app profiles (gentian-apps) with
this repository. An app in that catalog keeps the license of the project it
comes from.

1. **The OS is open source, all of it.** The core is MPL-2.0; the resource
   types, the CRDs generated from them and the bundle format are Apache-2.0.
   No code the project writes for the OS is under a source-available or
   network-copyleft license ([LICENSING.md](../../LICENSING.md)).
2. **Leaving is part of the core.** Export, import, restore, retire and purge
   are core functions under the core's license, so that a tenant's mobility
   never depends on an add-on ([LICENSING.md](../../LICENSING.md)).
3. **The bundle format belongs to the OS and there is one of it.** It is
   defined in `api/bundle` (§3); anything that reads or writes a tenant's
   data in bulk uses it, and it is readable with standard tools and no
   running service (§3.2).
4. **A bundle is always encrypted, and the tenant can have it encrypted to a
   key of their own**, which the provider cannot read (§3.4).
5. **An import does not depend on the cluster the bundle came from**, nor on
   its provider's recovery kit ([data-lifecycle.md](data-lifecycle.md) §4).
6. **The OS builds, installs and runs with no add-on and no store present.**
   Nothing in it — the APIs, the formats, leaving — depends on one. A
   default install may propose an add-on; it never requires one (§5).
7. **The cluster gates nothing on a license.** It holds no entitlement, and an
   install is asked of the person who makes it and of nothing else (AD-3,
   AD-14, [decisions/index.md](../decisions/index.md); [store-contract.md](store-contract.md)
   §2).

Two things are not part of the commitment:

- **A store.** The App Store app on a cluster shows the data of a store run
  outside it. A cluster with no store installs apps by command, and a
  cluster that has turned its usage statistics report off has no App Store
  app ([store-contract.md](store-contract.md) §6, §9); nothing else changes.
- **Software the OS installs that is not the project's.** The system
  services and the catalog's apps keep their own licenses
  ([LICENSING.md](../../LICENSING.md)).

## 3. The one object: the bundle

A **bundle** is a self-contained, encrypted snapshot of one tenant. It is the
unit everything else moves: an export produces one, a scheduled export is an
export on a timer, an import consumes one, a restore reads one, and deletion
is the promise that nothing the bundle would have contained survives. Its
format is normative here, in Gentian OS, in `api/bundle`
([bundle.go](../../api/bundle/bundle.go)), and anything built on top of the
OS produces or consumes exactly this format. There is no second format.

### 3.1 Contents

The manifest states its schema (`schemaVersion`); the schema written is 5.

| Part | What |
|---|---|
| `bundle-info.json`, in the clear | Tenant, schema, export, time, encryption mode, the recipients, and the literal command that decrypts the rest |
| `manifest.json`, encrypted and written last | The index: the tenant and the full `TenantSpec` as committed in git; the platform's version line; the domain, the cluster and the cluster's id the bundle was taken on; per app its profile, chart version, pinned digest, releases and database engine, whether it was uninstalled with its data kept, the window in which its writes were paused, and one entry per artifact naming what was captured and where it is; the realm capture; the desktop's database; the mailboxes and the archived ones; the rights that follow from nothing else; and what the tenant had that the bundle does not hold |
| `manifest.json.sig`, in the clear | The cluster's ed25519 signature over the manifest. A restore brings granted rights back only from a bundle the cluster it runs on signed; everything else is restored whichever cluster took the bundle |
| `identity` | The realm: groups, roles, clients, every person, every membership. No credentials |
| Per app: `postgres`, `postgresOwned`, `mariadb`, `mariadbOwned`, `s3`, `volume` | The app's stores as its profile declares them: a dump of each database and an archive of the databases the app made for itself, a tar of each bucket and of each volume |
| Per app: `secrets` | The app's own secrets, one JSON document, encrypted by the operator itself so that no value passes through a Job |
| Per app: `authorization` | The app's authorization store: its model and every tuple |
| `mailboxes` | The mail domain's mailboxes as Maildir++ trees, the archived ones beside them |
| The desktop's database | A PostgreSQL dump, filed under the tenant rather than under an app |

Every artifact is an age file. A reader refuses a schema newer than its own
and reads every older one back to schema 1, putting back what the bundle
holds and saying what it lacks.

### 3.2 Self-contained

A bundle is readable with [age](https://age-encryption.org) and a tar tool;
`bundle-info.json` says how. Nothing in it points at an address that has to
be alive. The one dependency a bundle keeps is on the **catalog entries** it
names: an import needs the profiles at the pinned digests, from a catalog
source the importing cluster declares. That dependency is stated in the
manifest rather than hidden, and an import that cannot have a definition
refuses and names it ([data-lifecycle.md](data-lifecycle.md) §4).

### 3.3 Two containers, one bundle

The set of artifacts is the bundle. It is held as

- a **prefix** in object storage: streamable, one object per artifact,
  resumable. This is how an export writes it and how it is stored;
- a **single file** `<tenant>-<export>.gentian`: a tar of the prefix, the
  artifacts still encrypted. This is what a download produces and what an
  import upload accepts; an upload is laid out as a prefix again before
  anything reads it.

Every flow takes "a bundle" without caring which
([operations.md](operations.md) §9.6).

### 3.4 Encrypted, always, to a key the tenant chooses

| Key | Who can open it |
|---|---|
| **platform key** — the cluster's backup recipients | the tenant's administrator, and the platform through its escrowed identity |
| **existing recipient** — an age public key whose private half the tenant holds | the tenant alone |
| **passphrase** | whoever holds the phrase |

The console offers the three, starts on the platform key and makes no key
itself; the platform keeps no passphrase. How an app's own secrets are
protected inside a bundle is [security/design.md](../security/design.md) §6.4.

## 4. The flows

| Flow | Who | What happens |
|---|---|---|
| **Export** | the tenant's administrator | Every store the inventory lists is captured, one app paused at a time, into a bundle in the cluster's own bucket; the bundle is downloaded as one `.gentian` file through the director |
| **Scheduled export** | declared in the tenant's backup policy | The same export, on a timer, to the destination the policy names |
| **Import** | the cluster's administrator | The director reads `manifest.json`, declares the tenant from the bundle's `TenantSpec` with the same commit a create makes, waits for the platform to make the tenant and its apps empty, and restores into it — on this cluster or another |
| **Restore** | an administrator | A bundle is loaded into the tenant it was taken of: pause, load, run the profile's restore commands, resume |
| **Retire** | the cluster's administrator | The tenant's manifest is removed; its data stays (`deletionPolicy: Retain`, the default) |
| **Purge** | the cluster's administrator | `deletionPolicy: Delete` is set, the cluster holds it, the manifest is removed: every inventoried store is destroyed, the backup bucket too unless `keepBundles` spares it |

Import is Create plus Restore; a purge destroys what an export captures. One
inventory, one bundle, one manifest path
([data-lifecycle.md](data-lifecycle.md) §2). Anything further is a
composition of these and not a flow of the OS: a recovery is an Import or a
Restore of the newest bundle, and a migration from or to another product is a
converter on either side of Import and Export. Whoever builds such a thing
calls the OS through the director like any other client (§5).

## 5. Add-ons, and where the OS ends

Everything a tenant needs to **own** their data is in Gentian OS, and every
commitment of §2 holds with nothing but Gentian OS installed: exports on
demand and on a schedule, the bundle, import, restore, retire and purge.

What goes beyond that — screens over schedules and destinations, an evidence
store for the audit log, recovery on a click, drills, moving data in from
other products — can be built on top by anyone, and a vendor may sell it.
Such an add-on is delivered as catalog entries under its own license, and it
brings its own packaging as every add-on does: its profiles, its controller
if it has one, and its own AppProject. What its license asks of whoever
installs it is stated with the add-on, not here, and the cluster gates no
install on it.

An add-on uses the OS the way every other client does:

- It calls the director with the signed-in person's token. It never writes
  to git itself — the director is the only writer (AD-2) — and it reads a
  tenant's data only through a bundle the person was allowed to open.
- It produces and consumes the bundle of §3 and no other format.

### 5.1 What the administration console says about add-ons

Where an OS flow has a continuation in an add-on, the administration console
says so: with the add-on installed it links to it, and without it the same
place says what the add-on would give the tenant. The OS path is always there
and always works: the Export screen takes an export and hands out the bundle
whether or not an add-on is installed.

### 5.2 What a default install proposes

A default install proposes a store and one add-on. Both arrive through
surfaces the OS already has, and the OS installs and runs without them.

- **The store.** The installer's scaffold of the Cluster claim sets
  `catalogue.storeUrl` and lists one catalog source. The App Store app shows
  the store's data and installs through the director (AD-3); nothing the
  store says decides what a tenant may install
  ([store-contract.md](store-contract.md)). The addresses are the installer's
  defaults and can be changed or left out
  ([reference/install.md](../reference/install.md)).
- **One add-on's profile.** The installer places the profile of the
  Operations Console, an add-on from the store's catalog, among the cluster's
  profiles, held to a digest like any install (AD-14,
  `GENTIAN_DEFAULT_PROFILES`; [store-contract.md](store-contract.md) §9). It
  is not part of this repository and carries its own license.
- **The switch.** `./install.sh --disable-api-extensions`
  (`GENTIAN_DISABLE_API_EXTENSIONS=1`) places no default profile. Nothing
  else changes: export, import, purge and the bundle are the OS's and need no
  extension.
