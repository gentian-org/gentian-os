# Gateway and Edge Routing

**Companion to:** [architecture.md](../architecture.md)

---

## 1. Edge Plane Overview

Gentian OS exposes all HTTP(S) entry points through **Gateway API** backed by
**Envoy Gateway**.

The edge plane has three responsibilities:

1. Publish kernel and tenant application endpoints.
2. Enforce routing, TLS, and browser security policy at the edge.
3. Keep route intent declarative and tenant-scoped.

The control model is:

- **GatewayClass**: `gentian-envoy`
- **Kernel Gateway**: shared platform entry points (`portal`, `id`, Argo CD, and
  other kernel hosts)
- **Tenant Gateways**: one Gateway per tenant namespace for tenant app hostnames
- **HTTPRoute**: one route object per exposed app endpoint
- **Envoy policy CRDs**: security, traffic, and header policy attached to
  Gateway/HTTPRoute resources

---

## 2. Resource Topology

### 2.1 Kernel scope

Kernel services are exposed through a shared Gateway in the kernel services
namespace.

- Gateway: `kernel-public-gateway`
- Listener protocol: HTTPS
- Hostnames: `portal.<kernelDomain>`, `id.<kernelDomain>`, and kernel service
  hosts
- TLS: kernel wildcard certificate managed by cert-manager

### 2.2 Tenant scope

Each tenant namespace contains its own Gateway.

- Gateway name: `tenant-<name>-gateway`
- Listener protocol: HTTPS
- Listener hostname: `*.${effectiveDomain}`
- TLS certificate: `tenant-<name>-wildcard-tls`
- Route attachment: HTTPRoutes in the same namespace

This keeps hostname ownership, certificate ownership, and RBAC boundaries aligned
with tenant isolation.

### 2.3 Route model

For each app endpoint, Gentian OS creates an HTTPRoute with:

- `parentRefs` pointing to the tenant Gateway
- `hostnames` set to `<subDomain>.<effectiveDomain>`
- one `PathPrefix` match for `/`
- one backendRef to the app Service

Additional app hosts (for example sidecar hosts such as `meet.<effectiveDomain>`)
are represented as separate HTTPRoutes.

---

## 3. Domain and TLS Model

The domain model is:

- `effectiveDomain = Tenant.spec.domain` when set
- otherwise:
  - `TENANCY_MODE=multi`: `<tenant>.<kernelDomain>`
  - `TENANCY_MODE=single`: `<kernelDomain>`

TLS issuance is handled by cert-manager:

- Kernel wildcard certificate for kernel hosts.
- One wildcard certificate per tenant:
  - DNS names: `*.${effectiveDomain}` and `${effectiveDomain}`
  - Secret name: `tenant-<name>-wildcard-tls`

Gateway listeners consume these certificate secrets directly.

---

## 4. Browser Security and Embedding

Gentian Portal embeds tenant applications in iframes. Edge policy enforces
embedding rules through explicit response header policy attached to routes.

Default app policy:

- remove upstream `X-Frame-Options`
- enforce `Content-Security-Policy` with `frame-ancestors 'self'` plus
  `https://portal.<kernelDomain>`

CryptPad policy:

- preserve upstream CSP directives that are required for runtime behavior
- append `frame-ancestors` entries for portal and CryptPad sandbox/main origins

Keycloak OIDC broker policy:

- kernel IdP routes include portal origin plus tenant app OIDC origins in
  `frame-ancestors`
- policy is reconciled from tenant/domain state so iframe-based login flows stay
  valid as tenants and apps change

---

## 5. Redirects and URL Control

Tenant apex redirects are modeled with HTTPRoute filters.

- Host: `${effectiveDomain}`
- Filter: HTTP redirect to `https://portal.<kernelDomain>/`

Application-specific redirects and rewrites are expressed with Gateway API route
filters or Envoy extension policies where advanced behavior is needed.

---

## 6. Tenant Isolation at the Edge

Tenant isolation is preserved in four layers:

1. **Namespace-scoped Gateways and HTTPRoutes** for ownership boundaries.
2. **Tenant-scoped TLS secrets** for certificate separation.
3. **NetworkPolicies** allowing ingress from Envoy data-plane namespaces to
   tenant workloads, and egress from tenant pods to the Envoy Gateway Service
   ClusterIP (via namespace selector on `envoy-gateway-system`) so in-cluster
   hairpin DNS overrides for kernel hostnames reach the programmed edge routes.
4. **Identity and token exchange controls** at app/service layers via
   IntegrationBindings and OIDC policy.

No tenant route may target backends in another tenant namespace unless explicitly
allowed by policy resources.

---

## 7. App Catalogue Contract

App catalogue entries declare HTTP exposure using typed route intent:

- hostname/subdomain intent
- backend service name/port
- TLS requirement
- optional edge policy profile (timeouts, body limits, headers)

The platform renders this intent into HTTPRoute and policy resources, so app
profiles stay controller-agnostic and do not encode implementation-specific
annotation keys.

---

## 8. Operations and Day-2

### 8.1 Observability

Operational visibility is provided by:

- Gateway and HTTPRoute status conditions (`Accepted`, `Programmed`, `ResolvedRefs`)
- Envoy access logs and metrics
- cert-manager certificate readiness
- tenant conditions in `Tenant.status.conditions`

### 8.2 Failure domains

Edge failures are scoped by resource layer:

- Route errors are isolated to affected HTTPRoutes.
- Tenant Gateway issues affect one tenant namespace.
- Kernel Gateway issues affect only kernel endpoints.

### 8.3 Drift and reconciliation

All Gateway API and policy resources are continuously reconciled by Gentian OS
controllers and managed declaratively in GitOps flows.

---

## 9. Security Posture

The edge plane enforces:

- TLS everywhere for external entry points
- explicit embedding policy instead of implicit browser behavior
- per-tenant hostname and certificate ownership
- least-privilege cross-namespace references
- auditable, declarative edge policy

This makes routing and browser-facing security controls consistent across kernel
services and tenant applications.

---

## 10. In-Cluster DNS Hairpin

Pods that call kernel public hostnames (for example `https://id.<kernelDomain>/…`
during Synapse OIDC bootstrap) cannot rely on external DNS or legacy nginx
Ingress when `ROUTING_MODE=gateway`. Gentian OS reconciles a CoreDNS `hosts`
override block (`# BEGIN gentian-hairpin`) so those names resolve to the
current edge proxy ClusterIP:

- **Gateway mode:** kernel Envoy Gateway Service in `envoy-gateway-system`
- **Ingress mode:** `ingress-controller` Service in `ingress`

The `mail.<kernelDomain>` entry is managed separately and continues to target
the Dovecot Service ClusterIP (see [mail.md](mail.md)).

The gateway-platform controller updates the hairpin block when the Envoy
Service or routing mode changes and rolls CoreDNS. Tenant NetworkPolicies are
refreshed when the edge Service changes so egress to `envoy-gateway-system`
stays allowed.
