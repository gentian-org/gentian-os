# Mail Kernel Extension

**Companion to:** [architecture.md](../architecture.md)

---

## 0. Where the knobs are

Three layers are easy to conflate:

| Layer | Knob | What it controls |
|---|---|---|
| **Cluster** | `mail.serviceMode` on the Cluster claim (`external` \| `kernel`) | Whether the kernel runs its own Postfix and Dovecot, or relays everything to an external SMTP host |
| **Per tenant** | `Tenant.spec.mail.mode` | What the operator provisions: the tenant's mail domain, its entries in the Postfix maps, and its Dovecot realm auth |
| **Per app** | `AppProfile` `mail.smtp` / `mail.imap` | Whether the operator hands an app kernel or external mail endpoints |

Everything runs in the kernel namespace — `platform-kernel` — alongside the
operator, because Postfix must mount a ConfigMap the operator writes and a Pod
can only mount from its own namespace. Services are named per stage:

```
postfix-<stage>.platform-kernel.svc.cluster.local:587   # submission
dovecot-<stage>.platform-kernel.svc.cluster.local:24    # LMTP, Postfix -> Dovecot
dovecot-<stage>.platform-kernel.svc.cluster.local:143   # IMAP
```

**Operational checks:**

```bash
kubectl get pods -n platform-kernel -l 'app.kubernetes.io/name in (postfix,dovecot)'
kubectl get cm postfix-kernel-virtual-mailbox-maps -n platform-kernel -o yaml
kubectl get tenants -o custom-columns='NAME:.metadata.name,MAIL:.spec.mail.mode'
```

---

## 0a. What works today, and what does not

Stated plainly, because the gap between "deployed" and "delivers mail" is where
this design has repeatedly been misread.

**Working, verified on a cluster:** a message addressed to a registered tenant
domain is accepted by Postfix, handed to Dovecot over LMTP, and written to a
maildir on persistent storage at `/var/mail/<domain>/<local-part>`. A mail
client authenticates over IMAP and reads it. Sending from a client inside the
cluster is accepted for any domain in the map.

**Not working yet:** anything involving the public internet. Port 25 is exposed
nowhere — the Postfix Service is ClusterIP on 587 — so no external sender can
reach this cluster, and no MX record would help until that changes. Outbound
mail to external recipients has never been exercised, and would be refused or
binned by most providers until the DNS records in §10 exist.

So: mail between users of this cluster works. Mail to and from the outside
world is the next milestone, not a finished feature.

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
and re-running `./install.sh`, which converges.

## 3. Shared Infrastructure with Tenant-Scoped Configuration

When the extension is enabled, **one** Postfix and **one** Dovecot instance handle all
tenant domains (placeholders under `kernel/services/postfix` and
`kernel/services/dovecot` — **UNTESTED**, public chart/image only). Tenant isolation is
enforced at the configuration level:

- **Postfix:** `virtual_mailbox_domains` lists all tenant domains;
  per-tenant SASL credentials authenticate SMTP submission.
- **Dovecot:** mailboxes stored at isolated paths
  (`/var/mail/{domain}/{user}`); IMAP authenticates against per-tenant
  credentials provisioned by the platform.
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
   (`smtp-credentials-{name}`) — host `postfix-<stage>.platform-kernel.svc.cluster.local:587`.
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

- SMTP host (`postfix-<stage>.platform-kernel.svc.cluster.local`), port,
  user (per-app SASL identity), password.
- IMAP host (`dovecot-<stage>.platform-kernel.svc.cluster.local`), port,
  per-tenant bind credentials from OpenBao.

The chart consumes these via standard `existingSecret` references —
no app-specific mail logic in the platform.

## 7. Security

- **DKIM private keys** are tenant-scoped; the shared Rspamd reads
  them from OpenBao at runtime, never persists them to disk
  unencrypted.
- **SMTP submission requires SASL.** No open relay. Per-app SASL
  identities mean a compromised app can be revoked without affecting
  other apps in the same tenant.
- **IMAP authentication** uses per-tenant credentials from OpenBao.
  Cross-tenant IMAP access is structurally impossible.
- **Rate limits** (`Tenant.spec.mail.rateLimit`) are enforced per
  tenant at the Postfix `smtpd_client_message_rate_limit` level.
- **Per-user quotas** (`Tenant.spec.mail.quotaPerUser`) are enforced
  by Dovecot.

