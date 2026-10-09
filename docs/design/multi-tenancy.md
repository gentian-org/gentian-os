# Multi-Tenancy, Domains and Security

**Companion to:**
- [architecture.md](../architecture.md)
- [iam.md](iam.md) (for detailed Identity and Access Management and role separation rules)

---

## 1. Tenancy Model

A **tenant** is an organisation. Each tenant gets:

- A dedicated Kubernetes namespace (`tenant-{name}`).
- A dedicated Keycloak realm with its own user pool, branding, and
  password policy.
- Per-app PostgreSQL/MariaDB databases with isolated users.
- Per-app MinIO buckets with IAM policies scoped to that tenant.
- Per-app Redis ACL users.
- (If mail enabled) a dedicated mail domain with isolated mailbox path
  and DKIM keys.

Kernel services (Keycloak, PostgreSQL, MinIO, Redis, Postfix, Dovecot)
are **shared infrastructure with tenant-scoped configuration** — the
same model every shared OS service uses (a Linux kernel runs one
filesystem driver and isolates users via UID/GID, not by booting one
ext4 per user).

## 2. Isolation Modes

| Mode | Mechanism | Best for |
|---|---|---|
| **namespace-per-tenant** (default) | K8s RBAC, ResourceQuotas, NetworkPolicies | Trusted internal tenants, cost efficiency |

## 3. Domains and TLS — Two Planes, Tenant Zones, and Tenancy Mode

The cluster's **tenancy mode** (`spec.tenancyMode` on the Cluster claim,
`multi` by default; asked at install step 0, `TENANCY_MODE` for an unattended
first run, mirrored to the operator's Helm `tenancyMode`) says how many user
tenants the cluster is for and where their hosts are. There are exactly two.
Both use the **same central IdP** at `id.<KERNEL_DOMAIN>/realms/<realm>`.

| Mode | Tenants | A user tenant's `effectiveDomain` | Example Jitsi URL |
|---|---|---|---|
| **`multi`** | The platform tenant plus any number of user tenants | `<tenant>.<KERNEL_DOMAIN>` | `https://meet.demo.platform.example.com` |
| **`single`** | The platform tenant plus exactly one user tenant, named `user` | `<KERNEL_DOMAIN>` itself | `https://meet.platform.example.com` |

The platform tenant is in every cluster, is never counted, and is at
`platform.<KERNEL_DOMAIN>` under either mode.

### The platform tenant, user tenants, and the two modes

Four statements, and the rest follows from them ([iam.md §1.1a](iam.md) has
the sign-in side and the table of addresses):

1. **The platform tenant is the platform's own.** It holds the platform's
   administrators (it adopts the kernel realm) and the platform's own
   components, which are installed with the cluster: the desktop at
   `platform.<KERNEL_DOMAIN>`, the administration console at
   `admin.platform.<KERNEL_DOMAIN>`, the concierge on the bare domain. Nobody
   installs catalogue apps or add-ons into it: the director answers `409` with
   the reason to `POST /v1/tenants/{t}/apps/{p}` and
   `PUT /v1/tenants/{t}/apps/{p}/addons` for a tenant whose manifest names a
   realm other than its own. An app's composition takes the tenant's name as
   its realm, so such an install could never have worked; it is refused where
   it is asked for, before anything is fetched or committed.
2. **Users live in a user tenant**, which is what a tenant has always been:
   its own realm, namespaces and zone.
3. **`multi`: any number of user tenants**, each on its own subdomain or a
   custom domain: its desktop is `desktop.<tenant>.<KERNEL_DOMAIN>`, its
   admin console `admin.<tenant>.<KERNEL_DOMAIN>`, its apps
   `<label>.<tenant>.<KERNEL_DOMAIN>`, or the same labels under the domain a
   `TenantDomain` binds it to. The install creates none; the platform admin
   does, in the admin console or with `kubectl gentian tenants create`. The bare domain,
   `www` and `desktop.<KERNEL_DOMAIN>` lead to the concierge's address form.
   No tenant can put a website on the main address here: it belongs to all
   of them. A tenant publishes its website under its own hosts, or on a
   domain of its own (a custom domain).
