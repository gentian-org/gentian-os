# Gentian OS Commands

This document lists key cluster-admin commands for Gentian OS operations.

For environment mapping, promotion flows, and GitOps layout, see
[deployment.md](deployment.md).

For tenant-admin app lifecycle commands, see:

- ../../gentian-deployments/README.md

## CLI entry points

The Gentian CLI is a kubectl plugin (`kubectl-gentian`), installed with
`make install-plugin` into `~/.local/bin`, with a shorthand symlink `gtnctl`.
It is a client of the director, like the admin console: sign in once with
`kubectl gentian login` (a code confirmed in the browser), and every command
asks the director as you, which checks, commits and records it. The commands
about a person -- `tenants activate-admin`, and the look-up behind
`platform-roles` -- ask the registrar, which checks the same way and reads or
acts at Keycloak. It reaches the director and the
registrar of the current kubectl context, so it needs no configuration.

People are invited, changed and removed in the Admin Console, *Members*;
there is no command for it. Removing a person who has a mailbox on the
cluster's own mail server asks there whether the mailbox is archived or
deleted ([mail.md §5c](design/mail.md)).

```bash
gtnctl tenants list    # same as kubectl gentian tenants list
```

Use `gtnctl` at the terminal if you prefer a shorter command. All examples below
use the canonical `kubectl gentian` form for consistency in docs and scripts.
`kubectl gentian --help` prints every command; `kubectl gentian whoami`,
`logout` and `version` complete the list below.

The examples use a tenant named `demo`. On a single-tenancy cluster
(`tenancyMode: single`) the one tenant for users is named `user`: read
`--tenant user` for `--tenant demo`.

## 1. Install the OS (Cluster Admin)

Run the installer from a checkout of the OS repository:

```bash
./install.sh
```

It installs the kernel services, Argo CD, OpenBao, the operator and the
director. [GETTING-STARTED.md](../GETTING-STARTED.md) is the walkthrough and
[install-reference.md](install-reference.md) the reference.

## 2. Verify Core Health

```bash
./install.sh --status
kubectl get applications -n kernel-gitops
kubectl get pods -n kernel-control
kubectl get tenants
```

## 3. Provision a Tenant

> **Bringing a tenant on briefly disrupts the shared kernel.** Provisioning adds
> listeners to the kernel Gateway, which reloads Envoy across every host it
> serves; expect transient `404`s on kernel hosts while it happens. Treat
> creating and retiring tenants as maintenance-window operations on a cluster
> with live users.

A tenant is created through the director — the admin console's **Tenants**
tab, or the CLI:

```bash
kubectl gentian tenants create demo --display-name "Demo AG"   # --no-mfa: no second factor for its admin
kubectl gentian tenants list
kubectl gentian tenants show demo
kubectl get tenant demo -w
```

One thing about a tenant is the cluster's administrator's alone to decide,
and it is off unless switched on: whether the tenant's own administrators may
add catalogs to install from. It is stated when the tenant is created or
changed afterwards, and `tenants list` and `tenants show` say what holds. Who
approves what the tenant publishes to the internet and its egress is a role
holder on the Cluster claim (`perimeterApprover`, see *Cluster roles held by
name* below):

```bash
kubectl gentian tenants create demo --admins-add-catalogues
kubectl gentian tenants set demo --admins-add-catalogues=true
```

A second switch is chosen when the tenant is made and not changed afterwards:
whether anybody may make an account in its realm themselves
(`--self-sign-up`, `spec.signUp.enabled`). The command says what that opens
before it asks the director: a registration page at the cluster's identity
provider, an e-mail confirmation and a proof-of-work check, and accounts that
are in none of the tenant's groups and so have nothing of the platform's. For
a tenant whose apps serve the public, not for a workspace
([iam.md §1.1b](design/iam.md)):

```bash
kubectl gentian tenants create customers --display-name "Customers" --self-sign-up
```

The country the tenant is in (`--country`, ISO 3166-1 alpha-2, `spec.country`)
is optional. It is changed afterwards by whoever may set the tenant's policy,
its own administrators too (`PUT /v1/tenants/<t>/country`, `can_set_policy`);
`""` removes it. A component whose profile names
`valueMapping.platform.countryKey` is told it: the App Store app asks a store
for the support offered there.

```bash
kubectl gentian tenants create acme --country CH
kubectl gentian tenants set acme --country DE
kubectl gentian tenants set acme --country ""
```

Who the tenant is — legal name, currency, time zone, fiscal year start,
address, website, email, phone, VAT and register numbers (`spec.organization`)
and its display name — is filled in by its administrators under Tenant
Settings in the admin console (`GET`/`PUT /v1/tenants/<t>/organization`,
`can_set_policy` to write). A currency left empty is the country's. The answer
names what is still `missing` of legal name, country, currency, time zone and
languages; the App Store warns until nothing is. A component whose profile
names `valueMapping.platform.organizationKey` is told all of it as one object,
`timeZoneKey` the zone alone (unset while there is none); an app sets
itself up from them.

A tenant's partners — realms of other clusters its realm links its people's
accounts with (`spec.partners`, AD-17, [identity-brokering.md](identity-brokering.md)).
`--issue`: our people sign in there and our apps call it for them; `--accept`:
their people arrive here, linked (`link`), made (`create`) or as an
organization. Written by the tenant's administrators (`can_administer`),
within the Cluster claim's `spec.partners.allowedIssuers`:

```bash
kubectl gentian tenants partners customers      # list them
kubectl gentian tenants partners acme add shop --issuer https://id.shop.example/auth/realms/customers --issue
kubectl gentian tenants partners customers add acme --issuer https://id.acme.example/auth/realms/acme \
  --accept organization --domain acme.example --display-name "ACME AG"
kubectl gentian tenants partners customers remove acme
```

The catalog switch is a line in the tenant's manifest in git
(`spec.catalogue.delegated`), committed by the director as you
(`PUT`/`DELETE /v1/clusters/<c>/tenants/<t>/catalogue-delegation`,
`can_configure` on the cluster). A tenant's own administrator cannot set it.
An imported tenant starts with it off, whatever held where its bundle came
from.

The director commits `clusters/<cluster>/tenants/demo/` as you; Argo CD syncs
it and the operator provisions the realm, namespaces, database and desktop.

On a single-tenancy cluster (`tenancyMode: single`) there is exactly one tenant
for users, named `user`, which the install creates after the handover (step
`E-04`); `tenants create` with any other name is refused with a message that
names the mode, and `user` itself is refused only because it exists. Nothing
in this section has to be run there. `tenants import` follows the same rule.

A tenant's administrator — the **tenant admin**, `admin@<tenant domain>`; on a
single-tenancy cluster the **user admin**, `user-admin@<kernel-domain>` — has
**no password**. Hand the
account over with a single-use, expiring activation link — mailed to a recovery
address, or printed once without one:

```bash
kubectl gentian tenants activate-admin demo --recovery-email owner@demo.example
kubectl gentian tenants activate-admin demo                       # prints the link
```

It waits out provisioning, so it can run straight after `create`. Running it
again issues a new link, which is also how a locked-out administrator gets back
in. The recovery address is kept on the account in Keycloak, never in git.

Check reconciliation:

```bash
kubectl get tenant demo -o yaml
kubectl describe tenant demo
```

### A domain of its own

A tenant is served under the cluster's domain unless it is bound to a domain
of its own. Binding one moves everything: its desktop to `desktop.<domain>`,
its admin console to `admin.<domain>`, every app to `<label>.<domain>`, and
with them its mail and the names its people sign in with.

```bash
kubectl gentian tenants domain demo                 # the domain git declares for it, if any
kubectl gentian tenants domain demo demo.example    # bind it
kubectl gentian tenants domain demo --remove        # back to its default address
```

The usage is `tenants domain <name> [<domain> | --remove] [--yes]`.
Binding and removing are for whoever may configure the cluster. Each prints
what moves and asks for a word typed out -- the domain to bind, the tenant's
name to remove -- which `--yes` skips in a script. The director refuses a
name that is not a hostname, a domain on or under the cluster's own, and one
another tenant is bound to; its refusal is printed as it worded it.

Binding is for a cluster with many tenants. On a single-tenancy cluster the
one tenant for users is on the cluster's own addresses, and the director
refuses to bind any tenant there; it refuses the platform tenant on every
cluster. Nothing is changed by a refused bind. `--remove` is never refused
for either reason.

Two things have to be true first, and neither the director nor the command
checks them:

