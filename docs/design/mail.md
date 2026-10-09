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

Postfix and Dovecot run in `system-mail`, and the operator writes what they
mount into that namespace: a Pod can only mount a ConfigMap or a Secret from
its own (§5a). Neither has a load balancer. What the internet reaches is a
proxy in `system-mail-dmz` (§9b). Services are named per stage:

```
mail-edge-<stage>.system-mail-dmz                        # the load balancer: 25, 587, 993
postfix-<stage>.system-mail.svc.cluster.local:587        # submission, for pods of the cluster
postfix-<stage>-edge.system-mail.svc.cluster.local       # 10025, 10587: the proxy only
dovecot-<stage>.system-mail.svc.cluster.local:24         # LMTP, Postfix -> Dovecot
dovecot-<stage>.system-mail.svc.cluster.local:143        # IMAP, for pods of the cluster
dovecot-<stage>-edge.system-mail.svc.cluster.local:10993 # the proxy only
```

**Operational checks:**

```bash
kubectl get pods -n system-mail
kubectl get pods,svc -n system-mail-dmz
kubectl get cm postfix-kernel-virtual-mailbox-maps -n system-mail -o yaml
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

**Built, and proven only outside a cluster:** the path from the internet. Ports
25, 587 and 993 are published by a proxy in the mail DMZ, which passes each
connection to Postfix or Dovecot with the client's address (§9b). That was run
against the real images in local containers, not on a cluster with a load
balancer in front of it. Outbound mail to external recipients would be refused
or binned by most providers until the DNS records in §10 exist.

**Was broken until the namespaces were put right (§5a):** on the layout with
`system-mail`, the operator wrote Postfix's maps and Dovecot's passwd-files
into namespaces neither server runs in. Postfix knew no tenant domain and
Dovecot no password, while every tenant reported its mail as ready.

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
tenant domains (`kernel/services/postfix` and `kernel/services/dovecot`), verified
delivering on a real cluster — see §0a. Tenant isolation is enforced at the
configuration level:

- **Postfix:** `virtual_mailbox_domains` lists all tenant domains; one shared SASL
  credential per tenant authenticates SMTP submission for that tenant's apps (§6, §7).
- **Dovecot:** mailboxes stored at isolated paths
  (`/var/mail/{domain}/{user}`); IMAP authenticates against per-user,
  per-client-app credentials (§7).
- **DKIM signing** is done by OpenDKIM inside the Postfix image, from
  operator-generated, per-domain keys mounted as a Secret — see §10. There is no
  Rspamd or other spam-filtering component in this stack; nothing here does
  spam scoring today.

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


### Shared sending reputation

Every tenant sends from one IP, so they share one reputation. A tenant that
sends spam degrades deliverability for every other tenant on the cluster, and
there is no per-tenant remedy once an IP is listed — the block is on the
address, not the domain.

This is not fixable by DNS. PTR is a property of the IP and there is exactly one
per address, which is correct and normal: receivers check that the IP's PTR name
resolves back to the IP, never that it matches the sender's domain. Shared mail
platforms all work this way. What *is* per-tenant is SPF, DKIM and DMARC, and
DMARC alignment compares the From: domain against the DKIM signing domain and
the envelope sender — the PTR takes no part in it.

So multi-tenant sending is sound, but the reputation is collective. The
mitigations are per-tenant outbound rate limits, bounce and complaint
monitoring, and a dedicated IP for any tenant whose volume justifies one.

## 5. Provisioning Flow (selfhosted mode)

When a Tenant with `mail.mode: selfhosted` is reconciled, the
**operator** (not a finished Crossplane-only pipeline) performs:

1. **DKIM keypair Secret**, an RSA-2048 key generated with a CSPRNG (not derived from
   the master password) and stored as a Kubernetes Secret (`dkim-{name}`) in the
   kernel namespace — see §10, which this used to disagree with.
2. **Virtual domain ConfigMap entry** patched into Postfix's
   `mail-postfix-virtual-domains` ConfigMap (registers the tenant's
   mail domain).
3. **SASL credentials Secret** in the tenant namespace
   (`smtp-credentials-{name}`) — host `mail.<kernelDomain>:587`.
4. **Dovecot domain config** patched into the shared
   `mail-dovecot-domains` ConfigMap when Dovecot is deployed.
5. **Status update** on the Tenant CR with the DNS records the customer
   must publish:
   - DKIM TXT record (`gtn._domainkey.<domain>`)
   - SPF record (`v=spf1 mx ~all`)
   - DMARC record (`_dmarc.<domain>`)

Reloader picks up the ConfigMap/Secret changes and rolls the
Postfix/Dovecot pods.

**Future:** emit the same objects from a mail Composition step instead of the
operator's own reconcile loop. `mail.serviceMode` — the cluster-level knob — is
already single-sourced, read from the Cluster claim via `gentian-cluster-config`
rather than from Helm values (see §0). What is still operator-driven, in plain
Go rather than a Composition, is everything in this list: the tenant is
provisioned by `internal/controller/mail_reconciler.go`, not by Crossplane.

## 5a. Where each object lives

An object goes where its reader reads it. Everything the operator writes for
mail, and who reads it:

| Object | Namespace | Read by |
|---|---|---|
| ConfigMap `postfix-kernel-virtual-mailbox-maps` | `system-mail` | Postfix's pod mounts it: the domains it accepts, where each address is delivered, who may send, the networks it trusts. The mount is required |
| Secret `postfix-dkim-tenants` | `system-mail` | Postfix's pod mounts the signing keys and tables |
| Secret `dovecot-app-passwords` | `system-mail` | Dovecot's pod mounts the passwd-files: app passwords and every submission credential |
| Secret `dovecot-realm-auth` | `system-mail` | Dovecot's pod mounts the per-realm token settings |
| ConfigMaps `mail-postfix-virtual-domains`, `mail-dovecot-domains`; Secrets `dkim-<tenant>`, `dkim-kernel`, `mail-apppw-seed-<tenant>` | `system-mail` | the operator alone |
| Secret `mail-submission-<tenant>` | `kernel-authentication` | the Job that configures the realm's mail, and the realm the tenant's Composition declares |
| Secrets `mail-app-passwords`, `smtp-credentials-<tenant>` | the tenant's namespace | the tenant's mail client and apps |
| DNSEndpoints | `system-mail-dmz` | external-dns |

The operator reads `keycloak-admin` and `keycloak-smtp-credentials` in
`kernel-authentication`, where they are written, and the address of the load
balancer from Service `mail-edge-<stage>` in `system-mail-dmz`.

A tenant's `MailReady` is true only once the map Postfix mounts names its
domain (`PostfixMapMissing` otherwise). On start the operator deletes the maps
and the passwd-files an earlier version wrote into `system-mail-dmz` and
`kernel-edge`, and moves each tenant's seed and submission password out of
`kernel-edge`: no credential is kept in the namespace of the Gateway.
`internal/controller/mail_objects_test.go` holds every row of the table to the
chart, Job or Composition that reads it.

## 6. Per-App Mail Wiring

For each app in the tenant that declares `mail.smtp` and/or
`mail.imap` in its `AppProfile`, the operator materialises:

- **SMTP**: host (`mail.<kernelDomain>`; without a kernel domain, `postfix-<stage>.system-mail.svc.cluster.local`), port, user,
  password — copied from the tenant's own `smtp-credentials-<tenant>` Secret into
  each app's OpenBao path. This is **one SASL identity shared by every app in the
  tenant**, not one per app; see the correction in §7.
- **IMAP**: host (`imap.<kernelDomain>`) and port only.
  No password travels this path — IMAP auth is per-*user*, per-client-app, and is
  issued separately; see §7.

The chart consumes these via standard `existingSecret` references —
no app-specific mail logic in the platform.

## 7. Security

- **DKIM private keys** live in a Kubernetes Secret in the kernel namespace,
  mounted into Postfix's own filesystem for OpenDKIM to sign with (§10). Nothing
  reads them over the network at runtime, and nothing but the operator writes
  them.
- **SMTP submission requires SASL.** No open relay. The identity is **one
  credential per tenant, shared across every app that tenant runs** — not one
  per app. A compromised app's stored SMTP credential is therefore usable by
  anything else that app can reach in the same tenant's traffic; it is not
  independently revocable per app. This was previously documented as a
  per-app identity with independent revocation, which was never what the code
  did — `seedPerAppMailSecrets` explicitly copies one tenant-wide credential
  into every app's OpenBao path *because* they all authenticate to the same
  submission endpoint as one user. Revoking one app's access to send mail
  means rotating the tenant's shared credential, which revokes every app.
- **IMAP authentication does not use a shared or per-tenant credential at
  all.** Each user gets a credential per client application — the pattern
  Google and Fastmail use — because Keycloak identities are OIDC and IMAP
  predates it, so a login yields no password a mail client can present. The
  password is derived (HMAC, not random) so it never needs its own storage;
  Dovecot verifies against an Argon2id hash kept in Dovecot's own namespace,
  and the plaintext is handed to the user once and kept only in the
  **tenant's own** namespace. A tenant therefore holds its own users'
  credentials and nobody else's — see `internal/controller/mail_apppassword.go`.
  Cross-tenant IMAP access is structurally impossible because each tenant's
  mailbox path and hash store are namespace-isolated.
- **Rate limits and per-user quotas are not enforced.** Postfix runs with
  `smtpd_client_message_rate_limit = 0`, its default, which is no limit, and
  Dovecot loads no quota plugin. This section previously described both as
  enforced, and two clusters carried settings for them: a tenant could ask for
  100/h and 5Gi and receive neither, with nothing to say so. The fields have
  been removed rather than left to read as configuration — see the roadmap for
  making them real.

See [security.md](security.md) for OpenBao path layout and key
derivation.

## 8. External-Mode Tenants

Two different settings carry the word "external", on two different objects, and
conflating them is how this section used to read:

| Setting | Scope | Decides |
|---|---|---|
| `mail.serviceMode` (`kernel` \| `external`) | The cluster, from its Claim | What the kernel **deploys** |
| `Tenant.spec.mail.mode` (§2) | One tenant | Where that tenant's mail **goes** |

**What the cluster setting changes.** With `mail.serviceMode: external`, Dovecot
is not deployed — no ApplicationSet generates it, and the operator does not
provision the per-realm Keycloak client, the domains ConfigMap or the LMTP
registration that only a running Dovecot would use. **Postfix stays in both
modes, deliberately.** It is the submission endpoint every app sends through, so
keeping it in the path keeps the relay credential in exactly one place: apps get
the same in-cluster SMTP host either way, and the credential for the upstream
relay is configured once on Postfix rather than copied into every tenant.

What is lost with Dovecot is *storage*: nothing accepts LMTP, so there is no
mailbox and no IMAP. Outbound and app notification mail still work.

**What the tenant setting changes.** `Tenant.spec.mail.mode: external` does not
deploy or skip anything. The tenant supplies its own SMTP credentials as a
Secret in the kernel namespace, named by `spec.mail.smtpCredentialsSecret`
(required for this mode — the operator refuses the tenant with `MissingConfig`
otherwise), and the operator copies it into the tenant namespace. Apps consume
it through the same `existingSecret` reference they would use in `selfhosted`
mode, so an app never knows whose SMTP server is on the other end. That is the
point of the contract in §6: apps declare that they send mail, and nothing else.

The two settings are independent. A `kernel` cluster can host an `external`
tenant that relays through its own provider, and an `external` cluster still
serves `selfhosted` tenants — they get operator-managed secrets and working
submission, but no mailbox until the cluster switches to `kernel` and
`./install.sh` is re-run, which converges (§2).

## 9. Operational View

```bash
# Per-tenant mail config and status
kubectl get tenants -o custom-columns='NAME:.metadata.name,MAIL:.spec.mail.mode,DOMAIN:.spec.mail.domain'