4. **`single`: exactly one user tenant, named `user`, on the cluster's own
   addresses.** Its desktop is `desktop.<KERNEL_DOMAIN>`, its admin console
   `admin.<KERNEL_DOMAIN>`, its apps `<app>.<KERNEL_DOMAIN>`, and the bare
   domain and `www` lead to its desktop -- or show its public website, if it
   has put one on the main address
   ([routing.md §5](routing.md#5-redirects-and-url-control)).
   `<KERNEL_DOMAIN>/sign-in` leads to the desktop either way. Its realm is
   its own, `user`. A
   second user tenant is refused in three places that share one rule
   (`internal/tenancy`): the admission webhook, the tenant reconciler and the
   director, each with a message naming the mode. The install creates the
   tenant, after the handover
   ([GETTING-STARTED.md](../../GETTING-STARTED.md#7-tenants)): its manifest
   is committed at step 0 with the cluster's definition and refused by the
   cluster until the platform admin has signed in once. The installer's last
   step, `E-04-user-tenant`, then asks Argo CD to try it again, waits for the
   tenant to be Ready and issues its administrator's activation link. A run
   that ends before that sign-in is finished later with
   `./install.sh --only E-04`. The step does nothing on a `multi` cluster.

**A website on the main address** is the user tenant's alone, and only under
`single`. It is published when all of this holds
(`internal/addresses/main_address.go`): the profile's entry is a perimeter
one that says `apex`; the tenant's approver published it and said `apex` too;
the approval has not expired; the entry stays off the paths the platform
keeps there (`/branding/`, `/sign-in`, `/.well-known/acme-challenge/`,
`/.well-known/pki-validation/`); the tenant is Ready and still on the
cluster's own domain; and no surface published earlier holds the address.
Otherwise nothing is published and the component says why
([routing.md §5](routing.md#5-redirects-and-url-control)).

The reason for the second statement is the realm. A realm's administrators can
see and change every account in it, and the kernel realm's accounts are the
ones that administer the cluster. Users of the cluster's apps do not belong
there, on a cluster of one organisation any more than on a shared one.

**What the user tenant of a single-tenancy cluster shares with the kernel.**
Its domain is the cluster's, so it has no listener, certificate, DNS wildcard
or apex route of its own: the cluster's certificate (`<KERNEL_DOMAIN>` and
`*.<KERNEL_DOMAIN>`) and catch-all listener serve it. The names the kernel
answers on at that level -- `id`, `platform`, `www`, `argocd`, `headlamp`,
`llm`, `mail`, `imap`, `mail-egress`, `corp` -- are refused to its components
and apps (`HostReserved`). The names the platform keeps in every tenant are
refused to apps there as everywhere.

**Reserved address names** are one list in two tiers
(`internal/hostnames/hostnames.go`; the tables are in
[routing.md §3.1](routing.md)). The first is kept in every tenant on every
cluster, whatever the tenant's domain: `desktop`, `admin` and `store`, each
admitted to the platform's own component for it and to nothing else, and
`console`, `platform`, `id`, `auth`, `login`, `signin`, `sign-in`, `sso`,
`account` and `accounts`, which nothing may take. The second is the kernel's
names listed above, refused only where a tenant's domain is the cluster's.
A name is matched exactly, and so is anything below it (`x.admin`). The
director refuses an install that would take one with `422` before anything
is committed, and the operator holds a component that would
(`HostReserved`).

Its mail domain is the cluster's too, which the kernel realm's people already
have addresses in. The mail stack keys on the address, not on the realm:

- One mail domain, registered twice (once for the kernel, once for the
  tenant) and deduplicated; mail for it is signed with the kernel's DKIM key,
  and that key's record is the one published.
- A mailbox is its address. `admin@` (the platform admin) and `user-admin@`
  (the user admin) are different mailboxes. A person created with the same
  address in both realms is two accounts and **one mailbox**; nothing refuses
  that.
- Under `MAIL_RECIPIENT_POLICY=strict` the accepted recipients of the domain
  are read from the kernel realm only, so the user tenant's people would be
  refused. The default (`catchall`) accepts both.

**Changing the mode on a running cluster.** `single` to `multi` is always
fine: the user tenant moves to `<label>.user.<KERNEL_DOMAIN>`. `multi` to
`single` with a user tenant other than `user` is refused by the director with
nothing written; a tenant that is there anyway is held by the operator
(`TenancyConstraint` on its status, nothing more provisioned, nothing
removed). The operator reads the mode at start.

| Plane | Domain | Example hosts | Origin TLS (cert-manager) | DNS responsibility |
|---|---|---|---|---|
| **Kernel** | `KERNEL_DOMAIN` | `id.platform.example.com`, `platform.platform.example.com` (the platform admin's desktop) | One DNS-01 wildcard `*.<kernel_domain>` at install | Cluster operator (kernel namespace only) |
| **Platform tenant** | `platform.<KERNEL_DOMAIN>` | `admin.platform.platform.example.com` (its admin console) | One DNS-01 wildcard `*.platform.<kernel_domain>`, issued like a tenant's | Platform zone |
| **Tenant apps** | `effectiveDomain` | `meet.demo.platform.example.com` (multi) or `meet.platform.example.com` (single) | One DNS-01 wildcard `*.<effectiveDomain>` **per tenant** (none on the kernel domain) | Platform zone; the customer's for a custom domain |

**Effective domain** (same for edge routing, mail, OIDC redirect URIs to apps):

- If a `TenantDomain` binds a custom domain → use it (e.g. `acme.com`; see below).
- Else, for the tenant named `user` under `tenancyMode: single` → `<KERNEL_DOMAIN>` (flat URLs).
- Else → `<tenant-name>.<KERNEL_DOMAIN>` (e.g. `demo.platform.example.com`; `platform.<KERNEL_DOMAIN>` for the platform tenant, under either mode).

App hostnames are always `{subDomain}.{effectiveDomain}`. The one host that is not is the platform tenant's desktop, which answers on its `effectiveDomain` itself.

**Portal contact deep links** (video call / chat from the address book): the
Gentian shell resolves per-tenant app URLs from `effectiveDomain` and entitlement
groups (`gentian:tenant:<t>:app:<profile>`).

The gentian-os operator creates, for every tenant with edge-routed apps:

1. One cert-manager `Certificate` with `dnsNames: [*.effectiveDomain]`. The apex
   is deliberately absent: the tenant listener is scoped to `*.effectiveDomain`
   and cannot route it, so naming it would let browsers coalesce apex requests
   onto a connection that answers 404. See `docs/design/routing.md` §3.
2. Secret `tenant-{name}-wildcard-tls` in the tenant namespace.
3. One Gateway API `HTTPRoute` per app host, attached to the tenant Gateway and
   `kernel-public-gateway`, all using that TLS secret on the tenant listener.

The kernel wildcard (`*.<kernel_domain>`) is **never** replicated into tenant namespaces. It does not cover `meet.demo.platform.example.com` (only one DNS label under the kernel domain).

### Why per-tenant wildcard (not kernel wildcard reuse)

- **Correct SANs:** `*.demo.platform.example.com` covers all app subdomains for that tenant.
- **Rate limits:** One ACME certificate per tenant, not one per app host.
- **CSP edge:** Multi-level names need their own edge cert when proxied (see below).
- **Custom domains:** Same code path when a `TenantDomain` binds `acme.com` — only DNS delegation changes.

### Issuer configuration (portable across DNS providers)

Wildcard certificates require **DNS-01**. The operator uses a single cluster-wide issuer name, configurable via `TENANT_DNS01_CLUSTER_ISSUER` (Helm: `tenantDNS01ClusterIssuer`, default `letsencrypt-dns01-cloudflare`). That `ClusterIssuer` must use a cert-manager DNS webhook matching your provider (Cloudflare, Route53, Azure DNS, Google Cloud DNS, etc.) and must be able to write `_acme-challenge` records in the zone that contains `effectiveDomain`.

`AppProfile.spec.ingress.clusterIssuer` is **not** used for tenant edge TLS today; it is reserved for future per-app overrides.

### Edge TLS (optional, CSP-specific)

When traffic is **proxied** (e.g. Cloudflare orange cloud), the CSP must also present a valid certificate for each hostname. Universal SSL on `*.platform.example.com` does **not** cover `meet.demo.platform.example.com`.

Optional operator integration (Cloudflare today): proxied CNAME `*.<effectiveDomain>` → tunnel target so **Total TLS** issues `*.<effectiveDomain>` at the edge. If disabled, use DNS-only (grey cloud) or TLS passthrough to the cluster origin cert.

Origin and edge are separate: cert-manager in the tenant namespace is the portable contract; Cloudflare/ACM/Front Door adapters are deployment options.

### Custom domains (`TenantDomain`)

A custom domain is not a field on the Tenant. It is a cluster-scoped
`TenantDomain` named after the tenant, committed beside its manifest
(`clusters/<cluster>/tenants/<tenant>/domain.yaml`) by the director's
`PUT /v1/clusters/{c}/tenants/{t}/domain` (`can_configure`). The operator copies
the domain to the Tenant's `status.domain`, which `EffectiveDomain` reads, and
reports `DomainBound`; a name that is not a hostname and a domain on or
under the kernel domain are refused, as is one another tenant holds. On the
command line it is `kubectl gentian tenants domain <name> [<domain> |
--remove]` ([commands.md](../commands.md)), which prints what moves and asks
for the domain to be typed. Neither the director nor the command checks the
domain's DNS or that a certificate can be issued for it.

A domain is bound to a user tenant of a multi-tenancy cluster and to no
other. Under `single` the user tenant is already on the cluster's own
addresses, and bound to another domain it would leave them and give up the
main address, so the director refuses a bind for any tenant there (`422`:
"this cluster's tenancy mode is single ... Nothing was changed"). It refuses
one for the platform tenant under either mode, whose addresses are the
cluster's own (`platform.<kernelDomain>` and below). `DELETE` is not
refused, which is how a tenant bound earlier is put back. The operator does
not ask the mode: a `TenantDomain` for the tenant `user` that is in the
cluster anyway moves that tenant to `<label>.<domain>`, and a website it had
on the main address is withdrawn (`OwnDomain`). The kernel's names stay
refused to its apps, because it is back on the cluster's domain the day the
binding is removed. The tenant's hosts, mail and logins move to it, and
the concierge finds it through `concierge-lookup`: one file per bound domain,
named by its SHA-256, so a domain is found by whoever already knows it and the
cluster's list of customers is not published.

- **OIDC issuer** stays at `https://id.<kernel_domain>/realms/<tenant>` — app URL changes do not invalidate tokens.
- **DNS:** Customer points `*.acme.com` (or per-host records) at the platform edge proxy/tunnel.
- **TLS:** Same per-tenant wildcard at origin if the platform can run DNS-01 in `acme.com` (delegated subzone or API token). If the customer will not grant DNS API access, a future tier can use HTTP-01 per hostname or BYO certificates — still the same HTTPRoute host naming.

### Kernel DNS credential

The Cloudflare (or other) API token for the **kernel** wildcard lives only in the kernel/`cert-manager` namespace (`gentian-os/kernel/dns/cloudflare` via OpenBao). The **tenant** DNS-01 issuer typically uses the same provider credentials at the cluster level but issues certs in each `tenant-*` namespace; it does not expose kernel secrets to tenants.

### ACME rate limits and dev staging

Let's Encrypt production enforces per-account and per-registered-domain limits (notably **50 certificates per registered domain per week** for `platform.example.com` and descendants). Each **reinstall** or issuer change that re-orders the kernel wildcard plus one wildcard per tenant can consume several certificates quickly.

| Environment | Recommendation |
|---|---|
| **Dev** | `ACME_ENV=staging` in `install.env`; staging `ClusterIssuer`s from `kernel/manifests/cert-manager/chart/templates/cluster-issuers-staging.yaml`; Helm `tenantDNS01ClusterIssuer: letsencrypt-staging-dns01-cloudflare` (see `gentian-deployments/profiles/dev.yaml`). Staging certs are **not** browser-trusted but use separate rate limits. `install.sh` and the operator bootstrap `gentian-staging-ca-tls`; compositions apply staging-only Synapse/Jitsi TLS workarounds when `ACME_STAGING=true`. See [security.md](security.md) §9. Re-apply with `./install.sh --only A-06-cluster-issuers,C-01-wildcard-cert`. |
| **Prod** | Production issuers only. One DNS-01 wildcard per tenant at origin; avoid `install.sh --uninstall` loops that re-issue everything. |
| **Tunnel + proxied (Cloudflare)** | Origin TLS (cert-manager) and **edge** TLS are independent. Enable **Total TLS** (or Advanced Certificate Manager) so `*.demo.platform.example.com` gets an edge cert — Universal SSL on `*.platform.example.com` does not cover multi-label tenant hosts. Optional: **Cloudflare Origin CA** at the origin to stop ordering public LE certs on every reinstall (edge still needs Total TLS when orange-cloud). |
| **Switching issuer on a live cluster** | Patch operator Helm value, run `./install.sh --only A-06-cluster-issuers,C-01-wildcard-cert`, delete existing `Certificate` CRs (kernel `wildcard-kernel`, tenant `tenant-*-wildcard`) so cert-manager re-issues against the new issuer. |

Manifests: production `cluster-issuers.yaml`; staging `cluster-issuers-staging.yaml`. Kernel wildcard `wildcard-kernel-cert.yaml` templates `DNS01_CLUSTER_ISSUER` from `ACME_ENV` at install time.

## 4. Network Boundaries

NetworkPolicies enforce three rules at the CNI level:

1. Tenant namespaces can reach kernel services (Keycloak, PostgreSQL,
   MinIO, Redis, Postfix).
2. Tenant namespaces **cannot** reach other tenant namespaces.
3. App-to-app calls within a tenant need a grant. Two profiles that
   name the same contract, one under `provides` and one under
   `integrations`, ask for a relation; the tenant's administrator gives
   it with an `AppGrant` for the consumer. Only then are two
   NetworkPolicies written: the consumer's pods may leave for the
   provider's, and the provider's admit the consumer's. A grant that is
   withdrawn takes both away. The network decides which app reaches
   which; it cannot tell one path of the provider from another, so what
   the consumer may do there is the provider's to check.
   For that the consumer is given a key of its own, in the Secret
   `contract-key-<consumer>-<contract>` (`CONTRACT_KEY`), and the
   provider the Secret `contract-callers-<provider>`: per contract an
   entry `<contract>.json`, a JSON object from the SHA-256 of each
   granted consumer's key to that consumer's name. A provider that asks
   for the key knows which consumer is calling, and holds nothing it
   could call as one of them with. Both Secrets go with the grant.

## 5. Identity and OIDC Trust Chain

**Suze** (Keycloak + OpenFGA) is the **single trust anchor** on new installs.
Each tenant gets a dedicated Keycloak realm; apps authenticate users via OIDC
against that realm, and its people sign in there too: the edge in front of a
tenant's desktop sends the browser to the tenant realm, and the concierge
on the kernel domain sends an address to its tenant's desktop (see
[iam.md](iam.md)). App-to-app calls use **OIDC token exchange (RFC 8693)** — app A
presents its user-bound token and receives a scoped token usable against app B.
The `IntegrationBinding` configures which exchanges are permitted; the binding's
status surfaces credential validity and last rotation time.

### 5.1 One realm · One namespace — the canonical isolation rule

Every tenant is identified by a pair that must be kept in 1:1 correspondence:

```
Keycloak realm <tenant>   ↔   namespace tenant-<tenant>
```

On the Suze path, users live in the **tenant realm**.

Breaking realm ↔ namespace correspondence is a configuration error and must never occur.

### 5.2 Identity and Access Management (IAM)

For Keycloak realm structure, group entitlements, Admin Console roles, and how
tenant admin vs member are separated, see [iam.md](iam.md) and
[admin-console.md](admin-console.md).

## 6. Database Isolation

Each app within each tenant gets a database named
`{databasePrefix}_{app}` (e.g., `gtn_demo_app`) with a dedicated
user that has grants limited to that database only. There is no
shared schema, no cross-app access, no possibility of one tenant
seeing another's data via SQL.

## 7. Mail Security

When the mail extension is enabled:

- A DKIM keypair is generated per tenant domain by the operator and kept as
  a Secret in `system-mail`; Postfix mounts the keys and OpenDKIM signs with
  them. There is no spam filter in the stack.
- SPF and DMARC records are generated per domain and surfaced in the
  Tenant status for DNS configuration.
- SMTP submission requires SASL authentication against one credential per
  tenant, shared by that tenant's apps — no open relay.
- IMAP authenticates each person, with a password per person derived by the
  platform, or with the person's sign-in token for an app whose profile
  declares `requires.services.mail.imap.tokenSignIn`.
- Only a proxy in `system-mail-dmz` faces the internet; Postfix and Dovecot
  are in `system-mail`, and each mail object is written in the namespace of
  its reader.

See [mail.md](mail.md) for the full mail extension model.

## 8. Operational Roles {#roles}

Three roles, three scopes:

| Role | Primary scope | Can do | Cannot do |
|---|---|---|---|
| **Cluster admin** | Cluster + kernel | Run installer, configure ArgoCD/OpenBao/cert-manager, manage kernel upgrade policy, approve tenant onboarding manifests | Perform tenant business actions, bypass GitOps in prod for tenant changes |
| **Tenant admin** | One tenant's apps | Install/uninstall apps for the tenant, edit tenant-level config, view tenant health and reconciliation state | Touch kernel components, modify other tenants, alter cluster-wide policy |
| **Tenant user** | Day-to-day app use | Use installed apps via SSO, consume integrations | Install/uninstall apps, modify tenant manifest, see admin surfaces |

### 8.1 Admin / User Separation of Duties

The **tenant admin and tenant user are strictly separate identities**.
It is strongly recommended that a single person does not use the same
account for both day-to-day app usage and tenant administration.

**Portal tile enforcement:**
Access to apps is controlled through **Keycloak group entitlements**
(`gentian:tenant:<t>:app:<profile>`) and OpenFGA `can_launch` checks.
Tenant admins and members are **mutually exclusive** roles provisioned
via the [Gentian Admin Console](admin-console.md).

**Current operating model:** tenant admins edit Tenant manifests in
the deployments repo via PR (process-controlled), and manage members/groups in
the Admin Console (when deployed).

**App Store (current):** tenant admins use the **App Store app** or
`kubectl gentian apps` to install apps. The store's data is served from
outside the cluster and its interface is an app on the cluster; an install is
a commit to `gentian-deployments` made by the director as the person who
asked. See [commands.md](../commands.md) §5.

**Future:** further self-service (tenant config, quotas) via the same surfaces
without requiring YAML edits.

## 9. Future: Capability Enforcement at Runtime

Today contracts between apps are trust-based: an app declaring
`webdav:read` is trusted not to attempt `webdav:write`. Future
versions may enforce capabilities at the network layer using a service
mesh (Istio AuthorizationPolicy) or an API gateway that inspects
requests against declared capabilities — the cloud-OS equivalent of
SELinux/seccomp adding mandatory access control on top of POSIX
discretionary permissions.
