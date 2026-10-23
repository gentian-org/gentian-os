# Cross-cluster identity: partner account linking

A person who has an account in a realm on one cluster and an account in a
realm on another links the two once. From then on an app on either cluster
can call an app on the other **on that person's behalf**, and the called app
acts as the person's own account there. Nothing in it is specific to an app:
any app on a cluster gets "a token for this person towards partner P" by one
standard call, and the receiving side turns it into its own local person. The
App Store's sign-in is one use of it, not a mechanism of its own.

The mechanism is Keycloak's, built from standards, and the same in both
directions:

| Concern | Standard | Keycloak 26.8 feature (status) |
|---|---|---|
| the link between the two accounts | OIDC identity brokering, federated identity | identity provider + first broker login (supported) |
| what the partner learns of the person | OIDC pairwise subject identifiers | pairwise `sub` mapper with a sector identifier (supported) |
| app A gets a token towards the partner | OAuth 2.0 Token Exchange, RFC 8693 | standard token exchange, `token-exchange-standard:v2` (supported, on by default) |
| app B turns it into a local token | JWT bearer authorization grant, RFC 7523 | JWT Authorization Grant (supported since 26.6) |
| apps authenticate to their realm without a secret | client assertions from a workload identity | federated client authentication, Kubernetes service account tokens (supported since 26.6) |

Not used: Keycloak's legacy token exchange (V1: external-to-internal,
internal-to-external, impersonation). It is preview and deprecated, needs
`--features=token-exchange` and fine-grained admin permissions v1, and the
JWT Authorization Grant replaces its external-to-internal case. Token
exchange delegation (`act`, preview) is not needed: the called app acts as
the person, not as "A on behalf of the person". The realm runs with no
preview feature switched on.

## 1. Terms

| Term | Means |
|---|---|
| **partner** | another realm — usually of another cluster — that this realm has a declared relation with, named locally (`name`) and identified by its issuer |
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
  (`as` = the name P gave X). It authenticates P by a JWT P's realm signs,
  checked against P's published keys: no secret crosses. Standard flow only;
  consent required; `fullScopeAllowed` off; default scope `partner-<name>`.
- **The scope `partner-<name>`**: an audience mapper putting P's issuer — and
  nothing else — in the access token's audience, and a pairwise `sub` mapper
  with the sector identifier `<P issuer>` and a salt generated once per
  partner. The same scope is the partner client's default and the optional
  scope of every app client allowed to call P, so the subject P stored at
  linking and the subject in every later assertion are the same value.
- **Per allowed app, the app's caller client** (§5): confidential,
  standard token exchange on, every other flow off, authenticated by the
  app's Kubernetes service account token, optional scope `partner-<name>`.

**Receiving side Y, per partner X:**

- **The identity provider `partner-<name>`** (Keycloak OIDC), issuer and keys
  read from X's discovery document, client ID = Y's own issuer (the partner
  client X made), client authentication *JWT signed with private key*. JWT
  Authorization Grant on, assertion reuse off, max assertion expiration
  2 minutes, clock skew 10 seconds, signature algorithm RS256, *limit access
  token expiration* on. Hidden on the login page; sync mode *import*; maps no
  group and no role.
- **What a first arrival becomes** (`accept.accounts`):
  - `link` — the person signs in to their existing account at Y once and the
    two are linked; no account is made through the provider;
  - `create` — an account is made at Y at the first arrival (registration),
    or an existing one linked as with `link` when the person proves it;
  - `organization` — every arrival is a member of an organization of Y's
    realm for X, with the domain given; for a partner whose people are a
    customer organization.
- **Per allowed app, the app's receiving client**: confidential, JWT
  Authorization Grant on with the allowed identity providers
  `partner-<name>`, authenticated by the app's Kubernetes service account
  token.

The person's group memberships and roles never cross. What a linked person
may do at Y is Y's own groups, as for any account of Y.

## 3. Declaring a partner

A partner is declared on the **Tenant**, because a realm is a tenant's:

