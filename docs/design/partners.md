---
audience: architect, app-author, operator
type: design
---

# Partners: identity across clusters

A person who has an account in a realm on one cluster and an account in a
realm on another links the two once. From then on an app on either cluster
can call an app on the other **on that person's behalf**, and the called app
acts as the person's own account there. Nothing in it is specific to an app:
any app on a cluster gets "a token for this person towards partner P" by one
standard call, and the receiving side turns it into its own local person.
Only minimal information crosses: a pairwise subject, and for administrators
the one claim `gentian_roles`.

The mechanism is Keycloak's, built from standards, and the same in both
directions:

| Concern | Standard | Keycloak feature |
|---|---|---|
| the link between the two accounts | OIDC identity brokering, federated identity | identity provider + first broker login |
| what the partner learns of the person | OIDC pairwise subject identifiers | pairwise `sub` mapper with a sector identifier |
| app A gets a token towards the partner | OAuth 2.0 Token Exchange, RFC 8693 | standard token exchange (`token-exchange-standard:v2`) |
| app B turns it into a local token | JWT bearer authorization grant, RFC 7523 | JWT Authorization Grant |
| apps authenticate to their realm without a secret | client assertions from a workload identity | federated client authentication with Kubernetes service account tokens |

Keycloak's legacy token exchange (external-to-internal, internal-to-external,
impersonation) is not used: it is a preview feature, and the JWT
Authorization Grant covers its external-to-internal case. Delegation (`act`)
is not needed either: the called app acts as the person, not as "A on behalf
of the person". The realms run with no preview feature switched on.

## 1. Terms

| Term | Means |
|---|---|
| **partner** | another realm, usually of another cluster, that this realm has a declared relation with, named locally (`name`) and identified by its issuer |
| **issuing side** | the realm whose people sign in at the partner and on whose behalf tokens go out ("X") |
| **receiving side** | the realm that trusts the issuing realm about who a person is ("Y") |
| **link** | a federated identity at Y: Y's account 456 ↔ (identity provider `partner-<name>`, pairwise subject of 123 at X) |
| **assertion** | a short-lived access token X's realm issues for a person, whose only audience is Y's issuer |

A relation can be one-way or both ways: each direction is one switch on each
side (§3). A partner need not be a Gentian cluster; any realm that speaks
OIDC brokering and RFC 7523 can be one.

## 2. What each side holds

**Issuing side X, per partner P** (composed by the platform in the tenant's
realm):

- **The partner client**, client ID = P's issuer URL. Its one redirect URI is
  P's broker endpoint for X, `<P issuer>/broker/partner-<as>/endpoint`
  (`as` = the name P gives X, §3). It authenticates P by a JWT P's realm
  signs, checked against P's published keys: no secret crosses. Standard
  flow only; consent required; `fullScopeAllowed` off; its one default scope
  is `partner-<name>`.
- **The scope `partner-<name>`**: an audience mapper putting P's issuer, and
  nothing else, in the access token's audience; a pairwise `sub` mapper with
  the sector identifier `<P issuer>` and a salt the operator derives once per
  tenant and partner; and a roles mapper putting the person's role on the
  tenant in the access token as the multi-valued claim `gentian_roles`:
  `tenant-admin` for a member of `gentian:tenant:<t>:admins`, `cluster-admin`
  for a member of `gentian:tenant:<t>:platform-admins`, both for a person in
  both, and no claim for anybody else. It is Keycloak's user attribute mapper
  aggregating the group attribute `gentianPartnerRole` over the person's
  groups, which only those two groups carry, so the claim follows from group
  membership and from nothing else ([iam.md §1.2](iam.md)); in the access
  token only, never in an ID token or at userinfo, and on this scope only, so
  no other client of the realm sees it. The same scope is the partner
  client's default and an optional scope of every app client allowed to call
  P, so the subject P stored at linking and the subject in every later
  assertion are the same value.
- **Per app allowed to call P, the app's client `partner-<profile>`** (§5):
  confidential, standard token exchange on, every other flow off,
  authenticated by the app's Kubernetes service account token, default scope
  `basic` only, optional scope `partner-<name>`. An assertion therefore
  carries the pairwise subject and the person's role on the tenant, and
  nothing else about them.

**Receiving side Y, per partner X:**

