# The App Store contract

A store is a service outside the cluster, run by a vendor, on infrastructure
the cluster does not trust. It holds the **data** about apps: what they are,
what they look like, what people say about them, which versions exist and
the digest of each, what they cost, and what a tenant has acquired.

The store's **interface** is not at the store. It is an app on the cluster,
the **App Store app**: it fetches that data, renders it, and does the
installing on the cluster's side, as the person signed in. Everything that
happens when a button is pressed happens on the cluster.

This document is the contract between the two: who calls whom, what a store
must serve, what the cluster does with each answer, and what a store is
trusted with. The format of every request and answer is in
[plans/artefacts/](../plans/artefacts/):

| | |
|---|---|
| [store-api.openapi.yaml](../plans/artefacts/store-api.openapi.yaml) | The store API, machine-readable. **Normative**: where this document or the next differs from it, the file is right |
| [store-api.md](../plans/artefacts/store-api.md) | The same in prose, field by field, with examples. It can be handed to whoever builds a store |
| [licence-report.openapi.yaml](../plans/artefacts/licence-report.openapi.yaml) | The report a cluster sends about itself, which a store depends on (§6.2). It is spelled out in [operations.md §6.2](operations.md) |

This specification and the three format files in `plans/artefacts/` are under
MPL-2.0, like the rest of the repository; see
[LICENSING.md](../../LICENSING.md).

## 1. Direction

| | |
|---|---|
| The cluster calls the store | yes. The App Store app's backend, outbound, over HTTPS, to the one address the Cluster claim names (`catalogue.storeUrl`) |
| The store calls the cluster | never. It holds no credential for a cluster, a cluster exposes no endpoint to it, and nothing in the API needs it to |
| A page of the store's inside the cluster's interface | never. The store's content arrives as data and is rendered by the cluster's own app. No frame, no script, no message passed between a store page and the cluster |
| What the store can state | what an app is, and which build an entry is: its coordinate and the content digest of its profile bundle |
| What the store can hand over | a credential for the repository an app's artefacts are pulled from, which that repository checks |
| What the store can never supply | an artefact. Profile bundles come from a catalogue the cluster or the tenant declares (§9), by digest; charts and images from where the bundle says |
| What the store can never decide | whether a person may install. The cluster holds no key of the store's and verifies no statement from it |

The cluster asks and the store answers. The store may describe and may hand
over a credential for its own repository. It may not supply and may not
decide for a tenant.

## 2. No entitlements

The cluster does no licence gating. There is no statement by which a store
tells a cluster that a tenant may have an app, no key of a store's pinned on
a cluster, and no relation in the authorization model for a tenant's right to
a catalogue entry.

This is a decision, not an omission. Whether a tenant has paid for something
is the business of whoever sells it, and it is enforced where the thing sold
is handed over: at the repository the app's artefacts are pulled from. The
store mints a credential for that repository when a tenant acquires an app;
the repository checks it on every pull. A tenant that holds the credential
gets the app; a tenant that does not installs something that pulls nothing.
The platform does not reproduce that decision in a second place, where it
could only be a copy that drifts from the one that counts — and where a
platform that wished to run without any store would have to switch a gate
off rather than simply not have one.

What the platform does decide is who may change a tenant: installing is asked
of the person (§3). And it decides *which build* is installed, by content
digest (§4).

**How a credential is used.** The tenant declares the repository — a
`Repository` of `type: oci` — through the director, under a name the
cluster's side chooses (for a store's repository the App Store app computes
it, §6.4), and sets its user name and password at the custodian. From that
the platform makes one Secret,
`repository-<name>-pull`, in that tenant's app namespace and in no other. The
namespace is selected by an exact match on the tenant the director recorded
from the route the caller was authorised on, not by anything a declaration
can carry.

| The chart's address | What happens |
|---|---|
| lies inside exactly one repository the tenant declared — same host and port, the repository's path a prefix by whole path segments | the chart is pulled with that repository's Secret |
| lies inside more than one | the app is not installed until the tenant narrows or removes one. The platform does not guess which credential is meant |
| lies inside none | the chart is pulled without a credential |

The tenant's apps are given all of the tenant's pull Secrets for their
images, after the cluster's own.

**What this does not guarantee.** Holding the credential is what makes a
private app arrive. It is not a guarantee that the app cannot be run
elsewhere on the same cluster:

* **A credential is a user name and a password.** A token with no user name
  fails at the chart installer.
* **Images are pulled with it only through the chart.** The credential
  reaches a pod through a chart that takes `imagePullSecrets` or
  `global.imagePullSecrets`. A chart that takes neither pulls its images
  without it.
* **A pulled chart is shared.** The chart installer is one process for the
  cluster and caches charts by name and version. A chart one tenant has
  pulled can be installed by another tenant whose profile names the same
  chart, until that process restarts. The credential is not disclosed; the
  chart is.
* **A pulled image is shared.** An image already present on a node can be
  started by any pod that does not force a pull.

## 3. Installing

```
POST /v1/tenants/{t}/apps/{profile}
{"coordinate": "<catalogue>/<app>", "digest": "sha256:<64 hex>", "defaultGrant": true}
```

to the director, with the person's token. The director asks one question:
may this person install apps in this tenant (`can_install_app` on
`tenant:<t>`). Nothing is asked about the app or the tenant's right to it.
The route is the same whoever calls it — the App Store app with a store's
confirmation in hand (§6.4), or the command line with no store at all (§9).

**Installing for everyone.** Installing makes an app exist; who may open it is
the membership of the app's group, decided separately. `"defaultGrant": true`
makes the common answer — everyone — part of the install: it is written on the
app's entry in the same commit (`spec.apps[].defaultGrant`), and the operator
acts on it once the app's group exists. The people who are members of the
tenant at that moment are added to the group, and the group is marked so that
somebody invited later has the app pre-selected. The first is done **once per
install** and noted in `Tenant.status.defaultGrantedApps`, so a person an
administrator later takes out of the group stays out; the mark stays. Until it
is done, and if it fails, the Tenant carries the condition
`DefaultGrantsApplied=False` with the reason (`WaitingForAppGroup`,
`GrantFailed`) and the operator tries again. No caller has to wait for the app
and ask a second time.

Because this gives people access, a request carrying `"defaultGrant": true` is
asked a second question, `can_grant` on `tenant:<t>`, and must pass both; it is
refused with `403` before anything is fetched or written when it does not. The
realm is taken to be the tenant's name on this path, as it is for
`provision-app` (§8).

