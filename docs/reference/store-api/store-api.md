---
audience: store-vendor, architect
type: reference
---

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

The version of the definition is the `info.version` of the OpenAPI file. A
field or operation added in a later minor version is marked with that
version in the tables below; a client of an earlier minor version reads
every answer as before.

The report a cluster sends about itself, which a store depends on (§4), is a
separate format: [usage-statistics.openapi.yaml](usage-statistics.openapi.yaml),
spelled out in [operations.md §6.2](../../design/operations.md).

## 1. Ground rules

**The cluster calls; the store answers.** Every request here is made from
the cluster's side, outbound, over HTTPS. A store never calls a cluster: it
holds no credential for one, and a cluster exposes nothing to it. Nothing in
this API may be implemented in a way that needs the reverse direction — no
webhook, no callback into a cluster. The registration (§3) is a page of the
store's that the person's browser opens.

**Base address.** A cluster names its store by one `https` address
(`spec.catalogue.storeUrl` on its Cluster claim), with no trailing slash.
Every path below is appended to it: `https://store.example` + `/v1/apps`.

**Open reads and signed-in calls.** The API has two halves, and the line
between them is whether a call concerns a tenant.

| | Calls | Token |
|---|---|---|
| **Open reads** | `GET /v1/meta`, `/v1/categories`, `/v1/apps`, `/v1/apps/{catalogue}/{app}` and its `/reviews` and `/reports` | none needed. What a store offers can be seen at once, by anyone, with no account. Never `401`, never `403` |
| **Signed-in calls** | `GET /v1/tenant`, every `/v1/acquisitions` operation, a support request, the person's own review and company | required (§3) |

A token *may* be presented on an open read. The store may then add what it
knows of the token's tenant to the answer — one member, `acquired`,
on an app (§5). It is never required. A token the store cannot validate, or
one for a tenant it does not serve, is ignored on an open read: the answer
is the anonymous one, not an error.

Whether a store serves a tenant (`tenant-refused`, §4) is a question of the
signed-in calls only. An anonymous visitor sees the catalog whatever the
standing of their tenant, and learns at the first signed-in call whether the
store serves it.

**What the App Store app does.** It shows the catalog immediately, with no
store account, from the open reads. It acts for the person at the store only
when they acquire something or open what the tenant has acquired: it asks
`GET /v1/tenant` first, and shows the refusal or the notices it is given, or
the registration page when the person is not linked yet.

**What a store receives.**

| On | The store learns |
|---|---|
| an open read with no token | the path and query — which categories, apps and pages are looked at — the language asked for, and the address the request comes from, which is the cluster's outbound address. Nothing else |
| a signed-in call, or an open read that carries a token | in addition the assertion: the issuer of the tenant's realm and a subject only this store knows the person by — never their name or address |
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
| an open read, not personalized | `Cache-Control: public, max-age=<seconds>` (the store chooses; minutes for listings, longer for `/v1/meta`), `ETag`, `Vary: Accept-Language, Authorization` |
| an open read the store personalized from a token | `Cache-Control: private, no-store`, no `ETag`, the same `Vary` |
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
| `GET /v1/meta` | **open** | The store's name, API version, its realm as a partner (`partner.issuer`), its registration page, languages, allowed origins, features, the catalog sources its entries come from, the company sizes and industries it knows, its rules for reviews |
| `GET /v1/categories` | **open** | The categories apps are filed under |
| `GET /v1/apps` | **open** | A page of apps; filters `category`, `edition`, `q` |
| `GET /v1/apps/{catalogue}/{app}` | **open** | One app in full: description, screenshots, versions with digests, add-ons, links |
| `GET /v1/apps/{catalogue}/{app}/reviews` | **open** | A page of reviews and the summary of all of them |
| `GET /v1/apps/{catalogue}/{app}/reports` | **open** | Evaluations and reports about the app |
| `GET`, `PUT`, `DELETE /v1/apps/{catalogue}/{app}/reviews/mine` | signed in | The signed-in person's own review of the app: read, written or replaced, withdrawn (1.1) |
| `GET /v1/account/company` | signed in | The company the signed-in person is with (1.1) |
| `POST /v1/apps/{catalogue}/{app}/support-requests` | signed in | Ask a partner for support with an app: `202 received` |
| `GET /v1/tenant` | signed in | Whether the store serves the tenant the assertion's issuer is, the reason if not, notices to show, and the account's role |
| `GET /v1/acquisitions` | signed in | What this tenant has acquired; filter `status` |
| `POST /v1/acquisitions` | signed in | Acquire an app: `201` decided, `202` waiting (checkout, approval, confirmation), `200` already acquired. May say who the tenant is (`organization`, 1.14) |
| `POST /v1/acquisitions/free` | assertion, no account (1.13) | Acquire an app whose price model is `free` without registering; answers as `POST /v1/acquisitions` |
| `GET /v1/acquisitions/{id}` | signed in | One acquisition; once confirmed, its confirmation with the repository credential |
| `POST /v1/acquisitions/{id}/credential` | signed in | Replaces the repository credentials now; answers the confirmation with the new ones |