See [security.md](security.md) for OpenBao path layout and key
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
kubectl get tenants -o custom-columns='NAME:.metadata.name,MAIL:.spec.mail.mode,DOMAIN:.spec.mail.domain'

# Shared infrastructure health
kubectl get pods -n platform-kernel -l 'app.kubernetes.io/name in (postfix,dovecot)'
kubectl logs -n platform-kernel postfix-<stage>-0 --tail=50

# Which domains may receive, and which may send. Both are written by the
# operator from the tenant registry; a domain missing here is a domain whose
# mail is refused.
kubectl get cm postfix-kernel-virtual-mailbox-maps -n platform-kernel -o yaml

# Did a message actually land? The maildir is the ground truth, not the log.
kubectl exec -n platform-kernel deploy/dovecot-<stage> -- find /var/mail -type f -name '*.M*'
```

**When a message is refused, read the Postfix log line rather than guessing.**
`Recipient address rejected` means the domain is missing from
`virtual_mailbox_domains`; `Sender address rejected` means it is missing from
`sender_access`; and `Server configuration error` against either means the map
file itself could not be opened — usually because it was added after Postfix
started, which needs a `postfix reload`.

---

## 10. DNS for real mail

None of this is needed for mail between users of one cluster, which is why it
can be skipped while testing. All of it is needed before mail crosses the
internet.

**Prerequisite, and it is not DNS.** Inbound mail arrives on port **25**, and
the kernel Postfix Service is ClusterIP on 587 only. An MX record pointing at a
cluster that does not listen on 25 changes nothing. Expose 25 through a
LoadBalancer first, then publish the records below.

| Record | Where | Value | Why |
|---|---|---|---|
| **MX** | `<tenant-domain>` | `10 mail.<kernel-domain>.` | Tells other servers where to deliver. Needs an A record for the target and port 25 reachable on it. |
| **A** | `mail.<kernel-domain>` | the mail LoadBalancer IP | The MX target must resolve to an address, never to a CNAME. |
| **SPF** (TXT) | `<tenant-domain>` | `v=spf1 ip4:<outbound-ip> -all` | Declares which addresses may send as this domain. Without it most providers mark the mail as spam. |
| **DKIM** (TXT) | `<selector>._domainkey.<tenant-domain>` | the public key from the Postfix container | Signs outbound mail so recipients can verify it was not altered. |
| **DMARC** (TXT) | `_dmarc.<tenant-domain>` | `v=DMARC1; p=quarantine; rua=mailto:postmaster@<domain>` | Tells recipients what to do when SPF or DKIM fail, and where to report. |
| **PTR** | the outbound IP | `mail.<kernel-domain>` | Reverse DNS. Set at the cloud provider, not in DNS hosting. Many providers refuse mail from an IP with no PTR. |

**DKIM keys are generated per domain** by the Postfix image, from the domains in
`ALLOWED_SENDER_DOMAINS`, into `/etc/opendkim/keys/<domain>.txt`. Read the
public key to publish it:

```bash
kubectl exec -n platform-kernel postfix-<stage>-0 -- cat /etc/opendkim/keys/<domain>.txt
```

Note the consequence: a tenant domain that is not in `ALLOWED_SENDER_DOMAINS`
gets no DKIM key, so its outbound mail is unsigned even once the map permits it
to send.

### Cloudflare specifics

**MX records must be DNS-only — the grey cloud, not the orange one.** Cloudflare
proxies HTTP and HTTPS; it does not proxy SMTP. An MX record left proxied, or
pointing at a proxied A record, resolves to Cloudflare's HTTP edge and mail
delivery fails silently. The same applies to the A record the MX points at: it
must be grey-clouded even though the rest of the zone is proxied.

TXT records for SPF, DKIM and DMARC are unaffected by the proxy setting.

If the cluster runs `networkMode: tunnel`, inbound mail is not possible at all —
a Cloudflare tunnel carries HTTP only. That is why `MAIL_SERVICE_MODE=kernel` is
rejected on tunnel clusters; use an external SMTP provider there.

### Verifying

```bash
dig +short MX <tenant-domain>
dig +short TXT <tenant-domain>                      # SPF
dig +short TXT <selector>._domainkey.<tenant-domain> # DKIM
dig +short -x <outbound-ip>                          # PTR
```

Send a message to a mailbox at a major provider and read the received headers:
they will state `spf=pass`, `dkim=pass` and `dmarc=pass`, or exactly which one
failed. That check is worth more than any of the individual lookups above.
