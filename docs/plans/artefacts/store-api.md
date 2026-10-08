# The store API

What a store serves, written for whoever builds one. A Gentian OS cluster
runs an **App Store app** for each tenant; that app shows the data a store
holds about apps, and lets a tenant's administrator acquire an app from the
store and install it on the cluster. This document defines every request
the app makes of the store and every answer it accepts.

Copyright The Gentian OS Authors. This document and the two OpenAPI files
beside it are under the Mozilla Public License 2.0
([LICENSE](../../../LICENSE)); SPDX-License-Identifier: MPL-2.0.

It can be read on its own. Three files say the same thing at different
depths, and they must agree:

| File | What it is |
|---|---|
| [store-api.openapi.yaml](store-api.openapi.yaml) | The machine-readable definition (OpenAPI 3.1). **It wins**: where this document or the contract differs from it, the difference is a defect in the prose |
| this document | The same definition in full, in prose, with annotated examples |
| [store-contract.md](../../design/store-contract.md) | Why it is shaped this way: who trusts whom, and what the cluster does with each answer |

The report a cluster sends about itself, which a store depends on (§4), is a
separate format: [licence-report.openapi.yaml](licence-report.openapi.yaml),
spelled out in [operations.md §6.2](../../design/operations.md).

## Changes to the definition

Whoever keeps a copy of these files: this is what moved, newest first.

**2026-10-07**

1. **`repository.name` is removed** from the confirmation — schema,
   examples, field tables. The store names nothing on the cluster. The App
   Store app computes the name a repository is declared under from the
   tenant's name and `repository.url` (§7, "The repository's name on the
   cluster").
2. **A repository credential does not expire, for now.** `expiresAt` stays
   in the schema, required and nullable; a store issues `null`, and every
   example says `null`. The rule "renew when under 30 days" and the
   renewal narrative are removed. The feature value `credential-renewal`
   is removed from `GET /v1/meta`.
3. **`POST /v1/acquisitions/{id}/credential` is redefined** from "renew" to
   "replace this credential now" (rotation). The replaced credential is
   refused 24 hours after the call. `operationId` is `replaceCredential`
   (was `renewCredential`). Every store serves it.
4. **What ends a credential** is stated: rotation, or the acquisition
   ending (§7).
5. **`credential.username` is one per tenant**, chosen by the store, opaque
   to the cluster. A credential is one per tenant and repository (§7).
6. **Accounts and tenants** (§3): a tenant is an organisation in the
   store's records, an account is a person, and by default exactly the
   tenant administrator's account acts for the tenant.
   `tenant-not-claimed` is specified as the refusal for an account that is
   not associated with the tenant (§4).
7. **The code is exchanged by the App Store app's backend**, and the store
   token stays there, bound to the administrator's cluster session. It is
   never handed to the browser (§3).
8. **The issuer redirects to exactly one address per tenant**,
   `https://store.<the tenant's base domain>/oauth/callback`, in place of
   "any host under the tenant's address" (§3).
9. **Reading a repository credential needs `store.acquire`.**
   `GET /v1/acquisitions/{id}` answered to a token with `store.read` alone
   carries the confirmation without `repository.credential` (§3, §6).
10. In `licence-report.openapi.yaml`, prose only: a receiver remembers the
    key first seen per **`cluster.url`**, not per `cluster.id`, and the
    account holder resets it. The format of a report is unchanged.
11. §9: the questions these decisions answer are removed — who may act
    for a tenant, credential lifetime, where the redirect lands, repository
    names, the user name convention — and so is the one on add-ons, whose
    pinning is built. Questions 2 to 5 of the list are new.

## 1. Ground rules

**The cluster calls; the store answers.** Every request here is made from
the cluster's side, outbound, over HTTPS. A store never calls a cluster: it
holds no credential for one, and a cluster exposes nothing to it. Nothing in
this API may be implemented in a way that needs the reverse direction — no
webhook, no callback into a cluster. The one thing that looks like a
callback, the OAuth redirect in §3, is a redirect of the person's browser.

**Base address.** A cluster names its store by one `https` address
(`spec.catalogue.storeUrl` on its Cluster claim), with no trailing slash.
Every path below is appended to it: `https://store.example` + `/v1/apps`.
A cluster installed with the installer's defaults names
`https://store.aluvian.io`; that default is provisional.

**Open reads and signed-in calls.** The API has two halves, and the line
between them is whether a call concerns a tenant.

| | Calls | Token |
|---|---|---|
| **Open reads** | `GET /v1/meta`, `/v1/categories`, `/v1/apps`, `/v1/apps/{catalogue}/{app}` and its `/reviews` and `/reports` | none needed. What a store offers can be seen at once, by anyone, with no account. Never `401`, never `403` |
| **Signed-in calls** | `GET /v1/tenant` and every `/v1/acquisitions` operation | required (§3) |

A token *may* be presented on an open read. The store may then add what it
knows of the token's tenant to the answer — today one member, `acquired`,
on an app (§5). It is never required. A token the store cannot validate, or
one for a tenant it does not serve, is ignored on an open read: the answer
is the anonymous one, not an error.

Whether a store serves a tenant (`tenant-refused`, §4) is a question of the
signed-in calls only. An anonymous visitor sees the catalogue whatever the
standing of their tenant, and learns at sign-in whether the store serves it.

**What the App Store app does.** It shows the catalogue immediately, with no
store account, from the open reads. It asks the person to sign in to the
store only when they acquire something or open what the tenant has acquired.
After a sign-in it asks `GET /v1/tenant` first, and shows the refusal or the
notices it is given.

**What a store receives.**

| On | The store learns |
|---|---|
| an open read with no token | the path and query — which categories, apps and pages are looked at — the language asked for, and the address the request comes from, which is the cluster's outbound address. Nothing else |
| a signed-in call, or an open read that carries a token | in addition the token, and thereby the store account and the tenant URL bound into it at sign-in |
| an image or a document it serves | the address of the person's browser. The app sets `Referrer-Policy: no-referrer`, so the request does not name the tenant's host |

On an anonymous call the app sends no cluster identifier, no tenant URL, no
cookie, and no `Referer` or `Origin` naming a tenant; its `User-Agent` names
the app and its version and nothing of the cluster. An anonymous read
therefore tells the store only where the request came from.

Never sent, signed in or not: who is in the tenant, what is installed, any
configuration of the cluster. The only personal data that reaches a store
through this API is the store account the person signed in with, which the
store already holds.

**Store content is data.** A text field is plain text unless this document
says it is Markdown. The app renders text as text: it interprets no HTML,
runs no script, and embeds no frame from a store. A Markdown field is the
subset **`store-markdown-1`**:

| Rendered | Shown as the characters written |
|---|---|
| Paragraphs, hard line breaks | Raw HTML |
| Emphasis, strong emphasis | Images |
| Inline code; fenced code blocks, preformatted | Tables, block quotes, thematic breaks |
| Bullet and ordered lists, at most two levels | Reference-style links, autolinks, footnotes |
| Headings of level 2 and 3 | Headings of any other level |
| Inline links to an absolute `https` URL, opened in a separate window with no opener | Links with any other scheme |

**URLs.** Every URL in an answer is absolute and `https`. An image URL must
be on an origin listed in `mediaOrigins` of `GET /v1/meta`, a checkout URL
on one listed in `checkoutOrigins`; the app refuses others. Images are
`image/png`, `image/jpeg` or `image/webp` — not SVG. Documents and links are
opened in a separate window and never embedded.

**Language.** A request carries `Accept-Language`. The store answers each
translatable field in the best match and names the language in
`Content-Language`. Matching is forgiving within a language and never across
one: `de-CH` is served `de` when there is no `de-CH`. With no match the
store answers in the `defaultLanguage` it states in `GET /v1/meta`. An
answer is in one language as a whole; a field with no translation in it is
served in the default language rather than left empty. Such answers carry
`Vary: Accept-Language`.

**Paging.** A list answers `{"items": [...], "nextCursor": "..."}`.
`nextCursor` is `null` on the last page; otherwise it is sent back as
`cursor`, unchanged, with the same filters. `limit` is 20 when absent and at
most 100. A cursor is opaque and may expire (`400 invalid-request`).

**Compatibility.** `/v1` changes only by addition: new optional members, new
operations, new values of an enumeration this document calls *open*. The
app ignores members it does not know and treats an unknown value of an open
enumeration as the fallback stated with it. A *closed* enumeration gains no
value within `/v1`. Anything else is `/v2`.

**Rate limiting.** Any answer may carry `RateLimit-Limit`,
`RateLimit-Remaining` and `RateLimit-Reset` (seconds). A `429` carries
`Retry-After` in seconds and the same three headers. A request with a valid
token is counted per token; one without is counted per client address. One
address is one cluster, not one person: every tenant of a cluster reaches
the store from the cluster's outbound address. A store sizes the anonymous
limit for a cluster's worth of administrators browsing, and the app caches
open reads so that they share answers.

**Caching.** An open read answered without regard to a token is the same for
everybody who asks in the same language:

| Answer | Headers |
|---|---|
| an open read, not personalised | `Cache-Control: public, max-age=<seconds>` (the store chooses; minutes for listings, longer for `/v1/meta`), `ETag`, `Vary: Accept-Language, Authorization` |
| an open read the store personalised from a token | `Cache-Control: private, no-store`, no `ETag`, the same `Vary` |
| every signed-in call | `Cache-Control: no-store` |

The app may repeat an open read with `If-None-Match: <the ETag>`; the store
answers `304` with no body when the answer still stands. A `304` counts
against the rate limit like any request. Images and documents are served
from the store's own origins under its own caching and are not part of this
API.

**Notation.** In the field tables, *Req.* says whether the member is always
present. A type written `string \| null` is present and may be `null`.

## 2. Endpoints

| Method and path | Token | Answers |
|---|---|---|
| `GET /v1/meta` | **open** | The store's name, API version, issuer, client id, languages, allowed origins, features |
| `GET /v1/categories` | **open** | The categories apps are filed under |
| `GET /v1/apps` | **open** | A page of apps; filters `category`, `edition`, `q` |
| `GET /v1/apps/{catalogue}/{app}` | **open** | One app in full: description, screenshots, versions with digests, add-ons, links |
| `GET /v1/apps/{catalogue}/{app}/reviews` | **open** | A page of reviews and the summary of all of them |
| `GET /v1/apps/{catalogue}/{app}/reports` | **open** | Evaluations and reports about the app |
| `GET /v1/tenant` | `store.read` | Whether the store serves the tenant in the token, the reason if not, and notices to show |
| `GET /v1/acquisitions` | `store.read` | What this tenant has acquired; filter `status` |
| `POST /v1/acquisitions` | `store.acquire` | Acquire an app: `201` confirmed, `202` checkout needed, `200` already acquired |
| `GET /v1/acquisitions/{id}` | `store.read` | One acquisition; once confirmed, its confirmation. The repository credential is in it only for a token that also carries `store.acquire` |
| `POST /v1/acquisitions/{id}/credential` | `store.acquire` | Replaces the repository credentials now; answers the confirmation with the new ones |

*Open* means no token is needed. One may be presented on the five catalogue
reads, where it only adds `acquired`; `GET /v1/meta` ignores it.

## 3. Signing in

Signing in is needed only for what concerns a tenant: `GET /v1/tenant` and
the `/v1/acquisitions` operations. Browsing the catalogue needs none, and
the app does not ask for it until the person acquires something or opens the
tenant's acquisitions.

The tenant's administrator signs in to the store, once, with an account at
the store. It is a different account from the one they are signed in to the
cluster with, and the cluster learns nothing about it. The result is a
**store token**, which the App Store app uses for what concerns the tenant:
its standing and its acquisitions.

**Accounts and tenants.** They are separate things in a store's records.

| | |
|---|---|
| A **tenant** | is an organisation. The store knows it by its tenant URL |
| An **account** | is a person, at the store's issuer |
| Who acts for a tenant | the tenant's administrator creates an account at the store's issuer, and the store associates that account with the tenant. By default **exactly that one account** acts for the tenant |
| More than one account per tenant | may be allowed later. It is outside this definition for now |
| An account that is not the tenant's | is refused on the signed-in calls with the reason `tenant-not-claimed` (§4) |

**What the store token is, and is not.** It authenticates the administrator
to the store. It is valid at the store and nowhere on the cluster. It is
**not** what pulls images or charts: pulls use the tenant's registry
credential, which a confirmation carries (§7) and which the App Store app
hands to the cluster's custodian once. The registry never sees the store
token, and the store token pulls nothing.

The flow is OAuth 2.0 authorization code with PKCE (RFC 6749, RFC 7636).
The App Store app is a **public client**: it has no client secret — a
secret shipped to every cluster is not one — and one client id serves every
cluster. *app* below is the App Store app's **backend**, on the cluster.