```yaml
spec:
  partners:
    - name: shop                      # local name; scope/IdP partner-shop
      issuer: https://id.example.net/auth/realms/customers
      as: f3b1c0de-…                  # the name the partner gives this realm
      issue:                          # X role: our people towards them
        apps: [app-store]             # components that may request tokens
        share: []                     # what a person may consent to give: email, profile
      accept:                         # Y role: their people here
        accounts: link                # link | create | organization
        domain: ""                    # organization only
        apps: []                      # components that accept their tokens
```

- **Who approves.** A tenant's administrator (`can_administer`) adds,
  changes and removes the tenant's partners through the director; the commit
  names them. The cluster's administrator bounds it on the Cluster claim:
  `spec.partners.allowedIssuers[]` (patterns of issuers a tenant may name;
  empty means none) and may declare partners for any tenant. A partner
  outside the bounds is refused by the director and not composed by the
  operator.
- **Commissioned by a component.** A component that onboards many partners —
  a service that links every customer cluster that registers with it — adds
  receiving entries with a key of its own, as an organization commissioner
  does: its profile declares `requires.services.partners: {}`, the target
  tenant names it in `spec.partnerCommissioners[]`, and it calls
  `PUT/DELETE/GET /v1/tenants/<t>/partners[/<name>]` with its
  `RIGHTS_CHECK_KEY`. It can add `accept` entries only, never `issue`.
- **Trust establishment.** Only public data crosses: each side learns the
  other's issuer from its own administrator (or, for a commissioned entry,
  from the request the partner made), and everything else — endpoints,
  signing keys — from the issuer's discovery document over https. Keys are
  read by JWKS URL, so a key rotation on either side needs nothing on the
  other. `as` is the one name that must be agreed: the receiving side tells
  it (a commissioning service answers it to the registration request; two
  administrators exchange it with the issuer).
- **Same cluster.** Two tenants of one cluster keep `signInTo`/`signInFrom`,
  which give groups; a partner gives none.

## 4. Linking once

Linking is a sign-in at Y through X while the person is signed in at X. It
starts from whichever side the person is on.

```text
person        app A (X)       realm X              realm Y            app B (Y)
  | uses A       |                |                    |                  |
  |------------->| call B ------------------------------------------------>|
  |              |<-------------------------------------- 401 partner-link: <Y link address>
  |<-- open link page (new window) |                    |                  |
  |-------------------------------------------------- GET Y auth, kc_idp_hint=partner-<X>
  |<------------------------------ redirect to X (partner client)          |
  |--------------------------> X: session exists; consent once (sub, + share) |
  |<------------------------------ code to Y broker endpoint               |
  |----------------------------------------------------->| first broker login:
  |                                    link: sign in to the Y account once  |
  |                                    create: account made (registration)  |
  |                                    link 456y ↔ (partner-X, pairwise(123x))
  |<--------------------------------------------------- done; window closes
```

From Y's side the same link is made in Y's account console (*Linked
accounts*, "link partner X"), or by an app of Y sending the person to
Keycloak's application-initiated action `kc_action=idp_link:partner-<name>`.
Registration with a partner is this, with `accept.accounts: create`: what
else the partner wants to know — a billing contact, a company's name — it asks
in its own pages after the link, not through the realm.

## 5. App A calls app B for the person

