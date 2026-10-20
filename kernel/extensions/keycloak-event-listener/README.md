# Keycloak event listener

Keycloak is the only place a group membership changes. OpenFGA holds a copy of
it, and this provider is what keeps the copy current: whenever a user's groups
may have changed, it states that user's **complete** group set to the director,
signed. The director compares the statement with what it holds and writes the
difference.

It holds a signing key and nothing else. It has no token, calls no API but the
one endpoint it is configured with, and the director believes nothing from it
beyond what a realm is entitled to say about its own tenant.

## What triggers a statement

| Keycloak event | Statement |
|---|---|
| admin: `GROUP_MEMBERSHIP` create / delete | the user's groups |
| admin: `USER` create | the user's groups (a user can be created with groups) |
| admin: `GROUP` update (rename) | the groups of every member |
| user: `LOGIN`, `REGISTER`, `IDENTITY_PROVIDER_FIRST_LOGIN` | the user's groups — default groups and federation mappers change membership without an admin event |
| model: group removed | `group.deleted` |
| model: user removed | `user.deleted` |

Only top-level groups are stated. Statements are sent after the transaction
commits, from one background thread, retried for about a minute.

## Wire format

```
POST <director-url>
X-Gentian-Signature: keyid=<key-id>,t=<unix seconds>,sig=<base64 Ed25519 over "<t>.<body>">

{"id":"<uuid>","time":<ms>,"realm":"demo","type":"user.memberships","user":"<user id>","groups":["gentian:tenant:demo:admins"]}
```

## Configuration

| Option (`spi-events-listener-gentian-director-…`) | Environment | |
|---|---|---|
| `director-url` | `KC_SPI_EVENTS_LISTENER_GENTIAN_DIRECTOR_DIRECTOR_URL` | the director's `/v1/events/keycloak` |
| `key-id` | `KC_SPI_EVENTS_LISTENER_GENTIAN_DIRECTOR_KEY_ID` | the id the director lists the public key under |
| `private-key-file` | `KC_SPI_EVENTS_LISTENER_GENTIAN_DIRECTOR_PRIVATE_KEY_FILE` | PKCS#8 PEM, Ed25519 |

The listener must also be enabled per realm (`eventsListeners` includes
`gentian-director`), with admin events on.

A key pair, and the public half in the form the director takes
(`DIRECTOR_LISTENER_KEYS=<key-id>=<base64>`):

```sh
openssl genpkey -algorithm ed25519 -out listener.pem
openssl pkey -in listener.pem -pubout -outform DER | tail -c 32 | base64
```

## Build

```sh
mvn package        # target/gentian-keycloak-event-listener.jar
```

`keycloak.version` in `pom.xml` must match the Keycloak the platform runs
(`keycloakVersion` in `kernel/services/keycloak-idp/manifests/values.yaml`): the
SPIs it implements are internal and move between minor releases too (the
admin permission classes the activation link uses changed package between
26.0 and 26.8). Bump both together and build before pushing.

## The registration form's proof-of-work check

The jar also carries `gentian-pow-captcha` (`io.gentianos.keycloak.captcha`),
a form action for the registration page of a tenant people sign up to
(`spec.signUp.enabled`; the tenant Composition puts it on the realm's own
registration flow). It asks the browser for a second of arithmetic -- a nonce
whose SHA-256 with the page's salt begins with 18 zero bits -- in place of
Keycloak's reCAPTCHA, which sends every visitor to Google. The challenge
lives on the authentication session and is spent on the first answer, so it
serves the registration it was made for and no other. The browser's half is
the login theme's `js/pow-captcha.js`; `PowChallengeTest` holds the two
halves to the same arithmetic.

## A client bound to a request parameter

`io.gentianos.keycloak.binding` holds two providers for a client whose one
registration serves many places, each with its own callback address computed
from a parameter of the authorization request
(`requires.services.identity.oidc.requestParameter`; docs/app-customization.md).
Both read the binding from the client's attributes, which the app Composition
writes, and from nowhere else (`RequestBinding`):

| Attribute | |
|---|---|
| `gentianos.io/request-bound-redirect` | `true`: what the realm's client policy matches |
| `gentianos.io/request-parameter` | the parameter, e.g. `tenant_url` |
| `gentianos.io/request-parameter-pattern` | what the value must be, matched as a whole |
| `gentianos.io/request-parameter-redirect-uri` | the one address, `${group}` filled from the match |

- `gentian-request-bound-redirect`, a client-policy executor: refuses an
  authorization request (`invalid_request`, an error page) unless the parameter
  is there once and matches, and `redirect_uri` is the address computed from
  it. The client is registered with `https://*`; this is what narrows it. The
  tenant Composition puts it in one profile and one policy per realm.
- `gentian-request-parameter-mapper`, a protocol mapper: puts the value in a
  claim at the exchange of a code, when that exchange's `redirect_uri` is the
  address computed from the value, and in no other token.

A value is matched against at most 512 characters, with a bound on the work
(`BoundedChars`): the pattern is a profile's, the value whoever sends the
request's. Anything missing or unreadable refuses the request and maps no claim.