| Field | Rule |
|---|---|
| `coordinate` | `<catalogue>/<app>`, optional. The catalogue must be one the tenant sees (§9): a source of the whole cluster, or a catalogue of this tenant's own. The profile bundle is fetched from it and committed before the install. A coordinate in any other catalogue is refused. The app named must be the `{profile}` in the path |
| `digest` | travels with a `coordinate`, and only with one: required beside it, refused without it. Stated with or without capitals; recorded as `sha256:<lowercase hex>` |
| `defaultGrant` | optional boolean. `true` installs for everyone and needs `can_grant` as well; `false` states that access is given per person and removes the key from an entry that had it; absent leaves an installed app's entry as it is, so moving a pin does not change who may open the app |

**An install comes from a catalogue source the cluster declares,** for all
its tenants or for this one (§9). A pin is
recorded only for a bundle the director fetched from such a source and saw
hash to the digest, so the two fields travel together or not at all:

| The request carries | What happens |
|---|---|
| `coordinate` and `digest`, the catalogue a declared source | the bundle is fetched, checked, committed, and the install pinned to it |
| `coordinate` in a catalogue the cluster declares no source for | refused with `422`, naming the catalogue and the sources there are. Nothing is fetched or written |
| `digest` and no `coordinate` | refused with `422`: there is nowhere to fetch the bytes it is the digest of |
| `coordinate` and no `digest` | refused with `400` |
| neither | nothing is fetched and nothing pinned. It installs a profile the cluster already holds, and on an installed app it states `defaultGrant` and leaves the pin as it is |

| Answer | Meaning |
|---|---|
| `202 {"status":"installed","commit":…}` | committed to `gentian-deployments` |
| `202 {"status":"updated","commit":…}` | the app was installed already, and the pin moved or `defaultGrant` was stated with another value than the entry had |
| `200 {"status":"already_installed"}` | nothing to change |
| `400` | the digest is not a sha256 digest, a coordinate comes with none, the coordinate is not `<catalogue>/<app>`, or it names another app |
| `403` | the caller may not install in this tenant, or asked to install for everyone and may not grant in it |
| `404` | the source does not serve this entry |
| `422` | the coordinate's catalogue is not a source this cluster declares, or a digest came with no coordinate: the build could not be verified. Or the entry is larger than the bundle the cluster carries beside a profile (180 KiB), so its digest could not be checked at rollout. Or the bundle holds what a bundle may not: a kind that is not a companion, an object that is not this profile's, or any companion when the catalogue is a tenant's own. Nothing was installed |
| `502` | the source could not be read, or served bytes that do not hash to the digest. Nothing was installed |

**Add-ons.** An add-on is a profile of its own, activated inside an app the
tenant has installed. Which ones are active is one list, replaced whole:

```
PUT /v1/tenants/{t}/apps/{profile}/addons
{"addons": ["<name>", {"coordinate": "<catalogue>/<addon>", "digest": "sha256:<64 hex>"}]}
```

under the same question, `can_install_app`. Each entry is a name, or the
build to install the add-on at; the add-on an object names is the second half
of its coordinate. A pinned add-on is installed like a pinned app and under
the same rule: its catalogue must be a declared source, its bundle is fetched
from there, checked against the digest and committed beside the profile, and
the pin is written on the app's entry. Every bundle of a request is fetched
and checked before the first is committed, so a list one of whose builds
does not verify changes nothing. The refusals are those of the table above.

**An add-on is pinned only inside a pinned app.** A `{coordinate, digest}`
entry is accepted only when the app's own entry carries a `digest`.
Otherwise the request is refused with `422` before anything is fetched or
written, and the answer says how: install the app at a stated build first —
`kubectl gentian apps install <app> --tenant <t>` pins it — and set its
add-ons again. An add-on takes effect in the release of its base, so a
stated build of an add-on inside a base at no stated build would pin half
of what runs. Names inside an app that carries no digest are set as before.

| An entry | What happens |
|---|---|
| `{coordinate, digest}`, the app itself pinned | fetched, checked, committed and pinned (§4) |
| `{coordinate, digest}`, the app not pinned | the whole request is refused with `422`; nothing is fetched or written |
| a name the entry already activates | left as it is: an add-on that was pinned stays pinned |
| a name that is new to the list | activated unpinned, from the profile the cluster holds |
| an add-on no longer in the list | deactivated, and its pin removed with it |

## 4. The digest

The digest pins the build. It is the sha256 of the profile bundle -- the one
file a catalogue publishes for an entry, holding the profile and, after it,
the few other objects the app needs on a cluster, its companions
([custom-catalogues.md](../custom-catalogues.md) §2) -- and it travels with
the request: a store's confirmation carries it (§6.4), and so does a
catalogue source's own index (§9). The digest pins the profile and its
companions; it does not pin the chart or the images the profile names.
One bundle is one digest: there is none for a profile apart from its
companions. Companions are taken from a catalogue of the whole cluster only;
a bundle from a tenant's own catalogue that holds one is refused (§3).

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
* **It is checked again at rollout.** The director's check is on the bytes it
  fetched, before the commit. What reaches the cluster is not those bytes:
  Argo CD applies the document and the API server prunes and defaults it, so
  the digest cannot be recomputed from the profile the cluster holds. The
  director therefore commits the verified bytes a second time, beside the
  profile, as a kustomize patch (`catalogue/<name>.bundle.yaml`) that puts
  them on the profile in the annotation `gentianos.io/profile-bundle`,
  base64-encoded. The committed file itself stays byte for byte what the
  source served, and Argo CD applies every document in it: the profile and
  its companions.

  Before it renders anything for a Component whose `profileRef` carries a
  digest, the operator hashes the annotation's bytes itself and compares the
  result with that digest; then it reads those bytes as a profile -- the way
  kustomize reads the file, with the schema's defaults applied -- and
  compares the result with the profile in the cluster: the whole `spec`, and
  every label and annotation in the `gentianos.io/` namespace. Both have to
  hold. It takes nobody's word for the annotation: bytes that are another
  build do not hash to the pin, and bytes that are the pinned build beside a
  profile that says something else do not compare.

  The same bytes say what the bundle brings beside the profile, and the
  operator holds them to the list of what a bundle may bring, as the director
  did, and then reads each companion from the cluster and compares it with
  them. All of them have to be there and be what the bundle says.

  If either fails, the Component reports `Ready=False` with the reason
  `DigestMismatch` (naming the pinned digest and the one found, or the part
  of `spec` that differs), `DigestUnverifiable` (the profile carries no
  bundle, or one that cannot be read), `BundleRefused` (the bundle holds
  what a bundle may not), `CompanionMissing` or `CompanionMismatch` (naming
  the companion), and an event says the same. Nothing is
  rendered from that profile, and nothing already rolled out is removed: the
  component is held as it runs until the profile is the pinned build again or
  the pin is moved. An install with no digest is not checked.

  What this does not cover. Only the component reconciler is held; the other
  readers of a profile -- the tile, the app's groups, the authorization
  projection -- read it by name as before. One name is one profile for the
  whole cluster, so a tenant that installs an entry at a newer digest replaces
  the profile under every other tenant pinned to the older one, and those are
  then held with `DigestMismatch` until they move their pin. And the check is
  on the profile, not on what the profile points at: a chart or image it
  names by tag is whatever that tag is when it is pulled.

