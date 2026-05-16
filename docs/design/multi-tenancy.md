# Multi-Tenancy, Domains and Security

**Companion to:** [architecture.md](../architecture.md)

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

## 3. Domains and TLS — Hybrid Two-Plane Model

| Plane | Domain | Hosts | TLS issuance | DNS responsibility |
|---|---|---|---|---|
| **Kernel plane** | `KERNEL_DOMAIN` (one per cluster, e.g. `desk.gentian.org`) | Keycloak, Argo CD, Portal, all kernel UIs | One DNS-01 wildcard cert `*.<kernel_domain>` issued at install | Cluster operator (one cert, controlled DNS API access) |
| **Tenant plane (default)** | `<tenant>.<kernel_domain>` (e.g. `gtn-demo.desk.gentian.org`) | Tenant apps when no vanity domain set | Reuses kernel wildcard (Secret replicated by the platform into each tenant namespace) | None — covered by wildcard A/CNAME |
| **Tenant plane (vanity)** | `Tenant.spec.domain` (e.g. `acme.com`) | Tenant apps when vanity domain set | HTTP-01 per-host certs by cert-manager | Customer creates A/CNAME pointing at cluster ingress IP |

**Why this split:**

- **One DNS API token.** The Cloudflare (or other DNS provider)
  credential for DNS-01 lives only in the kernel namespace; never
  replicated to tenant namespaces, never exposed to vanity-domain
  customers, never required for tenant onboarding.
- **Customers own DNS, not access.** Vanity domains require no
  platform-side DNS automation: the customer creates CNAME/A records
  to the cluster ingress IP and HTTP-01 handles certs.
- **Stable OIDC issuer.** The Keycloak issuer URL stays at
  `https://keycloak.<kernel_domain>/realms/<tenant>` regardless of
  where the tenant's apps live. Token validation is therefore
  independent of vanity domain choice; changing/removing the vanity
  domain does not invalidate any tokens.
- **Better cookie isolation.** Apps on different registrable domains
  cannot share third-party cookies — strict improvement over a single
  shared domain.
- **Predictable fallback.** A tenant with no `domain` field gets a
  working URL under the kernel wildcard — useful for demos and the
  period before vanity DNS is wired up.

**Migration from default to vanity:** customer (a) creates the DNS
records, (b) sets `Tenant.spec.domain`. The platform switches Ingresses
from the replicated wildcard Secret to per-host HTTP-01 certs without
touching Keycloak.

The Cloudflare API token used for the kernel wildcard is stored in
OpenBao (`gentian-os/kernel/dns/cloudflare`) and surfaced as a Secret
in the `cert-manager` namespace by ESO.

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

The identity provider (Keycloak in Nubus) is the **single trust
anchor**. Each tenant gets a dedicated realm; apps authenticate users
via OIDC against that realm. App-to-app calls use **OIDC token
exchange (RFC 8693)** — app A presents its user-bound token and
receives a scoped token usable against app B. The
`IntegrationBinding` configures which exchanges are permitted; the
binding's status surfaces credential validity and last rotation time.

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
- IMAP authentication uses the tenant's LDAP OU with the same bind
  credentials provisioned by the platform for other apps.

See [mail.md](mail.md) for the full mail extension model.

## 8. Operational Roles {#roles}

Three roles, three scopes:

| Role | Primary scope | Can do | Cannot do |
|---|---|---|---|
| **Cluster admin** | Cluster + kernel | Run installer, configure ArgoCD/OpenBao/cert-manager, manage kernel upgrade policy, approve tenant onboarding manifests | Perform tenant business actions, bypass GitOps in prod for tenant changes |
| **Tenant admin** | One tenant's apps | Install/uninstall apps for the tenant, edit tenant-level config, view tenant health and reconciliation state | Touch kernel components, modify other tenants, alter cluster-wide policy |
| **Tenant user** | Day-to-day app use | Use installed apps via SSO, consume integrations | Install/uninstall apps, modify tenant manifest, see admin surfaces |

**Current operating model:** tenant admins edit Tenant manifests in
the deployments repo via PR (process-controlled).

**Future operating model:** tenant admins use a CLI/WebUI; an
automation bot writes Git commits on their behalf. This preserves
GitOps as the source of truth while removing the requirement that
tenant admins know YAML.

## 9. Future: Capability Enforcement at Runtime

Today contracts between apps are trust-based: an app declaring
`webdav:read` is trusted not to attempt `webdav:write`. Future
versions may enforce capabilities at the network layer using a service
mesh (Istio AuthorizationPolicy) or an API gateway that inspects
requests against declared capabilities — the cloud-OS equivalent of
SELinux/seccomp adding mandatory access control on top of POSIX
discretionary permissions.