- **DNS:** `*.<domain>` resolves to this cluster. The platform publishes
  records in the DNS zone that holds the cluster's own domain and in the
  zones the cluster's settings list under `certificates.dnsZones`
  ([§6c](#6c-cluster-settings)); list the domain's zone there, or make the
  wildcard record yourself.
- **Certificate:** the cluster's DNS-01 issuer can write `_acme-challenge`
  records in the domain's zone, that is, its DNS token covers that zone. The
  tenant gets one wildcard certificate, `*.<domain>`
  ([design/multi-tenancy.md](design/multi-tenancy.md)).

The operator reports the outcome on the Tenant:

```bash
kubectl get tenant demo -o jsonpath='{.status.conditions[?(@.type=="DomainBound")].message}'
```

What the operator provisions for a tenant's mail is `spec.mail.mode`
([mail.md §2](design/mail.md)), the cluster administrator's to set as the
domain is (`PUT /v1/clusters/<c>/tenants/<t>/mail-mode`, `can_configure`):

```bash
kubectl gentian tenants mail-mode demo              # what git declares, or the cluster's default
kubectl gentian tenants mail-mode demo disabled     # one commit: selfhosted, external, transport-only or disabled
```

Decide it before binding a domain whose mail lives elsewhere: on a cluster
with its own mail the operator otherwise publishes MX, SPF, DKIM and DMARC
records for that domain, over the ones that are there. `disabled` publishes
nothing.

The user tenant of a single-tenancy cluster needs none of this: it is served
on the cluster's own domain (`desktop.<kernel-domain>`, `admin.<kernel-domain>`,
`<label>.<kernel-domain>`), and binding it to another domain is refused, as
said above: it would move the tenant off the cluster's addresses, and it could
then hold no website at the cluster's main address.

## 4. Uninstall a Tenant

```bash
kubectl gentian tenants retire demo           # keeps its data
kubectl gentian tenants retire demo --purge   # deletes its data
kubectl gentian tenants retire demo --purge --keep-bundles   # deletes its data, keeps its backup bundles
```

or **Retire** in the console, which asks the same question. Both ask for the
tenant's name typed out (`--yes` skips that in a script).

Retire removes the tenant's directory from git; Argo CD prunes the Tenant and
the operator tears it down under the manifest's `deletionPolicy`, `Retain`
unless edited, so the realm, databases and files stay. A retire of a tenant
git does not describe asks the cluster whether it still runs it: an Argo CD
sync that finished after the directory was removed can re-create the Tenant
with no Application owning it, and such an orphan is deleted by the operator
on that retire (and nothing else is — a Tenant made without Argo CD, or one
whose Application exists, is refused with the reason). Purge first commits
`deletionPolicy: Delete` (and the `gentianos.io/purge-requested` annotation),
waits until the live Tenant carries it, then removes the directory; the tenant
is listed as purging until then. A purge deletes the tenant's mailboxes
too, where the cluster runs its own mail. Not `kubectl delete tenant`: git is what the
cluster reconciles towards, so deleting the object just brings it back. The
platform tenant can be neither retired nor purged.

## 5. Tenant App Store

Tenant admins install apps from the **App Store app** or the CLI. On a
cluster with no store the CLI is the only way: the cluster shows no catalog
of its own.

### The App Store app

A person who may install apps in a tenant has an **App Store** tile whenever
the Cluster claim names a store (`catalogue.storeUrl`) that is not the App
Store app's own address on a cluster, and the cluster's usage statistics report is on
([design/operations.md §6.2](design/operations.md)). The
tile opens the App Store app, which runs on the cluster. It shows the data of
the store outside the cluster — apps, descriptions, reviews, versions, prices
— and installs through the director and the custodian as the person signed
in. The store itself never calls the cluster. See
[design/store-contract.md](design/store-contract.md).

Apps that are installed are administered in the admin console's **Apps** tab:
state, access, integrations, privileges, uninstall and purge.

### CLI

```bash
kubectl gentian apps list --tenant demo               # what the tenant has installed
kubectl gentian apps list --tenant demo --available   # what the tenant's catalogues offer
kubectl gentian apps install xwiki-ce --tenant demo   # the build the catalogue lists, pinned by digest
kubectl gentian apps uninstall xwiki-ce --tenant demo
```

### Catalogs

A catalog is an https address apps are installed from, or, for profiles that
must not be public, an OCI artifact read with the credential of a declared
repository. A directory of the cluster's own deployments repository is still
read, and deprecated. The cluster's administrator adds one for every tenant or
for one tenant; a tenant's own administrator adds an address or an artifact for
their tenant where the cluster's administrator delegated that, and never a
directory.

```bash
kubectl gentian catalogues list                        # everything the cluster declares
kubectl gentian catalogues list --tenant demo          # what demo installs from
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue                 # for every tenant
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant demo   # for demo only
kubectl gentian catalogues add acme --oci ghcr.io/acme/catalogues/acme:1.0.0 --repository acme --tenant demo   # an artifact, read with demo's repository acme
kubectl gentian catalogues add ours --oci ghcr.io/ours/catalogue:1 --repository ours   # for every tenant, read with the cluster's repository ours
kubectl gentian catalogues remove acme                 # the cluster's
kubectl gentian catalogues remove acme --tenant demo
kubectl gentian catalogues rename acme acme-apps       # the same catalogue under another name; what was installed from it follows
kubectl gentian catalogues rename acme acme-apps --tenant demo
kubectl gentian tenants delegate-catalogues demo on    # demo's administrators may add their own
kubectl gentian tenants delegate-catalogues demo off   # the same switch as: tenants set demo --admins-add-catalogues=true|false
kubectl gentian catalogues residue                     # what the catalogue left on the cluster
kubectl gentian catalogues residue remove OIDCPackCatalog/xwiki-ce-oidc   # one object, name typed again
```

```
$ kubectl gentian catalogues list
CATALOGUE  FOR           ADDED BY     ADDRESS
gentian    every tenant  the cluster  https://gentian-org.github.io/gentian-apps
acme       tenant demo   the tenant   https://acme.github.io/acme-catalogue
private    tenant demo   the tenant   oci: ghcr.io/acme/catalogues/acme:1.0.0 (repository acme)
Tenants whose administrators may add catalogues of their own: demo.
```

With `--tenant`, `add` and `remove` are done as the cluster's administrator
when the person is one, and otherwise as the tenant's administrator: that is
refused unless the tenant is delegated, and removes only what the tenant
added. Without `--tenant`, `add` and `remove` change the Cluster claim
(`spec.catalogue.sources`), which is also where a catalog for the whole
cluster can be written before the install, as a `name` and a `url`, or a
`name` and an `oci` (`repository`, `artifact`). An address
must be a public https one; anything else is refused. An `--oci` artifact is
`<registry host>/<repository>:<tag>` or `@sha256:<digest>`, on a public
registry, inside the address of the `--repository` named, which is declared for
the tenant with `--tenant` and for the cluster without
([custom-catalogues.md §4.4](custom-catalogues.md)). A catalog is never a
directory of the deployments repository, which holds the cluster's state only;
`--path` is refused.
[custom-catalogues.md](custom-catalogues.md) says how to build and publish a
catalog.

`rename` is the cluster administrator's, with `--tenant` as without, and is one
commit to everything that carries the name: the source entry, the `catalogue:`
of every app entry and add-on pin installed from it (every tenant's, for a
catalog of the cluster), and the origin of every profile materialized from
it. No build moves, since no digest changes; the next install from the new
name is the same profile again. Refused when the new name is taken — by the
cluster, by that tenant, or for a catalog of the cluster by any tenant — and
when the old one is not a catalog of that scope. Removing a catalog and
adding it under another name is not the same thing: the profiles' origin would
keep the old name, and an install from the new one would be refused as another
origin's.

Nothing a profile's bundle brought is removed automatically: a piece a newer
build drops stays, and so does a profile after its last uninstall. `residue`
lists what is left, and `residue remove` deletes one object of the list —
never anything a bundle on the cluster still brings.

```
$ kubectl gentian catalogues residue
KIND/NAME                               NAMESPACE            PROFILE     WHY             CREATED     IN EFFECT
ComponentProfile/acme-notes             -                    acme-notes  unused profile  2026-09-30  -
Composition/app-odoo                    -                    -           unowned         2026-07-02  -
ConfigMap/element-ce.portal-bridge-sso  kernel-provisioning  element-ce  dropped         2026-10-07  -
OIDCPackCatalog/gentian-element-oidc    -                    element-ce  dropped         2026-07-02  contested
OIDCPackCatalog/xwiki-ce-oidc           -                    xwiki-ce    orphaned        2026-08-14  no

  ComponentProfile/acme-notes: unused profile: no tenant has it installed or switched on as an add-on, and no tenant retains data for it; its bundle brings Composition app-acme-notes, which stay
  Composition/app-odoo: not owned by any bundle: it names no profile, and no bundle on this cluster brings it
  ConfigMap/element-ce.portal-bridge-sso: it carries the label gentianos.io/profile-name: element-ce, and the bundle now materialised for element-ce does not bring it
  OIDCPackCatalog/gentian-element-oidc: it carries the label gentianos.io/profile-name: element-ce, and the bundle now materialised for element-ce does not bring it; another catalog carries the same label, so these clients are refused until one is removed: element; read by the Composition of element-ce
  OIDCPackCatalog/xwiki-ce-oidc: it carries the label gentianos.io/profile-name: xwiki-ce, and no profile xwiki-ce is on this cluster

Nothing here is removed automatically. To remove one:
  kubectl gentian catalogues residue remove <kind>/<name>

$ kubectl gentian catalogues residue remove OIDCPackCatalog/xwiki-ce-oidc
Removing the OIDCPackCatalog xwiki-ce-oidc deletes it from the cluster. Nothing puts it back but
installing a build that brings it.
Type xwiki-ce-oidc to confirm: xwiki-ce-oidc
Deleted the OIDCPackCatalog xwiki-ce-oidc.
  the OIDCPackCatalog xwiki-ce-oidc was deleted, asked for by ada@example.com
```

`IN EFFECT` says whether a leftover OIDC pack catalog is still read when a
client is configured. The list is for whoever may audit the cluster; removing
is for whoever may configure it. `--yes` takes the place of typing the name,
and `--namespace` names the namespace of a ConfigMap or a customization
record, which is the catalog's and no other. An unused profile is removed
from the deployments repository first and deleted from the cluster once Argo
CD has synced that; [custom-catalogues.md](custom-catalogues.md) §6 has the
classes and the rules.

Guides:

- [custom-catalogues.md](custom-catalogues.md) — publish your own apps to a cluster or a tenant
- [gentian-apps/docs/custom-app-guide.md](../../gentian-apps/docs/custom-app-guide.md) — build new apps
- [gentian-apps/docs/app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) — publish upstream charts

Show all available `kubectl gentian` subcommands:

```bash
kubectl gentian --help
```

### Private registries

A catalog is public; the charts and images it names need not be. A tenant
declares the registry they are in, and the token is set afterwards in the
Admin Console of the tenant, under Credentials, as the name the command
prints. A chart whose address lies inside the declared one is pulled with it.

```bash
kubectl gentian repositories declare acme --tenant demo --url oci://ghcr.io/acme   # then: Credentials, repository-acme
kubectl gentian repositories remove acme --tenant demo
```

## 6. Install and Uninstall Apps

Apps are installed by the director adding them to the tenant's manifest in
git, as the person who asked; the operator reconciles them. The tenant's
administrator usually does this from the App Store app; the CLI does the same:

```bash
kubectl gentian apps list --tenant demo                              # what the tenant has installed
kubectl gentian apps list --tenant demo --available                  # what the tenant's catalogues offer
kubectl gentian apps install xwiki-ce --tenant demo                  # the build the catalogue lists
kubectl gentian apps install xwiki-ce --tenant demo --from in-house  # when several catalogues serve the name
kubectl gentian apps install xwiki-ce --tenant demo --digest sha256:<64 hex>   # a build you name yourself
kubectl gentian apps install xwiki-ce --tenant demo --for-everyone   # and grants it to every member
kubectl gentian apps uninstall xwiki-ce --tenant demo            # removes the app, keeps its data
kubectl gentian apps uninstall xwiki-ce --tenant demo --purge    # removes the app, then destroys its data
kubectl gentian apps outdated [--tenant demo]                     # which installs are behind their catalogue, and why they stay
kubectl gentian apps move-outdated [--tenant demo] [--app xwiki-ce]   # move them to the newest build, retry rolled-back moves (cluster administrator)
kubectl gentian apps update-mode xwiki-ce --tenant demo --mode tenant # who moves this app: latest | manual | tenant | default
```

**The command line installs a pinned build.** An app is installed from one
of the catalogs the tenant sees — the cluster's and the tenant's own
(`kubectl gentian catalogues list --tenant demo`) — and `install` sends the
director the entry's coordinate and digest. Nothing is on a cluster ahead of
an install: the director fetches that one profile, checks it against the
digest and commits it.

```
$ kubectl gentian apps list --tenant demo --available
SOURCE    APP                VERSION  EDITION  DIGEST               INSTALLED
gentian   nextcloud-base-ce  31.0.4   ce       sha256:2f1c0d9a77b3  yes
gentian   xwiki-ce           16.4.0   ce       sha256:9b41e6c05d12  -
in-house  xwiki-ce           16.5.0   ie       sha256:c07a1be4403f  -

$ kubectl gentian apps install xwiki-ce --tenant demo --from in-house
Pinned in-house/xwiki-ce  version 16.5.0  sha256:c07a1be4403f
Installing xwiki-ce into demo: committed (4e1f9a2c); the platform installs it.
```

`INSTALLED` says `yes` for the build the tenant has, `other build` when the
tenant has the app at another digest, and `unpinned` when it was installed
with none. The listing is the director's reading of each source's index, the
`ce`, `me` and `ie` entries only.

| | |
|---|---|
| One source serves the name | it is used |
| Several do | `--from <source>` says which; without it nothing is installed |
| None does | nothing is installed, and the sources are listed. With both `--from` and `--digest` an entry the listing does not show can still be asked for |
| `--digest` | the build to install instead of the one the listing states. The director fetches the bundle from the source and installs nothing unless it hashes to the digest sent |
| The tenant has no catalog | nothing can be installed by command; the cluster's administrator adds one (`kubectl gentian catalogues add`) |
| The name is taken by a profile from another catalog | nothing is installed, and the refusal says who has to rename ([custom-catalogues.md](custom-catalogues.md) §7) |
| An entry of the profile would answer on a name the platform keeps | nothing is installed, and the refusal lists the names. In every tenant: `desktop`, `admin`, `store`, `console`, `platform`, `id`, `auth`, `login`, `signin`, `sign-in`, `sso`, `account`, `accounts`. For the user tenant of a single-tenancy cluster also `argocd`, `corp`, `headlamp`, `imap`, `llm`, `mail`, `mail-egress`, `www` |

The director verifies the bundle against the digest before it commits, and
the operator verifies it again at rollout
([design/store-contract.md](design/store-contract.md) §3, §4). Installing an
app the tenant already has at another build moves it: who may is the app's
update mode's to say, and the move exports first, verifies after and rolls
back by itself ([custom-catalogues.md §6](custom-catalogues.md)). Installing
the build the app is already pinned to changes nothing, unless the move to it
was rolled back or did not start: then the director asks for the move once
more, under the same rights as a move, and says why the last one ended
(`retrying the move that was rolled back: …`). `apps outdated` prints the
command for every such app, and `apps move-outdated` retries them all. The
platform never retries a move by itself. The
`PLATFORM` column of `--available` is the build's floor, `spec.requires.platform`;
one marked `(not met)` needs a newer platform than this cluster runs.

Uninstalling and purging are two different acts, and the CLI says which one it
did.

**Uninstalling** removes the app from the tenant: its workloads and its
sign-in client go, and everything it stored stays — its database, its object
storage, its cache user, its files, its stored credentials, and the access
group with everybody who is in it. The app is then *retained*: uninstalled,
data retained. Installing it again in the same tenant finds all of it.
A backup taken after the uninstall goes on holding that data, as an
uninstalled app's; a backup asked for some apps only names the ones it left
out (`status.notIncluded`).

**Purging** destroys what uninstalling kept, and cannot be undone. It is
refused while the app is still the tenant's. `--purge` therefore does both, in
order: it uninstalls the app (or finds it already uninstalled), waits until
the app is gone from the cluster, and then purges it. "Gone" is the platform's
own answer — the tenant no longer names the app, its Component is gone, and
Helm has finished uninstalling its release. The wait is bounded by
`GENTIAN_UNINSTALL_WAIT` seconds (default 900); when it runs out the command
says so, nothing has been purged, and the same command can be run again.
What every act does with every kind of data is one table in
[design/data-lifecycle.md](design/data-lifecycle.md).

A purge is refused, with nothing destroyed, when the app's profile is not on
the cluster — what the app owns cannot be determined without it; installing
the app again from a catalog source puts the profile back — and when the
vault, the identity provider or the database it will need does not answer.

A release that is already out of the tenant and only waits for its provider
to uninstall it -- a provider still waiting out an upgrade of a broken release
gets there minutes later -- does not hold the purge back: the purge destroys
the data, waits for the release to go, deletes the app's volume claims once
more (a rollback or a kept claim puts one back), and answers only when nothing
of the app is left.

The purge itself is one request that is answered when it is over. If a step
fails, the command ends with which step failed, what had already been
destroyed and what was not attempted; nothing is rolled back, every step is
safe to repeat, and running the command again continues with what is left.
What each act keeps and destroys, kind by kind, is in
[design/store-contract.md](design/store-contract.md) §8.

`--for-everyone` writes `defaultGrant: true` on the app's entry in the same
commit. Every member of the tenant then has the app by default: the people who
are members when the app's group first exists are added to it once, and
somebody invited later has it pre-selected. It needs the right to grant in the
tenant as well as to install. Without the flag, access is given per person
([design/store-contract.md](design/store-contract.md) §3).

Inspect app reconciliation:

```bash
kubectl get component -A | grep xwiki     # Ready when it runs
kubectl get pods -n tenant-demo | grep xwiki
```

A cluster's stage (`dev`, `staging`, `prod`) is fixed at bootstrap via
`GENTIAN_DEPLOYMENTS_STAGE` in `install.env` (see
[deployment.md](deployment.md) §1) — `apps`/`tenants` commands don't take a
`--env`/`--stage` flag; they always target the one cluster selected by
`GENTIAN_DEPLOYMENTS_CLUSTER_ID`.

### Groups, and people of another tenant signing in

A tenant's own groups are declared in git, made by the platform and filled
in the Admin Console. People of another tenant of the cluster may sign in to
a tenant with their account there, given some of its groups (`members`, or
its own); see [identity-brokering.md](identity-brokering.md).

```bash
kubectl gentian tenants groups customers website-editors store-editors
kubectl gentian tenants sign-in customers --from team --groups website-editors,members
kubectl gentian tenants sign-in customers                 # what it declares
```

### Credentials a person supplies

A registry's token, an app's API keys: what a profile asks a person for is
set at the custodian, which checks each value before it stores it. A value is
read from a file or from standard input, never from the command line, and a
file's trailing newline is dropped.

```bash
kubectl gentian credentials list --tenant customers    # each requirement, its fields, and whether it is set
kubectl gentian credentials set app-customers-aluvian-website-stripe \
  --field secret_key=@stripe-restricted.txt --field publishable_key=@stripe-publishable.txt
openssl rand -hex 32 | kubectl gentian credentials set app-customers-aluvian-website-intake --field token=-
```

### What an app asks for beyond the default

An app whose profile asks for more than a tenant's apps get by default -- a
way out to an address (`egress`), a relaxed pod security (`podSecurity`),
cluster roles (`clusterRoles`) -- waits until a person answers each request;
its component reads `waiting for approval of <kind>/<name>`. The answer is a
commit through the director, with you as the approver and your reason in it.
An `egress` request is the tenant's perimeter approver's to answer (and the
cluster's administrator's, in a tenant the cluster operates); the other two
are the cluster's security officer's. The Admin Console's Apps page offers
the same as a button, to whoever holds the right the request needs.

```bash
kubectl gentian apps approve aluvian-commerce --tenant team     # what it waits for
kubectl gentian apps approve aluvian-commerce egress/intake --tenant team \
  --reason "reads the website's and the store's order requests over https"
```

`--expires <RFC 3339>` bounds the approval. Removing the entry from the
tenant's `privileges.yaml` in git withdraws it, and the app waits again.

A `podSecurity` or `clusterRoles` request is the security officer's to
answer, so the App Store's install step asks them for it (the director's
`POST /v1/tenants/{t}/apps/{p}/actions/request-privilege`, recorded on the
app's Component) and the platform console's Approvals page lists what was
asked. The same list, from the command line, carries every tenant's
pending privileges, each with who answers it (APPROVER: `security-officer`,
or `perimeter-approver` for egress):

```bash
kubectl gentian approvals list     # every tenant's waiting privileges: who answers, who asked, when, why, and what was declined
```

It is read with `cluster#can_approve`, `can_approve_perimeter` or
`can_configure` (the usher's `GET /v1/clusters/{c}/approvals`, each entry
with `approvedBy`); the answer is `apps approve` as above, with the
request's tenant -- or a no:

```bash
kubectl gentian apps decline aluvian-commerce podSecurity/run-as-root --tenant team \
  --reason "no root on shared nodes; the rootless build of the image runs without it"
```

Whoever may approve a request of the kind may decline it, and nobody else.
A decline commits nothing: the request stays pending and the app keeps
waiting, but the tenant's console and the App Store's install step show who
declined, when and why, and offer to ask again. Asking again clears the
decline, as does an approval made after it.

What was approved, for every tenant at once, is read with `cluster#can_audit`
from the director's `GET /v1/clusters/{c}/privileges` -- `200 {"cluster",
"privileges": [{"tenant","install","privilege","approver","approvedAt",
"reason","expiresAt"}]}`, each tenant's `privileges.yaml` in one list:

```bash
kubectl gentian approvals granted  # every tenant's approved exceptions: app, privilege, who approved, when, until when
```

### What a tenant has on the internet

An entry of a profile that the public reaches (`surface: perimeter`) answers
only once the tenant's perimeter approver has approved it
([app-customization.md](app-customization.md) §2.10). Installing the app
publishes nothing; the director lists what the tenant's installed apps ask
for, and keeps the registry of what was approved:

```bash
kubectl gentian exposures requests --tenant demo
kubectl gentian exposures approve cloud share --tenant demo     # <app instance> <entry>
kubectl gentian exposures approve cloud share --tenant demo --expires 2027-03-31 --reason "shared calendars"
kubectl gentian exposures list --tenant demo
kubectl gentian exposures withdraw cloud share --tenant demo
```

`requests` shows every entry an installed app declares for the internet: its
kind, the address it is published at, its paths, who can reach it, and its
state. The same list carries the one request that is not a public address:
an entry behind sign-in that asks to keep the app's own `Authorization`
header (KIND "Behind sign-in: keeps the app's own Authorization header").
It is approved, reviewed and withdrawn with the same commands, and nothing
is published for it.
`requested` is declared and not approved, so nothing is published.
`approved`, `review due` and `expired` are as in `list`, with who approved it
and until when. `unmatched` is an entry in the registry that matches no entry
of an installed app; nothing is published for it, and `withdraw` clears it.
Whoever may see the tenant may read the list.

Under SIGN-IN, `none: anyone` means anyone on the internet reaches the paths
without signing in, and no credential is passed to the app. `none: the app
checks the credential` is a public address whose callers' `Authorization`
header is passed to the app (`authMode: app`). The platform checks nobody at
a published address in either case. `required` is an entry behind sign-in.
KIND, and everything `approve` prints about an entry, are the director's own
words.

`approve` is for whoever may publish in the tenant (`can_expose`):

- the tenant's perimeter approver: an administrator of the tenant the
  cluster's administrator named (`platform-roles holders add perimeterApprover
  <tenant> <login> --reason <why>`);
- the cluster's perimeter approver, a person of the platform tenant named the
  same way, in every tenant;
- the cluster's administrator, in every tenant the cluster operates — not in
  a tenant that withdrew the operator.

Anybody else is answered 403. It reads the entry from
the director and prints its kind, the address, the paths, who can reach it
and the limit per client address, then
asks you to type `<app instance>/<entry>` (or pass `--yes`). Without
`--expires` the entry stays until it is withdrawn; a date means the end of
that day, UTC. It is reviewed after a year at the latest. Approving an entry
that is approved already is its review, and replaces its expiry and reason.
For a public address that passes the caller's credential to the app, what
it prints includes that the platform does not know or check who calls, and
that an app password or token of a person removed from the tenant keeps
working until the app itself revokes it. For an entry behind sign-in it
says that nothing is put on the internet and what changes instead. The
command sends the kind it showed with the approval, and the director refuses
it (409) if the app's entry has come to declare another meanwhile.
The command sends nothing for an entry the director does not list, and the
director itself refuses an app instance that is not installed in the tenant
and an entry that asks for nothing an approver decides. It also refuses
an approval that asks for the main address for an entry not declared for it,
or that does not ask for it for one that is; the command sends what the
entry is, and prints the director's refusal as it came.

An entry for the cluster's main address (`apex: true` in the profile) is
approved only with `--acknowledge-main-address-rule`. The command first
prints the director's rule for a website there in full, and the name of
whoever acknowledged it is recorded with the entry. `--yes` does not
acknowledge it, and the flag is refused for an entry that is not for the
main address. The director approves such an entry only for the one tenant
that may hold the main address: the user tenant of a single-tenancy cluster
(`--tenant user`), or the tenant the Cluster claim names for it
(`mainAddress.tenant`, under either mode); the website then answers at
`https://<kernel-domain>/` and `www.`.