*Open* means no token is needed. One may be presented on the five catalog
reads, where it only adds `acquired`; `GET /v1/meta` ignores it.

## 3. Partner assertions

A signed-in call — `GET /v1/tenant`, the `/v1/acquisitions` operations, a
support request — carries a **partner
assertion** as `Authorization: Bearer`. Browsing needs none.

The store's realm is a partner of the tenant's realm (AD-17,
[partners.md](../../design/partners.md)). For each
call the App Store app makes one request to its own realm's token endpoint,
OAuth 2.0 Token Exchange (RFC 8693), with the person's token as
`subject_token`, `scope=partner-<name>` and its service account token as
client assertion. The answer is the assertion:

| Claim | Is |
|---|---|
| `iss` | the tenant's realm, `https://id.<cluster domain>/auth/realms/<realm>` — the tenant the call is about |
| `aud` | the store's realm (`partner.issuer` of `GET /v1/meta`), and nothing else |
| `sub` | a pairwise subject: random to the store, stable for the person, different for every partner |
| `exp`, `jti` | minutes; the store's realm takes each one once |

Nothing else about the person is in it: no name, no address, no group.

**What the store does.** It posts the assertion to its own realm, the JWT
bearer grant (RFC 7523), and acts as the account the answer names. It finds
the tenant by the assertion's issuer among the tenants the usage statistics
reports name (`tenants[].issuer`). A store decides from its own records what
that account may do for the tenant.

**Not linked.** Where the store's realm has no partner entry for the
tenant's realm yet, or the person has not linked their account, the grant
fails and the store answers `401` with
`WWW-Authenticate: Partner link="<registrationUrl>?tenant=<tenant uid>"`.
The app shows the address in a new window.

**Registration.** The store's registration page (`registrationUrl`) is the
linking sign-in: the store has its realm accept the tenant's realm as a
partner, named by the tenant's uid, and sends the person to sign in through
their own realm, which asks their consent once and gives the subject only;
the store's account is made at that first arrival. The page links the
store's terms and privacy notice and asks the company's name, size,
country, address and industry, each optional. A store that needs a contact
asks the person for it there or in its checkout. The first account that
registers for a tenant manages it (§4).

### GET /v1/meta

Open: it takes no token and ignores one. Read before anything else;
cacheable for the `max-age` it states.