- **The identity provider `partner-<name>`** (Keycloak OIDC), issuer X's,
  endpoints and keys under it, client ID = Y's own issuer (the partner client
  X made), client authentication *JWT signed with private key*. JWT
  Authorization Grant on, assertion reuse off, max assertion expiration
  2 minutes, clock skew 10 seconds, signature algorithm RS256, *limit access
  token expiration* on. Hidden on the login page; sync mode *import*; trusts
  no address; maps no group and no role.
- **What a first arrival becomes** (`accept.accounts`):
  - `link`: the person signs in to their existing account at Y once and the
    two are linked; no account is made through the provider;
  - `create`: an account is made at Y at the first arrival (registration),
    with no page of the provider's; what Y's own profile rules require more
    of is asked for by Y. An existing account is linked as with `link` when
    the person proves it;
  - `organization`: every arrival is a member of an organization of Y's realm
    for X, with the domain given; for a partner whose people are a customer
    organization.
- **Per app allowed to accept, the app's client `partner-<profile>`**:
  confidential, JWT Authorization Grant on for every accepted partner's
  provider, authenticated by the app's Kubernetes service account token.

The person's group memberships never cross, and of their roles only the one
claim `gentian_roles` carries: whether they administer the tenant, or the
cluster. What a linked person may do at Y is Y's own groups, as for any
account of Y; what Y makes of the role is Y's.

**Service account trust.** Each realm with a partner app holds the identity
provider `kubernetes` (Keycloak's Kubernetes provider) for the cluster's
service account issuer, which the operator reads from its own token. An app
client names its service account as `jwt.credential.sub`
(`system:serviceaccount:<namespace>:<account>`); Keycloak checks the token's
signature against the issuer's published keys and its audience against the
realm's issuer.

## 3. Declaring a partner

A partner is declared on the **Tenant**, because a realm is a tenant's
(`TenantPartner`, `api/v1alpha1/tenant_types.go`):

```yaml
spec:
  partners:
    - name: shop                      # local name; scope/IdP partner-shop
      issuer: https://id.example.net/auth/realms/customers
      as: f3b1c0de-…                  # the name the partner gives this realm; default: metadata.uid
      issue: true                     # X role: our people towards them
      accept:                         # Y role: their people here
        accounts: link                # link | create | organization
        domain: ""                    # organization only
        displayName: ""               # organization only
```

- **Who approves.** A tenant's administrator (`can_administer`) adds,
  changes and removes the tenant's partners through the director
  (`PUT`, `DELETE /v1/tenants/{t}/partners/{name}`; `GET` under `can_view`),
  which writes them as `partners.yaml` beside the tenant; the commit names
  them. The cluster's administrator bounds it on the Cluster claim:
  `spec.partners.allowedIssuers[]` (issuers a tenant may name, `*` matching
  any run of characters; empty means none) and may declare partners for any
  tenant. A partner outside the bounds is refused by the director and not
  composed by the operator.
- **Partners of every tenant.** The Cluster claim's
  `spec.partners.defaults[]` (`name`, `issuer`) are issuing partners of every
  tenant, those made later included; a tenant's own entry of the same name
  replaces one. The installer declares the store's realm there, with the
  store's extra services, and allows its issuer; `--no-extra-services` leaves
  it out, as nothing would use it.
- **Which apps.** Each partner lists the apps allowed to call it, and an app
  gets on that list by declaring the partner in its profile:
  `requires.services.partners.call: [<name>]`. An app that takes partners'
  assertions declares `requires.services.partners.accept: true` and is
  allowed for every partner the tenant accepts. Installing the app is the
  tenant administrator's approval of either.
- **Commissioned by a component.** A component that onboards many partners,
  a service that links every customer cluster that registers with it, adds
  receiving entries with a key of its own: its profile declares
  `requires.services.partners.commission: true`, the target tenant names it
  in `spec.partnerCommissioners[]`, and it calls
  `PUT/DELETE/GET /v1/tenants/<t>/partners[/<name>]` with its
  `RIGHTS_CHECK_KEY`. It can add `accept` entries only, never `issue`.
- **Trust establishment.** Only public data crosses: each side learns the
  other's issuer from its own administrator (or, for a commissioned entry,
  from the request the partner made), and everything else, endpoints and
  signing keys, from the issuer over https. Keys are read by JWKS URL, so a
  key rotation on either side needs nothing on the other. `as` is the one
  name that must be agreed: by default it is the tenant's `metadata.uid`,
  which the usage statistics report tells a store; two administrators
  exchange another one with the issuer.
