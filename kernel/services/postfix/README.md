# Kernel Postfix (SMTP relay)

Outbound relay and inbound MTA for `MAIL_SERVICE_MODE=system`, built on a public
Helm chart and image. Wired to the tenant mail reconciler: the operator maintains
the virtual-domain and mailbox maps this deployment mounts, so tenant churn needs
no restart.

Inbound delivery to tenant mailboxes goes on to Dovecot — see
`kernel/services/dovecot/README.md`.

## Install location

| Item | Value |
| ------ | ------- |
| Manifests | `kernel/services/postfix/manifests/` (env-parameterised) |
| Namespace | `system-mail` — where the operator addresses mail |
| Release name | `postfix-<env>` (e.g. `postfix-dev`) |
| Service DNS | `postfix-dev.system-mail.svc.cluster.local:587` |
| Argo CD | ApplicationSet `gentian-mail`, composed only when the claim's `mail.serviceMode` is `system` |

The operator writes `postfix-kernel-virtual-mailbox-maps` into `system-mail`,
and a Pod can only mount a ConfigMap from its own namespace — so Postfix runs
where the operator addresses mail.

## Chart / image

- Chart: [bokysan/mail](https://artifacthub.io/packages/helm/docker-postfix/mail) (`https://bokysan.github.io/docker-postfix/`)
- Image: `boky/postfix` on Docker Hub (chart default)

Values merge order: `postfix-base-values` → `postfix-dev-values` → `postfix-sensitive-values` (OpenBao via ESO).