# Shared infrastructure health
kubectl get pods -n system-mail -l 'app.kubernetes.io/name in (postfix,dovecot)'
kubectl logs -n system-mail postfix-<stage>-0 --tail=50

# Which domains may receive, and which may send. Both are written by the
# operator from the tenant registry; a domain missing here is a domain whose
# mail is refused.
kubectl get cm postfix-kernel-virtual-mailbox-maps -n system-mail -o yaml

# Did a message actually land? The maildir is the ground truth, not the log.
kubectl exec -n system-mail deploy/dovecot-<stage> -- find /var/mail -type f -name '*.M*'
```

**When a message is refused, read the Postfix log line rather than guessing.**
`Recipient address rejected` means the domain is missing from
`virtual_mailbox_domains`; `Sender address rejected` means it is missing from
`sender_access`; and `Server configuration error` against either means the map
file itself could not be opened — usually because it was added after Postfix
started, which needs a `postfix reload`.

---

## 9a. What a Kubernetes cluster needs to host mail

Six requirements. Five are ordinary infrastructure; the sixth is the one that
decides whether self-hosting is viable at all, and it is a property of the
network rather than of anything in this repo.

**1. Inbound on port 25.** A LoadBalancer Service, not an HTTP gateway — SMTP is
not HTTP and cannot be proxied by one. Here it is the Service of the mail edge
(§9b), with `externalTrafficPolicy: Local`, because a sender's IP is evidence
and the default policy SNATs it away. The provider must attach a health monitor
against `healthCheckNodePort`; without one it round-robins across nodes that
have no proxy and most connections are dropped. A load balancer that opens its
own connection instead of forwarding packets hides the sender whatever the
policy says, and has to name it with the PROXY protocol (§9b).

**2. An outbound address whose PTR you control.** This is the hard one. Pod
egress usually leaves through the router's shared SNAT address, which is not the
load balancer address mail arrives on, and which you cannot set reverse DNS for.
Receivers check forward-confirmed reverse DNS — the PTR of the sending address
names a host, and that host resolves back to the same address — so mail must
leave through an address you own. Where it cannot, relay through a smarthost
instead; the alternative is mail that passes SPF and DMARC and still lands in
spam.

On OpenStack that means attaching a floating IP you own to the port of the node
Postfix runs on: a port with a floating IP associated egresses through it
instead of the router's shared address.

```bash
# the node Postfix is scheduled on, and its Neutron port
kubectl -n system-mail get pod postfix-<stage>-0 -o jsonpath='{.spec.nodeName}'
kubectl get node <node> -o jsonpath='{.spec.providerID}'      # openstack://<region>/<server-id>
openstack port list --device-id <server-id> -f value -c ID