- **Same cluster.** Two tenants of one cluster use `signInTo`/`signInFrom`,
  which give groups ([identity-brokering.md](../identity-brokering.md)); a
  partner gives none.

## 4. Linking once

Linking is a sign-in at Y through X while the person is signed in at X. It
starts from whichever side the person is on.

```text
person        app A (X)       realm X              realm Y            app B (Y)
  | uses A       |                |                    |                  |
  |------------->| call B ------------------------------------------------>|
  |              |<-------------------------------------- 401 Partner link="<Y link address>"
  |<-- open link page (new window) |                    |                  |
  |-------------------------------------------------- GET Y auth, kc_idp_hint=partner-<X>
  |<------------------------------ redirect to X (partner client)          |
  |--------------------------> X: session exists; consent once (subject only)
  |<------------------------------ code to Y broker endpoint               |
  |----------------------------------------------------->| first broker login:
  |                                    link: sign in to the Y account once  |
  |                                    create: account made (registration)  |
  |                                    link 456y ↔ (partner-X, pairwise(123x))
  |<--------------------------------------------------- done; window closes
```

From Y's side the same link is made in Y's account console (*Linked
accounts*), or by an app of Y sending the person to Keycloak's
application-initiated action `kc_action=idp_link:partner-<name>`.
Registration with a partner is this, with `accept.accounts: create`: what
else the partner wants to know, a billing contact, a company's name, it asks
in its own pages after the link, not through the realm.

## 5. App A calls app B for the person

