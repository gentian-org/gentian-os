# Routing and the Edge

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
| `https-tenant-<name>-wildcard` | 443 | `*.${effectiveDomain}` | `tenant-<name>-wildcard-tls` |

`https-wildcard` carries no hostname. Envoy selects a certificate by SNI, so an
unset hostname means "serve whatever this certificate covers", and the
certificate alone defines the listener's reach. A hostname would instead act as a
second, narrower filter, and the two disagree whenever a browser coalesces
requests: over HTTP/2 a browser reuses one connection for every hostname the
presented certificate covers, so a request for `portal.<kernelDomain>` may arrive
on a connection opened with SNI `<kernelDomain>`. A listener scoped to a subset
of its own certificate answers such a request from the wrong route table, with a
404.

Tenant listeners must carry `*.${effectiveDomain}`: Gateway API requires
listeners sharing a port to be distinguishable, and only one listener on `:443`
can leave its hostname unset.

The tenant certificate must therefore **not** name `${effectiveDomain}` itself
(§3). While it did, a browser could coalesce a request for the tenant apex onto
a connection opened for `<subDomain>.${effectiveDomain}` and reach this listener,
whose hostname does not match the apex and which therefore holds no route for
it — `404 route_not_found`, intermittently, depending on which connection
happened to be open.

That was survivable only while the apex carried nothing but a redirect. It no
longer does: the apex serves the portal itself, so that a login hint on the
tenant host is not discarded and the address bar stays on the tenant's name
(`kernel_gateway_routes.go`). The apex is a critical path now, and it is kept
reachable by keeping it out of the tenant certificate rather than by keeping it
empty.

The listener hostname also gates route attachment: a route attaches only where
its hostnames intersect the listener's. `parentRefs.sectionName` narrows that
further and is what routes rely on.

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
  - DNS names: `*.${effectiveDomain}` — the apex is **deliberately absent**
  - Secret name: `tenant-<name>-wildcard-tls`

A certificate must cover exactly what its listener can route. The tenant
listener is scoped to `*.${effectiveDomain}` (§2.1), and a listener hostname
gates route attachment, so no route for the bare apex can attach there. Naming
the apex in this certificate would advertise a name that listener cannot serve:
a browser reads the certificate, coalesces apex requests onto an open
`<sub>.${effectiveDomain}` connection, Envoy picks the filter chain by SNI, and
the request lands where no route matches — `404 route_not_found`. Leaving it out
means the apex opens its own connection with its own SNI and is served by the
hostname-less kernel listener, whose certificate covers
`<tenant>.<kernelDomain>` already.

Gateway listeners consume these certificate secrets directly, reading tenant
secrets across namespaces under the tenant's ReferenceGrant.

---

## 3a. The Cloudflare API token

A cluster whose `networkMode` is `tunnel` — the Cluster XRD's default — reaches
the internet through a Cloudflare Tunnel, and gentian-os asks one token to do
two unrelated jobs.

| Job | Where | Cloudflare permission |
|---|---|---|
| DNS-01 challenges and the proxied CNAMEs tenants resolve through | cert-manager, external-dns, the operator | **Zone → DNS → Edit** on the kernel domain's zone |
| Rewriting the tunnel's ingress rules so each new tenant hostname reaches the gateway | the operator, per tenant | **Account → Cloudflare One Connector: cloudflared → Edit** |

**Both are required on a tunnel cluster.** The second is easy to miss because
nothing else needs it: a token with only the DNS permission installs cleanly,
issues every certificate, and then fails the first time a tenant is deployed —
`TunnelIngressReady=False`, reason `CloudflareTunnelSyncFailed`, message
`cloudflare get tunnel config: [{1001 Not authorized}]`, retrying every few
seconds until the deploy times out. Everything else about that tenant is
healthy, which is what makes it confusing.

