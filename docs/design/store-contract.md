# The App Store contract

The App Store runs outside the cluster, on infrastructure the cluster does not
trust and never calls. This is everything that crosses the boundary, in both
directions. It is the interface a store implementation is written against.

This specification is licensed under Apache-2.0, so that anyone may implement
it; see [LICENSING.md](../../LICENSING.md).

## 1. Direction of trust

| | |
|---|---|
| The cluster calls the store | never |
| The store calls the cluster | never with an identity of its own. Requests reach the director from the signed-in person's browser, with that person's token |
| What the store can state | which build an entry is: its coordinate and the content digest of its profile bundle |
| What the store can read | whatever the signed-in person may read, through the director's read API |
| What the store can never supply | an artefact. Charts, images and profile bundles come from the catalogue source, by digest |
| What the store can never decide | whether a tenant may install. The cluster holds no key of the store's and verifies no statement from it |

The store may trigger and may read. It may not supply and may not decide for a
tenant.

## 2. No entitlements

The cluster does no licence gating. There is no statement by which a store
tells a cluster that a tenant may have an app, no key of a store's pinned on a
cluster, and no relation in the authorization model for a tenant's right to a
catalogue entry.

This is a decision, not an omission. Whether a tenant has paid for something
is the business of whoever sells it, and it is enforced where the thing sold
is handed over: at the repository the app's artefacts are pulled from. A
tenant that may have a licensed app holds a credential for that app's source
repository; a tenant that may not, does not, and the install it commits pulls
nothing. The platform does not reproduce that decision in a second place,
where it could only be a copy that drifts from the one that counts — and
where a platform that wished to run without any store would have to switch a
gate off rather than simply not have one.

What the platform does decide is who may change a tenant: installing is asked
of the person (§3). And it decides *which build* is installed, by content
digest (§4).

## 3. Installing

```
POST /v1/tenants/{t}/apps/{profile}
{"coordinate": "<catalogue>/<app>", "digest": "sha256:<64 hex>"}
```

with the person's token. The director asks one question: may this person
install apps in this tenant (`can_install_app` on `tenant:<t>`). Nothing is
asked about the app or the tenant's right to it.

The sequence a store drives is: the tenant's administrator asks the store for
the app; the store does whatever it does — a checkout, a contract, nothing at
all for a free entry — and confirms with the entry's coordinate and digest;
the desktop asks the director to install with exactly those (§7). The
confirmation is the store's own affair and is not shown to the cluster.

| Field | Rule |
|---|---|
| `coordinate` | `<catalogue>/<app>`, optional. When the catalogue is one of the cluster's sources (§6), the profile bundle is fetched from it and committed before the install; the app named must be the `{profile}` in the path |
| `digest` | optional, except when the bundle is fetched from a source: then it is required. Stated with or without capitals; recorded as `sha256:<lowercase hex>` |

| Answer | Meaning |
|---|---|
| `202 {"status":"installed","commit":…}` | committed to `gentian-deployments` |
| `202 {"status":"updated","commit":…}` | the app was installed already, at another digest; the pin moved |
| `200 {"status":"already_installed"}` | nothing to change |
| `400` | the digest is not a sha256 digest, a source install carries none, or the coordinate names another app |
| `403` | the caller may not install in this tenant |
| `404` | the source does not serve this entry |
| `502` | the source could not be read, or served bytes that do not hash to the digest. Nothing was installed |

## 4. The digest

The digest pins the build. It is the sha256 of the profile bundle, and it
travels with the request: the store's confirmation carries it, and the
cluster's own listing of a source gives it (§6).

It is not signed and it is not a permission. What it does:

* **It makes the source untrusted.** The bytes come from the catalogue source,
  which may be any web server. If they do not hash to the digest requested the
  install is refused and nothing is written, so a compromised source can fail
  an install and cannot change what is installed.
* **It is recorded.** The director writes it into the tenant's manifest as a
  field of the app's entry, beside the profile's name and never as part of it:

  ```yaml
  spec:
    apps:
    - profile: nextcloud-base-ce
      digest: sha256:…
  ```

  The name is what the app is across every build of it — its group, its object
  in the authorization store and its route are spelled from the name — and the
  digest says which build. The operator carries it onto the app's Component
  (`spec.profileRef.digest`). The commit that materialised the bundle names
  the digest as well.

Who states the digest is whoever may install: a person with `can_install_app`
can name any build of any entry a source serves. That is the same authority
they already hold over the tenant, and every use of it is a commit with their
name on it.

## 5. Reading

The store renders what a tenant has from two services, with the person's
token, filtered by what that person may see. What git declares is the
director's to answer; what the cluster holds right now is the usher's:

```
director   GET /v1/tenants/{t}/apps                  installed profiles, their digests and their addons
director   GET /v1/tenants/{t}/apps/{p}/addons
usher      GET /v1/tenants/{t}/apps/status           what the cluster made of them
usher      GET /v1/tenants/{t}/resources             the plan, and what is used of it
```

Reads are authorised by `can_view`, never by the write relation.

`/apps` answers from git: what the tenant is meant to have. `/apps/status` is
the operator's answer, relayed by the usher: each app is `installing`, `ready` or
`failing`, with the reason in the reconciler's own words, and carries what
its pods reserve against the tenant's plan. `failing` is a
workload that cannot start — an image that cannot be pulled, a container that
keeps exiting — which Kubernetes retries for ever and which therefore reads as
"still installing" to anything that only looks at readiness.

Two things can be done to an app that are not a change to what the tenant is,
and so are actions rather than commits; the person is named on the request:

```
POST /v1/tenants/{t}/actions/purge-app       {"profile": "<name>"}   can_install_app
POST /v1/tenants/{t}/actions/provision-app   {"profile": "<name>"}   can_grant
```

