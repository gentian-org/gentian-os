# Mail Kernel Extension

**Companion to:** [architecture.md](../architecture.md)

---

## 0. Cluster reality vs documentation layers

Three layers are easy to conflate:

| Layer | Knob | What it controls on dev today |
|---|---|---|
| **Cluster install** | `MAIL_SERVICE_MODE` in `install.env` (`external` \| `kernel`) | Whether `install.sh` step **15b** deploys Postfix/Dovecot helm Releases into **`gentian-dev`** and whether Postfix relays to an external SMTP host or delivers via Dovecot LMTP |
| **Per tenant** | `Tenant.spec.mail.mode` | What the **gentian-os operator** provisions: virtual domains, DKIM paths, `smtp-credentials-*` Secrets, DNS guidance |
| **Per app** | `AppProfile` `mail.smtp` / `mail.imap` | Whether the operator creates app SMTP/IMAP secrets pointing at kernel or external endpoints |

**In-cluster SMTP today (when kernel Postfix is deployed):**

```
postfix-dev.gentian-dev.svc.cluster.local:587
```

Legacy docs referred to `postfix.platform-kernel.svc.cluster.local`; that
Service does not exist on the current dev layout. The operator writes the
`gentian-dev` hostname into tenant `smtp-credentials-*` Secrets.

**Operational checks (dev):**

```bash
kubectl get release postfix-dev dovecot-dev -n gentian-dev
kubectl get pods -n gentian-dev -l 'app.kubernetes.io/name in (postfix,dovecot)'
kubectl get tenants -o custom-columns='NAME:.metadata.name,MAIL:.spec.mail.mode'
```

Switching install mode: [commands.md](../commands.md) §9 (`./update.sh` after editing `install.env`).

---

## 1. Why Mail Is an Extension, Not Core Kernel

Not every deployment of Gentian OS needs self-hosted mail. Some
tenants use Gmail or an existing on-prem mail server; others need only
outbound SMTP for notifications. Mail is therefore modelled as an
**optional kernel extension** — shared infrastructure with
tenant-scoped configuration, enabled per cluster.

Apps consume mail through the `smtp` and `imap` kernel requirements
declared in their `AppProfile`. The platform satisfies those
requirements differently depending on each tenant's chosen mode.

## 2. Tenant Mail Modes

Selected via `Tenant.spec.mail.mode`:

| Mode | Transport | Storage | Client | Use case |
|---|---|---|---|---|
| `selfhosted` | Shared kernel MTA | Shared kernel MDA (tenant-scoped path) | App → kernel mail | Full self-hosted mail, shared infrastructure |
| `external` | Tenant's own | Tenant's own | App → external IMAP/SMTP | Tenant uses Gmail / existing mail server |
| `transport-only` | Shared kernel MTA | External | App → external storage | Kernel handles SMTP relay only |
| `disabled` | — | — | App → SMTP relay (outbound only) | Outbound notifications only |

If `MAIL_SERVICE_MODE=external` at install time, kernel Dovecot/LMTP
may be absent; tenants in `selfhosted` mode still get operator-managed
secrets but full delivery requires switching install mode to `kernel`
and re-running `update.sh`.

## 3. Shared Infrastructure with Tenant-Scoped Configuration

When the extension is enabled, **one** Postfix, **one** Dovecot, and
**one** Rspamd instance handle all tenant domains. Tenant isolation is
enforced at the configuration level:

- **Postfix:** `virtual_mailbox_domains` lists all tenant domains;
  per-tenant SASL credentials authenticate SMTP submission.
- **Dovecot:** mailboxes stored at isolated paths
  (`/var/mail/{domain}/{user}`); IMAP authenticates against the
  tenant's LDAP OU.
- **Rspamd:** spam filtering for all tenants; DKIM signing uses
  per-domain keys fetched from OpenBao at runtime.

This is the same model every other shared kernel component uses:

| Component | Isolation mechanism | Pods |
|---|---|---|
| Keycloak | Realm per tenant | 2–3 (shared) |
| PostgreSQL | Database + user per tenant | 2–3 (shared) |
| MinIO | Bucket + IAM policy per tenant | 2–3 (shared) |
| Redis | ACL user per tenant | 2–3 (shared) |
| **Mail** | **SASL credentials + mailbox path per tenant** | **6–9 (shared)** |

## 4. Trade-Off: Blast Radius

The one genuine trade-off compared to a fully per-tenant stack is
blast radius: a shared Postfix crash affects all tenants
simultaneously. This is mitigated by standard HA practices (2+
replicas, PodDisruptionBudget) — the same approach used for every
other shared kernel component.

## 5. Provisioning Flow (selfhosted mode)

When a Tenant with `mail.mode: selfhosted` is reconciled, the
**operator** (not a finished Crossplane-only pipeline) performs:

1. **DKIM keypair Secret** in OpenBao at
   `tenants/{name}/mail/dkim`. Generated deterministically from the
   master password + tenant name.
2. **Virtual domain ConfigMap entry** patched into Postfix's
   `mail-postfix-virtual-domains` ConfigMap (registers the tenant's
   mail domain).
3. **SASL credentials Secret** in the tenant namespace
   (`smtp-credentials-{name}`) — host `postfix-dev.gentian-dev.svc.cluster.local:587` on dev.
4. **Dovecot domain config** patched into the shared
   `mail-dovecot-domains` ConfigMap when Dovecot is deployed.
5. **Status update** on the Tenant CR with the DNS records the customer
   must publish:
   - DKIM TXT record (`gtn._domainkey.<domain>`)
   - SPF record (`v=spf1 mx ~all`)
   - DMARC record (`_dmarc.<domain>`)

Reloader picks up the ConfigMap/Secret changes and rolls the
Postfix/Dovecot/Rspamd pods.

**Future:** emit the same objects from a mail Composition step once
`MAIL_SERVICE_MODE` and tenant modes are single-sourced in Crossplane.

## 6. Per-App Mail Wiring

For each app in the tenant that declares `mail.smtp` and/or
`mail.imap` in its `AppProfile`, the operator (and app Composition
`ExternalSecret` paths) materialise:

- SMTP host (`postfix-dev.gentian-dev.svc.cluster.local` on dev), port,
  user (per-app SASL identity), password.
- IMAP host (Dovecot service in `gentian-dev` when deployed), port,
  bind credentials (the same LDAP bind the app already uses for SSO).

The chart consumes these via standard `existingSecret` references —
no app-specific mail logic in the platform.

## 7. Security

- **DKIM private keys** are tenant-scoped; the shared Rspamd reads
  them from OpenBao at runtime, never persists them to disk
  unencrypted.
- **SMTP submission requires SASL.** No open relay. Per-app SASL
  identities mean a compromised app can be revoked without affecting
  other apps in the same tenant.
- **IMAP authentication** uses the tenant's LDAP OU. Cross-tenant IMAP
  access is structurally impossible.
- **Rate limits** (`Tenant.spec.mail.rateLimit`) are enforced per
  tenant at the Postfix `smtpd_client_message_rate_limit` level.
- **Per-user quotas** (`Tenant.spec.mail.quotaPerUser`) are enforced
  by Dovecot.

See [secrets.md](secrets.md) for OpenBao path layout and key
derivation.

## 8. External-Mode Tenants

When `mail.mode: external`, the platform skips Postfix/Dovecot
provisioning entirely and instead expects the customer to provide
SMTP/IMAP host + credentials in the Tenant spec (referenced via a
secret). The operator creates Secrets that point at those values; apps
consume them through the same `existingSecret` references — they never
know whether the SMTP server is the kernel's or someone else's.

## 9. Operational View

```bash
# Per-tenant mail config and status
kubectl get tenants -o custom-columns='NAME:.metadata.name,MAIL:.spec.mail.mode,DOMAIN:.spec.mail.domain,DKIM:.status.mail.dkimDnsRecord'

# Shared infrastructure health (dev layout)
kubectl get release -n gentian-dev postfix-dev dovecot-dev
kubectl logs -n gentian-dev -l app.kubernetes.io/name=postfix --tail=50
```
