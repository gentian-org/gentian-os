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
- **Gateway**: a single cluster Gateway, `kernel-public-gateway`, terminating TLS
  for every external hostname — kernel hosts and tenant app hosts alike
- **HTTPRoute**: one route object per exposed app endpoint, living in the
  namespace that owns its backend
- **Envoy policy CRDs**: security, traffic, and header policy attached to
  Gateway/HTTPRoute resources

---

## 2. Resource Topology

### 2.1 The cluster Gateway

`kernel-public-gateway` lives in the kernel services namespace and owns the
cluster's external address. Every externally reachable hostname is served by it.

One Gateway, rather than one per tenant, is a requirement rather than a
simplification. A Gateway maps to an Envoy deployment with its own Service, and
each such Service claims the cluster's external address; two of them contend for
one address, and a client's TLS handshake succeeds or fails depending on which
one holds it at that moment.

Its listeners are:

| Listener | Port | Hostname | Certificate |
| --- | --- | --- | --- |
| `http-redirect` | 80 | none | none — redirects to `https` |
| `https-wildcard` | 443 | none | kernel wildcard, `kernel-wildcard-tls` |
| `https-tenant-<name>-wildcard` | 443 | none | `tenant-<name>-wildcard-tls` |

HTTPS listeners carry no hostname. Envoy selects a certificate by SNI, so an
unset hostname means "serve whatever this certificate covers" and the certificate
alone defines the listener's reach. A listener hostname would instead act as a
second, narrower filter, and the two disagree whenever a browser coalesces
requests: with HTTP/2, a browser reuses one connection for every hostname the
presented certificate covers, so a request for `${effectiveDomain}` may arrive on
a connection opened with SNI `<subDomain>.${effectiveDomain}`. Under a
`*.${effectiveDomain}` listener hostname the apex request finds no matching
listener and is answered with a 404 by the wrong listener's route table.

The listener hostname also gates route attachment: a route attaches only where
its hostnames intersect the listener's. Leaving it unset keeps attachment under
the control of `parentRefs.sectionName`, which is explicit.

Tenant listeners are added to and removed from the Gateway as tenants are
created and deleted. `mergeGateways` is enabled on the `EnvoyProxy`, so all
listeners are programmed into one Envoy deployment.

### 2.2 Tenant scope

A tenant namespace owns its certificate and its routes, not a Gateway.

- TLS certificate: `tenant-<name>-wildcard-tls`, in the tenant namespace
- Gateway listener: `https-tenant-<name>-wildcard` on `kernel-public-gateway`
- ReferenceGrant: permits the Gateway to read that certificate across the
  namespace boundary
- HTTPRoutes: in the tenant namespace, attached to the tenant's listener

The certificate stays under tenant ownership and is never copied into the kernel
namespace; the ReferenceGrant is the only thing that crosses the boundary, and it
is granted per tenant, for one Secret, in one direction.

### 2.3 Route model

For each app endpoint, Gentian OS creates an HTTPRoute with:

- `parentRefs` naming `kernel-public-gateway` in the kernel services namespace,
  with `sectionName` pinning it to one listener
- `hostnames` set to `<subDomain>.<effectiveDomain>`
- one `PathPrefix` match for `/`
- one backendRef to the app Service

`sectionName` is mandatory. Without it a route attaches to every listener whose
hostname permits it, including the hostname-less `http-redirect` listener on
port 80. Gateway API ranks a route with a specific hostname above the redirect
route's absent one, so an unpinned route serves the app in the clear on `:80`
instead of redirecting to `https`.

The listener a route pins to follows its certificate: hosts under
`<subDomain>.${effectiveDomain}` pin to `https-tenant-<name>-wildcard`, and hosts
covered by the kernel wildcard — including the tenant apex under multi-tenancy —
pin to `https-wildcard`.

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

Gateway listeners consume these certificate secrets directly, reading tenant
secrets across namespaces under the tenant's ReferenceGrant.

---

## 4. Browser Security and Embedding

Gentian Portal embeds tenant applications in iframes. Edge policy enforces
embedding rules through explicit response header policy attached to routes.

Default app policy:

- remove upstream `X-Frame-Options`
- enforce `Content-Security-Policy` with `frame-ancestors 'self'` plus
  `https://portal.<kernelDomain>`

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

1. **Namespace-scoped HTTPRoutes** for ownership boundaries: a route lives with
   the backend it fronts, and a tenant may only create routes in its own
   namespace.
2. **Tenant-scoped TLS secrets** for certificate separation, exposed to the
   Gateway one Secret at a time by per-tenant ReferenceGrants.
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

- Route errors are isolated to the affected HTTPRoutes.
- A listener that fails to program — most often an unissued certificate — takes
  down the hostnames on that listener only; the Gateway keeps serving the rest.
- Gateway-level failures affect every external endpoint, so Gateway changes are
  reconciled as whole-object updates and validated through
  `Gateway.status.listeners`.

### 8.3 Drift and reconciliation

All Gateway API and policy resources are continuously reconciled by Gentian OS
controllers and managed declaratively in GitOps flows.

---

## 9. Security Posture

The edge plane enforces:

- TLS everywhere for external entry points
- explicit embedding policy instead of implicit browser behavior
- per-tenant hostname and certificate ownership, with cross-namespace
  certificate access granted explicitly per tenant
- least-privilege cross-namespace references
- auditable, declarative edge policy

This makes routing and browser-facing security controls consistent across kernel
services and tenant applications.

---

## 10. In-Cluster DNS Hairpin

Pods that call kernel public hostnames (for example `https://id.<kernelDomain>/…`
during Synapse OIDC bootstrap) cannot rely on external DNS alone. Gentian OS
reconciles a CoreDNS `hosts` override block (`# BEGIN gentian-hairpin`) so those
names resolve to the kernel Envoy Gateway Service ClusterIP in
`envoy-gateway-system`.

The `mail.<kernelDomain>` entry is managed separately and continues to target
the Dovecot Service ClusterIP (see [mail.md](mail.md)).

The gateway-platform controller updates the hairpin block when the Envoy
Service or routing mode changes and rolls CoreDNS. Tenant NetworkPolicies are
refreshed when the edge Service changes so egress to `envoy-gateway-system`
stays allowed.
