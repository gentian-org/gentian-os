# Kernel Postfix (SMTP relay)

**Status: UNTESTED** — placeholder using a public Helm chart and image. Not wired to
tenant mail reconciler behaviour yet; documents where cluster mail MTA belongs.

## Install location

| Item | Value |
|------|-------|
| Manifests | `kernel/services/postfix/manifests/<env>/` |
| Namespace | `gentian-<env>` (e.g. `gentian-dev`) |
| Release name | `postfix-<env>` (e.g. `postfix-dev`) |
| Service DNS | `postfix-dev.gentian-dev.svc.cluster.local:587` |
| Argo CD | ApplicationSet `gentian-infra-helm` (wave 9) |
| Bootstrap | `install.sh` step 15b when `MAIL_SERVICE_MODE=kernel` |

## Chart / image

- Chart: [bokysan/mail](https://artifacthub.io/packages/helm/docker-postfix/mail) (`https://bokysan.github.io/docker-postfix/`)
- Image: `boky/postfix` on Docker Hub (chart default)

Values merge order: `postfix-base-values` → `postfix-dev-values` → `postfix-sensitive-values` (OpenBao via ESO).
