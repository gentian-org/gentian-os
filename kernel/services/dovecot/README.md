# Kernel Dovecot (IMAP / LMTP)

Inbound MDA for `mail.serviceMode: internal`: accepts mail from Postfix over LMTP,
files it per tenant domain, and signs IMAP in with a Keycloak token (XOAUTH2,
OAUTHBEARER) or an app password. Dovecot 2.4.

No maintained public Dovecot Helm chart is available, so this is a raw Deployment
with a self-contained `dovecot.conf` in a ConfigMap.

## Authentication

IMAP users authenticate with a Keycloak access token (XOAUTH2), or with an app
password minted per user and client type. The `passwd-file` passdb is
deliberately empty and serves as the final deny.

Dovecot checks a token itself (`introspection_mode = local`), without asking
Keycloak: the signature against the realm's published keys, the issuer among
the registered realms, `gentian-dovecot` in the audience, the scope `mailbox`,
and the expiry. The audience and the scope come from the realm's `mailbox`
client scope, which only the client of an app that declares
`requires.services.mail.imap.tokenSignIn` may ask for. The mailbox opened is the
token's `email` claim, and the address the mail program gave has to equal it.

Dovecot 2.4 has one `oauth2` block for the whole server. The operator's mail
reconciler keeps each realm's part in the `dovecot-realm-auth` Secret
(`<realm>.issuer`, `<realm>.keys`) and writes from all of them:

- `oauth2.inc`: the `oauth2` block with every registered issuer, and the
  mechanisms `xoauth2` and `oauthbearer`. They come with the block because,
  listed without it, the auth process does not start, and password sign-ins and
  Postfix's SASL go with it.
- `validation-keys`: a dict-file of the realms' signing keys. Dovecot looks a
  key up as `shared/<azp>/<alg>/<kid>`, under the app the token was issued to,
  so each key is written once per app of the realm that may be given `mailbox`.
  The kernel realm has none, so it brings its issuer and no keys.

Dovecot reads the dict again when the file changes, so a new app or a realm's
new key needs no restart. A change to `oauth2.inc` (a realm added) restarts this
Pod: the operator stamps a hash of the Secret, without the keys, on the Pod
template. A key the realm stopped publishing stays in Dovecot's memory until the
next restart. Dovecot does not ask Keycloak whether a token is still active: a
person disabled or signed out keeps mail access until the token in hand expires.

The files have names that Dovecot 2.3 did not include (`*.inc`, not `*.conf`),
and the operator removes what it wrote for 2.3, so neither version reads the
other's configuration while the two are rolled out.

`scripts/tests/test-mailbox-exchange-lab.sh` signs a person in with a token
against the pinned Keycloak and this image; `scripts/tests/test-mail-edge-lab.sh`
runs this chart's configuration behind the mail edge.

**Known gap:** removing a tenant does not remove its realm from
`dovecot-realm-auth`. Its issuer stays accepted; the realm is gone, so no token
of it exists.

## Install location

| Item | Value |
| ------ | ------- |
| Manifests | `kernel/services/dovecot/manifests/` (env-parameterised) |
| Namespace | `system-mail` — where the operator addresses mail |
| Deployment / Service | `dovecot-<env>` (e.g. `dovecot-dev`) |
| LMTP DNS | `dovecot-dev.system-mail.svc.cluster.local:24` |
| Argo CD | ApplicationSet `gentian-mail`, composed only when the claim's `mail.serviceMode` is `internal` |

## Image

- `dovecot/dovecot:2.4.5-root` on Docker Hub ([Dovecot CE Docker](https://doc.dovecot.org/latest/installation/docker.html))

The `-root` variant runs Dovecot's master as root and keeps the ports this
configuration names (24, 143, 993, 12345); the default variant runs as `vmail`
on ports above 31000. The backup and restore Jobs run the same image's
`doveadm` as uid 1000.
