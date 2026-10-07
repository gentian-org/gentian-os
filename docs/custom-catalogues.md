# Custom catalogues

How to publish your own apps to a Gentian OS cluster: what a catalogue is, how to build one from an
empty git repository, how to add it to a cluster or to one tenant, and what the platform checks.

## 1. What a catalogue is

A catalogue is an **https address that serves static files**: an index, and one file per app.

```
https://<host>/<path>/index.yaml              what is in the catalogue
https://<host>/<path>/profiles/<name>.yaml    one ComponentProfile per file
```

Nothing is copied from a catalogue into a cluster ahead of time. A profile reaches a cluster when a
tenant installs it: the install names the entry and the digest of the build it means, the director
fetches that one file, checks it, and commits it to the deployments repository under
`clusters/<cluster>/catalogue/`. Argo CD applies it from there.

A catalogue exists on a cluster in one of three ways:

| Who sees it | Who adds it | Where it is declared |
|---|---|---|
| every tenant | the cluster's administrator | `spec.catalogue.sources` on the Cluster claim |
| one tenant | the cluster's administrator | `spec.catalogue.sources` on that tenant's manifest, `addedBy: cluster` |
| one tenant | that tenant's administrator, **only if the cluster's administrator delegated it** | the same list, `addedBy: tenant` |

All three are written by the director as commits, through the commands in §5. A tenant never sees
another tenant's catalogue: it is not listed, and a coordinate that names it is refused in the same
words as a catalogue that does not exist.

## 2. The format

### `profiles/<name>.yaml`

- **One `ComponentProfile` and nothing else.** A file with a second YAML document is refused.
- **`metadata.name` equals the file name** without `.yaml`. `profiles/acme-notes.yaml` holds the
  profile named `acme-notes`.
- **At most 180 KiB.** The director carries the file's bytes beside the profile so the operator can
  check them at rollout, and that is what fits.
- It must not carry the annotations `gentianos.io/profile-bundle` or `gentianos.io/catalogue-origin`.
  The platform writes those.
- The name is a DNS label: lower-case letters, digits and hyphens.

What goes into a profile is described in [design/app-profiles.md](design/app-profiles.md) and, for
customizing an app, in [app-customization.md](app-customization.md).

### `index.yaml`

```yaml
entries:
- name: acme-notes          # the profile's metadata.name, and the file name
  version: 1.0.0            # optional; shown in listings
  edition: pe               # ce, pe, me or ee; absent means ce
  digest: sha256:75b75bc45c9d3266a4ffd4eb78d286662bd3bfd870d50d9a9636c0f6b09d74be
  trustTier: experimental   # optional; repeats the profile's spec.trustTier
```

- **`digest` is the sha256 of the bytes of `profiles/<name>.yaml` exactly as served**, written
  `sha256:<64 lower-case hex characters>`. `sha256sum profiles/acme-notes.yaml` gives the number.
- A cluster lists the **`ce`** and **`pe`** entries. `pe` (private edition) is the one for your own
  apps; `me` and `ee` entries are counted and left to the App Store.
- An entry without a valid digest is listed and cannot be installed from the listing: there is
  nothing to pin the install to.
- The index is at most 1 MiB. The director keeps a fetched index for five minutes.
- A catalogue without an `index.yaml` works, but cannot be browsed: an entry is then installed by
  naming the catalogue and the digest by hand (`--from` and `--digest`, §5).

Other files beside these (`listings/`, an `index.html`) are ignored by the cluster.

## 3. The worked example: how the default catalogue is produced

