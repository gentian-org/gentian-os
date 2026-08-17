# Kernel Postfix (SMTP relay)

Outbound relay and inbound MTA for `MAIL_SERVICE_MODE=kernel`, built on a public
Helm chart and image. Wired to the tenant mail reconciler: the operator maintains
the virtual-domain and mailbox maps this deployment mounts, so tenant churn needs
no restart.

Inbound delivery to tenant mailboxes additionally requires Dovecot, which is not
yet configured — see `kernel/services/dovecot/README.md`.

## Install location

| Item | Value |
| ------ | ------- |
| Manifests | `kernel/services/postfix/manifests/` (env-parameterised) |
| Namespace | `platform-kernel` — the operator's `SERVICES_NAMESPACE` |
| Release name | `postfix-<env>` (e.g. `postfix-dev`) |
| Service DNS | `postfix-dev.platform-kernel.svc.cluster.local:587` |
| Argo CD | ApplicationSet `gentian-infra-helm` (wave 9) |
| Bootstrap | `install.sh` step `D-03-mail` when `MAIL_SERVICE_MODE=kernel` |

The namespace is not `gentian-<env>`. `mailSharedPostfixHost()` hands every tenant
app `postfix-<stage>.<servicesNamespace>`, the operator writes
`postfix-kernel-virtual-mailbox-maps` into that namespace, and a Pod can only mount
a ConfigMap from its own namespace — so Postfix runs where the operator addresses
it. The tenant NetworkPolicy baseline already permits egress to `platform-kernel`.

## Chart / image

- Chart: [bokysan/mail](https://artifacthub.io/packages/helm/docker-postfix/mail) (`https://bokysan.github.io/docker-postfix/`)
- Image: `boky/postfix` on Docker Hub (chart default)

Values merge order: `postfix-base-values` → `postfix-dev-values` → `postfix-sensitive-values` (OpenBao via ESO).
