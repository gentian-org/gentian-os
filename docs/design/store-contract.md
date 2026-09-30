# The App Store contract

The App Store runs outside the cluster, on infrastructure the cluster does not
trust and never calls. This is everything that crosses the boundary, in both
directions. It is the interface a store implementation is written against.

## 1. Direction of trust

| | |
|---|---|
| The cluster calls the store | never |
| The store calls the cluster | never with an identity of its own. Requests reach the director from the signed-in person's browser, with that person's token |
| What the store can assert | signed **entitlement statements**, believed because of the key that signed them |
| What the store can read | whatever the signed-in person may read, through the director's read API |
| What the store can never supply | an artefact. Charts, images and profile bundles come from the catalogue source, by digest |

The store may trigger and may read. It may not supply and may not decide for a
tenant.

## 2. Entitlement statements

A statement is a compact JWS (RFC 7515), algorithm `EdDSA` (Ed25519), with the
store's key id as `kid` in the protected header.

```json
{
  "iss": "https://store.example",
  "aud": "cluster:<cluster id>",
  "sub": "tenant:<tenant name>",
  "jti": "<the store's id for this statement>",
  "iat": 1790000000,
  "exp": 1821536000,
  "coordinate": "<catalogue>/<app>",
  "granted": true,
  "reason": "",
  "seats": 25
}
```

| Claim | Rule |
|---|---|
| `aud` | must be this cluster. A statement for another cluster is refused whoever signed it |
| `sub` | must be the tenant in the request path |
| `jti`, `iat` | required. `iat` orders statements about the same entry: **the newest recorded one wins**, and an older one is refused as stale — which is what keeps a grant, delivered again after the revocation that followed it, from bringing the entitlement back. `iat` may not lie in the cluster's future |
| `exp` | required when `granted` is true, and later than `iat`. It becomes the tuple's condition; an expired grant is recorded and entitles to nothing |
| `granted: false` | a revocation or a denial. `reason` is required |
| `coordinate` | `<catalogue>/<app>`, the object entitlement is checked against |

**Keys are pinned, not fetched.** The cluster believes the keys listed on its
Cluster claim (`store.signingKeys`, changed only under `can_configure`) and no
others. A key that could be fetched at verification time would make whoever
controls the fetch the issuer. Rotation is adding the new key to the claim
before the store signs with it, and removing the old one after.

## 3. Delivery

```
POST /v1/tenants/{t}/entitlements
{"grant": "<compact JWS>"}
```

| Statement | Who must deliver it | Why |
|---|---|---|
| grant | a person with `can_install_app` on the tenant, with their token | a grant adds access. The store says the tenant *may*; someone who may install for the tenant says it *does* |
| revocation | anyone; no token | it only removes access, and its authority is the signature. Requiring the tenant's administrator to deliver it would let them decline to |

| Answer | Meaning |
|---|---|
| `202 {"status":"recorded","commit":…}` | committed to `gentian-deployments` and reflected in the authorization store |
| `200 {"status":"unchanged"}` | this very statement is already the recorded one. Safe to repeat: a delivery that failed half-way is completed by delivering again |
| `401` | not verifiably the store's — or, for a grant, no valid token |
| `403` | for another cluster or tenant — or, for a grant, the caller may not install here |
| `409` | a newer statement about this entry is already recorded |
| `400` | the statement is incomplete |

The director records the latest fact per entry in
`clusters/<cluster>/tenants/<tenant>/entitlements.yaml`; history is the git log,
and the authorization store is rebuilt from that file. A grant is committed
before its tuple is written and a revocation removes the tuple before it is
committed, so a failure in between always leaves less access, never more.

Revocation governs install and upgrade. It does not stop a running app: a
billing event should not take a tenant's data offline.

## 4. Installing

```
POST /v1/tenants/{t}/apps/{profile}
{"coordinate": "<catalogue>/<app>"}
```

with the person's token. The director asks two questions: may this person
install in this tenant (`can_install_app`), and is this tenant entitled to this
entry now (`can_install`, with the current time). A cluster with no store sets
`DIRECTOR_ENTITLEMENTS=off`, explicitly, and the second question is not asked.

## 5. Reading

The store renders cluster state from the director's reads, with the person's
token, filtered by what that person may see:

```
GET /v1/tenants/{t}/apps                  installed profiles and their addons
GET /v1/tenants/{t}/apps/status           what the cluster made of them
GET /v1/tenants/{t}/apps/{p}/addons
GET /v1/tenants/{t}/resources             the plan, and what is used of it
GET /v1/tenants/{t}/entitlements          the recorded facts
```

Reads are authorised by `can_view`, never by the write relation.

`/apps` answers from git: what the tenant is meant to have. `/apps/status` is
the operator's answer, relayed: each app is `installing`, `ready` or
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
  - name: store            # the store's own catalogue; entries need a grant
    url: https://…
    access: entitled
  - name: in-house         # a platform administrator's own repository
    url: https://…
    access: open
    tenants: [demo]        # which tenants may install from it; empty means none
```

A source publishes `index.yaml` beside its `profiles/` directory, because an
https server does not list a directory and without it a cluster can install
from a source by name but cannot say what is in it. The index is the
technical half and nothing else: name, version, edition, trust tier, digest.

The director serves it — `GET /v1/tenants/{t}/catalogues` and
`GET /v1/tenants/{t}/catalogues/{source}/entries`, both under `can_view` — so
a cluster can answer what it holds with no store connection at all.

Three rules make that view the fallback rather than a rival to the store, and
they are the point rather than an omission:

* **Only `ce` and `pe` are listed.** They are the entries whose value does not
  depend on a supplier — community, and the operator's own. `me` and `ee`
  exist because somebody maintains or licenses them; the answer is returned as
  a count and `storeUrl`, not as rows.
* **Nothing a shop would show.** No display name, description, icon or price.
  The store keeps those current and a cluster copying them would go stale.
* **An entitled source's digest is dropped** before it reaches a caller. There
  the digest that governs is the one the store stated over its own TLS; a
  source's own number checked against the same source's own bytes is not a
  check. An open source's digest IS served, because the trust there is the
  claim naming the source and no store is in the picture.

When the store is reachable it is where people go, and it adds the listings
and the entitlements on top of the same coordinates.

Whether an entry may be installed is one question with two answers
(`catalogue_entry#can_install`): the store granted it, or the entry's source
is open to the tenant. Opening a source is the platform administrator's act,
under `can_configure`, recorded as a tuple the operator projects from the
claim — the same path as the cluster's roles, and declarative the same way,
so a tenant the claim stops naming loses the access on the next pass.
Nothing is open by default, and a source is opened per tenant, not per
cluster. Which catalogue serves an entry (`catalogue_entry#source`) is a
fact about the coordinate's own spelling and is recorded by the director the
first time somebody installs it, because a cluster holds no catalogue to
enumerate up front (AD-3).

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
desktop, which asks the director as the person sitting at it. §4 and §5 are
what is asked; this is how.

The wire is `window.postMessage` between the store's page and the desktop
that framed it:

```
store → desktop   {"gentian":"store-bridge","v":1,"id":"<id>","op":"<op>","args":{…}}
desktop → store   {"gentian":"store-bridge","v":1,"id":"<id>","ok":true,"status":200,"data":{…}}
                  {"gentian":"store-bridge","v":1,"id":"<id>","ok":false,"status":403,"error":"…"}
```

| Operation | Asks the director | Confirmed by the person |
|---|---|---|
| `context` | — (cluster, tenant, the caller's relations, their language) | |
| `apps.list` | `GET /apps` | |
| `apps.status` | `GET /apps/status` | |
| `addons.get` | `GET /apps/{p}/addons` | |
| `resources.get` | `GET /resources` | |
| `entitlements.list` | `GET /entitlements` | |
| `catalogues.list`, `catalogues.entries` | `GET /catalogues`, `GET /catalogues/{s}/entries` | |
| `apps.install` | `POST /apps/{p}` with the coordinate and the digest | yes |
| `apps.uninstall` | `DELETE /apps/{p}` | yes |
| `apps.purge` | `POST /actions/purge-app` | yes |
| `apps.provision` | `POST /actions/provision-app` | yes |
| `addons.set` | `PUT /apps/{p}/addons` | yes |
| `entitlements.deliver` | `POST /entitlements` with the statement | yes |

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
framed. What it produces — the grant — it hands to the framed page, and the
framed page is what delivers it: a tab the person opened has the store's
origin and is still not a frame of the desktop's, so it is not listened to.

Opened on its own, outside a desktop, the store has nobody to ask. It lists
and sells, and says that installing is done from the desktop.