openstack floating ip set --port <port-id> <floating-ip-id>
openstack ptr record set <region>:<floating-ip-id> <egress-host>.
```

Then publish the forward record — `<egress-host>` A `<floating-ip>` — because
receivers check both halves, and name the egress on the cluster claim so the
operator emits an SPF record that matches:

```yaml
  mail:
    serviceMode: system
    egressHost: <egress-host>
```

Confirm the address actually moved before testing delivery; this is the whole
assumption and it is one command:

```bash
kubectl -n system-mail exec postfix-<stage>-0 -c mail -- curl -s ifconfig.me
```

Two things this arrangement does not yet do for you. Nothing publishes the
`<egress-host>` A record — the floating IP is allocated at the provider, so that
record is manual. And nothing pins Postfix to the node carrying the address:
`egressHost` is read only to compose the SPF record, so a reschedule moves
sending back to the shared SNAT silently, and the next bounce is the signal.
Pin it deliberately until the operator does.

**3. A HELO name that is a FQDN and agrees with the PTR.** Postfix defaults to
its own hostname, which in Kubernetes is a Service name that resolves nowhere.

**4. Persistent storage, twice over.** Mailboxes obviously. Less obviously the
DKIM keys: an image that generates them at start writes them to the container
filesystem, so without a volume every restart produces a new key and silently
invalidates the DNS record published for the old one. Here they have their own
volume, separate from the queue.

**5. Identities that can authenticate.** IMAP and SMTP predate OIDC. Either the
client speaks XOAUTH2 — Dovecot does, most webmail does not — or each user needs
a credential per client application, minted and stored somewhere the tenant can
reach and no other tenant can.

**6. The special-use mailboxes.** Drafts, Sent, Trash, Junk, Archive. A client
with nowhere to file a sent message refuses to send at all, and reports it as a
send failure rather than a missing folder.

### Provider constraints worth checking first

Whether outbound port 25 is open at all — many providers block it permanently,
which rules out direct delivery before any of the above matters.

Whether reverse DNS is self-service. On Infomaniak Public Cloud a floating IP
you own is self-service through Designate:

```bash
openstack ptr record set <region>:<floating-ip-id> mail.example.com.
```

while addresses on the shared public network are "predefined by the platform"
and need a support request — and a shared SNAT address is not one you own, so it
will not be granted. See
[docs.infomaniak.cloud/dns_service/reverse_dns](https://docs.infomaniak.cloud/dns_service/reverse_dns/).

Whether load-balancer, port and instance quotas leave room. Octavia reports
exhaustion as `ConflictException: 409` from the amphora provider, naming none of
the three.

### When to relay instead

Relay outbound through a smarthost when the sending address is shared, its
reputation is not yours, or reverse DNS is not available. `relayHost` and
`relayPort` on the Postfix chart exist for this, with credentials at
`gentian-os/kernel/mail/postfix`. Inbound, storage and IMAP are unaffected —
only the last hop changes — and the platform's own DKIM signing still applies,
so the work is not wasted.

---

## 9b. The mail edge

```
        internet                         system-mail-dmz                      system-mail
                                                                        (no load balancer, no
  mail servers ──25──┐                                                   node port: ClusterIP)
  mail clients ─587──┤   ┌──────────────┐    ┌───────────────┐  10025 ┌─────────────────────┐
  mail clients ─993──┼──▶│ load balancer│──▶ │ HAProxy       │──10587▶│ Postfix + DKIM      │──25──▶ internet
                     │   │ mail-edge-   │    │ TCP only      │        │ certificate, queue  │
  apps, Keycloak     │   │ <stage>      │    │ no certificate│        └────────┬────────────┘
  (public names) ────┘   └──────────────┘    │ no mail       │           24, 12345
                                             │ no users      │  10993 ┌────────▼────────────┐
                                             └───────────────┘───────▶│ Dovecot             │
  pods of a tenant, by Service name ── 587 (Postfix), 143 / 993 ─────▶│ certificate, mail   │
                                                                      └─────────────────────┘