```jsonc
{
  "apiVersion": "1.0",                       // <major>.<minor>; the major is the /v1 of the paths
  "name": "Example Store",
  "partner": {"issuer": "https://id.store.example/auth/realms/customers"},   // 1.11: the audience of an assertion
  "registrationUrl": "https://store.example/register",                       // 1.11
  "defaultLanguage": "en",
  "languages": ["en", "de", "fr", "it"],
  "mediaOrigins": ["https://media.store.example"],   // images come from here and nowhere else
  "checkoutOrigins": ["https://store.example"],      // a checkoutUrl is on one of these
  "features": ["reviews", "reports", "checkout"],
  "catalogues": [                            // optional, 1.5: where the entries come from
    {"name": "gentian", "url": "https://catalogue.example/gentian", "displayName": "Gentian OS"},
    {"name": "publisher", "url": "https://catalogue.publisher.example", "displayName": "Example Publisher"}
  ],
  "links": {"terms": "https://store.example/terms", "privacy": "https://store.example/privacy",
            "support": "https://store.example/support", "account": "https://accounts.store.example/account"}
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `apiVersion` | string | yes | `1.<minor>` |
| `name` | string ≤ 80 | yes | What the store calls itself |
| `partner.issuer` | URL | yes | The store's realm as a partner: the one audience of an assertion (1.11, §3) |
| `registrationUrl` | URL | yes | The registration page, opened with `?tenant=<tenant uid>` (1.11, §3) |
| `defaultLanguage` | BCP 47 | yes | The language answered in when none asked for matches |
| `languages` | BCP 47[] | yes | The languages the store has translations in |
| `mediaOrigins` | origin[] | yes | Where image URLs may point. The app fixes its content security policy from it |
| `checkoutOrigins` | origin[] | yes | Where a `checkoutUrl` may point |
| `features` | string[] | yes | Open. `reviews`, `reports`, `checkout` (an acquisition may answer `202`), `support` (apps carry support offers, and one may be requested; 1.3). Unknown values are ignored |
| `catalogues` | `{name, url, displayName?}[]` | no | The catalog sources the store's entries come from. `name` is the first half of the coordinates from that source; `url` its https address (`index.yaml` and `profiles/` beneath it); `displayName` plain text ≤ 80. A cluster's installer adds each one the cluster has no source of that name for, and changes or removes none ([store-contract.md](../../design/store-contract.md) §9). Added in 1.5 |
| `links` | object | no | `terms`, `privacy`, `support`, `account`: pages of the store's, each a URL |

## 4. Tenant standing

A store that depends on usage statistics serves a tenant only when the
reports it has received list that tenant. The app asks before the call the
person wanted and shows what it is told. Standing governs the signed-in
calls only: the catalog stays readable to a tenant the store refuses, and
to somebody who never registers. A notice is likewise seen only on a
signed-in call.

### GET /v1/tenant

Answers for the tenant the assertion's issuer is. It is the one signed-in call that
never answers `403 tenant-refused`: a refusal is stated in the body, so the
app has something to show.

A tenant the store serves, on a cluster with no subscription:

```jsonc
{
  "tenantUrl": "https://acme.example",       // the tenant the assertion's issuer is, as reported
  "known": true,                             // at least one usage statistics report lists this URL
  "lastReportAt": "2026-10-06T05:06:07Z",    // sentAt of the latest such report
  "served": true,
  "refusal": null,
  "notices": [
    {
      "type": "free-license-limit",
      "severity": "info",
      "title": "This cluster has no subscription",
      "text": "Terms text supplied by the store.",
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
    "detail": "No usage statistics report names https://acme.example. The store serves tenants of clusters that report; ask the cluster's administrator whether reporting is on.",
    "link": {"rel": "documentation", "label": "Usage statistics", "url": "https://store.example/help/reporting"}
  },
  "notices": []
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `tenantUrl` | string | yes | The URL of the tenant the assertion's issuer is, as its report names it |
| `known` | boolean | yes | Whether the store holds a usage statistics report listing this URL |
| `lastReportAt` | date-time \| null | no | `sentAt` of the latest such report |
| `served` | boolean | yes | Whether the store serves this tenant |
| `refusal` | object \| null | yes | Present exactly when `served` is false |
| `refusal.reason` | string | yes | Open; see below. For an unknown value the app shows `detail` |
| `refusal.detail` | string | yes | The reason in words, in the language of the request |
| `refusal.link` | Link | no | Where to read more |
| `notices` | Notice[] | yes | Shown in order. None of them blocks anything |

| `refusal.reason` | Meaning |
|---|---|
| `no-reports-for-tenant` | No usage statistics report lists this URL: the cluster does not report, or the tenant's realm is not the one the report names |
| `tenant-not-claimed` | Reports exist, and the account signed in is not associated with this tenant: it is not the account of the tenant's administrator, or another the store lets act for the tenant (§3). Not used by a store that serves such an account and lets it ask to manage the tenant (§4) |
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

**Who manages the tenant** (1.2). `GET /v1/tenant` carries the account's
own role, `"access": {"role": "manager"}`.

| Role | Meaning |
|---|---|
| `manager` | Manages the tenant: acquires for it. The account the store associated with the tenant, or one approved by an owner or administrator of the organization the tenant belongs to, or by the store's operator |
| `requested` | Asked to manage the tenant, by acquiring for it. Its acquisitions wait for `approval` |
| `declined` | Such a request, turned down. Its waiting acquisitions failed with `access-declined` |
| `none` | Signed in; nothing decided. Reads the tenant's standing and acquisitions |

An account that does not manage the tenant is not refused when it
acquires: its acquisition waits for `approval`. An owner or administrator
of the organization the tenant belongs to, or the store's operator, approves
or declines it, outside this API. Approving lets the account's waiting
acquisitions go on; declining fails them with `access-declined`, and ends a
`manager`'s management.

**Notice**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `type` | string | yes | Open. `free-license-limit`: the tenant's cluster has no subscription, and the text states the terms in the store's own words. `reports-stale`: the latest report is old. `message`: anything else. An unknown type is shown like `message` |
| `severity` | `info` \| `warning` | yes | Closed. How prominently to show it |
| `title` | string ≤ 120 \| null | no | A short heading |
| `text` | string ≤ 2000 | yes | The notice, plain text, in the language of the request |
| `link` | Link | no | |

**Link** (used throughout)

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `rel` | string | no | Open: `homepage`, `documentation`, `source`, `support`, `license`, `privacy`, `terms`, `subscription`, `donate`, `video` (1.6: a tutorial or demonstration video) |
| `label` | string ≤ 80 \| null | no | What to call it |
| `url` | URL | yes | Opened in a separate window |

A notice, a refusal and a subscription are the store's business with the
tenant. Nothing on the cluster is blocked by any of them: apps that are
installed keep running, and the command-line install does not ask a store.

## 5. Catalog

All five operations here are open reads (§1): no token, cacheable, never
refused for a tenant's standing.

An entry is identified by its **coordinate**, `<catalogue>/<app>`: the name
of a catalog source and the name of the app's profile in it, each a DNS
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
                 "distribution": {"1": 0, "2": 0, "3": 0, "4": 1, "5": 1}},
      "addonCount": 1,                       // optional, 1.4: how many add-ons the detail names
      "maturityScore": 7,                    // optional, 1.6: 0-10 as the store defines it, or null
      "installations": 120                   // optional, 1.6: how many tenants the store counts
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
| `addonCount` | integer ≥ 0 | no | How many add-ons the entry's detail names in `addons`. A client that needs them reads the detail. Added in 1.4 |
| `maturityScore` | integer 0–10 \| null | no | How mature the store judges the entry. What each point means is the store's to define and publish; a client shows the number and does not interpret it. `null` or absent: not scored. Added in 1.6 |
| `installations` | integer ≥ 0 | no | How many tenants the store counts as having the entry; what it counts is the store's to say. Added in 1.6 |

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
              "summary": "Edit documents in the browser.",
              "iconUrl": "https://media.store.example/icons/nextcloud-office.png",   // optional, 1.4
              "edition": "ee",                                                     // optional, 1.4
              "price": {"model": "subscription", "amount": "2.00", "currency": "CHF",
                        "per": "user", "period": "month", "note": null}}],         // optional, 1.4
  "addonOf": null,                           // the coordinate this entry extends, when it is an add-on
  "links": [{"rel": "documentation", "label": "Administration manual", "url": "https://publisher.example/docs"}],
  "license": {"spdx": null, "name": "Example Publisher Enterprise License", "url": "https://publisher.example/license"},
  "support": [                               // optional, 1.3: partners that offer support
    {"id": "so_01JA2K7M",
     "partner": {"slug": "example-integrations", "name": "Example Integrations GmbH",
                 "url": "https://integrations.example",
                 "logoUrl": "https://media.store.example/partners/example-integrations.png",
                 "verified": true},
     "countries": ["CH", "DE", "AT"], "languages": ["de", "en"],
     "tiers": [{"name": "Standard", "description": "Answers on working days within one day."}],
     "priceHint": "On request",
     "contactUrl": "https://integrations.example/contact",
     "requestable": true}
  ],
  "customization": {"grade": "A", "rungs": ["L0", "L1", "L3", "L4"]}   // optional, 1.6
}
```

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `description` | Markdown (`store-markdown-1`) ≤ 20 000 | yes | The long text |
| `screenshots` | Screenshot[] | yes | In the order to show them |
| `versions` | Version[] 1–50 | yes | Newest first |
| `addons` | `{coordinate, name, summary, iconUrl?, edition?, price?}[]` | yes | The add-ons that exist for this entry; each is a store entry of its own. `iconUrl`, `edition` and `price` are as in the summary, optional, added in 1.4 |
| `addonOf` | coordinate \| null | yes | When this entry is itself an add-on, what it extends |
| `links` | Link[] | yes | |
| `license` | `{spdx, name, url}` \| null | yes | The license the app is distributed under. `spdx` and `url` may be `null` |
| `support` | SupportOffer[] | no | The partners that offer support, in the order to show them, in the language of the request. Absent or empty when none. Added in 1.3 |
| `customization` | `{grade, rungs}` | no | How far the entry can be customized, as its profile declares it: `grade` `A`–`D` or `unknown`, `rungs` the supported rungs of the customization ladder (`L0`–`L6`), lowest first. Added in 1.6 |

`?country=` (ISO 3166-1 alpha-2, 1.3) narrows `support` to the offers that
cover that country, worldwide ones among them. Without it, an anonymous
caller is shown every offer; for a signed-in caller the store may narrow them
to a country it knows of the account (its company's), in a personalized
answer (`Cache-Control: private, no-store`). A `country` asked always wins.

**SupportOffer** (1.3, feature `support`). Plain text throughout.

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `id` | string ≤ 64 | yes | Opaque; what a support request names |
| `partner.slug` | string | yes | Stable across offers and languages |
| `partner.name` | string ≤ 120 | yes | |
| `partner.url` | URL \| null | yes | The partner's page, opened in a separate window |
| `partner.logoUrl` | URL \| null | yes | On one of `mediaOrigins`, PNG, JPEG or WebP like every image |
| `partner.verified` | boolean | yes | Whether the store verified the partner; what that involves is the store's |
| `countries` | ISO 3166-1 alpha-2[] | yes | The countries covered; empty means worldwide |
| `languages` | BCP 47[] | yes | The languages support is given in |
| `tiers` | `{name ≤ 80, description ≤ 500 \| null}`[] | yes | The levels offered |
| `priceHint` | string ≤ 200 \| null | yes | What it costs, as the partner puts it |
| `contactUrl` | URL \| null | yes | Opened in a separate window, never embedded |
| `requestable` | boolean | yes | Whether `POST …/support-requests` takes a request for it |

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
| `version` | string ≤ 64 | yes | The catalog version, as the catalog source's index states it |
| `digest` | `sha256:<64 hex>` | yes | The SHA-256 of this version's profile bundle — the exact bytes the catalog source serves. Lower case. **What the cluster pins and verifies** |
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

Since 1.1 a review also carries `recommend` (boolean | null), `scores`
(`features`, `easeOfUse`, `easeOfSetup`, and on an `ee` entry `support` and
`value`, each 1–5) and `reviewer` (`size` and `industry` of the author's
company, never its name), and the summary carries `recommend.yes`,
`recommend.no` and the mean and count of each score. `?companySize=` and
`?industry=` narrow the list and the summary alike.

### PUT /v1/apps/{catalogue}/{app}/reviews/mine

The signed-in person's review, written or replaced; one per person and
entry.

```jsonc
{"recommend": true,
 "scores": {"features": 4, "easeOfUse": 5, "easeOfSetup": 3, "support": null, "value": null},
 "title": "Does what we need", "text": "Running it for forty people since spring.", "version": "31.0.4"}
