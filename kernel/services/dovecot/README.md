# Kernel Dovecot (IMAP / LMTP)

**Status: UNTESTED** — placeholder using the public `dovecot/dovecot` image. No maintained
public Dovecot Helm chart was available; this uses a raw Deployment until Gentian ships a
first-party chart. Documents where cluster mail MDA belongs.

## Install location

| Item | Value |
|------|-------|
| Manifests | `kernel/services/dovecot/manifests/<env>/` |
| Namespace | `gentian-<env>` (e.g. `gentian-dev`) |
| Deployment / Service | `dovecot-<env>` (e.g. `dovecot-dev`) |
| LMTP DNS | `dovecot-dev.gentian-dev.svc.cluster.local:24` |
| Argo CD | ApplicationSet `gentian-infra-helm` (wave 9) |
| Bootstrap | `install.sh` step 15b when `MAIL_SERVICE_MODE=kernel` |

## Image

- `dovecot/dovecot:2.3.21` on Docker Hub ([Dovecot CE Docker](https://doc.dovecot.org/latest/installation/docker.html))

The official image listens on non-privileged container ports; the Service maps cluster
port **24** (LMTP) and **143** (IMAP) to those ports.
