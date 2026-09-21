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

`keycloak.version` in `pom.xml` must match the Keycloak the platform runs: the
event listener SPI is private API and is not stable across major versions.
