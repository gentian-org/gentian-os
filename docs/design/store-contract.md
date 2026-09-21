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
GET /v1/tenants/{t}/apps/{p}/addons
GET /v1/tenants/{t}/entitlements          the recorded facts
```

Reads are authorised by `can_view`, never by the write relation.