```

Refused with `409 company-missing` while the account names no company, with
`403 review-not-acquired` where the store requires an acquisition
(`reviewing.requireAcquisition`) and no tenant the person signed in for has
one, and with `400` for `support` or `value` on an entry that is not `ee`.
`GET` answers one's own review or `404`; `DELETE` withdraws it.

### GET /v1/account/company

The company the person is with: `{"name": "Acme AG", "size": "10-49",
"industry": "information-communication"}`, or `404` while there is none.
The store's operator records it, outside this API. The sizes and
industries, with their names, are in `GET /v1/meta`.

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
| `kind` | string | yes | Open: `security`, `quality`, `accessibility`, `privacy`, `license`, `other`. An unknown value is shown as `other` |
| `title` | string ≤ 200 | yes | |
| `issuer` | string ≤ 120 \| null | no | Who wrote it |
| `date` | date | yes | The day the document is dated |
| `summary` | string ≤ 2000 | yes | What it found, plain text |
| `version` | string \| null | no | The version of the app it is about |
| `documentUrl` | URL | yes | Opened in a separate window, never embedded |
| `documentType` | media type \| null | no | |

The store states who issued a report; it does not vouch for it, and nothing
on the cluster reads one.

### POST /v1/apps/{catalogue}/{app}/support-requests

Signed in (1.3, feature `support`). The person asks for one of the app's
support offers:

```jsonc
{"offer": "so_01JA2K7M",                     // a SupportOffer id of this entry
 "message": "We run it for sixty people and would like help with the upgrade to 32."}   // ≤ 2000, or null