```bash
kubectl gentian exposures approve <app instance> <entry> --tenant user --acknowledge-main-address-rule
```

**An alias.** A published entry answers under its tenant's domain. Whoever
may configure the cluster can give it a second name directly under the
cluster's own domain, and take it away again:

```bash
kubectl gentian exposures alias <app instance> <entry> --tenant <tenant> --host <name>.<kernel-domain>
kubectl gentian exposures alias <app instance> <entry> --tenant <tenant> --remove
```

The entry has to be published first, by the tenant's approver; the alias
publishes nothing by itself and survives the approver's reviews. The
director refuses a name the platform keeps, a name a tenant is called, a
name another entry holds, and any alias on a single-tenancy cluster;
`exposures list` shows the alias in its own column. What is refused and why:
[design/routing.md §7](design/routing.md).

`list` shows each entry with who published it, when it is reviewed and when
it ends: `published` is in force, `review due` is in force and past its
review date, `expired` answers no longer and stays as the record. `withdraw`
takes one down, for whoever may publish in the tenant; it is one commit, and
the address stops answering once the cluster has taken it in.

## 6a. Resource Plans and Usage

A tenant's resource ceiling is chosen from a priced catalog of `ResourcePlan`
objects, never typed as a quantity — see
[design/resource-plans.md](design/resource-plans.md). The commands below read
the operator's answers — the same ones the Admin Console's **Resources** screen
shows — so the rules (the downgrade guard, the cap on what a tenant may
choose) are enforced once. Choosing a plan is not a command here: it is a commit the director makes
as the person who asked, from the console or from its API with that person's
token.

