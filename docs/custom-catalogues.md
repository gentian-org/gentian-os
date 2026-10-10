# Custom catalogues

How to publish your own apps to a Gentian OS cluster: what a catalogue is, how to build one from an
empty git repository, how to add it to a cluster or to one tenant, and what the platform checks.

## 1. What a catalogue is

A catalogue is an **https address that serves static files**: an index, and one file per app.

```
https://<host>/<path>/index.yaml              what is in the catalogue
https://<host>/<path>/profiles/<name>.yaml    one app's bundle per file: its ComponentProfile,
                                              and what travels with it
```

A catalogue whose profiles must not be public is instead **a directory of the cluster's own
deployments repository** holding the same two things (§4.4). The director reads it from the
repository it already works in. Everything else on this page holds for both.

Nothing is copied from a catalogue into a cluster ahead of time. A profile reaches a cluster when a
tenant installs it: the install names the entry and the digest of the build it means, the director
fetches that one file, checks it, and commits it to the deployments repository under
`clusters/<cluster>/catalogue/`. Argo CD applies it from there — the profile, and with it whatever
else the file holds (§2).

**One profile is fetched by the installer rather than the director.** Step 0 of an install places
the Operations Console's profile — `https://catalogue.aluvian.io/profiles/operations-console.yaml`,
or what `GENTIAN_STORE_CATALOGUE_URL` or `GENTIAN_DEFAULT_PROFILES` name — into the same directory,
before there is a director to ask. It is held to a digest like any other: the one the catalogue's
`index.yaml` lists for the entry, or one pinned in `GENTIAN_DEFAULT_PROFILES` (`…@sha256:<digest>`).
Bytes that do not hash to it stop the install; a catalogue that cannot be reached is a warning;
`--disable-api-extensions` places none. What is written is what the director writes — the file, and
its bundle and origin on the profile — so everything below about a materialised profile holds for it.
Of the checks of §2 the installer runs those on kinds, names and metadata. At rollout the operator
holds the profile and what its bundle brings to that recorded bundle, for the Component it creates
by default as for a pinned install ([install-reference.md §4](install-reference.md)).

A catalogue exists on a cluster in one of three ways:

| Who sees it | Who adds it | Where it is declared |
|---|---|---|
| every tenant | the cluster's administrator | `spec.catalogue.sources` on the Cluster claim |
| one tenant | the cluster's administrator | `spec.catalogue.sources` on that tenant's manifest, `addedBy: cluster` |
| one tenant | that tenant's administrator, **only if the cluster's administrator delegated it** | the same list, `addedBy: tenant` |

Each entry of those lists states where the catalogue is read from, once: `url`, an https address,
or `path`, a directory of the deployments repository. A `path` is declared by the cluster's
administrator only, in the first two rows; a tenant's administrator adds addresses.

All three are written by the director as commits, through the commands in §5. A tenant never sees
another tenant's catalogue: it is not listed, and a coordinate that names it is refused in the same
words as a catalogue that does not exist.

## 2. The format

### `profiles/<name>.yaml`: the bundle