**An add-on's pin** is recorded beside the list of names, keyed by the
add-on's name, and never inside the list:

```yaml
spec:
  apps:
  - profile: odoo-base-ce
    digest: sha256:…
    addons:
    - odoo-crm-ce
    - odoo-sales-ce
    addonPins:
    - name: odoo-crm-ce
      digest: sha256:…
      catalogue: gentian
```

The operator carries it onto the add-on's own Component
(`spec.profileRef.digest`) and onto the Component of the app it is activated
in (`spec.addonPins`). The second is where it is enforced. An add-on deploys
nothing itself: it takes effect in the release of its base, from the list the
base hands on. So the base is what is held. Before it renders anything for an
app, the operator checks every pinned add-on the app activates the way it
checks the app's own profile; if one fails, the *app's* Component reports
`Ready=False` with `DigestMismatch` or `DigestUnverifiable`, naming the
add-on, and nothing is rolled out for the app until the add-on's profile is
the pinned build again or the pin is moved. The app is held whole rather
than rolled out without the add-on, because a release rendered without an
add-on that is already active switches it off. An add-on with no pin is not
checked.

Who states the digest is whoever may install: a person with `can_install_app`
can name any build of any entry a source serves. That is the same authority
they already hold over the tenant, and every use of it is a commit with their
name on it.

## 5. What a store serves

One API, versioned under `/v1`, at the address the Cluster claim names. Every
operation is a request from the cluster's side. The full definition — every
parameter, field and example — is in
[store-api.md](../plans/artefacts/store-api.md); the machine-readable form is
[store-api.openapi.yaml](../plans/artefacts/store-api.openapi.yaml), and it
wins where the two differ.

**Browsing needs no sign-in.** The API has two halves, and the line between
them is whether a call concerns a tenant. What a store offers is readable at
once, with no account. A person signs in to the store only for what concerns
their tenant.

| Operation | Token | Answers |
|---|---|---|
| `GET /v1/meta` | open | The store's name, the API version, and what is needed to sign a person in: issuer, client id, scopes. Also the origins images and checkout pages may be on |
| `GET /v1/categories` | open | The categories apps are filed under |
| `GET /v1/apps` | open | A page of apps, filtered by category, edition or text: coordinate, name, summary, icon, publisher, editions offered, trust tier, latest version, list price |
| `GET /v1/apps/{catalogue}/{app}` | open | One app in full: description, screenshots, versions each with its digest, release notes, requirements, add-ons, links, licence |
| `GET /v1/apps/{catalogue}/{app}/reviews` | open | Reviews — rating, text, the author's name as the store shows it, date — and their summary |
| `GET /v1/apps/{catalogue}/{app}/reports` | open | Evaluations and reports about the app: kind, title, issuer, date, summary, document address |
| `GET /v1/tenant` | `store.read` | Whether the store serves the tenant in the token; if not, the reason; and notices to show (§6.2) |
| `GET /v1/acquisitions` | `store.read` | What this tenant has acquired |
| `POST /v1/acquisitions` | `store.acquire` | Acquire an app. `201` with a confirmation; or `202` with a checkout address at the store; or `200` with the acquisition the tenant already has |
| `GET /v1/acquisitions/{id}` | `store.read` | The outcome: `pending`, `confirmed`, `cancelled` or `failed`; once confirmed, the confirmation. Its repository credential is in it only for a token that also carries `store.acquire` |
| `POST /v1/acquisitions/{id}/credential` | `store.acquire` | Replaces the repository credentials now (rotation) and answers the confirmation with the new ones |

**Open** means no token is needed, and the call is never answered `401` or
`403`. A token may be presented on a catalogue read; the store may then mark
what the token's tenant has acquired (`acquired`). That is optional
information, present only with a token and never required, and a token the
store cannot use is ignored there rather than refused.

`store.read` covers reading what concerns the token's tenant — its standing,
its acquisitions, and one acquisition with its confirmation, without any
repository credential. `store.acquire` covers changing what the tenant has
at the store — acquiring, and replacing a credential — and reading a
repository credential: no answer carries one to a token without it. No
scope covers the open reads: they need none.

What follows from the open half:

* **A refusal applies to the signed-in calls only.** A store that does not
  serve a tenant (§6.2) answers `tenant-refused` on the `/v1/acquisitions`
  operations. The catalogue stays readable — to that tenant, and to somebody
  who never signs in. A person learns at sign-in whether the store serves
  their tenant, not before.
* **Prices on open reads are list prices**, the same for everybody. What a
  tenant is charged is what the checkout says.
* **Open reads are cacheable.** An answer given without regard to a token
  carries `Cache-Control: public, max-age=…` and an `ETag`, and the app may
  ask again with `If-None-Match`. A signed-in answer is `no-store`.
* **Anonymous reads are rate-limited by client address**, signed-in calls by
  token, with `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`,
  and `Retry-After` on a `429`. An address is a cluster, not a person: all
  of a cluster's tenants reach the store from one outbound address.
* **The open half is open to anyone**, not only to clusters. A store cannot
  tell the two apart, and the catalogue is not a secret.