List the catalog, or what one tenant may pick:

```bash
kubectl gentian resources plans --tenant corp
```

The tenant's current plan is marked `*`.

Show a tenant's ceiling and what is committed under it:

```bash
kubectl gentian resources show --tenant corp
```

Move a tenant to a plan — through the director, as yourself:

```bash
kubectl gentian resources set --tenant corp --plan nodes-2      # [--force]: see below
```

A plan change carries who chose it and which decision allowed it. The
director commits `clusters/<cluster>/tenants/corp/resource-plan.yaml` as the
caller; ArgoCD applies it on the next sync, the operator reconciles the
`tenant-quota` ResourceQuota and records the change in the tenant's usage
history. It is refused (409) when the plan is smaller than what the tenant is
using:

```
{"error": "plan small is smaller than what the tenant is using (limits.cpu: using 34, plan allows 32)"}
```

Kubernetes does not evict pods to fit a shrunken quota — it refuses the *next*
create — so shrinking a tenant too far would otherwise appear to work and fail
hours later at the next restart. `--force` overrides the guard; it is
accepted only from someone who may configure the cluster, who has accepted
that cost.

What a window resolves to for invoicing:

```bash
kubectl gentian resources report --tenant corp \
  --from 2026-01-01T00:00:00Z --to 2026-02-01T00:00:00Z
```

```
PLAN                  DAYS  FROM                 TO                   SKU
base                 17.05  2026-01-01T00:00:00  2026-01-18T01:12:00  sku-1node
nodes-2              13.95  2026-01-18T01:12:00  2026-02-01T00:00:00  sku-2node
```

Cap what a tenant may choose for itself (absent means uncapped):

```bash
kubectl annotate tenant corp gentianos.io/max-resource-tier=20 --overwrite
```

`plans`, `show` and `report` are read from the usher and `set` is sent to
the director, each through a port-forward the plugin establishes and tears
down per invocation.

Live consumption (as opposed to what is committed) needs metrics-server,
which the install brings with step `A-07-metrics-server`:

```bash
./install.sh --only A-07
```

## 6b. Models

The models the cluster's model gateway offers are the Cluster claim's
(`spec.llm`), and the cluster's administrator changes them: on the **Models**
tab of the admin console, or here. Both ask the director, which commits the
claim. [design/llms.md](design/llms.md) §5 has the rules.

```bash
kubectl gentian models list                  # each model, and what the claim says of it
kubectl gentian models show > models.json    # the settings: enabled, gpuAcceleration, console, instances, providers
kubectl gentian models set -f models.json    # make the claim declare exactly these
```

```
$ kubectl gentian models list
MODEL                     FROM                 STATE     NOTE
qwen-qwen2.5-7b-instruct  instance qwen        declared  served by vllm-qwen-inference
infomaniak/gemma-4-31b    provider infomaniak  declared  token: credential llm-provider-infomaniak, property infomaniak_api_key
Model serving is on, GPU serving on.
```

`set` sends the whole of the settings: a model the file does not name is
removed from the gateway. The director refuses a provider whose `apiBase` is
not a public `https` address (no port but 443, no credentials, nothing inside
the cluster or a private network), and one whose `apiKeyProperty` is not its
own: `<name>_api_key`, with `-` as `_`. `console.enabled` serves the gateway's
own console at `llm.<kernel domain>`, a public address; left out of the file,
it stays as it is.

A provider's token is never in the file; it is entered in the admin console
under Credentials, as `llm-provider-<name>`, which the cluster declares for
every provider on the claim once it has applied the commit.
Whether it has been supplied is shown on the console's Models tab, not here.
A model the cluster serves itself names its instance; whether it has finished
loading the model is not shown.

## 6c. Cluster settings

The Cluster claim's scalar settings — `tenancyMode`, `mainAddress.tenant`,
`mail.serviceMode`, the metrics and certificate switches — are changed the
same way: the director commits the claim, and the cluster takes the change
once Argo CD has synced. The same as editing the claim in the deployments
checkout and running `./install.sh`, without the checkout.

```bash
kubectl gentian settings show                                   # the claim's settings, as JSON
kubectl gentian settings set tenancyMode=multi mainAddress.tenant=customers   # one commit
kubectl gentian settings set certificates.dnsZones=gentian.org  # a list: comma-separated
kubectl gentian settings set logForwarding.destination.endpoint=https://evidence.example.org/ingest   # empty removes the forwarder
```

The director holds the list of what may be set and refuses anything else, and
refuses `tenancyMode=single` on a cluster carrying more than the one user
tenant a single-tenancy cluster admits. A change of `tenancyMode` moves the
tenants' addresses ([§3](#3-create-a-tenant)); nothing else here restarts
anything.