**One profile, one file, one fingerprint.** The file is the app's *bundle*: a YAML stream whose first
document is the `ComponentProfile`. Most bundles are that one document and nothing else. A profile
that needs other objects on the cluster brings them in the same file, after it, as its *companions*,
and the digest in the index is taken over the whole file — so an install pinned to a digest is pinned
to the profile and to every companion with it.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: acme-notes
spec:
  package:
    composition: app-acme-notes
    chart: {repository: oci://registry.example.com/acme/charts, name: notes, version: "1.0.0"}
  # …
---
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: app-acme-notes
  labels:
    gentianos.io/profile-name: acme-notes
spec:
  compositeTypeRef: {apiVersion: gentianos.io/v1alpha1, kind: XApp}
  # …
```

The profile:

- **Exactly one `ComponentProfile`, and it is first.**
- **`metadata.name` equals the file name** without `.yaml`. `profiles/acme-notes.yaml` holds the
  profile named `acme-notes`. The name is a DNS label: lower-case letters, digits and hyphens.
- It must not carry the annotations `gentianos.io/profile-bundle` or `gentianos.io/catalogue-origin`
  (the platform writes those), any label or annotation beginning `argocd.argoproj.io/`, or the label
  `gentianos.io/profile-name` with another profile's name.

What goes into a profile is described in [design/app-catalogue.md](design/app-catalogue.md) §3 and, for
customizing an app, in [app-customization.md](app-customization.md).

**The file is at most 180 KiB**, companions included. The director carries the file's bytes beside
the profile so the operator can check them at rollout, and that is what fits.

#### What a bundle may hold beside its profile

Everything in the file is applied to the cluster, so what may be in it is a short list. Anything not
on it — another kind, a second profile, a field or a label the table does not name — and the whole
bundle is refused: by the director when it is fetched, before anything is written, and again by the
operator before anything is rolled out.

| Kind | Its name | What else is checked | Applied | Read by |
|---|---|---|---|---|
| `Composition` (`apiextensions.crossplane.io/v1`) | `app-<profile>`; at most one | composes `XApp` (`gentianos.io/v1alpha1`) and nothing else; is the one the profile names in `spec.package.composition`; never `app-default` | cluster-wide | Crossplane, to render the app |
| `OIDCPackCatalog` (`gentianos.io/v1alpha1`) | `<profile>-oidc`; at most one | every pack is for a `clientId` (or `oidcPackRef`) the profile itself states; no `serviceClient` pack | cluster-wide | the operator and the app's Composition, to configure the app's client in a tenant's realm |
| `ConfigMap` (`v1`) | `<profile>.<asset>` | carries the label `gentianos.io/asset: <asset>`; `data` only, text only | the catalogue's namespace | the app's Composition, which finds it by its two labels; a sign-in handler (§4.1a) is read by the operator, from the bundle itself |
| `Customization` (`gentianos.io/v1alpha1`) | `<profile>.<record>` | `spec.target.profile` is this profile; `spec.scope` is `profile` | the catalogue's namespace | the operator, for the customization-debt report |

For every companion:

- **It is owned by its profile, by name.** The names above can only be produced from the profile's
  own name, so two bundles cannot both claim one object, and a bundle cannot name an object of the
  platform or of another profile.
- **It carries the label `gentianos.io/profile-name: <profile>`** and no label beside those the table
  names. Platform configuration is found by label; a companion cannot say it is some.
- **It states `apiVersion`, `kind`, `metadata.name`, `metadata.labels` and its `spec` (a ConfigMap:
  its `data`), and nothing else.** No namespace: a companion that has one is applied where the
  cluster applies its catalogue (the provisioning namespace), not where it says. No annotations, no
  owner, no finalizer, no status.
- **It comes from a catalogue of the whole cluster.** A bundle from a tenant's own catalogue is its
  profile alone; one that holds a companion is refused, saying so. Each companion is an object of
  the whole cluster or lives in a namespace of the platform, where a tenant decides nothing.

Secrets, Namespaces, RBAC objects, resource definitions, webhooks, workloads, Compositions of any
other kind and `AppPackage` presets are not on the list. A preset is presentation: it is published
under `packages/` for the App Store, and nothing on a cluster reads one.

**A Composition is trusted as the platform's own.** A Composition decides which objects are created
for an app, and Crossplane creates them with the providers' rights, which reach the whole cluster.
The checks above settle which Composition a bundle may bring and for which app; they do not, and
cannot, settle what it does. **A cluster administrator who adds a catalogue for the whole cluster
trusts every Composition it publishes, now and later, exactly as they trust the platform's.** That
is why only such a catalogue may bring one, and why delegating catalogues to a tenant's
administrators (§5) never extends to this.

A profile's Composition renders the app only when it arrived this way: the install is pinned to a
digest and the bundle of that digest brings `app-<profile>`. `spec.package.composition` by itself
selects nothing — an app whose bundle brings no Composition is rendered by `app-default`.

**A Composition composes only resource types the cluster has.** A cluster creates the Crossplane
provider types listed in gentian-os's `crossplane/providers/activation.yaml` and no others, so a
resource of a vault or Keycloak provider kind outside that list never appears and its app waits. The
check that says so is `scripts/lint/lint-provider-activation.py --tree <your catalogue's sources>`
from a gentian-os checkout; a type that is missing is added to that list, in gentian-os, before the
bundle that needs it is published ([install-reference.md §4](install-reference.md)).

### `index.yaml`

```yaml
entries:
- name: acme-notes          # the profile's metadata.name, and the file name
  version: 1.0.0            # optional; shown in listings
  edition: pe               # ce, pe, me or ee; absent means ce
  digest: sha256:75b75bc45c9d3266a4ffd4eb78d286662bd3bfd870d50d9a9636c0f6b09d74be
  trustTier: experimental   # optional; repeats the profile's spec.trustTier
```

- **`digest` is the sha256 of the bytes of `profiles/<name>.yaml` exactly as served** — the whole
  bundle, the profile and its companions — written `sha256:<64 lower-case hex characters>`.
  `sha256sum profiles/acme-notes.yaml` gives the number.
- A cluster lists the **`ce`** and **`pe`** entries. `pe` (private edition) is the one for your own
  apps; `me` and `ee` entries are counted and left to the App Store.
- An entry without a valid digest is listed and cannot be installed from the listing: there is
  nothing to pin the install to.
- The index is at most 1 MiB. The director keeps a fetched index for five minutes.
- A catalogue without an `index.yaml` works, but cannot be browsed: an entry is then installed by
  naming the catalogue and the digest by hand (`--from` and `--digest`, §5).

Other files beside these (`listings/`, `packages/`, an `index.html`) are ignored by the cluster.

## 3. The worked example: how the default catalogue is produced

The default catalogue, `gentian`, is built from the
[gentian-apps](https://github.com/gentian-org/gentian-apps) repository, which publishes it at two
addresses:

| Address | Built from | Named on the claim of a cluster installed from |
|---|---|---|
| `https://gentian-org.github.io/gentian-apps` | its `main`: the released catalogue | a gentian-os release tag (`GENTIAN_OS_BRANCH=v0.5.0`), or `main` |
| `https://gentian-org.github.io/gentian-apps/develop` | its `develop`: the catalogue under development | any other gentian-os branch |

The installer chooses when step 0 writes a new Cluster claim, prints the choice, and writes the
reason above the address. `GENTIAN_CATALOGUE_URL` in `install.env` names an address outright. A
claim that exists is not rewritten, so a cluster keeps the catalogue it was installed with until
somebody changes it:

```bash
kubectl gentian catalogues remove gentian
kubectl gentian catalogues add gentian https://gentian-org.github.io/gentian-apps
```

or by editing `spec.catalogue.sources` in `clusters/<cluster>/kernel/claims/cluster.yaml` and
committing. What is installed stays installed, at the digest it was installed at.

How a catalogue is produced, with gentian-apps as the example:

1. The repository keeps each app as a directory, `profiles/[<family>/]<name>/`, holding
   `profile.yaml`, optionally `listing.yaml`, and the sources of its companions: `composition.yaml`,
   `oidc-catalog.yaml`, `customizations/<record>.yaml`, files under `assets/`. The directory's
   `kustomization.yaml` says which of them go into the bundle.
2. `scripts/build-catalogue-source.py` turns that into the served shape. For every
   `profiles/**/profile.yaml` it assembles `dist/catalogue/profiles/<metadata.name>.yaml`: the bytes
   of `profile.yaml` unchanged, then one document for each companion the `kustomization.yaml` lists
   — each file under `resources`, unchanged, and one `ConfigMap` for each `configMapGenerator` entry,
   holding the listed files — ordered by kind and then by name. The same tree therefore always
   gives the same bytes. A profile with no companions is published byte for byte as its
   `profile.yaml`. The script then takes the sha256 of the assembled file and writes
   `dist/catalogue/index.yaml`. The edition is the `edition:` of `listing.yaml`, else the suffix of
   the name (`-ce`, `-pe`, `-me`, `-ee`), else `ce`.
   It holds every bundle to the rules of §2 and exits non-zero when one breaks them: a profile
   without a name, two profiles with the same name, a kind other than `ComponentProfile`, a
   companion of a kind that is not allowed or not named after its profile, a pack or an object
   two bundles both hold, a bundle over 180 KiB.
3. The workflow `.github/workflows/apps-ci.yaml` runs the script with `--check` on every push. On
   a push to `main` or to `develop` it builds both catalogues into one site — `main`'s at the root,
   `develop`'s under `develop/` — and the job `publish-catalogue` publishes that site to GitHub
   Pages (`actions/configure-pages`, `actions/upload-pages-artifact`, `actions/deploy-pages`). A
   repository has one Pages site, so every publication carries both. A push to `develop` is stopped
   if the released catalogue it built differs from the one being served. The repository's Pages
   source is set to "GitHub Actions", and its `github-pages` environment must let both branches
   deploy.

So each address follows its branch of that repository, and each published file has a digest in
the published index.

## 4. Your own catalogue, from an empty repository

The steps below build a catalogue with one app, `acme-notes`. They need `git`, `python3` with
PyYAML (`pip install pyyaml`), and `curl`.

### 4.1 Write a profile

```bash
mkdir acme-catalogue && cd acme-catalogue && git init
mkdir -p profiles/acme-notes scripts
```

`profiles/acme-notes/profile.yaml`:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: acme-notes
spec:
  classes: [app]
  launch: tile
  trustTier: experimental
  version: "1.0.0"
  package:
    chart:
      repository: oci://registry.example.com/acme/charts
      name: notes
      version: "1.0.0"
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: notes
      backend: {service: acme-notes, port: 8080}
      tile:
        displayName: Notes
        logo: data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA1MiA1MiI+PHJlY3Qgd2lkdGg9IjUyIiBoZWlnaHQ9IjUyIiByeD0iMTAiIGZpbGw9IiMzNTU4YTgiLz48L3N2Zz4=
        path: /
        relation: can_launch
```

`profiles/acme-notes/listing.yaml`, which marks it as your own edition:

```yaml
edition: pe
```

**Name your profiles `<tenant>-<name>` or `<organisation>-<name>`.** ComponentProfiles are
cluster-scoped: one name is one profile for every tenant of a cluster. A profile from a tenant's own
catalogue is refused when its name is already taken by a profile from anywhere else (§7), so a name
like `notes` will collide sooner or later and `acme-notes` will not.

**Some address names cannot be taken.** An app answers at `<subDomain>.<the tenant's domain>`, or at
its own name where an entry states no `subDomain`. These names are the platform's in every tenant,
and no entry of an app or an add-on may use one, or anything below one (`x.admin`): `desktop`,
`admin`, `store`, `console`, `platform`, `id`, `auth`, `login`, `signin`, `sign-in`, `sso`,
`account`, `accounts`. On a single-tenancy cluster, where the tenant's hosts are directly under the
cluster's domain, the kernel's own are refused as well: `argocd`, `corp`, `headlamp`, `imap`, `llm`,
`mail`, `mail-egress`, `www`. The install is refused with `422`:

```text
app acme-notes cannot be installed: exposure "web" would answer on admin.<the tenant's domain>,
and admin is an address name the platform keeps in every tenant (the tenant's administration
console): only the platform's own component for it, admin-console, may take it. Give the exposure
another subDomain. ...
```

A Component that reaches a cluster some other way is held with the condition `HostReserved` and the
same sentence, and nothing of it is installed or routed. Stating `trustTier: platform` changes
nothing. The list and the reasons: [design/routing.md §3.1](design/routing.md).

**An entry behind sign-in gets no token of the page's, unless it asks and is approved.** On a
`surface: gateway`, `authMode: oidc` entry the front door tells the app who is asking in headers.
It replaces the `Authorization` header a page sent with its own token and removes that again
before the app, with the session's cookies, so an app whose pages send its API a bearer token of
their own does not receive it. Such an app declares `clientAuthorization: app` on the entry. That
is a request to the tenant's perimeter approver, listed and approved like a public address
although it is none; until it is approved nothing changes and the app's own calls fail. Approved,
the header is left as the page sent it, sign-in and the right to use the app stay required, and
no platform token reaches the app. It cannot be combined with `forwardToken` or `exchangeToken` in one profile.
Signing out at the front door ends no session an app keeps itself unless the app is told. An app
with its own OIDC client says where, as a path on one of its own entries
(`requires.services.identity.oidc.backchannelLogout: {exposure, path}`); the platform builds the
address — that entry's Service inside the cluster — and the realm posts its logout token there.
A profile names no address: the older `backchannelLogoutUrl` is refused. The app must check the
token before it ends a session ([app-customization.md §2.10](app-customization.md)). An app that
declares nothing keeps its session until the app ends it.

**An entry on the internet is a request, and the proxy checks no caller.** A `surface: perimeter`
entry publishes nothing until the tenant's perimeter approver approves it (`kubectl gentian
exposures requests|approve|list|withdraw --tenant <tenant>`). It declares `authMode: none` (the
paths are for anyone; `Authorization`, `Cookie` and the identity headers are removed) or
`authMode: app` (the same, but the caller's `Authorization` header is passed to the app, which
alone checks it: for sync clients, API keys and webhooks holding a credential the app issued).
The approver of an `app` entry is told that the platform does not know who calls and that a
removed person's app password works until the app revokes it; cookies pass in neither direction
and the rate per client address is lower. `basic`, `bearer`, `jwt` and `signature` are refused on
a perimeter entry: the platform verifies none of them yet. An approval is for the kind of entry
it was given for; a profile that changes an approved entry's kind is a request again.
`apex: true` asks for the cluster's main address, for the user tenant of a single-tenancy cluster
only, with the approver's acknowledgement. Limits, reserved paths and the rule for such a website:
[app-customization.md §2.10](app-customization.md).

**A contract with another app carries traffic only once it is granted.** `spec.integrations`
(the consumer) and `spec.provides` (the provider) naming one contract ask for a relation; the
network path between the two apps exists while the tenant's administrator has granted it to the
consumer, and not before ([app-customization.md §2.3](app-customization.md)).

**An app that creates databases of its own** (`spec.requires.services.database.allowDynamicDatabaseCreation`)
has to name them the platform's way on MariaDB, where every tenant's databases are on one server:
the name of the database it was given, an underscore, then anything — `<database>_reports`. It can
create no database of another name, and holds no privilege on the server as a whole. If the app
chooses the names itself, the profile must configure it to prefix them with `<database>_`; an app
that cannot be configured so cannot use the field on MariaDB. Databases so named are exported,
restored and purged with the app. On PostgreSQL the app's role owns what it creates and the names
are free.

**An app that calls language models** declares the model gateway
(`spec.requires.services.llm: {}`) and receives its address and a key of its own, in the Secret
`llm-credentials-<profile>` and, for a chart that takes them as values, through
`spec.package.valueMapping.llm` (`baseUrlKey`, `apiKeyKey`). Without the declaration an app gets no
key, no Secret and no network path to the gateway, whatever the cluster runs. With it, on a cluster
that serves no models, the app is held and its Component says why
([app-customization.md §1.1](app-customization.md)).

**An app that verifies who is asking, and does not take the front door's headers on trust**, sets
`exchangeToken: true` on its gateway entry (`authMode: oidc`). Every request then reaches it with
`Authorization: Bearer <token>`: a token of the signed-in person from the tenant's realm, good for a
few minutes, whose audience (`aud`) is the app's profile name and nothing else. The app, or a
sidecar in front of it, checks the signature against the realm's keys, the issuer, the audience and
the expiry. The identity headers are not set on such an entry, and any a client sent are removed: the
token is all the app is told. Any profile may ask for this, since the token is worth nothing anywhere else. It is not
`forwardToken`, which hands on the session's own token and is for a component of platform trust; a
profile sets one or the other.

**A component that acts for a person who is not at a browser** has no session the edge could
check, and must still not act for somebody who may no longer use an app. It declares the rights
check (`spec.requires.services.rights: {}`) and receives, in the Secret `rights-check-<profile>`,
an address (`RIGHTS_CHECK_URL`) and a key of its own (`RIGHTS_CHECK_KEY`). With them it can ask one
question, `POST` with `{"person": "<subject>", "app": "<profile name>"}` and the key as bearer:
may this person use that app of this tenant. The answer is `{"allowed": true|false}`. The key
cannot write, cannot list, and cannot name another tenant's app, and the component holds no
credential of the authorization store. The answer says who in the tenant may use what, so the
declaration needs `trustTier: platform`.

**A component that obtains a person's token while the person is away** declares vouching
(`spec.requires.services.vouching`, with `keys.service`, `keys.port` and optionally `keys.path`:
where the component publishes its signing keys as a JWKS). It needs `trustTier: platform`. The
platform enters it in the tenant's realm as a trusted issuer under the alias `vouch-<profile>`,
gives it a client of the same name to ask with, and tells it what it has to know in the Secret
`vouching-<profile>` (`VOUCHING_ISSUER`, `VOUCHING_CLIENT_ID`, `VOUCHING_CLIENT_SECRET`,
`VOUCHING_TOKEN_URL`). The component signs a statement of at most a few minutes that names the
issuer, the person's subject (their id in the realm), an id of its own, and as its audience the
realm's issuer as the realm's discovery document names it, and posts it to the token address with the grant `urn:ietf:params:oauth:grant-type:jwt-bearer` and one scope,
`app-<profile of the app>`. The realm answers with a token of that person for that app and nothing
else.

It answers only for a person who is linked to that issuer, and who is linked is not the
component's to decide:

- **Linking needs the person's own token.** `POST /v1/tenants/<t>/vouching/<profile>/link` at the
  registrar, with the person's token as the bearer, links that person and nobody else. A component
  that shows a person a page asking for their consent holds that token while they are on it.
- **Unlinking only takes away.** `DELETE /v1/tenants/<t>/vouching/<profile>/people/<id>` is the
  person's, an administrator's of the tenant's people, and the component's own, presenting its key
  (`RIGHTS_CHECK_KEY`, which a vouching component is given as well) as the bearer. A component
  removes the links to its own issuer and no other.

So the people a component can speak for are the people who said so, for as long as nobody took it
back.

**An app that opens mailboxes with the person's sign-in token** (IMAP XOAUTH2) declares
`spec.requires.services.mail.imap.tokenSignIn: true`, beside the sign-in client it needs
(`spec.requires.services.identity.oidc`). Its client is given the optional scope `mailbox`; a token
the app obtained by asking for that scope opens the mailbox of the person it was issued to, and
nothing else of the app's does. Without the declaration no token of the app is accepted by the mail
server. On a cluster that does not run its own mail server, or for a tenant without mailboxes
there, the declaration is accepted and grants nothing
([app-customization.md §1.1](app-customization.md)).

### 4.1a An app with no single sign-on: declare the sign-in sidecar

For an app that, in the edition you publish, can do neither OIDC nor SAML. Only in a catalogue of
the whole cluster: the handler is a companion (§2), and a tenant's own catalogue brings none.

1. **Declare it** in the profile, under `requires.services.identity.sidecar`
   ([app-customization.md §2.3a](app-customization.md) has every field): the app's front page and
   its own sign-in page as `entryPaths`, and what the handler needs of the app — `database`,
   `secrets`, `appPort` — and no more. Put the app's own sign-in calls under the entry's
   `denyPaths`, above all a first-run or setup call.
2. **Write the handler**, one file, and add it to the bundle as the ConfigMap
   `<profile>.sign-in-handler` with the key `handler.js` and the labels
   `gentianos.io/profile-name: <profile>` and `gentianos.io/asset: sign-in-handler`:

   ```js
   module.exports = {
     // person: { email, name }; ctx: { sessionSeconds, origin, log }
     async onLogin(person, ctx) {
       // find or make the person's account in the app, as an ordinary member;
       // make a session that ends after ctx.sessionSeconds
       return { redirect: '/home', cookies: [{ name: 'session', value: token }] };
     },
   };
   ```

   It may use `pg`, `mysql2` and `jsonwebtoken`, which the sidecar's image carries, and what the
   profile declared (`DB_*`, `SECRET_<NAME>`, `APP_URL`). It cannot reach anything else, it never
   writes to the browser, and it must not give anybody a password, administrator's rights or a
   shared account. The full contract and two worked handlers are in gentian-apps
   (`images/gentian-sidecar-sso-saml/README.md`).
3. **Test it against the app itself**, at the version the chart pins, before publishing: a handler
   depends on how the app keeps a session, which no vendor promises to keep.

The digest of the bundle covers the handler. A changed handler is a new digest, and reaches a
cluster only when an install is moved to it.

### 4.2 Generate the index

Take the build script from gentian-apps and run it. It is on that repository's `develop` branch,
and not on its `main` yet:

```bash
curl -fsSLo scripts/build-catalogue-source.py \
  https://raw.githubusercontent.com/gentian-org/gentian-apps/develop/scripts/build-catalogue-source.py
python3 scripts/build-catalogue-source.py --out dist/catalogue
```

```
Catalogue source at dist/catalogue: 1 entries (1 pe)
  a cluster browsing this source lists 1; the other 0 are the App Store's to present.
```

`dist/catalogue` now holds `index.yaml`, `profiles/acme-notes.yaml` and `listings/acme-notes.yaml`.
This profile has no companions, so the published file is `profile.yaml` byte for byte. A catalogue
that is added for a whole cluster may give a profile companions: put their sources in the profile's
directory and list them in a `kustomization.yaml` beside `profile.yaml`, as §3 describes.
Check the digest yourself:

```bash
grep digest dist/catalogue/index.yaml
echo "sha256:$(sha256sum dist/catalogue/profiles/acme-notes.yaml | cut -d' ' -f1)"
```

The two lines show the same number. Add `dist/` to `.gitignore`: it is built, not kept.

If you would rather keep the served layout in git directly — `profiles/<name>.yaml`, flat — write the
files there and generate only the index with the tool gentian-os ships:

```bash
python3 scripts/tools/build-catalogue-index.py <directory-holding-profiles/>
```

Each file there is then the bundle as served: write the companions into it yourself, after the
profile, separated by `---`. The tool takes the digest of the whole file.

### 4.3 Publish it as static files

Any web server that serves the directory over **https, on port 443, at a public address, without a
login and without a redirect** will do.

**GitHub Pages.** In the repository's settings set Pages → Source to "GitHub Actions", and add
`.github/workflows/catalogue.yaml`:

```yaml
name: Catalogue
on:
  push:
    branches: [main]
jobs:
  publish:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pages: write
      id-token: write
    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}
    steps:
      - uses: actions/checkout@v4
      - run: pip install pyyaml
      - run: python3 scripts/build-catalogue-source.py --out dist/catalogue --with-landing-page
      - uses: actions/configure-pages@v5
      - uses: actions/upload-pages-artifact@v3
        with:
          path: dist/catalogue
      - id: deploy
        uses: actions/deploy-pages@v4
```

The catalogue's address is then `https://<owner>.github.io/<repository>`.

**GitLab Pages.** `.gitlab-ci.yml`:

```yaml
pages:
  image: python:3.12
  script:
    - pip install pyyaml
    - python3 scripts/build-catalogue-source.py --out public
  artifacts:
    paths: [public]
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
```

The project's Pages must be visible to everyone; the address is the one GitLab shows under
Deploy → Pages.

**Any web server.** Copy `dist/catalogue/` to the document root and serve it with a certificate a
public authority issued.

Check from outside that both files are there and that neither answer is a redirect:

```bash
curl -sI https://<host>/<path>/index.yaml | head -1
curl -sI https://<host>/<path>/profiles/acme-notes.yaml | head -1
```

Both must answer `200`.

### 4.4 Or keep it in the cluster's deployments repository

For profiles that must not be public. The deployments repository is the private git repository the
cluster is deployed from, which the director reads and writes with the credential it was installed
with. A catalogue is a directory in it, laid out as an address serves one:

```
catalogue/index.yaml
catalogue/profiles/acme-notes.yaml
clusters/<cluster>/...                        the cluster itself, as before
```

Write each bundle as `profiles/<name>.yaml`, generate the index, commit and push:

```bash
python3 scripts/tools/build-catalogue-index.py catalogue/     # from a gentian-os checkout
git add catalogue && git commit -m "Catalogue: acme-notes 1.0.0" && git push
```

**On a cluster that is already installed, that commit stops Argo CD.** Argo CD syncs the deployments
repository only while its newest commit is signed by one of the cluster's two keys (the director's
and the break-glass key, `clusters/<cluster>/kernel/signing/keys.env`). Your commit is signed by
neither, and step 0 of the installer commits only the cluster's own directories, never a catalogue
directory. After the push, on the machine the cluster was installed from, run