```

**What it is.** `kernel/services/mail-edge`: HAProxy as a TCP proxy, two
replicas, behind the one LoadBalancer Service of the cluster's mail. It takes
25, 587 and 993, and opens each connection again to the one server the port
belongs to. It reads no SMTP and no IMAP, stores nothing, mounts no Secret and
has no service-account token; it runs as an ordinary user with no capability on
a read-only filesystem. `mail.<kernelDomain>` and `imap.<kernelDomain>` both
resolve to its load balancer; the operator publishes both from that Service's
address.

**A proxy, not a second mail server.** The networking plan places Postfix
itself in the DMZ. That is a mail server with a queue — stored mail — in the
namespace that faces the internet, and it would need the tenant maps, the relay
credential and a path to the signing keys there. AD-9 asks for a stateless edge,
and a TCP proxy is one: Postfix stays the only MTA, in `system-mail`.

**HAProxy, not the platform's Envoy.** Envoy Gateway can route TCP and send a
PROXY header upstream (`BackendTrafficPolicy.proxyProtocol`), but by its API
reference its connection limit counts a listener's connections as a whole and
its rate limit counts HTTP requests -- neither is per client address on a TCP
listener. That was read, not tried. And a mail listener on the
shared fleet would put the cluster's mail and its web sign-in behind one
process in `kernel-edge`, with a path from that namespace into `system-mail`.
HAProxy is what mail operators put in this place; Postfix and Dovecot document
it by name.

**TLS ends at Postfix and Dovecot.** The proxy passes the bytes through:
STARTTLS on 25 and 587 is negotiated by the client with Postfix, and 993 is TLS
from the first byte with Dovecot. The certificate and its key are in
`system-mail` and nowhere else.

**The client's address survives.** In front of each connection the proxy sends
a PROXY-protocol header (version 2) naming the client. Postfix reads it on
10025 and 10587 (`smtpd_upstream_proxy_protocol = haproxy`), Dovecot on 10993
(`haproxy = yes`), so Postfix's client checks and limits, Dovecot's count of
failed logins and both logs are about the client. Without it every connection
would come from the proxy's pod — an address inside the range Postfix trusts
(`mynetworks`) — and the cluster would relay for the internet. Two things keep
that from happening by accident:

- the three ports that read a header **require** it: a connection without one
  gets no SMTP greeting and no TLS handshake;
- they admit the proxy's pods alone (NetworkPolicy), and the proxy can reach
  only them. The ports pods of the cluster use — 587, 143, 993 — take no
  header, so a pod cannot give itself another address.

**Limits, per client address,** at the proxy and before a server process is
spent: 20 connections at once and 60 a minute on 25 and on 587, 100 and 120 on
993 (`listeners.<name>.*` on the chart). Over that, the connection is closed
and logged with the client's address. Each replica counts on its own. Postfix's
limits apply behind them, to the same address, and answer in SMTP.

**No health check of the servers,** deliberately. HAProxy's check of a
PROXY-protocol port makes Postfix 3.10's `smtpd` exit, after which Postfix
holds that listener back for a minute. The proxy's own readiness is what the
load balancer sees.

**A load balancer that is itself a proxy** (an Octavia amphora) arrives from its
own address, so the proxy would name the load balancer as every client.
`loadBalancer.proxyProtocol.enabled` with `from: [<the load balancer's
addresses>]` makes the proxy take a PROXY header from those addresses and from
nobody else; the load balancer has to be switched to send one
(`loadbalancer.openstack.org/proxy-protocol: "true"`). Off by default, and no
field of the Cluster claim sets it yet.

**From the layout before.** `postfix-<stage>-smtp` and `dovecot-<stage>-imaps`,
the two load balancers in `system-mail`, are no longer rendered and Argo CD
prunes them. The one that replaces them gets an address of its own unless
`loadBalancer.ip` pins one; the operator republishes `mail.` and `imap.` from
it.

**Proven** by `make test-mail-edge-lab`, which runs the rendered proxy in front
of the rendered Postfix and Dovecot in local containers: mail on 25 with
STARTTLS, authenticated submission on 587 and IMAPS on 993 through the proxy,
each logged by the server with the client's address; a relay refused to that
same client; the header ports refusing a connection without a header; the
pods' listeners unchanged; both limits; and the load-balancer setting. Not
proven: anything that needs a cluster — the load balancer, the policies as a
CNI enforces them, a pod reaching the public name.

---

## 10. DNS for real mail

None of this is needed for mail between users of one cluster, which is why it
can be skipped while testing. All of it is needed before mail crosses the
internet.

**Prerequisite, and it is not DNS.** Inbound mail arrives on port **25** of the
mail edge's load balancer (§9b). An MX record pointing at an address that does
not answer on 25 changes nothing: the load balancer has to have its address,
and the provider has to let 25 in, before the records below mean anything. The
operator publishes the MX only once the Service reports an address.

| Record | Where | Value | Why |
|---|---|---|---|
| **MX** | `<tenant-domain>` | `0 mail.<kernel-domain>.` | Tells other servers where to deliver. Needs an A record for the target and port 25 reachable on it. Preference `0`, not `10`: external-dns's Cloudflare provider reads any preference back as `0`, so anything else never matches what it asked for and the record is deleted and recreated once a minute forever. See `mail_dnsendpoint.go`. |
| **A** | `mail.<kernel-domain>`, `imap.<kernel-domain>` | the address of the mail edge's load balancer (`mail-edge-<stage>` in `system-mail-dmz`); the operator publishes both | The MX target must resolve to an address, never to a CNAME. This is where mail ARRIVES; it is not the address mail leaves from, and the two differ on any cluster whose egress is not the load balancer. |
| **A** | `<egress-host>` | the outbound floating IP | The forward half of forward-confirmed reverse DNS. Receivers check that the PTR names a host and that the host resolves back to the same address, so this record and the PTR must agree. Nothing on the cluster publishes it: the floating IP is allocated at the provider, not by Kubernetes, so this record is manual. |
| **SPF** (TXT) | `<tenant-domain>` | `v=spf1 a:<egress-host> -all` | Declares which addresses may send as this domain. `a:` rather than an `ip4:` literal so the record follows the egress A record instead of having to be edited in two places — the one that gets forgotten fails closed and silently. Emitted only when `mail.egressHost` is set on the cluster claim; without it the operator falls back to `v=spf1 mx ~all`, which authorises the MX host — the INBOUND load balancer, which never sends anything — and so fails by construction while looking plausible. |
| **DKIM** (TXT) | `<selector>._domainkey.<tenant-domain>` | the public half of the operator-held key | Signs outbound mail so recipients can verify it was not altered. Published from the same value that signs, so the two cannot drift. |
| **DMARC** (TXT) | `_dmarc.<tenant-domain>` | `v=DMARC1; p=quarantine; rua=mailto:postmaster@<domain>` | Tells recipients what to do when SPF or DKIM fail, and where to report. |
| **PTR** | the outbound IP | `<egress-host>` | Reverse DNS. Set at the cloud provider, not in DNS hosting. Many providers refuse mail from an IP with no PTR — Gmail rejects with `550-5.7.25 ... does not have a PTR record setup`. It must name the egress host, NOT `mail.<kernel-domain>`: that name resolves to the inbound load balancer, so using it makes the forward and reverse disagree and the check fails. |

**Every signing key belongs to the operator.** It creates an RSA-2048 key per
tenant as a `dkim-<tenant>` Secret in the kernel namespace, and one for the
kernel domain as `dkim-kernel`, each generated once and never rotated
automatically — a rotated key stops matching the record already published for it.
The public halves reach DNS from the same values that sign, so the two cannot
disagree:

```bash
kubectl get tenant <name> -o jsonpath='{.status.mail.dkimPublicKey}'
```

The private halves are collected into one `postfix-dkim-tenants` Secret keyed by
domain (`<domain>.private`). Postfix mounts it, and an init container copies
those keys into `/etc/opendkim/keys` — a persistent volume — before the image
starts. The image generates a key only when none is present, so it adopts the
operator's and builds its `KeyTable` and `SigningTable` around them. The operator
owns the keys, the image owns the tables, and neither needs to know about the
other. Where the two disagree the operator's copy wins, because it is the half
already published.

Two things make this work that are easy to undo by accident:

- **The key directory must be persistent.** The image writes keys to the
  container filesystem, so without a volume every restart generates new ones and
  silently invalidates the published records. The chart's own persistence covers
  the mail queue, not the keys — they have their own volume.
- **A domain signs only if it is in `ALLOWED_SENDER_DOMAINS`.** OpenDKIM builds a
  `KeyTable` entry from that variable alone, so a domain missing from it sends
  unsigned however correct its key and DNS record are. The operator writes the
  full list to the `allowed_sender_domains` key of the
  `postfix-kernel-virtual-mailbox-maps` ConfigMap, and Postfix reads it from
  there.

Unlike the `texthash:` maps beside it, that variable is read once at start.
A new tenant therefore receives mail immediately but signs only after Postfix
restarts.

To read the key a domain is actually signing with, which is the value its DNS
record must carry:

```bash
kubectl exec -n system-mail postfix-<stage>-0 -- cat /etc/opendkim/keys/<domain>.txt
```

A published record that does not match this is worse than no record at all: an
absent record is neutral, while a wrong one is a `dkim=fail` the recipient acts
on.

### Cloudflare specifics

**MX records must be DNS-only — the grey cloud, not the orange one.** Cloudflare
proxies HTTP and HTTPS; it does not proxy SMTP. An MX record left proxied, or
pointing at a proxied A record, resolves to Cloudflare's HTTP edge and mail
delivery fails silently. The same applies to the A record the MX points at: it
must be grey-clouded even though the rest of the zone is proxied.

TXT records for SPF, DKIM and DMARC are unaffected by the proxy setting.

If the cluster runs `networkMode: tunnel`, inbound mail is not possible at all —
a Cloudflare tunnel carries HTTP only. That is why `MAIL_SERVICE_MODE=system` is
rejected on tunnel clusters; use an external SMTP provider there.

### Verifying

```bash
dig +short MX <tenant-domain>
dig +short TXT <tenant-domain>                      # SPF
dig +short TXT <selector>._domainkey.<tenant-domain> # DKIM
dig +short -x <outbound-ip>                          # PTR
dig +short A <egress-host>                           # must equal <outbound-ip>
dig +short A mail.<kernel-domain>                    # the MX target must resolve
```

The PTR and the egress A record are a pair: check both and check they agree.
A PTR alone passes `dig -x` and still fails at the receiver.

Compare the published DKIM key against the key that signs, rather than trusting
`opendkim-testkey`. It reports `key OK` for a record that is syntactically valid
and retrievable, which is not the same as one that matches this private key —
a tenant rebuilt with new keys leaves a stale record that passes that check and
fails every signature:

```bash
kubectl -n system-mail exec postfix-<stage>-0 -c mail -- \
  openssl rsa -in /etc/opendkim/keys/<domain>.private -pubout | grep -v -- ----- | tr -d '\n'
dig +short TXT <selector>._domainkey.<domain> | tr -d '" ' | sed 's/.*p=//'
```

Send a message to a mailbox at a major provider and read the received headers:
they will state `spf=pass`, `dkim=pass` and `dmarc=pass`, or exactly which one
failed. That check is worth more than any of the individual lookups above.