```
1. app   → store   GET /v1/meta                         issuer, clientId, scopes
2. app   → issuer  GET {issuer}/.well-known/oauth-authorization-server
                                                        authorization and token endpoints (RFC 8414)
   app keeps state and the PKCE verifier, with the administrator's cluster session
3. browser → issuer  authorization endpoint
       response_type=code
       client_id=<clientId>
       redirect_uri=https://store.<the tenant's base domain>/oauth/callback
       scope=store.read store.acquire
       state=<random>
       code_challenge=<S256 of the verifier>  code_challenge_method=S256
       tenant_url=https://<the tenant's host>
4. the person signs in at the store
5. issuer → browser  302 to redirect_uri with code and state    — the app's own callback
6. app   → issuer  token endpoint                               — from the cluster, never from the browser
       grant_type=authorization_code  code  redirect_uri  client_id  code_verifier
   issuer → app    {"access_token": "…", "token_type": "Bearer", "expires_in": …}
7. app   → store   signed-in calls:  Authorization: Bearer <access_token>
```

**`tenant_url`** is the address of the tenant the app is installed in:
`https://<host>`, lower case, no path, no trailing slash. It is the same
spelling the cluster uses for the tenant in its licence report
(`tenants[].url`), which is what lets a store match the two (§4).

**Three conditions the sign-in is held to.**

| | Condition | What it means |
|---|---|---|
| a | **The backend exchanges the code and keeps the token** | Step 6 is made by the App Store app's backend on the cluster. The store token — and a refresh token, if one is issued — is kept there, bound to the administrator's cluster session, for as long as that session lasts and nowhere durable. It is never handed to the browser: not in a page, not in a cookie, not in an answer of the backend's. The browser carries the code through the redirect and nothing else |
| b | **One redirect address per tenant** | The issuer redirects to exactly one address, the App Store app's own callback (below), not to any host under the tenant's address |
| c | **Reading a repository credential needs `store.acquire`** | A token with `store.read` alone is never answered a `credential` (scopes, below) |

**The redirect address** is

```
https://store.<the tenant's base domain>/oauth/callback
```

The App Store app is component `app-store` of a tenant, and its host label
is `store`. The tenant's base domain is `<tenant>.<cluster domain>` on a
multi-tenant cluster, the cluster domain itself for the one user tenant of
a single-tenant cluster, or the tenant's custom domain. It is the host of
`tenant_url`, so an issuer computes the one address from the request alone
— `https://store.<host of tenant_url>/oauth/callback` — and no address has
to be registered per cluster.

What the issuer must do:

| Rule | Why |
|---|---|
| Treat `clientId` as a public client: no secret, `S256` required, a request with no challenge refused | The app runs on somebody else's cluster and cannot keep a secret that every cluster would share |
| Accept a `redirect_uri` only when it is, character for character, the redirect address above computed from the request's `tenant_url`: `https`, the host `store.<host of tenant_url>`, no port, the path `/oauth/callback`, no query, no fragment. Any other host under the tenant's address is refused, and any other path on the right host | A code for a tenant is delivered only to the App Store app's backend of that tenant, and not to another app that happens to be served under the tenant's address |
| Refuse a request with no `tenant_url`, or one not in the spelling above (`error=invalid_request`) | A token is for one tenant |
| Bind the tenant into the token: the store's API must be able to read from the token the account (`sub`), the tenant URL (`tenant_url`), the scopes and the expiry, and act for that tenant URL and no other | Every authenticated answer is about "the tenant in the token" |
| Issue the token even when the store does not serve the tenant, or the account is not associated with it | `GET /v1/tenant` then states the refusal and the reason, which the app shows. A sign-in that fails at the issuer can show nothing |

The token is opaque to the app: it reads nothing from it and only the store
validates it, so its form (a JWT, a reference) is the store's choice. A
refresh token may be issued; it then rotates on use and stays bound to the
same tenant URL. Lifetimes are the store's to set (§9).

| Scope | Covers |
|---|---|
| `store.read` | Reading what concerns the token's tenant: `GET /v1/tenant` (its standing and notices), `GET /v1/acquisitions`, and `GET /v1/acquisitions/{id}` with its confirmation — **without** any repository credential. It is also what lets the store personalise an open read (`acquired`) |
| `store.acquire` | Changing what the tenant has at the store, and reading a repository credential: `POST /v1/acquisitions`, which may start a checkout; `POST /v1/acquisitions/{id}/credential`, which replaces a credential; and the `credential` members in the answer of `GET /v1/acquisitions/{id}`. No answer carries a `credential` to a token without this scope |

No scope is needed for the open reads and none covers them: they are
answered with no token at all. The app asks for both scopes at sign-in.

### GET /v1/meta

Open: it takes no token and ignores one. Read before anything else;
cacheable for the `max-age` it states.