```bash
./install.sh --only A-01      # or a plain ./install.sh
```

Step 0 runs before the first step of every install run, whichever steps are selected: it finds a
newest commit it does not trust and puts an empty commit signed with the break-glass key on top
(`Signed the head of the deployments repository (break-glass)`). `--only A-01` keeps what follows
to one step that is already satisfied. It needs the break-glass key in `~/.gentian/gnupg`; on
another machine `./install.sh --recover <kit>` imports it first. `--dry-run` and `--validate` sign
nothing. Every later change to the directory is the same: commit, push, run it again.

Then the cluster's administrator declares the directory (§5). The declaration is the directory and
nothing else:

```yaml
# clusters/<cluster>/kernel/claims/cluster.yaml: for every tenant
spec:
  catalogue:
    sources:
      - name: acme
        path: catalogue
```

```yaml
# clusters/<cluster>/tenants/<tenant>/tenant.yaml: for one tenant
spec:
  catalogue:
    sources:
      - name: acme
        path: catalogues/demo
        addedBy: cluster
```

- **It names a directory of this repository and can name nothing else.** There is no field for a
  repository, a host, a branch or a commit. `path` is names of letters, digits, `.`, `_` and `-`
  separated by `/`, from the top of the repository: no leading `/`, no `..`, no name beginning with
  a dot, at most 255 characters. An entry that states both `url` and `path` is not read.