**The second permission is not called what the API implies.** Cloudflare folded
tunnels into Cloudflare One and renamed it, so there is no "Cloudflare Tunnel"
entry in the permission list — it is **Cloudflare One Connector: cloudflared**,
and older accounts may still show *Argo Tunnel (Legacy)*, which covers the same
endpoints. Nothing under **Access** or **Zero Trust** grants them; those are the
identity layer in front of an application, not the tunnel's own configuration.

The two permissions have different shapes. DNS rights can be narrowed to a
single zone; tunnel permissions exist only at account level, with no
per-tunnel scoping. So granting both to one token necessarily widens it to the
account, and that token is held by cert-manager and external-dns as well as the
operator. **One token is the supported default** — it is one prompt, one OpenBao
path, one rotation, and the tunnel scope is needed by nearly every cluster since
`tunnel` is the default mode. Where that reach is too broad — a Cloudflare
account carrying tunnels beyond this cluster — set
`cloudflare.tunnelAPITokenSecretRef` in the operator's chart values to a second,
account-scoped token; the operator prefers it and falls back to the DNS token
when it is unset.

### Supplying it

The edge is two questions, and they have different owners:

```
what must a hostname RESOLVE to?   → external-dns.  Every provider, always.
how does traffic REACH a service?  → EdgeIngress.   Provider-specific.
```

**DNS is never provider-specific here.** external-dns writes this cluster's
records for all eight providers in `kernel/platforms.yaml`, from two sources it
is already configured with: `gateway-httproute` for every hostname the operator
routes, and `crd` (`DNSEndpoint`) for records with no HTTP object behind them,
which is how mail publishes. There is no per-provider DNS code in the operator,
and there was: a Cloudflare-only record writer, which meant a Route 53 cluster
had no DNS writer at all and a static-ip cluster had none either. Both failed
silently — names simply never resolved.

What a tunnel actually lacks is not a writer but a **target**. external-dns
publishes what a Gateway resolves to, and a tunnelled Gateway has no address:
traffic arrives through `cloudflared`, not a LoadBalancer. So the ingress
declares what its hostnames must point at, the operator stamps that on the
kernel Gateway, and external-dns does the rest exactly as on a static-ip
cluster:

```
external-dns.alpha.kubernetes.io/target            = <uuid>.cfargotunnel.com
external-dns.alpha.kubernetes.io/cloudflare-proxied = true
```

Proxied is not a preference: `cfargotunnel.com` resolves to nothing a client
could connect to, so an unproxied record answers and then refuses. It is set
per record, on the Gateway — the chart's global `cloudflare.proxied` stays
`false`, because proxying an MX target routes mail through an HTTP edge that
does not carry SMTP.

### The two credentials

| | credential | scope | OpenBao path | env |
|---|---|---|---|---|
| **DNS** — read by external-dns and cert-manager | `acme-dns-cloudflare`, under `dnsProviders.cloudflare` | Zone → Zone → Read **and** Zone → DNS → Edit | `gentian-os/kernel/dns/cloudflare` | `CF_API_TOKEN` |
| **Ingress** — read by the operator | `edge-ingress-cf-tunnel`, under `edgeIngress.cf-tunnel` | Account → Cloudflare One Connector: cloudflared → Edit | `gentian-os/kernel/edge/cf-tunnel` | `CF_TUNNEL_TOKEN` |

**One Cloudflare token may hold both grants, and many do.** Then the same value
is entered twice, once for each. That is deliberate: they are stored and probed
separately, so a cluster that later narrows one of them does not discover the
split at the same moment as the failure. The installer asks for the ingress
token whenever this cluster's ingress is `cf-tunnel`.

`cf-tunnel`, not `tunnel`: the name says whose. Ingress rules, the
`cfargotunnel.com` target and the account-scoped permission are all
Cloudflare's shape. inlets or frp would be their own entry in `edgeIngress`,
not another value of one "tunnel" setting.

Note what the DNS token is **not** used for any more: the operator does not
write records, so that credential belongs to external-dns and cert-manager. The
operator holds only the ingress token.

The zone id and tunnel CNAME are resolved from the token and the running
`cloudflared`, not asked for.