A purge deletes what an uninstalled app left behind — databases, object
storage, secrets. It is refused with `409` while the tenant still has the app
or while the cluster is still taking it down, so removing an app never takes
its data with it by accident. Provisioning grants an installed app — or an
add-on switched on inside one, by its own name — to everybody who is a member
now, and marks it granted by default to whoever joins later.

## 6. Without the store

The store is the default path to an app and the path of least resistance. It
is not a gate on the mechanism.

A **catalogue source** is a repository of profile bundles, named on the
Cluster claim:

```yaml
catalogue:
  storeUrl: https://…      # where people are sent for everything else
  sources:
  - name: gentian          # listed; open to no tenant until one is named
    url: https://…
  - name: in-house         # a platform administrator's own repository
    url: https://…
    tenants: [demo]        # the tenants this source is open to; empty means none
```

A source publishes `index.yaml` beside its `profiles/` directory, because an
https server does not list a directory and without it a cluster can install
from a source by name but cannot say what is in it. The index is the
technical half and nothing else: name, version, edition, trust tier, digest.

The director serves it — `GET /v1/tenants/{t}/catalogues` and
`GET /v1/tenants/{t}/catalogues/{source}/entries`, both under `can_view` — so
a cluster can answer what it holds with no store connection at all.

Two rules make that view the fallback rather than a rival to the store, and
they are the point rather than an omission:

* **Only `ce` and `pe` are listed.** They are the entries whose value does not
  depend on a supplier — community, and the operator's own. `me` and `ee`
  exist because somebody maintains or licenses them; the answer is returned as
  a count and `storeUrl`, not as rows.
* **Nothing a shop would show.** No display name, description, icon or price.
  The store keeps those current and a cluster copying them would go stale.

Every listed entry carries its digest, which is what an install of it from
this list sends back (§3).

**Open** is the one thing a source says about tenants. A source is open to the
tenants named on it: for them the listing marks its entries `installable`, and
the desktop offers the install from the cluster's own screen. For any other
tenant the entries are listed and the store is where to go. Opening a source
is the platform administrator's act, under `can_configure`, and it is recorded
as a tuple (`catalogue_source:<source>#open@tenant:<t>`) the operator projects
from the claim — the same path as the cluster's roles, and declarative the
same way, so a tenant the claim stops naming loses it on the next pass.
Nothing is open by default, and a source is opened per tenant, not per
cluster.

Open is what the cluster offers, not a licence, and the install route does not
ask it (§3). There is no access mode a store sets on a source.

What does not change without the store: the install mechanism (fetch the
bundle at the digest, apply the profile, commit as the person), the
attribution, the audit trail, and every check on the profile itself — a
side-loaded profile meets the same CEL rules, admission policies and
privilege approvals as one the store lists. Only the questions the store
answers — presentation and payment — go unanswered.

## 7. The desktop bridge

The store holds no credential for a cluster (§1), and the person's token is
forwarded to the desktop and to nothing else (AD-13). So the store is shown in
a window on the desktop, and what it needs of the cluster it asks of the
desktop, which asks the director as the person sitting at it. §3 and §5 are
what is asked; this is how.

The wire is `window.postMessage` between the store's page and the desktop
that framed it:

```
store → desktop   {"gentian":"store-bridge","v":1,"id":"<id>","op":"<op>","args":{…}}
desktop → store   {"gentian":"store-bridge","v":1,"id":"<id>","ok":true,"status":200,"data":{…}}
                  {"gentian":"store-bridge","v":1,"id":"<id>","ok":false,"status":403,"error":"…"}
```

| Operation | Asks the director, or the usher where named | Confirmed by the person |
|---|---|---|
| `context` | — (cluster, tenant, the caller's relations, their language) | |
| `apps.list` | `GET /apps` | |
| `apps.status` | usher: `GET /apps/status` | |
| `addons.get` | `GET /apps/{p}/addons` | |
| `resources.get` | usher: `GET /resources` | |
| `catalogues.list`, `catalogues.entries` | `GET /catalogues`, `GET /catalogues/{s}/entries` | |
| `apps.install` | `POST /apps/{p}` with the coordinate and the digest | yes |
| `apps.uninstall` | `DELETE /apps/{p}` | yes |
| `apps.purge` | `POST /actions/purge-app` | yes |
| `apps.provision` | `POST /actions/provision-app` | yes |
| `addons.set` | `PUT /apps/{p}/addons` | yes |

Four rules, and each is enforced by the desktop rather than asked of the
store:

* **One origin.** A message is read only if it comes from the origin of
  `catalogue.storeUrl` on the Cluster claim, and only from a frame the desktop
  itself opened. Which store may ask anything of a cluster is recorded in git.
  A cluster that names no store listens to nothing.
* **A closed list.** The table is all there is. No operation takes a path or
  a method, so there is no request the store can phrase that reaches another
  route of the director.
* **Names are names.** A profile or a catalogue lands in a URL path and is
  matched against what a name may be before anything is sent — in the browser
  and again in the desktop's backend.
* **Writes are confirmed on the cluster's origin.** Each waits for the person
  to say yes in a dialog the desktop draws. A page in a frame can ask for an
  install; it cannot press the button. A refusal answers with status `499`.

The checkout runs in a tab of its own, because a sign-in page will not be
framed. What it produces — the confirmation, with the entry's coordinate and
digest — it hands to the framed page, and the framed page is what asks for the
install: a tab the person opened has the store's origin and is still not a
frame of the desktop's, so it is not listened to.

Opened on its own, outside a desktop, the store has nobody to ask. It lists
and sells, and says that installing is done from the desktop.