An app is told the person by the edge: its route says `exchangeToken` and the
edge hands it a token whose audience is the app; the operator adds the app's
client `partner-<profile>` to that audience. A profile asks for partner
access with `requires.services.partners` (`call`, `accept`, and
`serviceAccount`, the account its pods run as; default: the profile's name).
The operator makes the client and writes, in the tenant's namespace, the
Secret `partners-<profile>`: `PARTNER_CLIENT_ID`, `PARTNER_TOKEN_URL` (the
realm's token endpoint on the cluster's network) and
`PARTNER_TOKEN_AUDIENCE` (the realm's issuer). Nothing in it is secret: the
app's chart mounts a projected service account token with that audience
(`valueMapping.platform.issuerKey` gives it at render) and presents it as its
client assertion.

```text
app A (X)                    realm X                      app B (Y)         realm Y
  | token exchange: subject_token = person's A token,                          |
  |   scope = partner-<P>, client assertion = A's service account token        |
  |------------------------->| checks: A may (scope assigned), person active   |
  |<-------------------------| assertion: sub = pairwise(123), aud = Y issuer, |
  |                          |   exp, jti                                      |
  |-- request, Authorization: Bearer <assertion> ----------->|                 |
  |                                                          |-- jwt-bearer grant, assertion
  |                                                          |   (B's client, B's service account token)
  |                                                          |<- access token for 456y,
  |                                                          |   no refresh
  |<--------------------------------------------- answer as 456y               |
```

- **The call A makes** is the same for every partner, one request to its own
  realm:

  ```text
  POST $PARTNER_TOKEN_URL
  grant_type=urn:ietf:params:oauth:grant-type:token-exchange
  client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
  client_assertion=<the projected service account token>
  subject_token=<the person's token the edge handed>
  subject_token_type=urn:ietf:params:oauth:token-type:access_token
  scope=partner-<name>
  ```

  The answer's `access_token` is the assertion. A keeps it no longer than
  its `expires_in`, and uses each one once.
- **What B does** is the same for every partner: post the assertion to its
  own realm (`grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`,
  `assertion=…`, the same client assertion) and act as the person the answer
  names (its `sub`). The issuer of the assertion says which partner, and so
  which tenant of the other side, the call is about; it is not a parameter.
- **Not linked**: Y answers `invalid_grant`; B answers 401 with
  `WWW-Authenticate: Partner link="<address>"`, where the address starts §4.
  A shows it to the person; nothing else happens.

## 6. Lifetimes and audience

| Token | Lifetime | Audience |
|---|---|---|
| assertion (X) | the realm's access-token lifetime, refused at Y beyond *max allowed assertion expiration* (2 min) | exactly Y's issuer; RFC 7523 refuses more than one value |
| Y's token from the grant | at most the assertion's remaining life (*limit access token expiration*) | B's client |
| the link | until unlinked (§7) | — |

Assertions are single use (`jti`, reuse off). No refresh token exists on
either side of a cross-cluster call, so the longest a stopped relation still
works is the assertion's lifetime plus Y's token's.

## 7. Revocation

| Who | Does | Effect |
|---|---|---|
| the person, at Y | removes the link (account console, *Linked accounts*) | the next assertion is refused; B offers linking again |
| the person, at X | revokes consent for the partner client (account console, *Applications*) | no new link; an existing link is ended at Y (row above): Keycloak's token exchange does not ask for the partner client's consent |
| X's tenant administrator | removes the partner, or its `issue` | client and scope removed: no sign-in, no assertion |
| Y's tenant administrator or commissioner | removes the partner, or its `accept` | identity provider removed and with it every link; the accounts stay at Y |
| either cluster's administrator | narrows `allowedIssuers` | the operator removes what falls outside |
| a person removed or disabled at X | — | no session, no exchange; Y's account stays, unreachable through X |

## 8. Privacy

- **Y learns** a pairwise subject: random to Y, stable for that person and
  that partner, different for every partner, so two partners cannot join
  their records by it; and, in an assertion, the person's role on the tenant
  (`gentian_roles`, §2): that they administer the tenant, or the cluster, and
  nothing when they do neither. Y never sees X's user id, name, e-mail
  address or groups, not at linking, not in an assertion. There is no switch
  to share more.
- **A partner that needs a contact** (billing) asks the person for it in its
  own pages, under its own privacy notice.
- **X learns** that the person linked, and when an app asked for an
  assertion: in its realm's event log, like any token.

## 9. Failure modes

| Failure | Behavior |
|---|---|
| Y's realm cannot reach X's keys | linking and grants fail; Y keeps the last keys it read until they expire from its cache; nothing at X is affected |
| X's key rotated | Y fetches the new JWKS on an unknown `kid` |
| clocks apart | 10 s skew tolerated; beyond, the grant fails |
| assertion replayed | refused (`jti`) |
| B's client does not list the provider | `invalid_grant` ("Identity Provider is not allowed for the client"); the profile does not declare `accept` |
| the partner scope missing on A's client | the exchange fails `invalid_scope`; the profile does not name the partner in `call` |
| a person's account at Y deleted | the link goes with it; B offers linking again |

## 10. Use

- **Tenant administrator**: adds a partner (`kubectl gentian tenants partners
  <t> add …`, [commands.md](../commands.md), or the director's `partners`
  API), chooses `issue` and `accept`; installs the apps that declare it. The
  operator's steps and what each switch composes:
  [identity-brokering.md](../identity-brokering.md).
- **Cluster administrator**: sets `allowedIssuers` and `defaults` on the
  Cluster claim; may declare a partner for any tenant; names commissioners.
- **App developer**: declares `requires.services.partners`
  ([app-customization.md §1.1](../app-customization.md)); mounts the
  projected service account token; makes the one call of §5 (issuing) or the
  JWT grant (receiving); handles 401 `Partner link=` by showing the link to
  the person.

## 11. What the realm configuration guarantees

Observed against Keycloak 26.8 with two realms:

- A pairwise mapper on a client scope decides the `sub` of an exchanged
  access token even beside `basic`'s `sub` mapper, and the ID token the
  partner client gives at linking carries the same value.
- The assertion's `aud` is exactly the partner's issuer; with the app
  client's default scope `basic` only it carries no name, address, group or
  role.
- Revoking consent does not stop the exchange: standard token exchange asks
  for the requesting client's consent, not the partner client's. §7 is
  written to that: the link is ended at the receiving side.
- The JWT grant answers an unlinked subject `invalid_grant` ("User not
  found") and a replay "Token reuse detected"; a client without the provider
  in `oauth2.jwt.authorization.grant.idp` (`##`-separated) refuses.
- Federated client authentication takes a service account token whose
  audience is the realm's issuer, more than once, and refuses another
  audience or another account.
- A first broker login with `create` makes the account with the pairwise
  subject as its user name and no address.