- **Never `clusters/<cluster>/catalogue`**, of this cluster or another in the same repository, or
  anything below it. That is where the director writes the profiles tenants installed (§1); it is
  not a catalogue, and declaring it one is refused.
- **Symbolic links are not followed.** The directory, every directory on the way to it and each
  file read must be a plain directory or file. A link is refused, wherever it points.
- **It is read from the director's checkout**, at the commit that checkout is at, with the
  credential the director already has. Nothing is fetched from anywhere, so §4.3 and the address
  checks of §7 do not apply. The checkout follows the repository within a few seconds; the index is
  kept for five minutes like any other.
- **Only the cluster's administrator declares one**, for the whole cluster or for one tenant. A
  tenant's administrator is refused, with or without delegation, and an entry with `path` and
  `addedBy: tenant` written into a manifest by hand is not read. Whoever can push to the
  deployments repository decides what is in the directory; that is the cluster's administrator.
- **It is trusted exactly as an address the cluster's administrator added.** The install is pinned
  to the digest the index states, the director refuses bytes that do not hash to it, the bundle is
  checked (§2), the profile is committed to `clusters/<cluster>/catalogue/` with its origin, and
  the operator checks again at rollout (§7). A directory declared for the whole cluster may bring
  companions; one declared for a tenant brings profiles only. Being in the repository earns a
  profile nothing: `trustTier` is what the profile states and is held to as anywhere.