```jsonc
{
  "apiVersion": "1.0",                       // <major>.<minor>; the major is the /v1 of the paths
  "name": "Example Store",                   // shown as the title of the sign-in
  "issuer": "https://accounts.store.example",
  "clientId": "gentian-app-store",
  "scopes": ["store.read", "store.acquire"],
  "defaultLanguage": "en",
  "languages": ["en", "de", "fr", "it"],
  "mediaOrigins": ["https://media.store.example"],   // images come from here and nowhere else
  "checkoutOrigins": ["https://store.example"],      // a checkoutUrl is on one of these
  "features": ["reviews", "reports", "checkout"],
  "links": {"terms": "https://store.example/terms", "privacy": "https://store.example/privacy",
            "support": "https://store.example/support", "account": "https://accounts.store.example/account"}
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `apiVersion` | string | yes | `1.<minor>` |
| `name` | string ≤ 80 | yes | What the store calls itself |
| `issuer` | URL | yes | The OAuth 2.0 issuer; its metadata is at `{issuer}/.well-known/oauth-authorization-server` |
| `clientId` | string | yes | The public client id |
| `scopes` | string[] | yes | The scopes the app asks for |
| `defaultLanguage` | BCP 47 | yes | The language answered in when none asked for matches |
| `languages` | BCP 47[] | yes | The languages the store has translations in |
| `mediaOrigins` | origin[] | yes | Where image URLs may point. The app fixes its content security policy from it |
| `checkoutOrigins` | origin[] | yes | Where a `checkoutUrl` may point |
| `features` | string[] | yes | Open. `reviews`, `reports`, `checkout` (an acquisition may answer `202`). Unknown values are ignored |
| `links` | object | no | `terms`, `privacy`, `support`, `account`: pages of the store's, each a URL |

## 4. Tenant standing

A store that depends on licence reporting serves a tenant only when the
reports it has received list that tenant's URL. The app asks once after
each sign-in and shows what it is told. Standing governs the signed-in
calls only: the catalogue stays readable to a tenant the store refuses, and
to somebody who never signs in. A notice is likewise seen only after
sign-in.

### GET /v1/tenant

Answers for the tenant URL in the token. It is the one signed-in call that
never answers `403 tenant-refused`: a refusal is stated in the body, so the
app has something to show.

A tenant the store serves, on a cluster with no subscription:

```jsonc
{
  "tenantUrl": "https://acme.example",       // from the token, echoed
  "known": true,                             // at least one licence report lists this URL
  "lastReportAt": "2026-10-06T05:06:07Z",    // sentAt of the latest such report
  "served": true,
  "refusal": null,
  "notices": [
    {
      "type": "free-licence-limit",
      "severity": "info",
      "title": "This cluster has no subscription",
      "text": "The free licence covers clusters of fewer than 50 users that are not operated for resale. A cluster beyond that needs a subscription.",
      "link": {"rel": "subscription", "label": "About subscriptions", "url": "https://store.example/subscription"}
    }
  ]
}
```

A tenant the store does not serve:

```jsonc
{
  "tenantUrl": "https://acme.example",
  "known": false,
  "lastReportAt": null,
  "served": false,                           // every /v1/acquisitions operation now answers 403 tenant-refused
  "refusal": {
    "reason": "no-reports-for-tenant",
    "detail": "No licence report names https://acme.example. The store serves tenants of clusters that report; ask the cluster's administrator whether reporting is on.",
    "link": {"rel": "documentation", "label": "Licence reporting", "url": "https://store.example/help/reporting"}
  },
  "notices": []
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `tenantUrl` | string | yes | The tenant URL the token is bound to |
| `known` | boolean | yes | Whether the store holds a licence report listing this URL |
| `lastReportAt` | date-time \| null | no | `sentAt` of the latest such report |
| `served` | boolean | yes | Whether the store serves this tenant |
| `refusal` | object \| null | yes | Present exactly when `served` is false |
| `refusal.reason` | string | yes | Open; see below. For an unknown value the app shows `detail` |
| `refusal.detail` | string | yes | The reason in words, in the language of the request |
| `refusal.link` | Link | no | Where to read more |
| `notices` | Notice[] | yes | Shown in order. None of them blocks anything |

| `refusal.reason` | Meaning |
|---|---|
| `no-reports-for-tenant` | No licence report lists this URL: the cluster does not report, or the URL stated at sign-in is not the tenant's |
| `tenant-not-claimed` | Reports exist, and the account signed in is not associated with this tenant: it is not the account of the tenant's administrator, or another the store lets act for the tenant (§3) |
| `reports-stale` | Reports exist and the latest is older than the store accepts |
| `tenant-blocked` | The store has decided not to serve this tenant |

**An account that is not the tenant's.** What the API does for it, with the
reason `tenant-not-claimed`:

| Call | Answer |
|---|---|
| `GET /v1/tenant` | `200`, `served: false`, `refusal.reason: tenant-not-claimed`, `known: true` |
| every `/v1/acquisitions` operation | `403 tenant-refused` with `reason: tenant-not-claimed` — also for an `id` that exists, so the account learns nothing of the tenant's acquisitions |
| an open read carrying the token | the anonymous answer, without `acquired` |

```jsonc
{
  "tenantUrl": "https://acme.example",
  "known": true,
  "lastReportAt": "2026-10-06T05:06:07Z",
  "served": false,
  "refusal": {
    "reason": "tenant-not-claimed",
    "detail": "This account is not associated with https://acme.example. The store lets the account of the tenant's administrator act for a tenant."
  },
  "notices": []
}
```

**Notice**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `type` | string | yes | Open. `free-licence-limit`: the tenant's cluster has no subscription, and the text states that the free licence covers clusters of fewer than 50 users that are not operated for resale. `reports-stale`: the latest report is old. `message`: anything else. An unknown type is shown like `message` |
| `severity` | `info` \| `warning` | yes | Closed. How prominently to show it |
| `title` | string ≤ 120 \| null | no | A short heading |
| `text` | string ≤ 2000 | yes | The notice, plain text, in the language of the request |
| `link` | Link | no | |

**Link** (used throughout)

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `rel` | string | no | Open: `homepage`, `documentation`, `source`, `support`, `licence`, `privacy`, `terms`, `subscription` |
| `label` | string ≤ 80 \| null | no | What to call it |
| `url` | URL | yes | Opened in a separate window |

A notice, a refusal and a subscription are the store's business with the
tenant. Nothing on the cluster is blocked by any of them: apps that are
installed keep running, and the command-line install does not ask a store.

## 5. Catalogue

All five operations here are open reads (§1): no token, cacheable, never
refused for a tenant's standing.

An entry is identified by its **coordinate**, `<catalogue>/<app>`: the name
of a catalogue source and the name of the app's profile in it, each a DNS
label. It is the value the cluster's install request carries. A profile's
name includes its edition, so each edition of an app is an entry of its own;
`family` ties them together.

### GET /v1/categories

```jsonc
{"items": [{"id": "collaboration", "name": "Collaboration"},
           {"id": "files", "name": "Files and documents"}]}
```

The whole list, unpaged, in the order the store shows it. `id` is stable
across languages; `name` is in the language of the request.

### GET /v1/apps

| Parameter | In | Meaning |
|---|---|---|
| `category` | query | A category `id`. Only entries filed under it |
| `edition` | query | `ce`, `pe`, `me` or `ee`. Only entries of that edition |
| `q` | query | Free text, ≤ 200 characters, matched against name, summary and description in the language of the request. How it matches is the store's affair |
| `cursor`, `limit` | query | Paging (§1) |
| `Accept-Language` | header | §1 |
| `If-None-Match` | header | Optional; §1, caching |
| `Authorization` | header | Optional. With a valid token each entry may carry `acquired` |

Filters combine with AND. The order is the store's and is stable across the
pages of one listing.

```jsonc
{
  "items": [
    {
      "coordinate": "gentian/nextcloud-base-ee",
      "family": "nextcloud",                 // the same on every edition of the app
      "name": "Nextcloud",
      "summary": "Files, calendars and contacts for a team, on your own cluster.",
      "iconUrl": "https://media.store.example/icons/nextcloud.png",
      "categories": ["files", "collaboration"],
      "publisher": {"name": "Example Publisher AG", "url": "https://publisher.example"},
      "edition": "ee",
      "editions": [                          // every edition offered, this one included
        {"edition": "ce", "coordinate": "gentian/nextcloud-base-ce"},
        {"edition": "ee", "coordinate": "gentian/nextcloud-base-ee"}
      ],
      "trustTier": "certified",
      "latestVersion": "31.0.4",
      "price": {"model": "subscription", "amount": "4.50", "currency": "CHF",
                "per": "user", "period": "month", "note": null},
      "acquired": false,                     // ONLY with a token: for the tenant in it. Absent from an anonymous answer
      "rating": {"count": 2, "average": 4.5,
                 "distribution": {"1": 0, "2": 0, "3": 0, "4": 1, "5": 1}}
    }
  ],
  "nextCursor": null
}
```

**App summary**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `coordinate` | `<catalogue>/<app>` | yes | The entry's identity |
| `family` | DNS label | yes | The app this entry is an edition of. Not shown; it groups entries |
| `name` | string ≤ 80 | yes | Display name |
| `summary` | string ≤ 300 | yes | One or two sentences, plain text |
| `iconUrl` | URL \| null | yes | Square, at least 128 × 128, on one of `mediaOrigins` |
| `categories` | string[] | yes | Category ids |
| `publisher` | `{name, url}` | yes | Who maintains or licenses the entry. `url` may be `null` |
| `edition` | `ce` \| `pe` \| `me` \| `ee` | yes | Closed; the values a profile carries on the cluster. `ce` community, `pe` an operator's own, `me` community with active maintenance, `ee` commercially licensed |
| `editions` | `{edition, coordinate}[]` | yes | Every edition the app is offered in, each with the coordinate of the entry that is that edition |
| `trustTier` | `platform` \| `certified` \| `experimental` | yes | Closed; the review level the entry's profile states |
| `latestVersion` | string | yes | The newest version listed |
| `price` | Price | yes | The list price |
| `acquired` | boolean | no | Whether the tenant in the token has a `confirmed` acquisition of this coordinate. Present only on an answer to a request that carried a valid token of a tenant the store serves; absent from every anonymous answer. The app never depends on it: `GET /v1/acquisitions` says the same |
| `rating` | Review summary \| null | no | `null` when the store carries no reviews |

**Price** — the **list price**: what the store asks of anybody, the same with a
token and without. Shown, never computed with. What a tenant is charged — a
contract rate, a discount, tax — is what the checkout says, and is not in
this API.

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `model` | `free` \| `one-time` \| `subscription` \| `quote` | yes | Closed. `free`: nothing is charged and no checkout follows. `quote`: no amount is named; `note` says how to obtain one |
| `amount` | decimal string \| null | no | `"4.50"`: a point, no grouping. `null` for `free` and `quote` |
| `currency` | ISO 4217 \| null | no | `null` exactly when `amount` is |
| `per` | `tenant` \| `user` \| null | no | What the amount is multiplied by; `null` for a flat amount |
| `period` | `month` \| `year` \| null | no | For `subscription` |
| `note` | string ≤ 300 \| null | no | Anything the numbers do not say |

### GET /v1/apps/{catalogue}/{app}

Everything in the summary, and:

```jsonc
{
  // … every member of the summary …
  "description": "Nextcloud keeps a team's files …\n\n## What is included\n\n- File sync and sharing\n",
  "screenshots": [
    {"url": "https://media.store.example/shots/nextcloud-files.png", "contentType": "image/png",
     "width": 1600, "height": 1000, "caption": "The files view"}
  ],
  "versions": [                              // newest first, at most 50; the first is latestVersion
    {
      "version": "31.0.4",
      "digest": "sha256:3f6c0a1e5b7d9c2a4e6f8091a3b5c7d9e1f20314253647586970a1b2c3d4e5f6",
      "releasedAt": "2026-09-20T08:00:00Z",
      "releaseNotes": "Fixes two issues in calendar sharing.",
      "requirements": {"platform": ">=0.5",
                       "resources": {"cpu": "500m", "memory": "1Gi", "storage": "10Gi"},
                       "notes": ["Needs the tenant's mail service to send invitations."]}
    }
  ],
  "addons": [{"coordinate": "gentian/nextcloud-office-ee", "name": "Office",
              "summary": "Edit documents in the browser."}],
  "addonOf": null,                           // the coordinate this entry extends, when it is an add-on
  "links": [{"rel": "documentation", "label": "Administration manual", "url": "https://publisher.example/docs"}],
  "licence": {"spdx": null, "name": "Example Publisher Enterprise Licence", "url": "https://publisher.example/licence"}
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `description` | Markdown (`store-markdown-1`) ≤ 20 000 | yes | The long text |
| `screenshots` | Screenshot[] | yes | In the order to show them |
| `versions` | Version[] 1–50 | yes | Newest first |
| `addons` | `{coordinate, name, summary}[]` | yes | The add-ons that exist for this entry; each is a store entry of its own |
| `addonOf` | coordinate \| null | yes | When this entry is itself an add-on, what it extends |
| `links` | Link[] | yes | |
| `licence` | `{spdx, name, url}` \| null | yes | The licence the app is distributed under. `spdx` and `url` may be `null` |

**Screenshot**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `url` | URL | yes | On one of `mediaOrigins` |
| `contentType` | `image/png` \| `image/jpeg` \| `image/webp` | yes | |
| `width`, `height` | integer \| null | no | Pixels, when known |
| `caption` | string ≤ 200 \| null | no | Also the image's alternative text |

**Version**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `version` | string ≤ 64 | yes | The catalogue version, as the catalogue source's index states it |
| `digest` | `sha256:<64 hex>` | yes | The SHA-256 of this version's profile bundle — the exact bytes the catalogue source serves. Lower case. **What the cluster pins and verifies** |
| `releasedAt` | date-time | yes | RFC 3339, UTC |
| `releaseNotes` | Markdown (`store-markdown-1`) ≤ 20 000 | yes | May be empty |
| `requirements` | object | yes | `platform` (a version range, `>=0.5`), `resources` (`cpu`, `memory`, `storage` as Kubernetes quantities), `notes` (sentences). Each may be absent or `null` |

`requirements` is for a person to read before acquiring. What the cluster
enforces is what the profile bundle itself declares, read from the bundle,
never from the store.

### GET /v1/apps/{catalogue}/{app}/reviews

Newest first, paged. `summary` is over every review of the entry, not over
the page.

```jsonc
{
  "summary": {"count": 2, "average": 4.5,
              "distribution": {"1": 0, "2": 0, "3": 0, "4": 1, "5": 1}},
  "items": [
    {"id": "rv_01J9ZC1E", "rating": 5, "title": "Does what we need",
     "text": "Running it for forty people since spring. Upgrades have been uneventful.",
     "author": "M. K.", "date": "2026-09-12", "language": "en", "version": "31.0.4"}
  ],
  "nextCursor": null
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `summary.count` | integer ≥ 0 | yes | |
| `summary.average` | number 1–5 \| null | yes | To one decimal; `null` when `count` is 0 |
| `summary.distribution` | object | yes | Keys `"1"` to `"5"`, all present: how many reviews gave each rating |
| `id` | string | yes | |
| `rating` | integer 1–5 | yes | |
| `title` | string ≤ 120 \| null | no | |
| `text` | string ≤ 5000 | yes | Plain text, in the language its author wrote it in. Not translated |
| `author` | string ≤ 80 | yes | The author's display name **as the store chooses to show it** — a name, initials, a description. It is all this API carries about the author |
| `date` | date | yes | The day it was written |
| `language` | BCP 47 \| null | no | The language of `text` |
| `version` | string \| null | no | The version reviewed, when the author said |

A store with no reviews answers an empty list and a summary with `count` 0.
This version has no operation that writes a review (§9).

### GET /v1/apps/{catalogue}/{app}/reports

Documents somebody wrote about the app. Newest first, unpaged.

```jsonc
{"items": [
  {"id": "rp_01J8T3QH", "kind": "security",
   "title": "Penetration test of the 31.0 release",
   "issuer": "Example Security GmbH", "date": "2026-07-02",
   "summary": "Two findings of medium severity, both fixed in 31.0.3. No finding of high severity.",
   "version": "31.0.3",
   "documentUrl": "https://media.store.example/reports/rp_01J8T3QH.pdf",
   "documentType": "application/pdf"}
]}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `id` | string | yes | |
| `kind` | string | yes | Open: `security`, `quality`, `accessibility`, `privacy`, `licence`, `other`. An unknown value is shown as `other` |
| `title` | string ≤ 200 | yes | |
| `issuer` | string ≤ 120 \| null | no | Who wrote it |
| `date` | date | yes | The day the document is dated |
| `summary` | string ≤ 2000 | yes | What it found, plain text |
| `version` | string \| null | no | The version of the app it is about |
| `documentUrl` | URL | yes | Opened in a separate window, never embedded |
| `documentType` | media type \| null | no | |

The store states who issued a report; it does not vouch for it, and nothing
on the cluster reads one.

## 6. Acquisitions

An **acquisition** is the store's record that a tenant has an app. Acquiring
is not installing: the store records it and, for an app whose artefacts are
in a private repository, mints the credential that repository checks. The
install happens afterwards, on the cluster, under the cluster's own checks.

A tenant has at most one live (`pending` or `confirmed`) acquisition per
coordinate.

| Status | Meaning | Carries |
|---|---|---|
| `pending` | A checkout is open | `checkoutUrl` |
| `confirmed` | The tenant has the app | `confirmation` |
| `cancelled` | The person left the checkout, or the acquisition was ended later. Ending an acquisition ends the credential issued for it (§7) | |
| `failed` | The checkout did not complete | `failure` |

`pending` becomes one of the other three and never the reverse; `confirmed`
may become `cancelled`. Closed.

**Acquisition**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `id` | string, `[A-Za-z0-9_-]{1,64}` | yes | The store's name for it. Opaque |
| `coordinate` | coordinate | yes | |
| `status` | see above | yes | |
| `createdAt`, `updatedAt` | date-time | yes | `updatedAt` is when the status last changed |
| `checkoutUrl` | URL \| null | yes | Present exactly while `pending`. On one of `checkoutOrigins`. Usable only by a person signed in to the store |
| `checkoutExpiresAt` | date-time \| null | yes | When an unfinished checkout is given up and the acquisition becomes `cancelled` |
| `failure` | `{reason, detail}` \| null | yes | Present exactly when `failed`. `reason` is open (`payment-declined`, `checkout-expired`, `not-available`); `detail` is for the person |
| `confirmation` | Confirmation \| null | yes | Present exactly when `confirmed` (§7) |

### POST /v1/acquisitions

```jsonc
// request
{"coordinate": "gentian/nextcloud-base-ee",
 "version": "31.0.4"}                        // optional; absent means the latest listed
```

| Header | Meaning |
|---|---|
| `Idempotency-Key` | Optional. 1–128 characters, unique per attempt. A request repeated with the same key and body within 24 hours answers what the first answered and creates nothing; the same key with another body is `409 idempotency-conflict` |

| Answer | When | Body |
|---|---|---|
| `201` | Acquired now: a free app, or a paid one that needs no checkout | The acquisition, `confirmed`, with its confirmation. `Location: /v1/acquisitions/{id}` |
| `202` | A checkout is needed | The acquisition, `pending`, with `checkoutUrl`. `Location`, and `Retry-After` |
| `200` | The tenant has a live acquisition of this coordinate already | That acquisition. When `confirmed`, its confirmation is for the `version` asked — or the latest it covers. This is how the app obtains the digest of a newer version of an app already acquired |
| `404 not-found` | The store lists no such coordinate | |
| `422 not-acquirable` | The app or version exists and this tenant cannot acquire it (withdrawn, not offered to it) | |

```jsonc
// 202 — a checkout is needed
{
  "id": "acq_01JA2M8Y",
  "coordinate": "gentian/nextcloud-base-ee",
  "status": "pending",
  "createdAt": "2026-10-01T09:14:03Z",
  "updatedAt": "2026-10-01T09:14:03Z",
  "checkoutUrl": "https://store.example/checkout/acq_01JA2M8Y",
  "checkoutExpiresAt": "2026-10-01T10:14:03Z",
  "failure": null,
  "confirmation": null
}
```

**Payment never passes through the cluster.** The app opens `checkoutUrl` in
a separate window; the person pays there, at the store, signed in to the
store. The app then asks `GET /v1/acquisitions/{id}` for the outcome. The
store does not tell the cluster: there is no callback.

### GET /v1/acquisitions/{id}

The outcome. While `pending` the answer carries `Retry-After`, which the app
honours before asking again, and never asks more often than every two
seconds. Once `confirmed` the answer carries the confirmation. `?version=`
names the version the confirmation should be for; otherwise it is for the
latest the acquisition covers.

**Reading the repository credential needs `store.acquire`.** The operation
itself needs only `store.read`.

| The token carries | A confirmed acquisition's confirmation |
|---|---|
| `store.read` and `store.acquire` | carries `repository.credential`, the one valid now, on the app and on each add-on |
| `store.read` alone | is the same with no `credential` member anywhere. The answer is `200`, not `403`: the acquisition may be read, the secret may not |

An acquisition of another tenant answers `404`, not `403`.

```jsonc
// 200 — confirmed
{
  "id": "acq_01JA2M8Y",
  "coordinate": "gentian/nextcloud-base-ee",
  "status": "confirmed",
  "createdAt": "2026-10-01T09:14:03Z",
  "updatedAt": "2026-10-01T09:16:40Z",
  "checkoutUrl": null,
  "checkoutExpiresAt": null,
  "failure": null,
  "confirmation": { /* §7 */ }
}
```

### GET /v1/acquisitions

Every acquisition of the tenant in the token, in any status, newest first,
paged; `?status=` filters. In this listing a confirmation's repositories
**never carry a `credential`**, whatever the scopes of the token: a
credential is handed out one acquisition at a time, by the three operations
around it, and only to a token with `store.acquire`.

### POST /v1/acquisitions/{id}/credential

**Replace this credential now.** Rotation, for when a credential may have
leaked or an administrator asks. No body. It replaces the credential of
every repository the acquisition's confirmation names with a newly minted
one and answers the **confirmation** (§7) with the new ones in place.
`?version=` as above. Nothing calls it on a schedule: a credential does not
expire (§7), so there is nothing to renew. Every store serves it.

| Rule | |
|---|---|
| Overlap | The credential it replaces keeps working for 24 hours from this call and is refused by the repository from then on. A cluster in the middle of replacing one never holds only a credential the repository refuses |
| User name | Unchanged: it is the tenant's (§7). Only the token is new |
| Ending the old one sooner | Only by ending the acquisition |
| No repository | The confirmation is answered unchanged |
| Not `confirmed` | `409 acquisition-not-confirmed` |

All four acquisition answers carry `Cache-Control: no-store`.

## 7. The confirmation

The confirmation is what the store hands the cluster's side when a tenant
has an app: which build, and — when the build's artefacts are in a private
repository — where they are and the credential that repository checks.

```jsonc
{
  "coordinate": "gentian/nextcloud-base-ee",
  "version": "31.0.4",                       // for a person; the cluster pins the digest, not this
  "digest": "sha256:3f6c0a1e5b7d9c2a4e6f8091a3b5c7d9e1f20314253647586970a1b2c3d4e5f6",
  "repository": {                            // absent for an app whose artefacts are public
    "type": "oci",
    "url": "oci://registry.store.example/apps",
    "credential": {                          // absent in GET /v1/acquisitions, and for a token without store.acquire
      "username": "tenant-7c1d9e02ab",       // one per tenant
      "token": "…",
      "expiresAt": null                      // it does not expire
    }
  },
  "addons": [                                // the add-ons this acquisition includes; [] when none
    {
      "coordinate": "gentian/nextcloud-office-ee",
      "version": "31.0.4",
      "digest": "sha256:0b1c2d3e4f5a69788796a5f4e3d2c1b00b1c2d3e4f5a69788796a5f4e3d2c1b0",
      "repository": { /* as above; usually the same repository */ }
    }
  ]
}
```

A free app's confirmation:

```jsonc
{"coordinate": "gentian/xwiki-ce", "version": "16.4.1",
 "digest": "sha256:5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e",
 "addons": []}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `coordinate` | coordinate | yes | The entry confirmed. The cluster's install request carries exactly this |
| `version` | string | yes | The version confirmed, for a person to read |
| `digest` | `sha256:<64 hex>` | yes | **What the cluster pins and verifies.** The cluster fetches the profile bundle from the catalogue source its own Cluster claim names for `<catalogue>` and installs only if those bytes hash to this value |
| `repository` | Repository | no | The private repository the app's chart and images are pulled from. Absent when they are public |
| `addons` | item[] | yes | Each with `coordinate`, `version`, `digest` and optionally `repository`, exactly as above. Add-ons do not nest |

**Repository**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `type` | `oci` | yes | Closed, one value: an OCI registry of charts and images. It is the only type a cluster pulls a tenant's app charts and images from with a credential today. The cluster's director also knows `git`; the app refuses it from a store, because on a cluster a git repository of apps is a source of profiles, which is what a store may not supply |
| `url` | `oci://<host>[/<path>]` | yes | No credentials in it, no trailing slash. It must contain the address the entry's profile bundle names for its chart: the same host and port, and a path that is a prefix of the chart's by whole segments. The cluster pulls from where the verified bundle says, and this only supplies the credential for that address. It must not overlap another repository the same tenant has declared: the cluster does not install a chart whose address lies inside two of a tenant's repositories, rather than guess which credential is meant. A chart inside none is pulled with no credential |
| `credential` | Credential | no | Absent in `GET /v1/acquisitions`, and in every answer to a token without `store.acquire` |

A repository carries no name. **The store names nothing on the cluster**;
see "The repository's name on the cluster" below. A store hands a tenant
the same `url` for the same repository every time, in one spelling.

**Credential**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `username` | string 1–255 | yes | The user name the registry expects with the token. **One per tenant**: the store chooses it, hands the same tenant the same one every time, and hands it to no other tenant. The cluster treats it as opaque and stores it beside the token. **Required**: the cluster's chart installer needs a user name and a password, and a token with no user name fails there |
| `token` | string 1–4096 | yes | The secret. No leading or trailing whitespace |
| `expiresAt` | date-time \| null | yes | `null`: the credential does not expire. A store issues every credential with `null` for now. The member stays, required, so that an expiry can be introduced later without changing the format; an app that is handed a date shows it and does nothing else on its account |

The credential is the tenant's **registry credential**, and it is **opaque
to the cluster**. The store mints it for one tenant and the store's
repository checks it on every pull; that check is where "this tenant may
have this app" is enforced, and it is the only place. The App Store app
hands the user name and the token to the cluster's custodian once and does
not keep them; the cluster stores them in its vault, hands them to what
pulls, and never interprets, logs or returns them. It is not the store
token (§3): that one authenticates a person to the store, this one pulls.

**One per tenant and repository.** The cluster keeps one credential for each
repository address a tenant has declared. Every confirmation a store hands
one tenant for the same `url` therefore carries the same `username` and a
token that pulls everything that tenant has acquired in that repository.

**It does not expire, for now.** Two things end a credential, and nothing
else does:

| | |
|---|---|
| **Rotation** | `POST /v1/acquisitions/{id}/credential` replaces it; the replaced one is refused 24 hours after that call (§6) |
| **The acquisition ending** | A credential stops pulling an app when the tenant's acquisition of it ends (`cancelled`), and is refused altogether once it serves no acquisition |

**What holding the credential does not guarantee.** It is what makes a
private app arrive on a cluster. It does not keep the app to one tenant of
that cluster:

| Limit on the cluster | Consequence for a store |
|---|---|
| The chart installer is one process for the cluster and caches charts by name and version | A chart one tenant pulled can be installed by another tenant of the same cluster whose profile names the same chart, until that process restarts. The credential is not disclosed; the chart is |
| An image already on a node can be started by any pod that does not force a pull | The same, for images |
| Image credentials reach a pod only through a chart that takes `imagePullSecrets` or `global.imagePullSecrets` | A private app's chart must take one of them, or its images must be public |

A store's repository therefore controls the first pull into a cluster. What
runs on a cluster after that is stated by the cluster's licence report, not
enforced by the credential.

The confirmation is a statement of facts. It is not signed, the cluster
verifies nothing about where it came from beyond the TLS connection, and it
grants nothing on the cluster.

**The repository's name on the cluster.** On a cluster a repository is
declared under a name, and that name is one object whoever declares it. The
store does not choose it. **The App Store app, on the cluster's side,
computes it** from the tenant's name on the cluster — the `{t}` of the
director's routes — and the confirmation's `repository.url`:

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

The same tenant and the same `url` always give the same name, so acquiring
a second app from a repository declares nothing new. Two tenants never share
a name, and neither do two addresses of one tenant: the hash is always
there, because the readable part alone is ambiguous — tenant `a-b` with
`oci://c.example` and tenant `a` with `oci://b.c.example` both read
`a-b-c-example`, and are `a-b-c-example-559df09f` and
`a-b-c-example-67520272`. Why 40, and what the cluster derives from the
name, is in [store-contract.md](../../design/store-contract.md) §6.4.

**What the App Store app does with it**, in this order, each step under the
signed-in person's own cluster token and the cluster's own checks:

```
for each distinct repository in the confirmation and its add-ons:
  name = the name computed above from the tenant and the repository's url
  director   PUT /v1/tenants/{t}/repositories/{name}       {"role": "apps", "type": "oci", "url": "<url>"}
  custodian  PUT /v1/credentials/repository-{name}         {"fields": {"username": "<username>", "password": "<token>"}}
then:
  director   POST /v1/tenants/{t}/apps/{app}               {"coordinate": "…", "digest": "…", "defaultGrant": true|false}
```

`defaultGrant` is the person's choice in the app ("install for everyone"),
not the store's.

## 8. Errors

Every answer with a status of 400 or above is `application/problem+json`
(RFC 9457):

```jsonc
{
  "type": "urn:gentian:store:problem:tenant-refused",   // the prefix, then code
  "code": "tenant-refused",                              // what the app branches on
  "title": "The store does not serve this tenant",       // fixed per code, in the language of the request
  "status": 403,
  "detail": "No licence report names https://acme.example. …",   // this occurrence, for a person
  "instance": "7f3a91c2",                                // optional: the store's reference, to quote
  "reason": "no-reports-for-tenant"                      // with tenant-refused only; the values of §4
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `type` | string | yes | `urn:gentian:store:problem:<code>` |
| `code` | string | yes | Open. The app treats one it does not know by its `status` |
| `title` | string | yes | |
| `status` | integer | yes | The HTTP status, repeated |
| `detail` | string | no | Plain text |
| `instance` | string | no | |
| `reason` | string | no | With `tenant-refused` |
| `errors` | `{field, detail}[]` | no | With `invalid-request`: the parameter or body member that is wrong |

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid-request` | A parameter or the body is malformed; `errors` names the fields. Also an expired cursor |
| 401 | `unauthenticated` | Signed-in calls only. No token, or one that is expired or not this store's. Carries `WWW-Authenticate: Bearer`. The app signs the person in again |
| 403 | `insufficient-scope` | Signed-in calls only. The token lacks the scope the operation needs |
| 403 | `tenant-refused` | `/v1/acquisitions` operations only. The store does not serve the tenant in the token; `reason` says why. Never from `GET /v1/tenant`, never from an open read |
| 404 | `not-found` | No such app, acquisition or page — or one that is another tenant's |
| 409 | `acquisition-not-confirmed` | A credential was asked for an acquisition that is not `confirmed` |
| 409 | `idempotency-conflict` | The `Idempotency-Key` was used before with a different body |
| 422 | `not-acquirable` | The app or version exists and cannot be acquired by this tenant |
| 429 | `rate-limited` | Carries `Retry-After` |
| 500 | `internal` | The store failed |
| 503 | `unavailable` | Temporarily not serving. May carry `Retry-After` |

What the app does when the store cannot be reached, or answers 5xx: it says
so, and offers nothing that would need the store. Everything installed is
unaffected; it does not depend on the store being there.

## 9. Open questions

Decisions this definition leaves to the store's owner, and points where a
choice was made that should be confirmed before the store is built on it.

**Answered, and no longer open:** who may act for a tenant (§3: the tenant
administrator's account, the tenant an organisation); credential lifetime
and renewal (§7: no expiry for now, rotation on request); where the
redirect lands (§3: one address per tenant, the App Store app's callback);
who names the repository (§7: the cluster's side); the user name
convention (§7: one per tenant); what an add-on's digest pins (built:
[store-contract.md](../../design/store-contract.md) §3 and §4).

1. **Token lifetimes.** How long an access token lasts, whether refresh
   tokens are issued, and how long those last. The app keeps tokens in its
   backend for the length of the administrator's cluster session and
   nowhere durable.
2. **How an account becomes the tenant's.** By what act a store associates
   the tenant administrator's account with a tenant the first time, what it
   checks, and how the association moves when the administrator changes.
3. **More than one account per tenant.** Outside the definition for now
   (§3).
4. **Cutting a leaked credential off at once.** Rotation leaves the
   replaced credential working for 24 hours. Whether a store needs a way to
   end it immediately, short of ending the acquisition.
5. **An ended acquisition and a shared credential.** A credential is one
   per tenant and repository (§7), so it may serve several acquisitions.
   The definition says it stops pulling the app whose acquisition ended and
   ends with the last; whether a registry is to enforce that per app is to
   be confirmed.
6. **Reviews.** Who may write one (any account, only an account whose
   tenant acquired the app), and whether that is an operation of this API
   or a page of the store's. This version reads reviews only.
7. **Only `oci`.** A store cannot hand over a git repository. If a store
   ever needs to, the cluster first needs a kind of repository that carries
   a credential without being a source of profiles.
8. **How stale is stale.** After how long without a report a store answers
   `reports-stale`, as a notice or as a refusal.
9. **Tax in the list price.** `price` is the list price. Whether it
   includes tax is the store's to state in `note` until the format needs
   to say.
10. **Open to whom.** The open reads take no token, so they are open to
    anyone who can reach the store, not only to clusters; a store cannot
    tell the two apart. Whether a store wants more than a rate limit in
    front of them is its own decision and does not change this format.