Common to all operations: answers in the language asked for
(`Accept-Language` in, `Content-Language` out, falling back within a language
and then to the store's default); lists paged by cursor; errors as
`application/problem+json` with a stable `code`. All of it is in the
artefact.

**What the store receives.**

* *On an anonymous read:* which categories, apps and pages are looked at,
  the language asked for, and the address the request comes from — the
  cluster's outbound address. Nothing else. The App Store app sends no
  cluster identifier and no tenant identifier on an anonymous call: no
  tenant URL, no cookie, no `Referer` or `Origin` naming a tenant, and a
  `User-Agent` that names the app and its version only. An anonymous read
  tells the store only where the request came from.
* *On a signed-in call:* in addition the person's store token, and thereby
  their store account and the tenant URL bound into it.
* *Never:* who is in the tenant, what is installed, or any configuration of
  the cluster. The only personal data that reaches a store is the store
  account the person signed in with.

## 6. The App Store app

A platform UI of its own, component `app-store`, installed per tenant. Its
tile is shown only to people who may install apps in that tenant. It is
absent on a cluster whose licence report is off (§6.2) and on one that names
no store.

**Where it is, and when.** The operator chart ships the component profile
`app-store`, and the operator places a Component of it on every tenant
except the platform tenant, where nobody installs apps, for as long as two
things hold: licence reporting is on, and the Cluster claim names a store
(`spec.catalogue.storeUrl`, an https address). When either stops holding the
Component is removed again — the workload only; the app owns no data — and
it is placed again when both hold. The operator watches the claim, so a
change there takes effect when Argo CD has applied it; the reporting setting
is the operator's own and changes with its rollout. The usher's answer
(`appStore` beside the tiles) is the same verdict, written by the operator,
and gives the reason when there is none: `licence-report-disabled`,
`no-store-configured` or `store-address-is-own-host`.

It answers at `store.<the tenant's base domain>`, behind the tenant's
session. A store's API must therefore not be given an address a cluster's
own App Store app could have — `store.<a domain a cluster is installed
under>` — which is why the installer's default for a new claim is
`https://store-service.aluvian.io` (`GENTIAN_STORE_URL` names another); a
cluster whose claim names the host of one of its own tenants' App Store app
offers no store and says `store-address-is-own-host`. Its routes and its tile ask the same question, `can_install_app` on
the tenant: a member who knows the address is refused at the edge. `/api`,
`/oauth/callback`, `/healthz` and `/readyz` reach its API with the person's
token forwarded; everything else reaches its pages. The profile tells it
where the director, the custodian and the usher are, its own host, and the
store's address from the claim.

Its pods may reach the control namespace, the edge, and TCP 443 on public
IPv4 addresses — not the cluster's own ranges, private networks or the
cloud metadata address. The last is wider than the store alone because a
store names its issuer and the origins of its pictures in its own metadata,
and a network policy matches addresses, not names; which hosts are asked is
the app's to restrict. It applies only while a store is offered.

It **shows the catalogue immediately**, with no store account: the catalogue
reads are open (§5), and the app makes them anonymously. It asks the person
to sign in to the store only when they acquire something or open what the
tenant has acquired.

It has two sides and holds them apart. Toward the store it is a client of
the API in §5 — anonymous for browsing, and with the token of the person's
store account for what concerns the tenant. Toward the
cluster it is a caller like any other: it asks the director and the
custodian with the token of the person's cluster account, and is subject to
the same checks as the command line. It has no authority of its own in
either direction.

### 6.1 Signing in to the store

Needed only to acquire and to see the tenant's acquisitions; never to
browse. The tenant's administrator signs in at the store's issuer, once,
with an account at the store — not the account they are signed in to the
cluster with. The result is a **store token**, which the App Store app uses
for what concerns the tenant: its standing and its acquisitions.

**Who acts for a tenant at the store.** A tenant is an organisation in the
store's records, and an account is a person; the two are separate things.
The tenant's administrator creates an account at the store's issuer, and
that account is associated with the tenant. By default exactly that one
account acts for the tenant. More may be allowed later; that is outside the
definition for now. An account that is not associated with the tenant is
refused by the store with the reason `tenant-not-claimed` (§6.2), and
learns nothing of what the tenant has acquired.

**What the store token is, and is not.** It authenticates the administrator
to the store, and is valid nowhere on the cluster. It is not what pulls
images: pulls use the tenant's registry credential, which the confirmation
carries (§6.4) and which the app hands to the cluster's custodian once.

The flow is OAuth 2.0 authorization code with PKCE against the store's
issuer. The app is a public client: it holds no client secret, because a
secret shipped to every cluster is not one. The request carries
`tenant_url`, the address of the tenant the app is installed in, and the
issuer binds it into the token. From then on every signed-in answer is
about that tenant.

The sign-in is held to three conditions:

1. **The backend exchanges the code and keeps the token.** The
   authorization code is exchanged by the App Store app's backend on the
   cluster, and the store token is kept there, bound to the administrator's
   cluster session and for no longer than it. It is never handed to the
   browser.
2. **The issuer redirects to exactly one address per tenant**, the App
   Store app's own callback:
   `https://store.<the tenant's base domain>/oauth/callback`. The app is
   component `app-store` of the tenant, with host label `store`; the
   tenant's base domain is `<tenant>.<cluster domain>` on a multi-tenant
   cluster, the cluster domain itself for the one user tenant of a
   single-tenant cluster, or the tenant's custom domain. No other host
   under the tenant's address receives a code, and no other path.
3. **Reading a repository credential needs the scope `store.acquire`.** A
   token with `store.read` alone is answered a confirmation without its
   credential, and the listing of acquisitions never carries one.

This is a sign-in flow run by a component behind the edge, which AD-13
rules out. It is the one exception AD-13 states, and the three conditions
are what make it acceptable: it is against an issuer outside the cluster,
yields a token valid at the store and nowhere on the cluster, and
establishes no session with the platform. The cluster's own sign-in stays
the edge's alone.

### 6.2 Standing, and the dependence on licence reporting

The store depends on the cluster's licence report
([operations.md §6.2](operations.md)). A cluster that does not report has no
App Store app: the usher says so and no tile is shown. That is a condition
on the app existing on a cluster, and it is unchanged by the catalogue being
open: the cluster decides it from its own setting, without asking the store.

A store's judgement of a tenant, by contrast, comes only at sign-in. Until
then the app shows the catalogue and no notice.

After each sign-in the app asks `GET /v1/tenant`, before the call the person
wanted. The store matches the tenant
URL in the token against the tenant URLs in the reports it has received.

| The store answers | The app |
|---|---|
| `served: true` | proceeds, and shows every notice it is given |
| `served: false`, with a reason — `no-reports-for-tenant`, `tenant-not-claimed`, … | shows the reason in the store's words and offers no acquiring. The catalogue stays readable |
| a notice `free-licence-limit` — the cluster has no subscription | shows it: the free licence covers clusters of fewer than 50 users that are not operated for resale |

**Nothing on the cluster is blocked by any of this.** A refusal means the
store will not let this tenant acquire. Installed apps keep running, and an app
can still be installed by command (§9).

### 6.3 Rendering

The store's content is data. The app renders text as text. Two fields — an
app's description and a version's release notes — are a restricted Markdown
subset defined in the artefact; everything else is plain. No HTML from a
store is interpreted, no script from a store runs, and no page of a store is
framed. Images are loaded by address from the origins the store's
`GET /v1/meta` names and from nowhere else. Links, report documents and the
checkout open in a separate window.

### 6.4 Acquiring and installing

```
person            App Store app                      store                      cluster
  │  browses            │  GET /v1/apps, …  (no token)  │                          │
  │────────────────────►│──────────────────────────────►│                          │
  │  "get this app"     │                               │                          │
  │  signs in to the store, once (§6.1); GET /v1/tenant │                          │
  │────────────────────►│  POST /v1/acquisitions        │                          │
  │                     │──────────────────────────────►│                          │
  │                     │  201 confirmation             │   free, or no checkout   │
  │                     │◄──────────────────────────────│                          │
  │                     │  — or —                       │                          │
  │                     │  202 {id, checkoutUrl}        │   paid                   │
  │                     │◄──────────────────────────────│                          │
  │  checkout, in a separate window, at the store       │                          │
  │────────────────────────────────────────────────────►│                          │
  │                     │  GET /v1/acquisitions/{id}    │                          │
  │                     │──────────────────────────────►│                          │
  │                     │  confirmed + confirmation     │                          │
  │                     │◄──────────────────────────────│                          │
  │  "install", and for everyone or not                 │                          │
  │────────────────────►│  declare the repository       (director)                 │
  │                     │─────────────────────────────────────────────────────────►│
  │                     │  set its credential           (custodian)                │
  │                     │─────────────────────────────────────────────────────────►│
  │                     │  install {coordinate, digest, defaultGrant}  (director)  │
  │                     │─────────────────────────────────────────────────────────►│
```

**Payment never passes through the cluster.** For a paid acquisition the
store answers with a checkout address; the app opens it in a separate
window, the person pays at the store, and the app asks the store for the
outcome. The store does not tell the cluster.

**The confirmation** is what the store hands over when a tenant has an app:

```jsonc
{
  "coordinate": "gentian/nextcloud-base-ee",
  "version": "31.0.4",
  "digest": "sha256:<64 hex>",
  "repository": {                    // absent for an app whose artefacts are public
    "type": "oci",
    "url": "oci://registry.store.example/apps",
    "credential": {"username": "…", "token": "…", "expiresAt": null}
  },
  "addons": [                        // each with coordinate, version, digest and optionally repository
  ]
}
```

| Field | What it is to the cluster |
|---|---|
| `coordinate` | which entry. Sent to the director as it is |
| `version` | for a person to read. The cluster does not pin it |
| `digest` | **what the cluster pins and verifies** (§4) |
| `repository.type` | `oci`, and nothing else: it is the only type a tenant's app charts and images are pulled from with a credential today (§2), and the only one accepted from a store (§7) |
| `repository.url` | the registry the bundle's chart and images are in. It must contain the chart's address — same host and port, its path a prefix by whole segments — and must not overlap another repository the tenant has declared (§2) |
| `repository.credential` | **opaque to the cluster.** A user name **and** a token, both required, which the store minted for this tenant and the store's repository checks on pull. The user name is one per tenant, chosen by the store. `expiresAt` is `null`: a credential does not expire, for now. Present only in an answer to a token with `store.acquire` |
| `addons` | add-ons the acquisition includes, each an entry of its own |

A free app's confirmation carries no repository and no credential.

**The cluster names the repository, not the store.** A confirmation carries
no name for it. The App Store app computes the name of the `Repository`
claim itself, from the tenant's name on the cluster (the `{t}` of the
director's routes) and the registry's address (`repository.url`):

```
address = url without the leading "oci://", lower-cased          host[:port][/path]
slug    = address with every run of characters outside [a-z0-9]
          replaced by one "-", then leading and trailing "-" removed
hash    = the first 8 hexadecimal characters, lower case, of
          SHA-256( tenant + LF + address )                       UTF-8; LF is the one byte 0x0A
stem    = tenant + "-" + slug, cut to its first 31 characters,
          then trailing "-" removed
name    = stem + "-" + hash                                      at most 40 characters
```

| Tenant | `url` | `tenant-slug` | Name |
|---|---|---|---|
| `acme` | `oci://registry.store.example/apps` | `acme-registry-store-example-apps` (32, cut to 31) | `acme-registry-store-example-app-991ae7e1` |
| `demo` | `oci://ghcr.io/example` | `demo-ghcr-io-example` (20) | `demo-ghcr-io-example-59b6b43d` |

What the rule gives, and why it is this one:

* **The same tenant and the same address always give the same claim**, so
  a second app from the same registry declares nothing new and sets the
  one credential again.
* **Two tenants never share a name, and neither do two addresses of one
  tenant.** The hash is always there, not only on a name that was cut: the
  readable part alone is ambiguous, because a hyphen is all a name has to
  separate with. Tenant `a-b` with `oci://c.example` and tenant `a` with
  `oci://b.c.example` both read `a-b-c-example`; they are
  `a-b-c-example-559df09f` and `a-b-c-example-67520272`. Should two names
  ever coincide all the same, nothing is overwritten: the director answers
  a name another tenant holds with `404`, and a name the tenant holds for
  another address with `428` (§7).
* **The name is a DNS label**, which is what the director accepts for one
  (lower-case letters, digits and hyphens, at most 63 characters). It
  starts with the tenant's name and ends with the hash, so it starts and
  ends with a letter or a digit.
* **40 characters, not 63**, because the cluster derives further names from
  it and each should itself fit in 63: the pull Secret
  `repository-<name>-pull` (56), the composite the claim is bound to,
  `<name>-<5 generated characters>` (46), and the credential requirement
  named after that composite, `repository-<name>-<5>` (57).

It is one function of two strings, to be implemented once, in the App Store
app, and tested against the two rows above.

**What the app does with it**, each step with the person's own cluster
token:

1. For each repository named: computes its name as above; declares it for
   the tenant at the director (`PUT /v1/tenants/{t}/repositories/{name}`,
   role `apps`), which commits the address; then sets its credential at the
   custodian (`PUT /v1/credentials/repository-{name}`), which writes the
   user name and the token to the vault. Both ask `can_write_credential` on
   the tenant. The app hands the credential over once and does not keep it.
2. Installs at the director (§3) with exactly the confirmation's
   `coordinate` and `digest`, and the `defaultGrant` the person chose.
3. For the add-ons the confirmation lists: sets the app's add-ons at the
   director (§3), each as the item's `coordinate` and `digest`. Step 2 has
   pinned the app, which is what the director requires of a pinned add-on.

The OS decides nothing about supply at any step. Whether the app then
arrives is the repository's answer to the credential.

**What ends a credential.** A repository credential does not expire, for
now: a store issues it with `expiresAt: null`, and nothing has to be
renewed, attended or not. Two things end one:

* **Rotation.** `POST /v1/acquisitions/{id}/credential` means "replace this
  credential now" — after a leak, say. The store answers the confirmation
  with a new token under the same user name; the app sets it at the
  custodian as above. The replaced one keeps working for 24 hours and is
  refused from then on, so there is no moment at which the cluster holds
  only a credential the repository refuses.
* **The acquisition ending.** A credential stops pulling an app whose
  acquisition has ended, and is refused altogether once it serves none.

A credential that has ended stops new pulls — a pod that is rescheduled
onto a node without the image does not start — and leaves what is running
untouched.

## 7. Trust

**The store's TLS identity is all that authenticates it.** The cluster
trusts whatever answers at `catalogue.storeUrl` with a certificate valid for
that name. Which store a cluster asks is a commit on the Cluster claim.
Nothing a store says is signed, and the cluster holds no key of a store's.

**Store content is data** (§6.3). The rule is what stops a store's words
from being instructions to the person's browser on the cluster's origin.

**The digest is what pins a build**, and the catalogue source — not the
store — is what serves it (§4).

What follows is what a store that has been compromised, or is hostile, can
and cannot do. It is reasoned from what the director and the operator do,
not from what the App Store app is expected to do.

**It can:**

* **Lie about everything it shows.** Descriptions, prices, publishers,
  reviews, reports, versions. It can show one app and confirm the coordinate
  of another. The bound on that is below.
* **Refuse, or be absent.** It can serve no tenant, or none of the apps.
* **Learn what the API is sent** (§5): from anonymous browsing, which apps
  are looked at from which address; after a sign-in, the tenant URL, the
  store account and what is acquired; and the addresses image loads come
  from.
* **Hand over any repository address and any token.** The cluster then holds
  a credential for that address and presents it there. The token is of the
  store's own making, so this discloses nothing of the cluster's.
* **Misuse its own pages.** The sign-in and the checkout are the store's,
  in a separate window. What a person types there, the store has.

**It cannot:**

* **Change which bytes are installed.** The director fetches the bundle from
  the catalogue source declared on the cluster's side under the name of the
  coordinate's catalogue (§9), at `profiles/<app>.yaml`. A source is addressed by name, not by
  digest: it serves one build of an entry. A digest the store invents
  therefore does not select another build — it matches what the source
  serves, or the install is refused with `502` and nothing is written. The
  same holds for the digest of an older build the source no longer serves.
* **Install something from a place of its choosing.** A coordinate in a
  catalogue the tenant sees no source for is refused by the director
  (§3), and so is a digest with no coordinate. No digest is recorded that
  the director did not check against bytes it fetched from a declared
  source.
* **Supply a profile through the repository.** The App Store app declares
  what a store names as an OCI registry with role `apps`, and refuses any
  other type. A *git* repository with role `apps` used to be a source of
  profiles that Argo CD copied into the cluster, with no digest involved.
  That copying is retired: the composition makes no ApplicationSet for such
  a claim, and the director refuses to declare one. The declaration
  an OCI registry gets carries a credential and names no content: what is
  pulled is what the verified bundle names.
* **Replace a repository the tenant already has.** A store names nothing
  on the cluster: the name is computed from the tenant and the address
  (§6.4), so another address is another name. Should a computed name all
  the same be one the tenant holds with another address, the director
  answers `428` and asks for the name to be repeated. The app does not
  repeat it on a store's word; it stops and shows the administrator what is
  asked. A name another tenant or the cluster holds is answered `404`.
* **Read the cluster, or act on it.** It has no way in and no credential.
* **Decide who may install**, or lift any check. Every step in §6.4 is asked
  of the person by the director or the custodian.

**The real bound on a lying store** is therefore this. It can bring an
administrator to install an entry they did not mean — but only an entry that
a catalogue source named on the cluster's side (by the platform
administrator, or by the tenant's own where that was delegated, §9) serves
right now, at
the build that source serves, by a person who holds `can_install_app`, in a
commit with their name and the coordinate on it. The app shows the
coordinate and the digest it is about to send, on the cluster's side, before
it sends them. Whatever the installed profile asks for beyond the baseline —
egress, a pod-security waiver, an elevated role — still waits for its own
approval (AD-5).

**Where the bound stops.** Three things it does not cover, each already a
property of the design rather than of this contract:

* **A store and a catalogue source run by the same party** are one trust,
  not two. The digest protects against a source that is compromised alone
  and a store that is compromised alone. A party that controls both can
  publish a bundle and name its digest. What stands then is what stands for
  any profile: its validation, its admission policies, and the approval of
  every privilege it asks for.
* **One cluster is one audience for what it has pulled.** Charts and images
  a tenant pulled with its credential can be used by other tenants of the
  same cluster (§2). The repository controls the first pull into a cluster.
  What runs on it afterwards is stated by the licence report, not enforced
  by the credential.
* **The repository decides what a tag is.** The digest pins the profile, not
  the chart or image the profile names by tag (§4). Whoever controls the
  repository an app is pulled from controls what runs. That is where supply
  was put on purpose (§2), and it is the cost of it.
* **A person can be misled.** The cluster verifies bytes, not intentions.
  The description that made somebody want an app is the store's word.

## 8. Administering what is installed

Not the App Store app's business, and not the desktop's. An app that is
installed is administered in the admin console's **Apps** tab
([admin-console.md](admin-console.md)), from the cluster's own
reads. The store is not asked: what is installed, how it is doing and who
may open it are facts the cluster holds.

```
director   GET /v1/tenants/{t}/apps                  installed profiles, their digests, their addons and each addon's pin
director   GET /v1/tenants/{t}/apps/{p}/addons       one app's addons, and addonPins: {name, digest, catalogue} for each that is pinned
usher      GET /v1/tenants/{t}/apps/status           what the cluster made of them
usher      GET /v1/tenants/{t}/apps/retained         which uninstalled apps still hold data
usher      GET /v1/tenants/{t}/resources             the plan, and what is used of it
```

Reads are authorised by `can_view`, never by the write relation.

`/apps` answers from git: what the tenant is meant to have. `/apps/status` is
the operator's answer, relayed by the usher: each app is `installing`, `ready` or
`failing`, with the reason in the reconciler's own words, and carries what
its pods reserve against the tenant's plan. `failing` is a
workload that cannot start — an image that cannot be pulled, a container that
keeps exiting — which Kubernetes retries for ever and which therefore reads as
"still installing" to anything that only looks at readiness. An app whose
repository credential is missing or refused reads as `failing` here, with
that reason.

**Uninstalling and purging are two different acts.**

```
DELETE /v1/tenants/{t}/apps/{p}                                        can_install_app
POST   /v1/tenants/{t}/actions/purge-app       {"profile": "<name>"}   can_install_app
usher  GET /v1/tenants/{t}/apps/retained                               can_view
```

*Uninstalling* removes the app and **keeps its data**. It is a commit: the
app leaves the tenant's manifest, and the cluster takes its workloads and its
sign-in client away. Everything the app stored stays, and installing the app
again in the same tenant finds it. *Purging* **destroys the data** of an app
that is no longer installed, and cannot be undone. No single act does both,
so removing an app never takes its data with it by accident.

| Kind of data | Uninstall | Purge |
| --- | --- | --- |
| Files — the volumes the app's chart creates | **kept**: every volume claim of the release is marked for Helm to leave in place, and the release has one name per tenant and app, so the next install takes the claims over | **destroyed**: the claims are deleted, with any finished pod still holding one, and the purge waits until they are gone |
| Database | **kept**, with its role and its password | **destroyed**: the database and the role (on PostgreSQL also every database the role created) |
| Object storage | **kept**: the bucket, its user and its policy | **destroyed**: the bucket with its contents, the user and the policy |
| Cache | **kept**: the app's user in the shared instance | the app's user is **removed**; the keys it wrote are not (see the limits below) |
| Stored credentials — the app's vault paths, and its extensions' | **kept** | **destroyed**, every version |
| Sign-in (OIDC) client | **removed** | — (already gone) |
| Access group, and who is in it | **kept**: the app installed again is open to the people it was open to | **destroyed**: the group, and with it every membership; likewise the group of each extension |

An app that is uninstalled and still holds data is **retained**. `GET
/apps/retained` lists the tenant's retained apps, relayed by the usher from
the operator:

```json
{
  "tenant": "demo",
  "apps": [{
    "profile": "odoo-base-ce",
    "state": "retained",
    "profileAvailable": true,
    "kinds": {
      "database": "present", "files": "present", "credentials": "present",
      "accessGroup": "present", "objectStorage": "unknown", "cache": "absent"
    },
    "volumes": ["odoo-base-ce-release-data"]
  }],
  "unknown": { "objectStorage": "…why…", "cache": "…why…", "database": "…why…" }
}
```

Each kind is `present`, `absent` or `unknown`, and an app is listed when at
least one is `present`. The read uses the names and the matching the purge
uses, so what it reports for an app is what a purge of that app would destroy.
It only reads, and runs nothing: a bucket and a cache user can be asked only
of the store itself, so they are `unknown` for an app whose profile declares
them, as is a MariaDB database; a PostgreSQL database is known from the record
the cluster keeps of it. `unknown` at the top says why, including when the
vault or the identity provider could not be asked this time. An app that is
installed, or still being taken down, is never listed.

**A purge is one request, and it fails loudly.** The operator does all of it
before it answers and continues nothing afterwards.

It is refused with `409`, having destroyed nothing:

- while the tenant still has the app, or has it switched on as an add-on;
- while the cluster is still taking it down — until Helm has finished
  uninstalling its release, not merely until its Component is gone;
- while another purge of the same app is running;
- when the app's ComponentProfile is not on the cluster. Which stores an app
  has is declared by its profile and by nothing else, so without it what the
  app owns cannot be determined, and a purge does not guess. A profile is
  placed on the cluster when the app is installed from a catalogue source
  that serves it (§3): installing the app again, in any tenant, puts it back,
  and the purge can then be asked for. The retained list shows such an app
  with `profileAvailable: false`.

Before it destroys anything it establishes whatever can be known beforehand
about whether it can finish: the tenant's realm and that the identity provider
answers for it, that the vault answers, that the database server has a primary
to drop on, and that the app's database is of an engine the platform can drop.
If one of these fails the answer is `503`, and nothing was destroyed.

Once admitted, it destroys kind by kind in the platform's one teardown order —
the order provisioning makes things in, reversed, which the deletion of a
tenant follows too ([data-lifecycle.md](data-lifecycle.md) §2): files, cache user,
object storage, database, access group, stored credentials, and last the
provisioning records. The first step that fails ends it: the answer is `500`
and says which step failed, what had already been destroyed and what was not
attempted. Nothing is rolled back; every step is safe to repeat, and asking
again continues with what is left. The operator gives a purge 4 min 30 s in
all and the director waits 5 min for it, so the answer always arrives; a purge
that runs out of its time says at which step, in the same way.

The answer of a purge that completed:

```json
{ "status": "purged", "purged": true, "complete": true,
  "destroyed": ["files", "cache", "objectStorage", "database", "accessGroup", "credentials", "provisioningRecords"] }
```

The access group is removed from the tenant's own realm —
`spec.isolation.keycloakRealm` when the tenant names one, as the platform
tenant does, and otherwise the realm called after the tenant.

Limits that remain:

- **Cache keys.** The cache is one shared instance. A purge removes the app's
  user; the keys the app wrote carry no owner and stay until they expire.
- **Names that overlap.** An extension's stores are kept under
  `<app>-<extension>`. If the tenant has an app of exactly that name, what is
  under it is that app's and a purge of the first leaves it alone. A volume
  that records a Helm release is destroyed only with the app whose release it
  is; one that records none is matched by its labels and its name.
- **Contracts.** A credential shared through an integration contract belongs
  to neither side and is not removed with either.
- **Databases an app made for itself.** On PostgreSQL every database the
  app's role owns goes with it. On MariaDB every database named with the
  provisioned one's name and an underscore as its prefix does, unless another
  account holds rights on it (operations.md §9.3).

Uninstalling tells the store nothing. The acquisition stays the tenant's;
ending it is between the tenant and the store.

```
POST /v1/tenants/{t}/actions/provision-app   {"profile": "<name>"}   can_grant
```

Provisioning grants an installed app — or an add-on switched on inside one,
by its own name — to everybody who is a member now, and marks it granted by
default to whoever joins later. It works only once the app exists in the
cluster; an install that should be for everyone says so in the install itself
(`defaultGrant`, §3) and needs no second call. The action remains for an app
that is already installed, and for an add-on, which has no entry of its own
to carry the field.

## 9. Without a store

A cluster that names no store, or does not report, has no App Store app.
**Apps are then installed by command only**:

```bash
kubectl gentian apps list --tenant <t> --available
kubectl gentian apps install <app> --tenant <t> [--from <source>] [--digest sha256:…] [--for-everyone]
```

through the director, on the same route and under the same question as §3.
The command looks the entry up in what the director lists of the cluster's
sources, takes the digest that listing states unless `--digest` names
another, and sends `{coordinate, digest, defaultGrant}` like every other
caller. When several sources serve the name it asks for `--from`; on a
cluster that declares no source it installs nothing.
The cluster renders no catalogue of its own in any interface. A second,
barer shop beside the one that is maintained would be worse at everything a
shop is for, and an install action in an administrative screen is one more
place that logic would live.

A **catalogue source** remains what it was: an https address serving
profile bundles, which the director fetches from. Those of the whole cluster
are named on the Cluster claim:

```yaml
catalogue:
  storeUrl: https://…      # the store's API; the App Store app calls it
  sources:
  - name: gentian
    url: https://…
  - name: in-house         # a platform administrator's own repository
    url: https://…
```

A source publishes `profiles/<name>.yaml` for each entry and `index.yaml`
beside them: name, version, edition, trust tier, digest, and nothing else.
The director fetches a bundle from the source named by a coordinate's
catalogue and checks it against the digest (§4). It still answers a source's
index — `GET /v1/tenants/{t}/catalogues` and
`GET /v1/tenants/{t}/catalogues/{source}/entries`, under `can_view`, `ce` and
`pe` entries only — so that the digest of an entry can be looked up without
a store; no screen lists it.

**The default source.** A new claim names one source, `gentian`: the public
catalogue of the platform's apps repository. Which of its two the installer
writes follows from the ref the cluster is installed from: a release tag or
`main` gets the released catalogue
(`https://gentian-org.github.io/gentian-apps`), any other branch the one
under development (`…/gentian-apps/develop`), and a ref that cannot be read
the released one. `GENTIAN_CATALOGUE_URL` names another address whatever the
ref. The reason is written above the address in the claim. A claim that
exists is not rewritten.

**Catalogues exist at two levels.**

| | Where it is declared | Who adds it | Who installs from it |
|---|---|---|---|
| A catalogue of the whole cluster | `catalogue.sources` on the Cluster claim | whoever may configure the cluster | every tenant |
| A tenant's own catalogue | `spec.catalogue.sources` on the Tenant | the cluster's administrator; or the tenant's own, where the cluster's administrator turned that on (`spec.catalogue.delegated`) | that tenant only |

Both are written by the director's catalogue routes
(`kubectl gentian catalogues`), are outside the store, and are documented
with the catalogue format ([custom-catalogues.md](../custom-catalogues.md)).
A profile materialised from a tenant's catalogue records where it came from
and is installable in that tenant only; such a catalogue brings no
companions (§4).

There is no tuple for a source: the install route asks whether the person
may install apps in the tenant, and resolves the coordinate's catalogue among
those the tenant sees (§3).

**Nothing copies profiles into a cluster ahead of an install.** The
ApplicationSet that synced every profile of a git repository with role
`apps` is retired, and such a repository is no longer declared (§7). A
profile reaches a cluster when it is installed, with one exception.

**The installer's default profile: the one exception, at a digest.** At
install, before there is a director, the installer places the Operations
Console's profile in the cluster's catalogue directory (AD-14, decided
2026-10-09). The address is
`https://catalogue.aluvian.io/profiles/operations-console.yaml` by default;
`GENTIAN_STORE_CATALOGUE_URL` or `GENTIAN_DEFAULT_PROFILES` name others, each
the https address of a profile in a catalogue. The installer reads that
catalogue's `index.yaml`, takes the digest listed for the entry — or the
digest the person installing pinned with `@sha256:<digest>`, which the index
does not override — downloads the file, and writes nothing unless the bytes
hash to it. Bytes that do not, an entry the index does not list when nothing
is pinned, and a file that holds what a bundle may not all stop the install;
a catalogue that cannot be reached is a warning, and
`--disable-api-extensions` places none. What is written is what the director
writes for an install of the same bundle, byte for byte: the file as served,
and its bundle and the catalogue's origin on the profile, so the director
reads it as a materialised profile with an origin and a digest. The digest
and who stated it are in the signed commit. A definition that already holds
another build is kept and reported.

Two things it is not. It is not checked at rollout by default: the operator
makes that comparison for a Component pinned to a digest (§4), and the
Component it creates for a profile that declares `defaultForTenants` carries
no pin. And the installer does not run the whole of the check the director
runs on what travels with a profile, only the part on kinds, names and
metadata; the Operations Console's bundle is the profile alone.

What does not change without a store: the install mechanism (fetch the
bundle at the digest, apply the profile, commit as the person), the
attribution, the audit trail, and every check on the profile itself — a
side-loaded profile meets the same CEL rules, admission policies and
privilege approvals as one a store lists. Only the questions a store
answers — what an app is, and what it costs — go unanswered.

## 10. When the store fails

| What happens | What the cluster does |
|---|---|
| The store cannot be reached, or answers `5xx` | The App Store app says so. It offers nothing that needs the store. Nothing is retried in the background |
| The store answers `401` on a signed-in call | The app signs the person in to the store again. Browsing is not interrupted: it needs no token |
| The store refuses the tenant | The app shows the reason (§6.2) and offers no acquiring; the catalogue stays readable |
| The store answers something that is not the format | The app treats it as unreachable. A confirmation that does not validate is not acted on in part |
| A step on the cluster fails after a confirmation — the declaration, the credential, the install | The app shows the director's or the custodian's answer and stops. The acquisition is still the tenant's; the steps can be repeated, and each is safe to repeat |

**Installed apps are unaffected by the store's absence.** Nothing that runs
asks the store anything. The one dependence that outlives an install is on
the *repository*, for pulls, with the credential the tenant holds.

## 11. Not in the contract

* **No call from the store into the cluster.** No webhook, no push, no
  callback. An operation that needed one would not be part of this contract.
* **No signed statement.** No entitlement, no revocation, no licence file,
  no key of a store's on a cluster.
* **No cluster state sent to the store.** What is installed, who is in the
  tenant, the plan and its use are not sent. A store learns which apps and
  pinned add-ons a tenant runs from the licence report, which is a separate, open, signed
  statement the cluster makes on its own schedule.
* **No uninstall notice.** The store is not told when an app is removed.
* **No store content executed.** No HTML, script, style sheet or frame.
* **No payment data on the cluster.**
* **No exclusivity on the cluster.** A private chart is shared across the
  tenants of a cluster through the chart installer's cache, and a pulled
  image through the node it is on (§2). The contract is that a tenant
  without the credential cannot *fetch* a private app from the repository.
  It is not that a second tenant of the same cluster can never run what a
  first one fetched.
* **No writing of reviews** in this version of the API.
* **How a store first associates the administrator's account with a
  tenant**, what it asks of an account before it confirms an app, and how
  it prices, are the store's own. That a tenant is an organisation and
  that one account acts for it by default is in the contract (§6.1).
