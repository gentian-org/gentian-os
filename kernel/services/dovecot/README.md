# Kernel Dovecot (IMAP / LMTP)

Inbound MDA for `MAIL_SERVICE_MODE=kernel`: accepts mail from Postfix over LMTP,
files it per tenant domain, and authenticates IMAP against Keycloak with XOAUTH2.

No maintained public Dovecot Helm chart was available, so this is a raw Deployment
with a self-contained `dovecot.conf` in a ConfigMap.

## Authentication

IMAP users authenticate with a Keycloak access token (XOAUTH2), not a password.
The `passwd-file` passdb is deliberately empty and serves as the final deny.

One `oauth2` passdb exists per Keycloak realm whose users have mailboxes — the
kernel realm for the cluster admin, plus one per tenant using selfhosted mail —
because Dovecot's `oauth2` passdb takes a single introspection URL. The operator's
mail reconciler writes them into the `dovecot-realm-auth` Secret as
`<realm>.conf` + `<realm>.oauth2.ext`; `dovecot.conf` picks them up with
`!include_try /etc/dovecot/realms/*.conf`.

Adding a realm restarts this Pod. Dovecot reads passdb blocks only at startup, so
the operator stamps a hash of the realm set on the Pod template — unlike the
Postfix domain maps, which are `texthash:` files re-read per lookup and therefore
restart-free. Postfix retries delivery across the restart.

**Known gap:** removing a tenant does not remove its realm from
`dovecot-realm-auth`. A stale block costs one failed introspection per login
attempt against a realm that no longer exists; it does not deny valid users,
because `result_failure = continue` moves on to the next realm.

## Install location

| Item | Value |
| ------ | ------- |
| Manifests | `kernel/services/dovecot/manifests/` (env-parameterised) |
| Namespace | `platform-kernel` — the operator's `SERVICES_NAMESPACE` |
| Deployment / Service | `dovecot-<env>` (e.g. `dovecot-dev`) |
| LMTP DNS | `dovecot-dev.platform-kernel.svc.cluster.local:24` |
| Argo CD | ApplicationSet `gentian-infra-helm` (wave 9) |
| Bootstrap | `install.sh` step `D-04-mail` when `MAIL_SERVICE_MODE=kernel` |

## Image

- `dovecot/dovecot:2.3.21` on Docker Hub ([Dovecot CE Docker](https://doc.dovecot.org/latest/installation/docker.html))

The official image listens on non-privileged container ports; the Service maps cluster
port **24** (LMTP) and **143** (IMAP) to those ports.