The default catalogue, `gentian`, is `https://gentian-org.github.io/gentian-apps`. It is built from
the [gentian-apps](https://github.com/gentian-org/gentian-apps) repository:

1. The repository keeps each app as a directory, `profiles/[<family>/]<name>/`, holding
   `profile.yaml` and optionally `listing.yaml`.
2. `scripts/build-catalogue-source.py` turns that into the served shape. For every
   `profiles/**/profile.yaml` it copies the file to `dist/catalogue/profiles/<metadata.name>.yaml`,
   takes the sha256 of the copy, and writes `dist/catalogue/index.yaml`. The edition is the
   `edition:` of `listing.yaml`, else the suffix of the name (`-ce`, `-pe`, `-me`, `-ee`), else `ce`.
   It refuses a profile without a name, two profiles with the same name, and a kind other than
   `ComponentProfile`, and then exits non-zero.
3. The workflow `.github/workflows/apps-ci.yaml` runs the script with `--check` on every push, and
   on `main` the job `publish-catalogue` runs it for real and publishes `dist/catalogue` to GitHub
   Pages (`actions/configure-pages`, `actions/upload-pages-artifact`, `actions/deploy-pages`). The
   repository's Pages source is set to "GitHub Actions".

So the catalogue follows `main` of that repository, and each published file has a digest in the
published index.

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

### 4.2 Generate the index

Take the build script from gentian-apps and run it:

```bash
curl -fsSLo scripts/build-catalogue-source.py \
  https://raw.githubusercontent.com/gentian-org/gentian-apps/main/scripts/build-catalogue-source.py
python3 scripts/build-catalogue-source.py --out dist/catalogue
```

```
Catalogue source at dist/catalogue: 1 entries (1 pe)
  a cluster browsing this source lists 1; the other 0 are the App Store's to present.
```

`dist/catalogue` now holds `index.yaml`, `profiles/acme-notes.yaml` and `listings/acme-notes.yaml`.
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
kubectl gentian tenants delegate-catalogues demo on        # the cluster's administrator
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant demo
```

Delegation is off for every tenant until it is turned on, and `off` turns it off again. A tenant's
administrator removes only catalogues the tenant added.

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

A changed profile is a new build with a new digest. Publishing it changes nothing on any cluster:
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

## 7. What is checked, and what is not

**The catalogue is not trusted.** It is a web server, possibly yours, possibly compromised.

- **The digest pins the profile.** The director refuses bytes that do not hash to the digest the
  install named, and commits nothing. The operator checks again before every rollout: the profile in
  the cluster must be what the committed bytes say.
- **The digest does not pin the chart or the images the profile names.** A profile says
  `chart: {repository, name, version}`; what that registry serves under that version is the
  registry's to decide. Use immutable chart versions and image digests in your chart if you need the
  same guarantee further down.
- **The address is checked**, when the catalogue is added and again on every fetch: https only,
  port 443, no user name or password, and a host that resolves to public addresses only. Loopback,
  private and link-local ranges, carrier-grade NAT, metadata addresses, and names inside a cluster
  (`*.svc`, `*.cluster.local`, single-label names) are refused. Redirects are not followed.
- **A name is one profile.** A profile from a tenant's catalogue is refused when a profile of that
  name already exists from another origin — a catalogue of the cluster, another tenant's catalogue,
  or a component the platform ships (`desktop`, `concierge`, `admin-console`). The refusal says the
  name is taken and to publish it as `<tenant>-<name>`. A cluster catalogue's entry is refused the
  same way when a tenant's catalogue already holds the name.
- **A tenant's own profile is installable only in that tenant.** The director refuses anybody else,
  and the operator refuses to roll out a Component in another tenant from it
  (`ProfileOfAnotherTenant`).
- **Nothing here is a licence check.** Whether an app arrives is decided where its chart and images
  are pulled.

An app or an add-on named without a build — `{"profile": …}` with no coordinate and digest — is
installed only when its profile is already on the cluster. Otherwise the director refuses and says to
give the coordinate and the digest.

## 8. Private charts and images

The catalogue is public; the chart and the images need not be. Declare the registry for the tenant,
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

- **Public https addresses only.** A catalogue inside the cluster, on a private network, on another
  port, or behind a redirect is not fetched from. An air-gapped cluster cannot use an in-cluster
  catalogue today.
- **Profiles only.** A catalogue serves `ComponentProfile`s and nothing beside them. A profile that
  needs other objects on the cluster — a Composition of its own (`spec.package.composition`), an
  `OIDCPackCatalog`, `Customization` records — does not bring them; they have to be put there
  separately. A profile delivered by a chart alone needs none.
- **No password-protected catalogues.** No credential is sent to a catalogue. Keep what is private in
  the registry (§8), not in the profile.
- **No proxy.** The director connects to the catalogue directly.
- **No console screen.** Catalogues are managed with the commands above.
- **Profiles are not removed automatically.** A profile stays in `clusters/<cluster>/catalogue/`
  after the last uninstall, because the data of an uninstalled app is purged with its profile.
- **Removing a catalogue does not remove what was installed from it**, and does not free the profile
  names it used.
