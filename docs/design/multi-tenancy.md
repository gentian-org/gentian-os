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

Install-time **`TENANCY_MODE`** (`multi` default, set in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env` and mirrored
to operator Helm `tenancyMode`) selects the default app URL shape when
`Tenant.spec.domain` is unset.
Both modes use the **same central IdP** at `id.<KERNEL_DOMAIN>/realms/<tenant>`.

| Mode | Cluster profile | Default `effectiveDomain` | Example Jitsi URL |
|---|---|---|---|
| **`multi`** | Shared SaaS | `<tenant>.<KERNEL_DOMAIN>` | `https://meet.demo.desk.gentian.org` |
| **`single`** | Dedicated / demanding customer | `<KERNEL_DOMAIN>` (flat) | `https://meet.desk.gentian.org` |

**Single-tenancy rules:** exactly one `Tenant` CR named `default`, operator env `TENANCY_MODE=single`. Vanity `spec.domain` still overrides
in either mode. Legacy LDAP clusters also use `ou=default` under `dc=swp-ldap,dc=internal`.

| Plane | Domain | Example hosts | Origin TLS (cert-manager) | DNS responsibility |
|---|---|---|---|---|
| **Kernel** | `KERNEL_DOMAIN` | `portal.desk.gentian.org`, `id.desk.gentian.org` | One DNS-01 wildcard `*.<kernel_domain>` at install | Cluster operator (kernel namespace only) |
| **Tenant apps** | `effectiveDomain` | `meet.demo.desk.gentian.org` (multi) or `meet.desk.gentian.org` (single) | One DNS-01 wildcard `*.<effectiveDomain>` **per tenant** | Platform zone for default tenants; customer for vanity |

**Effective domain** (same for edge routing, mail, OIDC redirect URIs to apps):

- If `Tenant.spec.domain` is set → use it (customer vanity, e.g. `acme.com`).
- Else if `TENANCY_MODE=single` → `<KERNEL_DOMAIN>` (flat OpenDesk-style URLs).
- Else (`multi`) → `<tenant-name>.<KERNEL_DOMAIN>` (e.g. `demo.desk.gentian.org`).

App hostnames are always `{subDomain}.{effectiveDomain}`.

**Portal contact deep links** (video call / chat from the address book): on the Suze path,
the Gentian shell resolves per-tenant app URLs from `effectiveDomain` and entitlement
groups — not Nubus portal-server LDAP entries. Legacy clusters (`IDENTITY_MODE=legacy-ldap`)
still use UDM entries `swp.realtime_videoconference_<tenant>` with `allowedGroups` scoped
to the tenant LDAP OU.

The gentian-os operator creates, for every tenant with edge-routed apps:

1. One cert-manager `Certificate` with `dnsNames: [*.effectiveDomain, effectiveDomain]`.
2. Secret `tenant-{name}-wildcard-tls` in the tenant namespace.
3. One Gateway API `HTTPRoute` per app host, attached to the tenant Gateway and
   `kernel-public-gateway`, all using that TLS secret on the tenant listener.

The kernel wildcard (`*.<kernel_domain>`) is **never** replicated into tenant namespaces. It does not cover `meet.demo.desk.gentian.org` (only one DNS label under the kernel domain).

### Why per-tenant wildcard (not kernel wildcard reuse)

- **Correct SANs:** `*.demo.desk.gentian.org` covers all app subdomains for that tenant.
- **Rate limits:** One ACME certificate per tenant, not one per app host.
- **CSP edge:** Multi-level names need their own edge cert when proxied (see below).
- **Customer vanity:** Same code path when `spec.domain` is `acme.com` — only DNS delegation changes.

### Issuer configuration (portable across DNS providers)

Wildcard certificates require **DNS-01**. The operator uses a single cluster-wide issuer name, configurable via `TENANT_DNS01_CLUSTER_ISSUER` (Helm: `tenantDNS01ClusterIssuer`, default `letsencrypt-dns01-cloudflare`). That `ClusterIssuer` must use a cert-manager DNS webhook matching your provider (Cloudflare, Route53, Azure DNS, Google Cloud DNS, etc.) and must be able to write `_acme-challenge` records in the zone that contains `effectiveDomain`.

`AppProfile.spec.ingress.clusterIssuer` is **not** used for tenant edge TLS today; it is reserved for future per-app overrides.

### Edge TLS (optional, CSP-specific)

When traffic is **proxied** (e.g. Cloudflare orange cloud), the CSP must also present a valid certificate for each hostname. Universal SSL on `*.desk.gentian.org` does **not** cover `meet.demo.desk.gentian.org`.

Optional operator integration (Cloudflare today): proxied CNAME `*.<effectiveDomain>` → tunnel target so **Total TLS** issues `*.<effectiveDomain>` at the edge. If disabled, use DNS-only (grey cloud) or TLS passthrough to the cluster origin cert.

Origin and edge are separate: cert-manager in the tenant namespace is the portable contract; Cloudflare/ACM/Front Door adapters are deployment options.

### Customer vanity domains (`spec.domain`)

- **OIDC issuer** stays at `https://id.<kernel_domain>/realms/<tenant>` — app URL changes do not invalidate tokens.
- **DNS:** Customer points `*.acme.com` (or per-host records) at the platform edge proxy/tunnel.
- **TLS:** Same per-tenant wildcard at origin if the platform can run DNS-01 in `acme.com` (delegated subzone or API token). If the customer will not grant DNS API access, a future tier can use HTTP-01 per hostname or BYO certificates — still the same HTTPRoute host naming.

### Kernel DNS credential

The Cloudflare (or other) API token for the **kernel** wildcard lives only in the kernel/`cert-manager` namespace (`gentian-os/kernel/dns/cloudflare` via OpenBao). The **tenant** DNS-01 issuer typically uses the same provider credentials at the cluster level but issues certs in each `tenant-*` namespace; it does not expose kernel secrets to tenants.

### ACME rate limits and dev staging

Let's Encrypt production enforces per-account and per-registered-domain limits (notably **50 certificates per registered domain per week** for `desk.gentian.org` and descendants). Each **reinstall** or issuer change that re-orders the kernel wildcard plus one wildcard per tenant can consume several certificates quickly.

| Environment | Recommendation |
|---|---|
| **Dev** | `ACME_ENV=staging` in `install.env`; staging `ClusterIssuer`s from `kernel/manifests/cert-manager/cluster-issuers-staging.yaml`; Helm `tenantDNS01ClusterIssuer: letsencrypt-staging-dns01-cloudflare` (see `gentian-deployments/clusters/<cluster>/kernel/values-dev.yaml`). Staging certs are **not** browser-trusted but use separate rate limits. `install.sh` and the operator bootstrap `gentian-staging-ca-tls`; compositions apply staging-only Synapse/Jitsi TLS workarounds when `ACME_STAGING=true`. See [security.md](security.md) §9. Re-apply with `./update.sh --acme-issuers`. |
| **Prod** | Production issuers only. One DNS-01 wildcard per tenant at origin; avoid `uninstall.sh -f` loops that re-issue everything. |
| **Tunnel + proxied (Cloudflare)** | Origin TLS (cert-manager) and **edge** TLS are independent. Enable **Total TLS** (or Advanced Certificate Manager) so `*.demo.desk.gentian.org` gets an edge cert — Universal SSL on `*.desk.gentian.org` does not cover multi-label tenant hosts. Optional: **Cloudflare Origin CA** at the origin to stop ordering public LE certs on every reinstall (edge still needs Total TLS when orange-cloud). |
| **Switching issuer on a live cluster** | Patch operator Helm value, run `./update.sh --acme-issuers`, delete existing `Certificate` CRs (kernel `wildcard-kernel`, tenant `tenant-*-wildcard`) so cert-manager re-issues against the new issuer. |

Manifests: production `cluster-issuers.yaml`; staging `cluster-issuers-staging.yaml`. Kernel wildcard `wildcard-kernel-cert.yaml` templates `DNS01_CLUSTER_ISSUER` from `ACME_ENV` at install time.

## 4. Network Boundaries

NetworkPolicies enforce three rules at the CNI level:

1. Tenant namespaces can reach kernel services (Keycloak, PostgreSQL,
   MinIO, Redis, Postfix).
2. Tenant namespaces **cannot** reach other tenant namespaces.
3. App-to-app calls within a tenant are scoped by the
   `IntegrationBinding` — the binding emits a NetworkPolicy that
   allows the consumer to reach the provider only for the declared
   capabilities.

## 5. Identity and OIDC Trust Chain

**Suze** (Keycloak + OpenFGA) is the **single trust anchor** on new installs.
Each tenant gets a dedicated Keycloak realm; apps authenticate users via OIDC
against that realm. The shared portal at `portal.<KERNEL_DOMAIN>` uses the
**kernel realm**, which brokers to the correct tenant realm by email / tenant
resolution. App-to-app calls use **OIDC token exchange (RFC 8693)** — app A
presents its user-bound token and receives a scoped token usable against app B.
The `IntegrationBinding` configures which exchanges are permitted; the binding's
status surfaces credential validity and last rotation time.

### 5.1 One realm · One namespace — the canonical isolation rule

Every tenant is identified by a pair that must be kept in 1:1 correspondence:

```
Keycloak realm <tenant>   ↔   namespace tenant-<tenant>
```

On the Suze path, users live in the **tenant realm** (not a shared LDAP OU).
Legacy clusters (`IDENTITY_MODE=legacy-ldap`) also maintain
`ou=<tenant>,dc=swp-ldap,dc=internal` — see [iam.md § Legacy](iam.md#5-legacy-nubus--ldap-path).

Breaking realm ↔ namespace correspondence is a configuration error and must never occur.

### 5.2 Identity and Access Management (IAM)

For Keycloak realm structure, group entitlements, Admin Console roles, and how
tenant admin vs member are separated, see [iam.md](iam.md) and
[admin-console.md](admin-console.md).

## 6. Database Isolation

Each app within each tenant gets a database named
`{databasePrefix}_{app}` (e.g., `gtn_openproject`) with a dedicated
user that has grants limited to that database only. There is no
shared schema, no cross-app access, no possibility of one tenant
seeing another's data via SQL.

## 7. Mail Security

When the mail extension is enabled:

- DKIM keypairs are generated per tenant domain and stored in OpenBao
  at `tenants/{name}/mail/dkim`. The shared Rspamd instance fetches
  them at runtime.
- SPF and DMARC records are generated per domain and surfaced in the
  Tenant status for DNS configuration.
- SMTP submission requires SASL authentication against per-tenant
  credentials — no open relay.
- IMAP authentication uses per-tenant credentials provisioned by the platform
  (legacy path: LDAP OU bind; Suze path: native auth backend when migrated).

See [mail.md](mail.md) for the full mail extension model.

## 8. Operational Roles {#roles}

Three roles, three scopes:

| Role | Primary scope | Can do | Cannot do |
|---|---|---|---|
| **Cluster admin** | Cluster + kernel | Run installer, configure ArgoCD/OpenBao/cert-manager, manage kernel upgrade policy, approve tenant onboarding manifests | Perform tenant business actions, bypass GitOps in prod for tenant changes |
| **Tenant admin** | One tenant's apps | Install/uninstall apps for the tenant, edit tenant-level config, view tenant health and reconciliation state | Touch kernel components, modify other tenants, alter cluster-wide policy |
| **Tenant user** | Day-to-day app use | Use installed apps via SSO, consume integrations | Install/uninstall apps, modify tenant manifest, see admin surfaces |

### 8.1 Admin / User Separation of Duties

Following the openDesk model, the **tenant admin and tenant user are strictly
separate identities**. It is strongly recommended that a single person does not
use the same account for both day-to-day app usage and tenant administration.

**Portal tile enforcement (Suze path):**
Access to apps is controlled through **Keycloak group entitlements**
(`gentian:tenant:<t>:app:<profile>`) and OpenFGA `can_launch` checks — not LDAP
`managed-by-attribute-*` groups or UMC templates. Tenant admins and members are
**mutually exclusive** roles provisioned via the [Gentian Admin Console](admin-console.md).

Legacy clusters still use UMC `Admin User` / `App User` templates — see
[iam.md § Legacy](iam.md#5-legacy-nubus--ldap-path).

**Current operating model:** tenant admins edit Tenant manifests in
the deployments repo via PR (process-controlled), and manage members/groups in
the Admin Console (when deployed).

**App Store (current):** tenant admins use the **App Store** web UI
(`app-store` AppProfile) or `kubectl gentian apps` to install catalogue apps.
Installs commit to `gentian-deployments` (GitOps) or create namespace-scoped
`App` claims when `INSTALL_MODE=k8s`. See [commands.md](../commands.md) §5.

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