```

Answered `202 {"id": "sr_01JA2M3Q", "status": "received"}`. What the store
does with the request — passing it to the partner, and how the partner
answers — is the store's; nothing on the cluster waits for it. `404` for no
such app or offer, `422 not-requestable` for an offer with `requestable:
false`, whose `contactUrl` is the way to it.

## 6. Acquisitions

An **acquisition** is the store's record that a tenant has an app. Acquiring
is not installing: the store records it and, for an app whose artifacts are
in a private repository, mints the credential that repository checks. The
install happens afterwards, on the cluster, under the cluster's own checks.

A tenant has at most one live (`pending` or `confirmed`) acquisition per
coordinate.

| Status | Meaning | Carries |
|---|---|---|
| `pending` | Not decided yet; `waitingFor` says on what: `confirmation` (the store, usually within a second or two), `approval` (the account does not manage the tenant yet, §4), or `checkout` | `waitingFor`; `checkoutUrl` while it waits for a checkout |
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
| `waitingFor` | `confirmation` \| `approval` \| `checkout` \| null | no | What a `pending` acquisition waits for; null otherwise. Open: an unknown value is treated like `confirmation`. Added in 1.2 |
| `checkoutUrl` | URL \| null | yes | Present exactly while `pending` and waiting for `checkout`. On one of `checkoutOrigins`. Usable only by a person signed in to the store |
| `checkoutExpiresAt` | date-time \| null | yes | When an unfinished checkout is given up and the acquisition becomes `cancelled` |
| `failure` | `{reason, detail}` \| null | yes | Present exactly when `failed`. `reason` is open (`payment-declined`, `checkout-expired`, `not-available`, `access-declined`); `detail` is for the person |
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
| `201` | Decided at once: usually a free app the account may acquire | The acquisition, `confirmed` with its confirmation, or (rarely) `failed`. `Location: /v1/acquisitions/{id}` |
| `202` | Not decided yet: a checkout is needed, an approval, or the store's own confirmation | The acquisition, `pending`, with `waitingFor` (and `checkoutUrl` while it waits for a checkout). `Location`, and `Retry-After` |
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
  "waitingFor": "checkout",
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

### POST /v1/acquisitions/free

Added in 1.13, with the feature `free-acquisition`. A tenant takes an app
whose price model is `free` without anybody registering with the store. The
store stays the point of record of every tenant that takes an app from it,
under its terms; it makes no account.

```jsonc
{
  "coordinate": "gentian/xwiki-ce",
  "tenant": "3f2b9c1e-7d4a-4e0b-9a51-2c8d6f0e1b7a",   // the tenant's metadata.uid
  "terms": {"version": "2026-10", "acceptedAt": "2026-10-24T09:12:00Z"}  // as spec.storeTerms records it
}
```

The bearer is a partner assertion obtained as for any signed-in call (§3).
The store does not post it to its realm; it checks it itself and takes it
only when all of these hold:

| Rule | Otherwise |
|---|---|
| The entry's price model is `free`. `freemium` and every priced model go the signed-in way | `403 registration-required`, `WWW-Authenticate: Partner link="…"` |
| The signature verifies with a key the issuer publishes (`<iss>/.well-known/openid-configuration` → `jwks_uri`, both `https`; cached for minutes, read again for an unknown key id) | `401` |
| The latest usage statistics report naming the tenant `uid` names this issuer for it, on the reporting cluster's own domain | `403 tenant-refused`, `no-reports-for-tenant` |
| `aud` is exactly the store's realm (`partner.issuer`); not expired; `jti` not seen before | `401` |
| The tenant accepted the terms in force: the report says so, or — where the report has not caught up — the body does, and the store records that the acceptance came with the request. A store with no `termsVersion` takes nothing this way | `403 terms-not-accepted` |

The acquisition is recorded against the tenant `uid` and the pairwise
subject. The answers are those of `POST /v1/acquisitions`; while one is
`pending` the client asks again with this call and a new assertion, since
`GET /v1/acquisitions/{id}` needs an account. A confirmation answered here
carries its repository credential: it is the tenant's.

### GET /v1/acquisitions/{id}

The outcome. While `pending` the answer carries `Retry-After`, which the app
honors before asking again, and never asks more often than every two
seconds. Once `confirmed` the answer carries the confirmation. `?version=`
names the version the confirmation should be for; otherwise it is for the
latest the acquisition covers.

A confirmed acquisition's confirmation carries `repository.credential`, the
one valid now, on the app and on each add-on — for an account that manages
the tenant (§4). For any other account it is the same with no `credential`
member anywhere. The answer is `200`, not `403`: the acquisition may be
read, the secret may not.

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
**never carry a `credential`**: a credential is handed out one acquisition
at a time, by the three operations around it.

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
has an app: which build, and — when the build's artifacts are in a private
repository — where they are and the credential that repository checks.

```jsonc
{
  "coordinate": "gentian/nextcloud-base-ee",
  "version": "31.0.4",                       // for a person; the cluster pins the digest, not this
  "digest": "sha256:3f6c0a1e5b7d9c2a4e6f8091a3b5c7d9e1f20314253647586970a1b2c3d4e5f6",
  "repository": {                            // absent for an app whose artifacts are public
    "type": "oci",
    "url": "oci://registry.store.example/apps",
    "credential": {                          // absent in GET /v1/acquisitions, and for an account not managing the tenant
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
| `digest` | `sha256:<64 hex>` | yes | **What the cluster pins and verifies.** The cluster fetches the profile bundle from the catalog source its own Cluster claim names for `<catalogue>` and installs only if those bytes hash to this value |
| `repository` | Repository | no | The private repository the app's chart and images are pulled from. Absent when they are public |
| `addons` | item[] | yes | Each with `coordinate`, `version`, `digest` and optionally `repository`, exactly as above. Add-ons do not nest |

**Repository**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `type` | `oci` | yes | Closed, one value: an OCI registry of charts and images. It is the only type a cluster pulls a tenant's app charts and images from with a credential. The cluster's director also knows `git`; the app refuses it from a store, because on a cluster a git repository of apps is a source of profiles, which is what a store may not supply |
| `url` | `oci://<host>[/<path>]` | yes | No credentials in it, no trailing slash. It must contain the address the entry's profile bundle names for its chart: the same host and port, and a path that is a prefix of the chart's by whole segments. The cluster pulls from where the verified bundle says, and this only supplies the credential for that address. It must not overlap another repository the same tenant has declared: the cluster does not install a chart whose address lies inside two of a tenant's repositories, rather than guess which credential is meant. A chart inside none is pulled with no credential |
| `credential` | Credential | no | Absent in `GET /v1/acquisitions`, and for an account that does not manage the tenant |

A repository carries no name. **The store names nothing on the cluster**;
see "The repository's name on the cluster" below. A store hands a tenant
the same `url` for the same repository every time, in one spelling.

**Credential**

| Field | Type | Req. | Meaning |
|---|---|---|---|
| `username` | string 1–255 | yes | The user name the registry expects with the token. **One per tenant**: the store chooses it, hands the same tenant the same one every time, and hands it to no other tenant. The cluster treats it as opaque and stores it beside the token. **Required**: the cluster's chart installer needs a user name and a password, and a token with no user name fails there |
| `token` | string 1–4096 | yes | The secret. No leading or trailing whitespace |
| `expiresAt` | date-time \| null | yes | `null`: the credential does not expire, and a store issues every credential with `null`. The member is required so that an expiry could be introduced without changing the format; an app that is handed a date shows it and does nothing else on its account |

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

**It does not expire.** Two things end a credential, and nothing
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
runs on a cluster after that is stated by the cluster's usage statistics report, not
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
  "detail": "No usage statistics report names https://acme.example. …",   // this occurrence, for a person
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
| 403 | `not-tenant-manager` | `POST /v1/acquisitions/{id}/credential` only. Only an account that manages the tenant is handed a credential |
| 403 | `registration-required` | `POST /v1/acquisitions/free` only. The app is not free; the registration page is in `WWW-Authenticate: Partner link=…` |
| 403 | `terms-not-accepted` | `POST /v1/acquisitions/free` only. The tenant has not accepted the store's terms in force |
| 403 | `tenant-refused` | `/v1/acquisitions` operations only. The store does not serve the tenant in the token; `reason` says why. Never from `GET /v1/tenant`, never from an open read |
| 404 | `not-found` | No such app, acquisition, support offer or page — or one that is another tenant's |
| 409 | `acquisition-not-confirmed` | A credential was asked for an acquisition that is not `confirmed` |
| 409 | `idempotency-conflict` | The `Idempotency-Key` was used before with a different body |
| 422 | `not-acquirable` | The app or version exists and cannot be acquired by this tenant |
| 422 | `not-requestable` | The support offer exists and takes no request through the API (1.3) |
| 429 | `rate-limited` | Carries `Retry-After` |
| 500 | `internal` | The store failed |
| 503 | `unavailable` | Temporarily not serving. May carry `Retry-After` |

What the app does when the store cannot be reached, or answers 5xx: it says
so, and offers nothing that would need the store. Everything installed is
unaffected; it does not depend on the store being there.

## 9. What the definition leaves to the store

The format decides what crosses between the App Store app and a store, and
nothing else. Three things a store might expect to find here are absent by
design:

* **No sign-in flow and no store token.** A signed-in call carries a partner
  assertion the tenant's own realm issues (§3). The App Store app runs no
  authorization-code flow against the store, keeps no token of the store's,
  and the store issues none to it. There are therefore no token lifetimes
  to agree on: an assertion lasts minutes and is used once.
* **No redirect and no callback.** Nothing of the store's lands on the
  cluster: not a redirect after a sign-in, not a callback after a checkout.
  The registration page and the checkout are the store's own pages, opened
  in a separate window; the app learns the outcome by asking
  (`GET /v1/acquisitions/{id}`).
* **No name for anything on the cluster.** The repository's name is
  computed on the cluster's side (§7), the tenant a call is about is the
  assertion's issuer, and the install request is the cluster's own.

What a store decides for itself, and states in its own terms:

1. **Cutting a leaked credential off at once.** Rotation leaves the
   replaced credential working for 24 hours (§6). A store that needs to end
   one immediately ends the acquisition.
2. **An ended acquisition and a shared credential.** A credential is one
   per tenant and repository (§7), so it may serve several acquisitions. It
   stops pulling the app whose acquisition ended and ends with the last;
   whether the registry enforces that per app is the store's.
3. **Only `oci`.** A store hands over no git repository. A cluster has no
   kind of repository that carries a credential without being a source of
   profiles, and a store may not supply profiles.
4. **How stale is stale.** After how long without a report a store answers
   `reports-stale`, as a notice or as a refusal (§4).
5. **Tax in the list price.** `price` is the list price. Whether it includes
   tax is the store's to state in `note` (§5).
6. **Who may write a review.** `reviewing.requireAcquisition` in
   `GET /v1/meta` says whether a review needs an acquisition; a company on
   the account is required either way (§5).
7. **Open to whom.** The open reads take no token, so they are open to
   anyone who can reach the store, not only to clusters; a store cannot tell
   the two apart. Whether a store wants more than a rate limit in front of
   them is its own decision and does not change the format.