Each token is probed against what it will actually be used for, before either
is written:

- The **DNS** token is walked up to the enclosing zone, then asked to *write* —
  a TXT record created and deleted immediately, under a name of ours that
  resolves to nothing. Reading a zone is `Zone:Read`; writing a record is
  `DNS:Edit`, and a read-scoped token passes every check short of the write
  itself and then fails when external-dns tries to publish.
- The **ingress** token is asked for the tunnel configuration the operator
  rewrites. A DNS-only token does **not** fail the tunnel *list* endpoint
  outright — it comes back `200` with an empty result — so only reading the
  running tunnel's configuration distinguishes "no permission" from "no tunnel
  yet". On a first install, where `cloudflared` is not running to be named,
  that check is inconclusive and says so rather than passing quietly.

Resolving the account for that second probe reads the zone, which is a
DNS-side grant. It is carried over from the DNS probe rather than re-derived,
and `CLOUDFLARE_ACCOUNT_ID` supplies it outright so an ingress token can carry
the account permission and nothing else.

### If a cluster has no tunnel

Set `networkMode: static-ip` on the Cluster claim. There is then no ingress to
program: the LoadBalancer routes by address, external-dns reads that address
off the Gateway and publishes it, and the Gateway carries no target annotation
because none is needed. Only the DNS credential is asked for —
`edge-ingress-cf-tunnel` does not apply to a cluster whose ingress is `none`.

This is the same DNS path a tunnel cluster uses. The difference is one
annotation, not a different mechanism.

---

## 4. Browser Security and Embedding

Gentian Portal embeds tenant applications in iframes. Edge policy enforces
embedding rules through explicit response header policy attached to routes.

Default app policy:

- remove upstream `X-Frame-Options`
- enforce `Content-Security-Policy` with `frame-ancestors 'self'` plus every
  origin the portal answers on — `https://portal.<kernelDomain>` and
  `https://<tenantEffectiveDomain>` — plus `https://*.<tenantEffectiveDomain>`
- a route may replace that list via `gentianos.io/gateway-frame-ancestors`; its
  `portal` token expands through the same `portalOrigins` helper, so a narrowed
  policy cannot fall behind the hosts the portal is actually routed on

Keycloak OIDC broker policy:

- kernel IdP routes include portal origin plus tenant app OIDC origins in
  `frame-ancestors`
- policy is reconciled from tenant/domain state so iframe-based login flows stay
  valid as tenants and apps change

---

## 5. Redirects and URL Control

The **kernel** apex redirects with an HTTPRoute filter:

- Host: `<kernelDomain>`
- Filter: HTTP redirect to `https://portal.<kernelDomain>/`

The **tenant** apex does not redirect — it serves the portal directly, on the
same backends as `portal.<kernelDomain>`. A redirect filter replaces path and
query wholesale, which discarded a login hint arriving on the tenant host, and
it moved the address bar off the tenant's name. One portal deployment answers on
both names; it is not copied per tenant, which would put the portal's Keycloak
admin credentials inside every tenant's blast radius.

Worth knowing: tokens live in `sessionStorage`, which is per origin, so a user
signed in on the tenant host holds a different session from the same user on
`portal.<kernelDomain>`. Keycloak's SSO cookie makes crossing between them
silent, but they are two sessions.

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

The block holds `<kernelDomain>` itself, and a CoreDNS `hosts` entry answers
every query type for its name. In-cluster lookups of the zone apex's SOA and NS
records therefore return empty, so nothing in the cluster can discover the zone's
authoritative nameservers through cluster DNS. cert-manager needs exactly that
to confirm a DNS-01 challenge record has propagated. The installer therefore
runs it with `--dns01-recursive-nameservers-only` against the resolvers in the
Cluster claim's `certificates.dns01RecursiveNameservers` (public resolvers by
default; `cluster` keeps cluster DNS for zones only an internal server knows).
A-05 reconciles those flags on an existing Helm release as well as a new one.