One setting is the installer's rather than the operator's:
`certificates.dnsZones`, the zones external-dns may write besides the kernel
domain's, for tenants served on a domain of their own
([§3](#3-provision-a-tenant)). The commit lands like any other,
and the cluster takes it when the step that installs external-dns runs
again, which the command says: `./install.sh --only B-01` on the install
host. The DNS credential has to cover every zone listed.

## 7. Administrator Accounts

No administrator is given a password. The **platform admin**
(`admin@<kernel-domain>`, who signs in at `https://platform.<kernel-domain>/`)
and every **tenant admin** — on a single-tenancy cluster, the one **user
admin**, `user-admin@<kernel-domain>` — are created without
one and handed over through a single-use, expiring link that sets it — and
enrols a second factor unless that was switched off — mailed to a recovery
address or shown once to whoever issued it. There is nothing to retrieve.

A new link, for a lost password or a link that expired:

```bash
kubectl gentian tenants activate-admin <tenant> [--recovery-email <address>]
./install.sh --activate-admin                        # the platform admin
```

The first asks the registrar, which changes nothing about a member of a
group that holds a platform role or a person of the platform tenant who
holds one by name, and answers 403 for the platform admin's
account. That account's link comes from the install host, with the installer's
own credential — which is also the way in when nobody can sign in.

### Cluster roles held by name

`admin` and `breakGlass` are held through a group of the kernel realm
(`spec.platformRoles`). Every other cluster role is held by a person the
cluster's administrator names on the Cluster claim, who keeps their own
account: an entry of `spec.platformAccess.roleHolders`, with a reason and an
optional expiry ([iam.md §1.1c](design/iam.md)). The role is held only with a
token of that person's realm, and used from their tenant's console.

| Role | Who may be named | Scope |
|---|---|---|
| `securityOfficer` -- approving what escapes the default posture: a pod-security waiver, a cluster role, `trustTier: platform` (`cluster#can_approve`) | a person of the platform tenant, or an administrator of another tenant | `cluster` |
| `auditor` -- reading across the cluster (`cluster#can_audit`) | the same | `cluster` |
| `serviceAdmin` -- the claim's system section (`can_operate_system`) | a person of the platform tenant | `cluster` |
| `sharedAppsAdmin` -- shared apps (`can_install_shared`, `can_grant_shared`) | a person of the platform tenant | `cluster` |
| `perimeterApprover` -- public addresses and egress beyond the baseline (`can_expose`, `can_approve_privilege`) | a person of the platform tenant, for every tenant; an administrator of a tenant, for that tenant | `cluster`; `tenant:<t>` |
| `tenantAuditor` -- reading one tenant's audit evidence from the platform console (`tenant#can_read_evidence`) | a person of the platform tenant, for the one tenant `--scope` names | `tenant:<t>` |

```bash
kubectl gentian platform-roles candidates                               # who could hold one: the platform tenant's people, every other tenant's administrators
kubectl gentian platform-roles holders                                  # ROLE SCOPE TENANT LOGIN EMAIL EXPIRES REASON IN EFFECT, as the claim declares
kubectl gentian platform-roles members                                  # who holds admin and breakGlass through their kernel Keycloak group
kubectl gentian platform-roles holders add securityOfficer acme it@acme.example --reason "acme's CISO approves waivers"   # one commit
kubectl gentian platform-roles holders add perimeterApprover acme it@acme.example --reason "acme publishes its shop" --expires 2027-06-30T00:00:00Z
kubectl gentian platform-roles holders add tenantAuditor platform acme-audit@<kernel-domain> --reason "acme reads its own records" --scope tenant:acme
kubectl gentian platform-roles holders remove securityOfficer acme it@acme.example
```

`add` looks the login up among the candidates through the registrar and
commits the account's subject, with the scope that follows from role and
tenant; `--reason` is required. Once Argo CD has synced, the operator writes
the role into OpenFGA for that account of that realm. An entry whose
`--expires` has passed grants nothing: the operator drops its tuple at that
moment, and the director refuses to add one already expired. `remove` takes
the entry away, and the role with it.

The director's routes, all on the Cluster claim:

| Route | Relation | Body | Answer |
|---|---|---|---|
| `GET /v1/clusters/{c}/platform-roles/holders` | `can_audit` | | `200 {"cluster", "roles": ["auditor","perimeterApprover","securityOfficer","serviceAdmin","sharedAppsAdmin","tenantAuditor"], "roleHolders": [{"role","scope","tenant","subject","username","email","reason","expiresAt"}]}` |
| `PUT /v1/clusters/{c}/platform-roles/holders/{role}/{tenant}/{subject}` | `can_configure` | `{"username", "email", "reason", "expiresAt"?, "scope"?}` -- `scope` `tenant:<t>` required for `tenantAuditor` | `202 {"status":"updated","commit"}`, `200 {"status":"unchanged"}`, `400` without a reason, `422` for a role, tenant or subject that cannot hold one -- a person of another tenant who is not its administrator, or an entry already expired |
| `DELETE /v1/clusters/{c}/platform-roles/holders/{role}/{tenant}/{subject}` | `can_configure` | | `202` or `200 unchanged`, as above |

The candidates are the registrar's: `GET /v1/clusters/{c}/role-candidates`,
`can_configure`, answers `200 {"cluster", "complete", "tenants": [{"tenant","displayName","realm","platform","people": [...],"unavailable"}]}`.

The members of the `admin` and `breakGlass` groups are the registrar's to read, from the
kernel realm, and Keycloak's to change: `GET /v1/clusters/{c}/platform-role-members`,
`can_audit`, answers `200 {"cluster", "complete", "roles": [{"role","group","members": [{"id","username","email",...}],
"unavailable"}]}`; a group that could not be read has `members: null`,
`unavailable: true` and makes `complete` false.

The other way round, a tenant's administrators may let the platform's
administrators sign in to their tenant: the switch is the tenant's own, off by
default, and it is not the tenant's `operated_by` consent (that one covers the
director, this one the sign-in).

```bash
kubectl gentian tenants platform-access acme on      # members of gentian:platform:admin may enter acme as its administrators
kubectl gentian tenants platform-access acme off
```

The way in is *Enter tenant* in the platform console, which is
`https://desktop.acme.<kernel-domain>/?kc_idp_hint=kernel`; the tenant's own
sign-in page shows no button for it.

Keycloak master-realm admin:

```bash
kubectl get secret keycloak-admin -n kernel-authentication \
  -o jsonpath='{.data.password}' | base64 -d && echo
```

ArgoCD has no local admin: D-03 sets `admin.enabled: "false"` and deletes
`argocd-initial-admin-secret`. Sign in through Keycloak as a member of
`gentian:platform:admin`. Break glass, when Keycloak itself is down, is
kubectl in the gitops namespace: set a new password, switch the account on,
and reach the server by port-forward (the edge needs Keycloak too):

```bash
kubectl -n kernel-gitops patch secret argocd-secret -p \
  "{\"stringData\":{\"admin.password\":\"$(argocd account bcrypt --password '<new>')\",\"admin.passwordMtime\":\"$(date -u +%FT%TZ)\"}}"
kubectl -n kernel-gitops patch configmap argocd-cm --type merge -p '{"data":{"admin.enabled":"true"}}'
kubectl -n kernel-gitops port-forward svc/argocd-server 8080:443
# afterwards: ./install.sh --force --only D-03 switches it off again
```

## 8. Key URLs

Given KERNEL_DOMAIN, the main URLs are:

- Platform admin's desktop: https://platform.<KERNEL_DOMAIN>
- Platform admin's console: https://admin.platform.<KERNEL_DOMAIN>
- Identity provider: https://id.<KERNEL_DOMAIN>
- Argo CD and Headlamp: https://argocd.<KERNEL_DOMAIN>, https://headlamp.<KERNEL_DOMAIN>
- A tenant, multi-tenancy: https://desktop.<tenant>.<KERNEL_DOMAIN>,
  `admin.<tenant>.…`, `store.<tenant>.…`, `<app>.<tenant>.…`
- The user tenant, single-tenancy: https://desktop.<KERNEL_DOMAIN>,
  `admin.…`, `store.…`, `<app>.…`; https://<KERNEL_DOMAIN> leads to the desktop
- A tenant bound to a domain of its own: https://desktop.<its domain>

## 9. Useful Troubleshooting Commands

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -n 50
kubectl logs -n kernel-control deploy/gentian-os -f
kubectl get integrationbindings -A
kubectl describe application -n kernel-gitops gentian-os
```

### Reaching OpenBao with the `bao` CLI

OpenBao has no public address. From a machine with a kubeconfig for the
cluster, forward its port and sign in as the platform admin:

```bash
kubectl port-forward -n kernel-secrets svc/openbao 8200:8200 &
export BAO_ADDR=https://localhost:8200        # localhost is one of the certificate's names
export BAO_SKIP_VERIFY=true                   # the certificate is self-signed; or BAO_CACERT=<ca.crt of the Secret openbao-tls there>
bao login -method=oidc -path=oidc role=cluster-admin   # opens a browser; sign in as admin@<kernel-domain>
export BAO_TOKEN="$(bao print token)"
```

The `cluster-admin` role admits the platform administrators' group and may
read and write `secret/gentian-os/kernel/*`. After the handover this is the
only way in: the installer's own token is revoked by `E-03`.
`scripts/recovery.sh --from-vault` and the `bao kv get` examples in §11 and
in [install-reference.md](install-reference.md) §4 assume `BAO_ADDR` and
`BAO_TOKEN` are set this way.

### Gateway API edge routing

```bash
# Platform Gateways and routes
kubectl get gatewayclass gentian-envoy
kubectl get gateway -A
kubectl get httproute -A -l app.kubernetes.io/managed-by=gentian-os
kubectl describe gateway authenticated -n kernel-edge    # and: perimeter

# Envoy Gateway and its data plane
kubectl get pods -n kernel-edge
kubectl get svc -n kernel-edge

# Tenant edge status
kubectl get tenant -o custom-columns=NAME:.metadata.name,GATEWAY:.status.conditions[?(@.type==\"GatewayReady\")].status,TUNNEL:.status.conditions[?(@.type==\"TunnelIngressReady\")].status

# Envoy policies attached to routes
kubectl get backendtrafficpolicy -n tenant-demo
kubectl get backendtrafficpolicy -n kernel-edge -l app.kubernetes.io/managed-by=gentian-os
```

On `networkMode: tunnel`, `Gateway.status.conditions[Programmed]` may be
`False` (`AddressNotAssigned`) while listener conditions are `Programmed=True`
and traffic reaches Envoy via Cloudflare tunnel. Check `TunnelIngressReady` on
the Tenant and external curl to the public hostname.

### OIDC pack catalog

An app whose sign-in client is configured from an OIDC pack depends on the
cluster-scoped `OIDCPackCatalog` its profile bundle brings (a bundle from a
catalog added for the whole cluster; one added for a single tenant brings
profiles alone). Check that the pack is on the cluster before debugging pack
Jobs or missing client scopes:

```bash
kubectl get oidcpackcatalog -l gentianos.io/profile-name=demo-app -o yaml
```

List pack keys and confirm a profile's `clientId` is present:

```bash
kubectl get oidcpackcatalog -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.packs}{"\n"}{end}'
```

An app whose client the operator renders with its release (Odoo, for one)
needs no pack entry. See [app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) §8.

## 10. Kernel Mail Stack (Dovecot + Postfix)

**Two knobs:** `spec.mail.serviceMode` on the **Cluster claim**
(`clusters/<cluster>/kernel/claims/cluster.yaml` in the deployments
repository) says whether the cluster runs its own mail servers (`system`) or
relays through a provider (`external`, the default).
**`Tenant.spec.mail.mode`** controls what the **operator** provisions per
tenant. See [design/mail.md](design/mail.md) §0.

**Tunnel clusters:** `mail.serviceMode: system` is refused with
`networkMode: tunnel`, at pre-flight. A tunnel carries HTTP and HTTPS only;
use `external` there.

### What runs where under `system`

No installer step deploys mail. The `gentian-mail` ApplicationSet exists only
while the claim says `system`, and syncs three charts:

| Application | Namespace | What it is |
|---|---|---|
| `postfix-<stage>` | `system-mail` | the mail server and DKIM signer; no load balancer |
| `dovecot-<stage>` | `system-mail` | the mailbox store; no load balancer |
| `mail-edge-<stage>` | `system-mail-dmz` | the mail edge: an HAProxy that takes ports 25, 587 and 993 on one load balancer and passes each connection on. It holds no mail, no user, no certificate and no key; TLS ends at the servers |

The proxy hands the sender's address to Postfix and Dovecot in a
PROXY-protocol header, on ports of their own (10025, 10587, 10993) that
admit the proxy alone. The load balancer in front of it has to hand the
sender's address on as well: by forwarding packets
(`externalTrafficPolicy: Local`, which the Service sets), or, where it opens
its own connection, with the PROXY protocol switched on at both ends
(`loadBalancer.proxyProtocol` in `kernel/services/mail-edge`,
[design/mail.md §9b](design/mail.md#9b-the-mail-edge)). The mail edge is
built, not yet run on a cluster.

### Switch a cluster to its own mail

```yaml
# clusters/<cluster>/kernel/claims/cluster.yaml
spec:
  networkMode: static-ip
  mail:
    serviceMode: system
```

Edit the claim in the deployments checkout and run `./install.sh`: step 0
commits the edit signed and pushes it, and the cluster reconciles.

### Check mail component health

```bash
kubectl get application -n kernel-gitops | grep -E 'postfix|dovecot|mail-edge'
kubectl get pods -n system-mail
kubectl get pods,svc -n system-mail-dmz          # the Service has the mail address under EXTERNAL-IP
kubectl logs -n system-mail-dmz -l app.kubernetes.io/name=mail-edge --tail=20
kubectl get dnsendpoint -n system-mail-dmz       # mail.<domain>, imap.<domain>, MX, SPF, DKIM, DMARC
kubectl get tenant <tenant> -o jsonpath='{.status.conditions[?(@.type=="MailReady")]}'
```

With a DNS provider the operator publishes `mail.<kernel-domain>` and
`imap.<kernel-domain>` from the load balancer's address, and the MX, SPF,
DKIM and DMARC records; it publishes no address record before the load
balancer has one. The PTR of the address mail leaves from is set at the
provider ([design/mail.md §10](design/mail.md#10-dns-for-real-mail)).

### Switch back to external relay mode

```yaml
# clusters/<cluster>/kernel/claims/cluster.yaml
spec:
  mail:
    serviceMode: external
    host: smtp.example.com
    port: 587
    ssl: false
    starttls: true
    # username/password go through the custodian, not the claim —
    # see the "smtp-relay" CredentialRequirement (credentials.yaml)
```

Run `./install.sh` as above, then supply the relay's credentials in the
admin console's **Credentials** tab.


## 11. Tenant Backup (Export)

Tenant admins take backups from the **Admin Console → Backup** tab; the guide
written for them is [tenant-backup-guide.md](tenant-backup-guide.md), and §1 of
it is the key-setup procedure they will forward to you. An export
captures the workspace's databases, buckets, volumes and Keycloak realm into one
encrypted bundle, pausing each app in turn so its data is internally consistent.

### Encryption — choose before you need it

| Choice | Who can decrypt | Use for |
|---|---|---|
| **Platform key** (default) | whoever holds the identity for `backupRecipients` — in the recovery kit, and in OpenBao when the cluster escrows it | routine and scheduled backups; the only mode support can help restore |
| **My passphrase** | only the passphrase holder | a bundle the platform must not be able to read |

Both produce ordinary [age](https://age-encryption.org) files. A lost passphrase
means a lost bundle — there is no recovery path, by design.

A cluster needs a key pair before the default mode works, and
`./install.sh --export-recovery-kit` makes one on a cluster that has none: the
public half goes to OpenBao at `gentian-os/kernel/backup/recipients`, the
private half into the kit and, unless `spec.backup.escrowIdentity` is `false`,
to `gentian-os/kernel/backup/identity` as well. It never regenerates — a second
pair would orphan every bundle written to the first.

Pin the public half in git so the operator prefers a value the cluster cannot
rewrite:

```yaml
# gentian-deployments/clusters/<cluster>/kernel/values.yaml
backupRecipients:
  - age1...          # public key; safe to commit
```

With no recipient anywhere, an export fails rather than writing a tenant's data
unencrypted.

### From the CLI

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExport
metadata:
  name: nightly-2026-08-18
  namespace: tenant-demo
spec: {}                 # every installed app; platform-key encryption
EOF

kubectl get tenantexports -n tenant-demo
kubectl describe tenantexport nightly-2026-08-18 -n tenant-demo
```

Because the default mode needs no input, a `CronJob` that applies a resource
like this is all a scheduled backup requires.

### Watching one

```bash
# Phase, bundle location, and which apps are paused right now
kubectl get tenantexport nightly-2026-08-18 -n tenant-demo \
  -o custom-columns=PHASE:.status.phase,BUNDLE:.status.bundle.prefix,PAUSED:.status.quiesced

# The capture Jobs themselves. Each runs where its credential is: a database
# dump beside its database, the realm export in the identity namespace, a
# bucket and the manifest beside the object store, a volume archive in the
# tenant's namespace (design/operations.md §9.4a).
for ns in system-postgresql system-mariadb system-s3 kernel-authentication tenant-demo; do
  kubectl get jobs -n "$ns" -l gentianos.io/tenant-export=nightly-2026-08-18,gentianos.io/tenant=demo
done
```

An app listed in `.status.quiesced` is **offline right now**. That is normal
mid-export and worth investigating if it persists: the operator resumes an app
as soon as its capture finishes, and resumes anything it finds paused on every
reconcile, including after a restart.

### Deleting one

Tenant admins delete backups from the Backup tab; this is the same operation:

```bash
kubectl delete tenantexport nightly-2026-08-18 -n tenant-demo
```

A finalizer holds the resource until a cleanup Job has removed the bundle's
objects from the bucket — a deleted backup is gone from storage, not merely
from this list. Deleting a **Running** export is the abort mechanism: paused
apps are resumed and outstanding capture Jobs are stopped first. If the
cleanup Job itself fails, the export is released anyway and the operator log
names the bucket and prefix left behind.

**Retiring a tenant keeps its bundles; deleting it destroys them, unless it
was told to keep them.** These resources live in the tenant's namespace, so a
teardown removes them. The operator recognizes a teardown and does not delete
a bundle because its `TenantExport` went. What happens to the bundles is then
the deletion policy's to say: with `Retain` the backup bucket stays; with
`Delete` the bucket is destroyed with the tenant's other stores, unless the
purge was asked for with `keepBundles` (or the bundles are on external
storage, which the platform never deletes). A bundle you want to outlive its
tenant has to be downloaded, or kept that way
([design/data-lifecycle.md](design/data-lifecycle.md) §2).

Bundles a retired tenant left behind stay until they are removed by hand. On
an erasure request, or simply to reclaim the space, remove them deliberately
once you are sure:

```bash
# What a deleted tenant left behind
rclone lsf gentian:demo-gentian-backup/

# Removing it is not reversible and no backup remains afterwards
rclone purge gentian:demo-gentian-backup
```

`gentian:` is an rclone remote for the platform's own object store, set up
from its administrator's Secret and a port-forward to its Service:

```bash
kubectl port-forward -n system-s3 svc/s3 19000:9000 &
export RCLONE_CONFIG_GENTIAN_TYPE=s3 RCLONE_CONFIG_GENTIAN_PROVIDER=Other \
  RCLONE_CONFIG_GENTIAN_ENDPOINT=http://localhost:19000 RCLONE_S3_NO_CHECK_BUCKET=true
export RCLONE_CONFIG_GENTIAN_ACCESS_KEY_ID="$(kubectl get secret s3-admin -n system-s3 -o jsonpath='{.data.accessKey}' | base64 -d)"
export RCLONE_CONFIG_GENTIAN_SECRET_ACCESS_KEY="$(kubectl get secret s3-admin -n system-s3 -o jsonpath='{.data.secretKey}' | base64 -d)"
```

### Reading a bundle

The bundle is a prefix in the tenant's backup bucket. Every artifact is
encrypted; `bundle-info.json` is deliberately not, and names the command that
opens the rest:

```bash
rclone cat gentian:demo-gentian-backup/nightly-2026-08-18/bundle-info.json

# Platform key
age -d -i /path/to/identity manifest.json.age > manifest.json
# Your passphrase
age -d manifest.json.age > manifest.json
```

`manifest.json` lists what was captured per app, the chart versions at capture
time, and the pause window each app saw. It is what a restore goes by. Since
format 2 (`schemaVersion: 2`) each app's `stores` has one entry per artifact
with its `kind`, the `name` of what it was captured from and its `path` in the
bundle, and the app carries its `digest`, `databaseEngine` and `releases`; a
format 1 manifest names apps and kinds only and still restores
([operations.md](design/operations.md) §9.4). Format 3 (`schemaVersion: 3`)
adds `retained` on an app that was uninstalled with its data kept, the
tenant's `mailboxes` where the cluster runs its own mail server, and
`rights`: the entries of the rights store that follow from nothing else. A
format 2 bundle still restores. Format 4 (`schemaVersion: 4`) adds, among an
app's `stores`, an entry of `kind: secrets` at `secrets/<app>.json`: the
app's own secrets, encrypted like every artifact. A format 3 bundle has none
and still restores. Format 4 is what an export writes now.

#### When the bundle is on external storage

`gentian:` above is the platform's own object store. A tenant whose policy
names an external endpoint writes there instead, with a credential the
operator keeps in the kernel namespace rather than the tenant's — so reading
such a bundle means a remote for that endpoint first:

```bash
ns=system-s3                             # not the tenant namespace
sec=backup-destination-<tenant>          # from status.bundle.credentialSecret
ak=$(kubectl get secret "$sec" -n "$ns" -o jsonpath='{.data.accessKey}'  | base64 -d)
sk=$(kubectl get secret "$sec" -n "$ns" -o jsonpath='{.data.secretKey}' | base64 -d)

export RCLONE_CONFIG_EXT_TYPE=s3 RCLONE_CONFIG_EXT_PROVIDER=Other \
  RCLONE_CONFIG_EXT_ENDPOINT=https://sos-ch-dk-2.exo.io RCLONE_CONFIG_EXT_REGION=ch-dk-2 \
  RCLONE_CONFIG_EXT_ACCESS_KEY_ID="$ak" RCLONE_CONFIG_EXT_SECRET_ACCESS_KEY="$sk" \
  RCLONE_S3_NO_CHECK_BUCKET=true
# a bundle with status.bundle.addressingStyle: virtualHost also needs
#   export RCLONE_CONFIG_EXT_FORCE_PATH_STYLE=false
rclone lsf ext:<bucket>/<prefix>/
rclone cat ext:<bucket>/<prefix>/bundle-info.json
```

`kubectl get tenantexport <name> -n tenant-<t> -o jsonpath='{.status.bundle}'`
names the endpoint, bucket, prefix and credential for any given export, so
nothing here has to be guessed.

The identity to decrypt with comes from the recovery kit, from the printed QR,
or — on a cluster that escrows it — straight out of OpenBao:

```bash
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > id.txt
rclone cat ext:<bucket>/<prefix>/manifest.json.age | age -d -i id.txt
```

## 12. Tenant Restore

A restore **replaces** live data with what a bundle recorded. Anything written
since is gone. It is deliberately awkward to trigger by accident.

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata:
  name: restore-2026-08-18
  namespace: tenant-demo
spec:
  exportRef: nightly-2026-08-18     # or bundle: {bucket, prefix}
  confirmTenant: demo               # must equal the tenant, or it refuses
  decryption:
    identitySecretRef:              # platform-key bundle
      name: backup-identity
    # passphraseSecretRef:          # passphrase bundle
    #   name: my-passphrase
EOF
```

Where the identity comes from depends on `spec.backup.escrowIdentity`. With it
`true` (the default) OpenBao holds a copy at `gentian-os/kernel/backup/identity`,
readable by `cluster-admin` and denied to External Secrets — so a cluster
administrator can restore without the kit. With it `false` the kit is the only
copy, and a restore is where you prove you still have it:

```bash
kubectl create secret generic backup-identity -n tenant-demo \
  --from-file=identity=/path/to/age-identity.txt
```

Delete that Secret once the restore is done.

`spec.intoNewTenant: true` says the tenant was made new for this bundle. An
import (§12a) sets it; a restore into the tenant the bundle was taken of
leaves it out. With it, the data of an app the bundle holds as uninstalled
with its data kept is brought with stores made for it; without it such data
is put back only where the tenant still holds it, so an app purged since
stays purged. The access rights the bundle records as granted, a person's
among them, are granted with or without the field only from a bundle this
cluster signed and taken on it; from any
other they are named in the result, and `status.rights.provenance` says why
([security.md §6.4](design/security.md)).

### From a console, or directly against the director

A console restores through the director, as the
person signed in. The tenant's administrators restore their own tenant; the
cluster's administrator any tenant:

```
POST /v1/tenants/{t}/bundles                        body: the .gentian bytes -> {bundle}
POST /v1/tenants/{t}/bundles/inspect                {backup | bundle | source, decryption} -> manifest
POST /v1/tenants/{t}/actions/restore                {backup | bundle | source, decryption, apps?} -> 202 {name, ...}
POST /v1/clusters/{c}/tenants/{t}/bundles/inspect   the same, as the cluster's administrator
POST /v1/clusters/{c}/tenants/{t}/actions/restore   the same, as the cluster's administrator
GET  /v1/tenants/{t}/restores/{name}                the status (the usher)
```

The tenant routes ask `can_set_policy` on the tenant, the cluster routes
`can_configure` on the cluster; the cluster's administrator uploads at
`POST /v1/clusters/{c}/bundles`. `backup` is the name of one of the tenant's
backups, wherever its policy wrote it; `bundle` is what an upload answered,
and nothing else: a reference with an endpoint or a credential of its own is
refused. `source` is a bundle where the tenant's backups are written that the
cluster may no longer list as a backup, `{"name": "storage"|"destination",
"prefix": "..."}`, as `GET /v1/tenants/{t}/bundle-sources/{name}/bundles`
lists them (§12a). The director opens the manifest with the key first and refuses a
bundle of another tenant with `422` (bringing one here is an import, §12a).
The key goes to the operator in the request and into a Secret the restore
owns; it is never committed or logged. The platform's version line, which
a console compares a bundle's with, is in `GET /v1/tenants/{t}/me`
(`platformVersionLine`) for whoever may enter the tenant.

### What it does, in order

1. **Preflight** — confirmation matches, no export or restore already running,
   bundle exists and is `Ready`, decryption key present. Nothing is touched
   until all of these pass.
2. **Plan** — the operator opens the bundle's manifest with the key and
   decides, against the tenant as it stands, what it puts back. A bundle of
   another tenant is refused (`OtherTenant`): a restore does not rename;
   rename the bundle with a tool that rewrites it first. Only apps the
   manifest lists are touched. An app is restored whole or not at all; one
   that is not installed, runs an older build than wrote the data (unless
   `spec.skipVersionCheck`), or has a store the bundle's does not match, is
   named in `status.notRestored` with the reason. With `spec.apps`, an app
   named that cannot be restored refuses the whole restore instead. Still
   nothing has been touched.
3. **Per app** — pause, load the database (on PostgreSQL also every database
   the app's role owned), make the bucket with its user and policy and load
   it, unpack volumes, run the profile's `restore.post` hooks, run
   `restore.verify`, resume. One app at a time.
4. **Tenant-wide, last** — Keycloak realm and the desktop's database. Last
   deliberately: restoring identity earlier would let members sign in to
   half-restored data.

```bash
kubectl get tenantrestore restore-2026-08-18 -n tenant-demo -o jsonpath='{.status}' | jq \
  '{phase, complete, notRestored, nameDerivation, bundleSchemaVersion, notes}'
```

`phase: Ready` with `complete: false` is a restore that left something out;
`notRestored` says what and why. `notes` is what no restore brings back
([data-lifecycle.md](design/data-lifecycle.md) §5).

### After a restore, members cannot sign in

Keycloak's export carries no password hashes, so accounts come back without
credentials. `status.passwordResetRequired` says so. Send members through a
reset from **Admin Console → Members**.

### After a restore, credentials somebody entered are missing

Of the stored credentials a bundle holds each app's own secrets and no
other. The platform's other credentials were made for the tenant when it was
provisioned and are unchanged; anything a person typed in — a repository's
password, an SMTP relay's, an API key — is not in the bundle and has to be
entered again. Data an app sealed with a secret the platform generated for it
reads wherever the restore set that secret from the bundle; `status.notes`
says for each app whether it did. A bundle older than format 4 holds no
secrets, and such data then reads only where the app's secrets are the ones
it was written with.

## 12a. Tenant Import

Create plus Restore, from a bundle, on this cluster or another one
(sovereignty-concept.md §4.3). The bundle is the `.gentian` file **Download**
produced, or a bucket prefix the cluster can reach.

```bash
kubectl gentian tenants import acme-nightly.gentian --identity-file acme-key.txt
kubectl gentian tenants import acme-nightly.gentian --passphrase          # prompts
kubectl gentian tenants import --bucket acme-gentian-backup --prefix nightly-20261001 --identity-file k.txt
kubectl gentian tenants import --bucket b --prefix p --endpoint https://s3.example.org --addressing-style virtualHost --identity-file k.txt
kubectl gentian tenants import acme-nightly.gentian --identity-file acme-key.txt --drop-custom-domain
```

What happens: the file is uploaded to the cluster's own storage; the director
opens the manifest with the key and refuses a name the cluster already has.
It then makes sure of the definition of every app the bundle's tenant lists:
on the cluster already at the build the bundle records, or fetched at that
build from one of the cluster's catalogs and committed. If one cannot be
had, the import is refused with `422`, names each, and has changed nothing.
A platform component of an earlier release that this one no longer has
(`app-store-me`) is left out instead: the command prints it as skipped, with
the reason, and its data is not restored.
It commits the tenant from the manifest's settings (deletionPolicy Retain,
whatever the bundle said), waits until the operator reports the tenant Ready
with its apps up, and starts a `TenantRestore`. An app that asks for a
credential only a person can provide waits for it, since no bundle carries
one; the status names the app and the credential, which is set under
Credentials in the tenant's Admin Console. The wait ends after two hours. The command follows the
import and prints each phase: `declared`, `provisioning`, `restoring`,
`ready`.

The tenant is made under the name the bundle carries; an import does not
rename, and a request that names another is refused with `422`. A bundle
meant for another name is rewritten into it, by a tool that does so, first. Its realm,
database prefix and bucket prefix follow from that name, whatever the bundle
states. A tenant whose realm or prefixes are already another tenant's is
refused with `409`, on import and on create.

The import is recorded in git beside the tenant (`import.json`) until it has
finished, so it survives a restart of the director. The record holds no key.
If the director restarts before the restore has started, the status says
`awaiting-key`: run the same command again, with the same bundle and key,
and the import goes on. If the restore was already running, nothing is
needed.

Directly against the director:

```
POST /v1/clusters/{c}/bundles                 body: the .gentian bytes -> {bundle}
POST /v1/clusters/{c}/bundles/uploads         an empty upload -> {bundle}
POST /v1/clusters/{c}/bundles?into={prefix}   body: the .gentian bytes, or those not yet there -> {bundle}
GET  /v1/clusters/{c}/bundles/{prefix}/files  what an upload holds -> {files: {name: size}, complete}
POST /v1/clusters/{c}/bundles/inspect         {bundle, decryption} -> manifest
POST /v1/clusters/{c}/tenants/import          {bundle, decryption, keepCustomDomain?} -> 202 status
GET  /v1/clusters/{c}/tenants/{t}/import      the status
```

A large bundle is uploaded in steps, so that a transfer that stops part-way
is continued rather than begun again: start an upload, fill it with `?into=`,
and after a stop send the files `files` does not list into the same upload.
What arrived stays until the upload is complete or swept; nothing restores
from an upload not yet complete. The tenant routes take the same three, under
`/v1/tenants/{t}/bundles`.

**A bundle in storage outside the cluster** is read from a *bundle source*
the cluster's administrator declares, never with an address and keys typed
at the import. The address is a commit (`BundleSource`, in
`clusters/<c>/kernel/claims/bundle-source-<name>.yaml`); the keys are set at
the custodian as `bundle-source-<name>` (Admin Console, Credentials), stored
at `gentian-os/kernel/backup/sources/<name>`, and read only by the platform.
Reading needs only `s3:ListBucket` and `s3:GetObject`. A tenant's own sources
are where its backups go: `storage` (its bucket here) and `destination` (its
backup policy's, when it names one).

```
GET    /v1/clusters/{c}/bundle-sources                          declared, with whether the keys are there
PUT    /v1/clusters/{c}/bundle-sources/{name}                   {endpoint, bucket, prefix?, region?, addressingStyle?, description?}
DELETE /v1/clusters/{c}/bundle-sources/{name}                   removed from git and from the cluster
GET    /v1/clusters/{c}/bundle-sources/{name}/bundles           [{prefix, info}]: each bundle's plaintext header
POST   /v1/clusters/{c}/bundle-sources/{name}/actions/fetch     {prefix} -> {name, bundle}: copied into gentian-imports
GET    /v1/clusters/{c}/tenants/{t}/bundle-sources[/{name}/bundles]   a tenant's own (also POST .../{name}/actions/fetch)
GET    /v1/clusters/{c}/bundle-fetches/{f}                      running | ready | failed
GET    /v1/clusters/{c}/bundles/{prefix}/download               a fetched or uploaded bundle, as one file
GET    /v1/tenants/{t}/bundle-sources[/{name}/bundles]          the tenant's own, under can_set_policy
POST   /v1/tenants/{t}/bundle-sources/{name}/actions/fetch      {prefix}: the tenant's administrator's fetch
GET    /v1/tenants/{t}/bundle-fetches/{f}                       a fetch made on the tenant's route
GET    /v1/tenants/{t}/bundles/{prefix}/download                its copy, or the tenant's upload
```

A tenant's administrator fetches from the tenant's own sources alone, only a
bundle whose header names the tenant (`422` otherwise), and the copy counts
towards the tenant's limits in `gentian-imports`, as its uploads do: one
bundle at most 20 GiB, 50 GiB at once (`413`). Everything there goes once
it is restored or imported, or after seven days (data-lifecycle.md §4).

All of the cluster's are `can_configure`. An import names a bundle at a
source instead of a bucket: `{"source": {"name": "old", "prefix":
"nightly-20261001"}, "decryption": ...}`, read where it is. A fetched bundle
is in `gentian-imports` like an upload, for a console that rewrites it before
importing it.

`decryption` is `{"passphrase": "..."}` or `{"identity": "AGE-SECRET-KEY-1..."}`.
The key is written into a Secret the restore owns and goes with it. Members
come back without credentials (no bundle carries them); the status says
`passwordResetRequired`, and carries the restore's `complete`, `notRestored`
and `notes`, and `outsideGuarantee` when the bundle is of an older platform
version line, or records none, or the tenant's domain here is another (the
command prints it). A bundle of a newer version line is refused. The cluster's own
version line is in the answer to `GET /v1/clusters/{c}/settings` (whoever may
audit the cluster): `platformVersionLine`, with `build`, the commit the
operator's image was built from, when the build recorded one.

A custom domain the bundle's tenant had (`customDomain` in the manifest
`inspect` answers) is kept by default: the `TenantDomain` is committed with
the tenant, under the rules and the right (`can_configure` on the cluster) of
`tenants domain`, and the restore starts only once the operator has put the
tenant on it, so nothing is moved. A domain this cluster has bound to another
tenant is refused with `409`, one that cannot be bound here (on the kernel
domain, a single-tenancy cluster) with `422`; both before anything is written.
The status carries `customDomain`; pointing its DNS at the cluster is yours.
`keepCustomDomain: false` (`--drop-custom-domain`) makes the tenant at
`<tenant>.<kernel-domain>` instead: a move, as above, after which the domain
can be declared again with `tenants domain <name> <domain>`.

The uploaded file is kept in the cluster's `gentian-imports` bucket until a
restore of it has run to its end, restored or failed, and is then removed. A
restore refused before it changed anything leaves it, so the import can be
asked for again without uploading twice; an upload that is never restored
stays until it is removed by hand (`rclone purge
gentian:gentian-imports/<prefix>`). App grants are not in a bundle: they are
declared in git, and a tenant imported into another cluster has them to set
again. An import brings the data of apps that were uninstalled with their
data kept, as uninstalled apps, when this cluster has their definitions. It
grants the access rights granted in the tenant the bundle was taken of only
when the bundle is this cluster's own, of a tenant of the same name; any
other bundle's are each named in `notes`, to be granted again here.

## 13. Backup Policy

Where bundles go, how often, and how long they are kept. One cluster default,
overridden per tenant.

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: BackupPolicy
metadata:
  name: default          # the cluster policy is a singleton by this name
spec:
  scope: cluster
  destination:
    endpoint: https://sos-ch-gva-2.exo.io
    bucket: gentian-bundles
    region: ch-gva-2
  schedule: "0 3 * * *"  # UTC; omit for no scheduled backups
  retention:
    keepLast: 7
    keepWeekly: 4
    keepMonthly: 12
EOF

kubectl get backuppolicies
```

**Naming an endpoint declares a credential rather than taking one.** The
operator creates a `CredentialRequirement` — `backup-destination` for the
cluster, `backup-destination-<tenant>` for a tenant — and the policy reports
`Accepted=False` with `CredentialUnsatisfied` until the keys are supplied
through the custodian. That is deliberate: the gap shows when the
destination is set, not at 03:00 when the first export fails.

```bash
kubectl get backuppolicy default \
  -o jsonpath='{.status.credentialRequirement} satisfied={.status.credentialSatisfied}{"\n"}'
```

**The name is not free.** Every reader fetches a policy by name: the cluster's
as `default`, a tenant's as the tenant's own name. Admission enforces both, so a
misnamed policy is rejected at `kubectl apply` rather than accepted and then
read by nothing.

A tenant states its own with `scope: tenant` and a `tenant` field. Name it from
one variable so the two cannot drift:

```bash
TENANT=demo
kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: BackupPolicy
metadata:
  name: ${TENANT}          # must equal spec.tenant
spec:
  scope: tenant
  tenant: ${TENANT}
  destination:
    endpoint: https://sos-ch-gva-2.exo.io
    bucket: ${TENANT}-bundles
    region: ch-gva-2
  schedule: "0 3 * * *"
EOF
```

Every field is optional and an unset one inherits. `suspendSchedule: true` means
*none*, as distinct from *not stated* — without it a tenant could not opt out of
a cluster-wide schedule. Set `allowTenantOverride: false` on the cluster policy
to refuse tenant policies outright; they are then rejected rather than ignored.

Retention tiers are a union: a bundle any rule keeps survives. `keepLast: 7`
alone reaches back seven nights; adding `keepMonthly: 12` reaches back a year
for the cost of twelve more bundles. With nothing set, nothing is deleted.

**A bundle records where it was written.** Changing a destination does not move
what already exists, so restore and cleanup read each bundle's own endpoint
rather than the policy's current answer — last month's bundles stay reachable
after a tenant moves its storage.

**`addressingStyle`** says how a request names the bucket at the endpoint:
`path` (`https://host/bucket/key`, the default when unset) or `virtualHost`
(`https://bucket.host/key`), for a provider that serves a bucket only by its
own host name. Anything else is refused by the schema, and by the director
before it commits. A manual export's `mode: custom` destination takes the same
field. Each bundle records the style it was written with
(`status.bundle.addressingStyle`), and its restore, download and deletion use
it.

```yaml
  destination:
    endpoint: https://s3.example.org
    bucket: gentian-bundles
    addressingStyle: virtualHost
```

**Leaving the cluster.** A backup's or a restore's steps in the tenant's
namespace (a volume's capture and restore) may reach the destination's
endpoint and nothing else outside the cluster, and only while the run lasts:
its port, and its address when the endpoint names an address rather than a
host name. A run to the platform's own storage gets no way out.

### Object Lock

Full bundles suit WORM storage because no object is ever rewritten; each export
occupies a fresh prefix. Object Lock is a property a bucket is made with and
cannot be switched on later, so this is for an external destination whose
bucket you create with it, with a default retention every uploaded object
inherits (e.g. `aws s3api put-object-lock-configuration` there). The
platform's own store makes the bundle bucket itself, without a lock.

Retention deletes will then fail for locked objects until they expire, which is
the lock doing its job. Size the policy's tiers against the lock period rather
than against the bucket.

## 14. Scheduled Backups

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExportSchedule
metadata:
  name: nightly
  namespace: tenant-demo
spec:
  schedule: "0 3 * * *"    # UTC, always
  keepLast: 7              # older finished exports are deleted; 0 keeps all
EOF

kubectl get tenantexportschedules -n tenant-demo
```

Encryption defaults to the cluster's recipients, which is the only mode that
works unattended — a passphrase has nobody to type it at 03:00.

`status.lastSuccessfulTime` is the field to watch. A schedule that fires nightly
but never succeeds looks healthy by every other measure, and that is precisely
the failure a backup regime cannot afford.

Two behaviors worth knowing:

- A new schedule does **not** fire immediately. Creating a backup the moment
  someone writes YAML would pause a tenant's apps as a side effect.
- A window missed by more than an hour is skipped rather than caught up. Waking
  from a long outage should not take six identical backups, each pausing the
  tenant's apps again.

## 15. Restore Drill

Run this on a scratch tenant before you need it. An untested backup is a
hypothesis. For the procedures themselves — rebuilding a cluster, restoring a
tenant with the cluster key, or with a tenant's own — see
[recovery-playbook.md](recovery-playbook.md), or run `scripts/recovery.sh`,
which performs them.

```bash
# 1. Take a bundle
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExport
metadata: {name: drill-before, namespace: tenant-demo}
spec: {}
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  tenantexport/drill-before -n tenant-demo --timeout=30m

# 2. Change something you can recognise — a file, a user, a project.

# 3. Put it back
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata: {name: drill-restore, namespace: tenant-demo}
spec:
  exportRef: drill-before
  confirmTenant: demo
  decryption:
    identitySecretRef: {name: backup-identity}
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  tenantrestore/drill-restore -n tenant-demo --timeout=60m

# 4. Check the change is gone, other tenants are untouched, and time it.
kubectl get tenantrestore drill-restore -n tenant-demo \
  -o jsonpath='{.status.startedAt} -> {.status.completedAt}{"\n"}'
```

The measured time is the RTO. Publish it rather than assuming one.

### The other recovery: the kit

A tenant bundle restores a tenant into a working cluster. It cannot rebuild the
cluster — the derived credentials every kernel service authenticates with come
from the master password and the derivation salt, which live in OpenBao, and a
disaster that loses OpenBao's storage loses them. That is what the recovery kit
carries, and it is worth proving it round-trips before trusting it:

```bash
./install.sh --export-recovery-kit          # writes an encrypted kit, mode 0600
make verify-recovery-kit                    # export, load back, compare byte-exact
```

`verify-recovery-kit` needs no cluster. It exports a kit from known values and
reads it back in a clean shell, asserting the salt and every credential return
unaltered through newlines, quotes and shell metacharacters, and that a
tampered key name, a file that is not a kit, and a missing identity are each
refused rather than partially applied. A salt that comes back wrong by one byte
reproduces a whole cluster of wrong passwords, and the symptom is an admin
login that fails for no visible reason.

The kit also carries the cluster's bundle signing key (`BUNDLE_SIGNING_SEED`),
and the export seeds one on a cluster that has none. A cluster rebuilt with
`--recover` from the kit and the same deployments repository keeps the key
and its id (`spec.clusterUID` of the Cluster claim), so the bundles it wrote
before still bring back the rights granted in their tenants
([security.md §6.4](design/security.md)).

Storing the kit in the cluster it protects defeats it. It belongs wherever the
organization keeps break-glass material, beside the backup identity.

**If a drill wedges:** an app stuck in `.status.quiesced` is offline. The
operator resumes anything it finds paused on the next reconcile, including
after a restart, so deleting the stuck `TenantExport`/`TenantRestore` is the
recovery — the workload's `gentianos.io/pre-export-replicas` annotation records
what it should be scaled back to.

Both of those depend on the operator still reconciling, so establish that
first. Every step above is a reconcile away from working, and a controller that
has stopped will not take the delete either: a `TenantExport` carries a
finalizer, and deleting it hangs rather than resuming anything.

```bash
# The export controller's own log. Silence since the last "paused app for
# capture", while other controllers keep logging, is a stopped worker.
kubectl -n kernel-control logs deploy/gentian-os | grep tenantexport | tail

# What a stopped worker usually is: a cache that cannot sync, because the
# ServiceAccount may not list the type something read through it.
kubectl -n kernel-control logs deploy/gentian-os | grep -i forbidden
```

A restart clears the wedge and the resume follows from it — the operator
resumes anything it finds paused. It does not clear the cause: if the log names
a Forbidden, the missing rule is a marker and a `make gen-all` away, and the
export will wedge again on the next attempt without it.

```bash
kubectl -n kernel-control rollout restart deploy/gentian-os
```