The chart and the images are private the same way as for any catalogue (§8).

## 5. Add it, list it, install from it

Sign in first: `kubectl gentian login`.

**For every tenant of the cluster** (the cluster's administrator):

```bash
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue
```

**For one tenant** (the cluster's administrator):

```bash
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant demo
```

**For one tenant, by that tenant's own administrator.** The cluster's administrator delegates it
once, and the tenant's administrator then runs the same command:

```bash
kubectl gentian tenants delegate-catalogues demo on        # the cluster's administrator; or: tenants set demo --admins-add-catalogues=true
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant demo
```

Delegation is off for every tenant until it is turned on, and `off` turns it off again. A tenant's
administrator removes only catalogues the tenant added.

**A catalogue kept in the deployments repository** (§4.4) is added by naming its directory in place
of an address, by the cluster's administrator only:

```bash
kubectl gentian catalogues add acme --path catalogue                       # for every tenant
kubectl gentian catalogues add acme --path catalogues/demo --tenant demo   # for one tenant
```

The directory must be in the repository when the command is run. A tenant's administrator who runs
it is answered `403`.

A tenant's catalogue cannot take the name of a catalogue of the whole cluster, and the other way
round. Two tenants may each have a catalogue called `acme`; each sees its own.

**List** what a tenant installs from, or everything the cluster declares:

```bash
kubectl gentian catalogues list --tenant demo
kubectl gentian catalogues list
```

```
CATALOGUE  FOR           ADDED BY     ADDRESS
gentian    every tenant  the cluster  https://gentian-org.github.io/gentian-apps
own        every tenant  the cluster  deployments repository: catalogue
acme       tenant demo   the tenant   https://acme.github.io/acme-catalogue
Tenants whose administrators may add catalogues of their own: demo.
```

**See what is on offer and install:**

```bash
kubectl gentian apps list --tenant demo --available
kubectl gentian apps install acme-notes --tenant demo
```

`apps install` looks the entry up in the catalogues the tenant sees and sends its coordinate,
`acme/acme-notes`, and the digest the index lists. When two catalogues serve the same name, say which
with `--from <catalogue>`. A catalogue without an index is installed from with both stated:
`--from acme --digest sha256:<hex>`.

**Remove** a catalogue:

```bash
kubectl gentian catalogues remove acme --tenant demo
```

What was installed from it stays installed.

## 6. Updates

A changed profile is a new build with a new digest, and so is a changed companion: the digest is of
the whole file. Publishing it changes nothing on any cluster:
every install is pinned to the digest it was made at.

To move a tenant to the new build, publish, and install again:

```bash
kubectl gentian apps list --tenant demo --available     # INSTALLED shows "other build"
kubectl gentian apps install acme-notes --tenant demo   # "Moving acme-notes in demo to this build"
```

The index is cached for five minutes, so a build published a moment ago may not be listed yet.

A profile is one object for the whole cluster. When two tenants have the same app from a catalogue
of the cluster and one moves to a new build, the other's install is still pinned to the old digest:
the operator holds it as it is (`DigestMismatch` on its Component) and rolls nothing out for it until
that tenant moves too. Publish a build that must coexist with the old one under a new name.

### What a newer build leaves behind

**Nothing a bundle brought is removed automatically.** The Application that applies the catalogue
directory does not prune, for companions as for profiles. A companion a new build no longer brings
stays on the cluster, and so does a profile after its last uninstall. That is deliberate: a tenant
moved back to the older build finds its pieces, and the data of an uninstalled app is purged with
its profile. What stays is no longer checked and no bundle owns it — and an `OIDCPackCatalog` left
this way is still read.

The cluster's administrator sees what is left, and removes one piece at a time:

```bash
kubectl gentian catalogues residue
kubectl gentian catalogues residue remove OIDCPackCatalog/xwiki-ce-oidc
```

The list holds four classes, each object with the profile it names, why it is listed and when it was
created (the cluster does not record when a piece stopped being owned):

| `WHY` | What it is |
|---|---|
| `dropped` | An object of a companion kind that names a profile on the cluster — by its label `gentianos.io/profile-name`, or by a name only that profile's companion has — and that the bundle now materialised for the profile does not bring. |
| `orphaned` | One that carries the label of a profile that is not on the cluster. |
| `unowned` | One that no bundle owns and that names no profile with a bundle: what installations from before bundles left. A `Composition` that composes an app and is not `app-default`, any `OIDCPackCatalog`, and a `ConfigMap` or `Customization` carrying the asset or the profile label. |
| `unused profile` | A materialised profile that no tenant has installed or switched on as an add-on, and for which no tenant retains data. |

Never listed: `app-default`; anything a chart ships (the platform's profiles, its own pack catalog);
anything a bundle on the cluster brings; a pack catalog that declares a service client; a
`Composition` that composes anything but an app; and anything in a namespace other than the one the
catalogue is applied in — no object of a tenant's namespace is ever looked at. A profile whose bundle
cannot be read owns nobody knows what, so nothing that names it is listed; and when what a tenant
retains could not be read, no profile is listed as unused. The list says which of the two it could
not establish.

**`IN EFFECT` is the one column that says a leftover still changes behaviour.** A client is configured
from the first `OIDCPackCatalog` on the cluster that holds a pack under its client id, whoever
brought the catalog, and an app's Composition reads the catalog that carries its profile's label. A
leftover pack catalog is `YES` when it is the only one holding one of its client ids, or the only
one labelled for an installed profile; `contested` when another catalog holds the same, and which
is read is then not defined; `no` otherwise. Removing one that is in effect changes what the next
sign-in client is given.

Removing asks for the name typed again, and removes exactly the one object named:

- **A companion** is deleted from the cluster by the operator. It works the list out again at that
  moment and deletes only an object that is on it, so a companion a bundle brings, `app-default`, or
  a ConfigMap that is nobody's companion is refused with the reason, and so is an object a new
  install adopted a moment ago. It is also refused while a bundle file in the deployments repository
  declares the object, or Argo CD still finds it declared: what is declared would be applied again.
- **An unused profile** is first removed from the deployments repository: its bundle, the carrier
  beside it and its kustomization entries, as one commit in the person's name. That is refused while
  any tenant's manifest names the profile, or a tenant retains data for it. The object is then
  deleted from the cluster by the operator, once Argo CD reports that the directory no longer
  declares it — a few minutes at most; the list shows the profile until then, and the command can
  be repeated. Pruning is not switched on for this: it would remove everything else that is left
  over in the same sync. The companions the profile's bundle brought stay, and are then listed as
  `orphaned`.

The answer says what was deleted, or why nothing was. From the console's side these are
`GET /v1/clusters/<cluster>/catalogue/residue` at the usher (whoever may audit the cluster) and
`POST /v1/clusters/<cluster>/actions/remove-catalogue-residue` at the director (whoever may
configure it).

**A tenant's administrator sees the leftovers of the apps the tenant has, and removes them only on a
cluster with one user tenant.** A profile is on a cluster once, under one name, for every tenant
that installed the app, and so is what its bundle brought.

- *The read* is `GET /v1/tenants/<tenant>/apps/<profile>/residue` at the usher, for whoever may
  view the tenant. It answers only for a profile the tenant has, as an app or as an add-on that is
  switched on, and is not found for any other — it is no way to read the cluster's catalogue. It
  holds the `dropped` and `orphaned` objects that name the profile or, for an app, one of its
  add-ons, in the shape of the cluster's list; never an `unowned` object or an unused profile, and
  nothing about another tenant or another profile. `removableBy` says who may remove them:
  `tenant` when the cluster's tenancy mode is `single` and this is its one user tenant, `platform`
  everywhere else.
- *The removal* is `POST /v1/tenants/<tenant>/apps/<profile>/actions/remove-residue` at the
  director, for whoever may install apps in the tenant, with the name typed again. On a cluster
  with more than one user tenant it answers 403: "These pieces are shared by every tenant that
  uses this app. Ask the platform admin to remove them." Otherwise it is the removal of a
  companion described above, with one more condition the operator checks when it works the list
  out again: the object is `dropped` or `orphaned`, names this profile, and the tenant has the
  profile. Anything else is refused and nothing is deleted.

The operator may delete a `Composition` for this, a right it did not have. It uses it only for one
that composes an app, is named `app-<profile>`, is not `app-default` and is on the list when asked.

## 7. What is checked, and what is not

**The catalogue is not trusted.** It is a web server, possibly yours, possibly compromised — or a
directory of the deployments repository, which is held to the same checks.

- **The digest pins the profile and its companions.** The director refuses bytes that do not hash to
  the digest the install named, and commits nothing. The operator checks again before every rollout:
  the profile in the cluster must be what the committed bytes say, and so must every companion — a
  missing one holds the rollout (`CompanionMissing`), and so does one that differs
  (`CompanionMismatch`, naming it and the field).
- **How closely a companion is compared.** A ConfigMap's data, an OIDCPackCatalog and a
  Customization have to be exactly what the bundle says. A Composition is Crossplane's kind and the
  operator has no schema for it: everything the bundle states has to be in the cluster unchanged,
  no pipeline step may be added or removed, and what each step is given (`input`, where the
  templates are) has to be exact; a field added beside those the bundle states, outside an `input`,
  is not noticed.
- **What a bundle holds is checked** (§2), at the fetch and again at rollout. A bundle that is the
  pinned build and holds what it may not is not rolled out (`BundleRefused`).
- **The digest does not pin the chart or the images the profile names.** A profile says
  `chart: {repository, name, version}`; what that registry serves under that version is the
  registry's to decide. Use immutable chart versions and image digests in your chart if you need the
  same guarantee further down.
- **What a chart contains is not inspected, and it is installed with the rights of the whole
  cluster.** Nothing reads the templates of the chart a profile names; the pods it starts are held
  to the admission policies, its other objects to nothing. Two things follow for secrets. An
  `ExternalSecret` a chart puts in the tenant's namespace can name only that tenant's store,
  `openbao-tenant-<tenant>`: it reads that tenant's app, repository and contract credentials, no
  path of the kernel and none of another tenant
  ([security.md §5](design/security.md)). That holds for the stores that exist. A chart can still
  create objects of its own anywhere in the cluster, a secret store included, and can read a
  Kubernetes Secret directly, so a chart is trusted like the platform itself, and so is whoever can
  add the catalogue it comes from. A Composition of your own that writes an `ExternalSecret` into
  the tenant's namespace names the tenant's store, as `app-default` does; one that names `openbao`
  there gets no Secret.
- **The address is checked**, when the catalogue is added and again on every fetch: https only,
  port 443, no user name or password, and a host that resolves to public addresses only. Loopback,
  private and link-local ranges, carrier-grade NAT, metadata addresses, and names inside a cluster
  (`*.svc`, `*.cluster.local`, single-label names) are refused. Redirects are not followed.
- **A directory is checked**, when the catalogue is added and again on every read (§4.4): a
  relative path of plain names inside the deployments repository, not the directory installed
  profiles are written into, and no symbolic link on the way or at the end. A directory that
  stops passing is not read, and the install or the listing answers `502` with the reason.
- **A name is one profile.** A profile from a tenant's catalogue is refused when a profile of that
  name already exists from another origin — a catalogue of the cluster, another tenant's catalogue,
  or a component the platform ships (`desktop`, `concierge`, `admin-console`). The refusal says the
  name is taken and to publish it as `<tenant>-<name>`. A cluster catalogue's entry is refused the
  same way when a tenant's catalogue already holds the name.
- **An address name the platform depends on is not an app's to take** (§4.1). The director refuses
  the install or the add-on (`422`); the operator holds the Component (`HostReserved`).
- **A tenant's own profile is installable only in that tenant.** The director refuses anybody else,
  and the operator refuses to roll out a Component in another tenant from it
  (`ProfileOfAnotherTenant`).
- **A sign-in handler runs only from a cluster catalogue's pinned bundle.** A profile that declares
  `requires.services.identity.sidecar` brings code that is handed its app's signing key and
  database (§4.1a). The operator runs it only for an install pinned to a digest whose bundle, from
  a catalogue of the whole cluster, carries it, and holds every other such install
  (`SignInSidecarRefused`). A tenant's own catalogue cannot bring one.
- **A profile asks for a cluster role by name and cannot write one.** An entry under
  `requires.privileges.clusterRoles` names one of the roles the platform defines, and the
  ServiceAccount its chart runs under. The role is bound only where the platform has it, the
  cluster's `PlatformSecurityPolicy` permits it for the profile, and the security officer granted
  it on the install; otherwise nothing is bound and the Component says why (`ClusterRolesBound`).
  Rules stated on an entry are ignored and make it bind nothing. The platform's set is empty
  today, so a catalogue cannot obtain any access to the Kubernetes API for an app
  ([security.md §3.4](design/security.md)).
- **Nothing here is a licence check.** Whether an app arrives is decided where its chart and images
  are pulled.

An app or an add-on named without a build — `{"profile": …}` with no coordinate and digest — is
installed only when its profile is already on the cluster. Otherwise the director refuses and says to
give the coordinate and the digest. The profile the installer placed (§1) is on the cluster with
no digest ever stated for it, so an install that names it without one meets none of the checks
above that start from a digest.

## 8. Private charts and images

A catalogue at an address is public; the chart and the images need not be. Declare the registry for the tenant,
then set its password:

```
PUT /v1/tenants/<tenant>/repositories/<name>
{"role": "apps", "type": "oci", "url": "oci://registry.example.com/acme"}
```

The password is set at the custodian, as for every repository, and becomes a pull Secret in that
tenant's namespace only. A chart whose address lies inside the declared registry
is pulled with it. The App Store app does both steps itself for what it sells; for your own registry
they are done once by the tenant's administrator.

## 9. Current limits

- **Public https addresses, or the cluster's own deployments repository.** A catalogue inside the
  cluster, on a private network, on another port, or behind a redirect is not fetched from, and no
  other git repository is read. A cluster that cannot reach the internet can install its own
  profiles from a directory of its deployments repository (§4.4), where it reaches that repository.
- **A tenant's own catalogue serves profiles only.** Companions (§2) come from a catalogue of the
  whole cluster. A tenant's own app that needs an OIDC pack, a Composition of its own or a sign-in
  handler has to be published in one.
- **Four kinds of companion, and no others.** A profile that needs anything else on the cluster does
  not bring it.
- **Two profiles that declare the same OIDC `clientId` are not told apart.** Each may bring a pack
  for it, and which one is used is not defined. gentian-apps' build refuses this inside one
  catalogue; across catalogues nothing does.
- **Nothing a bundle brought is removed automatically.** It is listed, and removed one object at a
  time by the cluster's administrator (§6).
- **The installer's default profile has no pin of its own.** It is placed at a stated digest and
  compared with its recorded bundle at rollout (§1), but no second place states that digest: a
  profile replaced together with its bundle is not noticed.
- **No password-protected catalogues.** No credential is sent to a catalogue. Keep what is private in
  the registry (§8); a profile that is itself private goes into the deployments repository (§4.4).
- **A tenant's administrator has no private catalogue of their own.** A directory of the
  deployments repository is declared by the cluster's administrator, also for one tenant.
- **No proxy.** The director connects to the catalogue directly.
- **No console screen.** Catalogues are managed with the commands above.
- **Profiles are not removed automatically.** A profile stays in `clusters/<cluster>/catalogue/`
  after the last uninstall, because the data of an uninstalled app is purged with its profile. Once
  no tenant has it installed or retains data for it, it is listed as an unused profile (§6).
- **Removing a catalogue does not remove what was installed from it**, and does not free the profile
  names it used.