An app is told the person by the edge as before: its route says
`exchangeToken` and the edge hands it a token whose audience is the app (the
app's caller client ID is that audience). A profile asks for partner access
with `requires.services.partnerTokens: {}`; the operator then makes the
caller client (issuing side) or the receiving client (receiving side), for
the partners whose `apps` name the component, and mounts nothing secret: the
app authenticates with its projected service account token
(`client_assertion_type=…:jwt-bearer`, audience its realm).

```text
app A (X)                    realm X                      app B (Y)         realm Y
  | token exchange: subject_token = person's A token,                          |
  |   scope = partner-<P>, client = A's caller client                          |
  |------------------------->| checks: A may (scope assigned), person active   |
  |<-------------------------| assertion: sub = pairwise(123), aud = Y issuer, |
  |                          |   exp ≤ 2 min, jti                              |
  |-- request, Authorization: Bearer <assertion> ----------->|                 |
  |                                                          |-- jwt-bearer grant, assertion
  |                                                          |   (B's receiving client)
  |                                                          |<- access token for 456y,
  |                                                          |   transient session, no refresh
  |<--------------------------------------------- answer as 456y               |
```

- **The call A makes** is the same for every partner: one token-exchange
  request to its own realm, `scope=partner-<name>`. A library in the SDK the
  platform publishes does it and caches nothing longer than the assertion.
- **What B does** is the same for every partner: post the assertion to its
  own realm and act as the person the answer names. It may keep the local
  token for its lifetime; a call with an expired one is answered 401, and A
  asks X for a new assertion.
- **Not linked**: Y answers `invalid_grant`; B answers 401 with
  `WWW-Authenticate: Partner link="<address>"`, where the address starts §4.
  A shows it to the person; nothing else happens.

## 6. Lifetimes and audience

| Token | Lifetime | Audience |
|---|---|---|
| assertion (X) | the realm's access-token lifetime, capped at Y by *max allowed assertion expiration* (2 min) | exactly Y's issuer — RFC 7523 refuses more than one value |
| Y's token from the grant | at most the assertion's remaining life (*limit access token expiration*) | B's client, as Y's mappers say |
| the link | until unlinked (§7) | — |

Assertions are single use (`jti`, reuse off). No refresh token exists on
either side of a cross-cluster call, so the longest a stopped relation still
works is the assertion's lifetime plus Y's token's: under five minutes.

## 7. Revocation

| Who | Does | Effect |
|---|---|---|
| the person, at Y | removes the link (account console, *Linked accounts*) | the next assertion is refused; B offers linking again |
| the person, at X | revokes consent for the partner client (account console, *Applications*) | no new link; no new assertion (the realm refuses an exchange for a client without consent) |
| X's tenant administrator | removes the partner, or its `issue` | client and scope removed: no sign-in, no assertion |
| Y's tenant administrator or commissioner | removes the partner, or its `accept` | identity provider removed and with it every link; the accounts stay at Y |
| either cluster's administrator | narrows `allowedIssuers` | the operator removes what falls outside |
| a person removed at X | — | no session, no exchange; Y's account stays, unreachable through X |

## 8. Privacy

- **Y learns** a pairwise subject: random to Y, stable for that person and
  that partner, different for every partner, so two partners cannot join
  their records by it. Y never sees X's user id, groups or roles.
- **Name and e-mail address** reach Y only when X's partner entry lists them
  in `share` and the person consents on the consent page at linking. Default:
  nothing but the subject. A partner that needs a contact (billing) asks the
  person for it in its own pages.
- **X learns** that the person linked, and when an app asked for an
  assertion — in its realm's event log, like any token.
- An assertion says nothing a partner did not already have at linking.

## 9. Failure modes

| Failure | Behaviour |
|---|---|
| Y's realm cannot reach X's discovery document or keys | linking and grants fail; Y keeps the last keys it read until they expire from its cache; nothing at X is affected |
| X's key rotated | Y fetches the new JWKS on an unknown `kid` |
| clocks apart | 10 s skew tolerated; beyond, the grant fails |
| assertion replayed | refused (`jti`) |
| B's client does not list the provider | `unauthorized_client`; a configuration error the Component reports |
| the partner client or scope missing at X | the exchange fails `invalid_scope`; the Tenant's status names the partner |
| a person's account at Y deleted | the link goes with it; B offers linking again |

## 10. Use

- **Tenant administrator**: adds a partner (Admin Console, *Partners*, or
  `kubectl gentian tenants partners <t> add …`), names the apps allowed per
  direction, chooses `accounts` and `share`; reads the partner's status
  (composed, keys reachable).
- **Cluster administrator**: sets `allowedIssuers` on the Cluster claim;
  may declare a partner for any tenant; names commissioners.
- **App developer**: declares `requires.services.partnerTokens: {}`; calls
  the token exchange (issuing) or the JWT grant (receiving) with the SDK
  helper; handles 401 `Partner link=` by showing the link to the person.

## 11. The App Store as a partner

The store is a partner of the tenant's realm, on the store's cluster — an
example of such a service is a vendor running its store as a tenant app of
its own Gentian cluster. Every store-specific piece is an instance of the
above:

| Store piece | Is |
|---|---|
| `spec.store.signIn` and its client `broker-to-store` | the tenant's partner entry for the store's realm with `issue: {apps: [app-store]}`; the App Store app asks the director for it (`can_administer`), with the issuer and `as` the store returned to the registration |
| `brokeredTo[]` and `broker-to-<name>` | a partner entry with `issue` |
| the store realm's provider `org-<tenant uid>` | the store tenant's partner entry `partner-<tenant uid>`, commissioned by the store's component |
| `brokeredFrom[]` with `domain` / `linkExisting` / `createAccounts` | `accept.accounts: organization` / `link` / `create` |
| `organizationCommissioners[]` and the `brokered-from` API | `partnerCommissioners[]` and the `partners` API |
| the tenant uid in the usage statistics report | unchanged: it is the `as` the store gives a tenant's realm, and tells the store which reported tenant a partner is |

The store contract (§6.1) then says: the App Store app runs **no sign-in
flow**. To act for the tenant it exchanges the administrator's token for an
assertion towards the store and sends it with the call; the store converts
it by the JWT grant and decides from its own records, as before, what that
account may do for the tenant — condition (c) unchanged. The tenant a call is
about is the issuer of the assertion, not a parameter. Registration is the
linking sign-in with `accounts: create`; the company details stay a form of
the store's. A store that is not linked answers 401 with the link address,
and browsing still needs no sign-in. AD-13's first exception — a code flow
run by the App Store app — falls away; requesting an assertion is a token
exchange at the tenant's own realm, which every app with `partnerTokens`
does, and holds no secret.

## 12. Order of work

1. Partner types on the Tenant and Cluster claim; director routes and CLI;
   operator composes both sides (clients, scope, pairwise and audience
   mappers, identity provider with JWT grant).
2. Federated client authentication: the realm trusts the cluster's service
   account issuer; caller and receiving clients per allowed component.
3. SDK helper for both calls; the 401 `Partner` convention.
4. Store: the App Store app and the store API on partner tokens; the store
   contract, the store API document and AD-13 rewritten to it; `store.signIn`,
   `brokeredTo`, `brokeredFrom` and `organizationCommissioners` removed.
5. Tests: a two-realm render test of both sides; an integration test that a
   linked person's assertion becomes the linked account, an unlinked one is
   refused, a replay is refused, and a removed partner stops within the
   stated bound.

Checked during step 1 against Keycloak 26.8.0, each with a test that fails
if Keycloak behaves otherwise: the pairwise mapper is accepted on a client
scope and applies to access tokens after `basic`'s `sub` mapper (otherwise
the operator puts the identical mapper, same sector and salt, on every client
that needs it); the exchanged token's audience is the single value the
scope maps; standard token exchange refuses a person who revoked consent for
the partner client (otherwise the person-at-X row of §7 needs a group gate).

## 13. Open

- **Where partners are declared first**: on the Tenant with the Cluster
  claim bounding it (as written), or on the Cluster claim only for a first
  step.
- **App client authentication**: Kubernetes service account tokens (as
  written; no secret, needs the cluster's service-account issuer reachable
  by Keycloak) or a generated client secret per component.
- **Which apps may call a partner**: listed per partner (as written) or any
  app of the tenant that declares `partnerTokens`.
- **`share`**: may a partner ask for name and e-mail at all, with the
  person's consent, or only ever the pairwise subject.
- **Existing store links** were made with the tenant realm's plain subject;
  with pairwise subjects each person links once more, or the store migrates
  its links.
