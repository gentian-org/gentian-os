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

The apex carries a redirect to the tenant's console (§5), and it is kept
reachable by keeping it out of the tenant certificate.

Two tenants are not like the others.

The **platform tenant** has a listener like any tenant's, for
`*.platform.<kernelDomain>`, with a wildcard certificate of its own issued the
way every tenant's is. Its administration console is
`admin.platform.<kernelDomain>`, two labels under the cluster's domain, which
the cluster's own certificate (`<kernelDomain>` and `*.<kernelDomain>`) does
not name. Its desktop is `platform.<kernelDomain>` itself -- the zone's own
name, not `console.` under it -- which that certificate does name, so the
desktop's routes attach to `https-wildcard` and there is no apex redirect.

The **user tenant of a single-tenancy cluster** is on the kernel domain itself
and gets no listener or certificate of its own: `*.<kernelDomain>` beside the
catch-all would be the more specific match for every kernel host and route
them nowhere, so its routes attach to `https-wildcard` instead.

The rule a route's listener follows is one line: a host that is the cluster's
domain or exactly one label under it is on `https-wildcard`; anything deeper is
on its tenant's listener (`edgeZone.listenerFor`).

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

- `effectiveDomain = ` the custom domain a `TenantDomain` binds (copied to the
  Tenant's `status.domain`), when there is one
- otherwise:
  - `<tenant>.<kernelDomain>` -- `platform.<kernelDomain>` for the platform
    tenant, under either tenancy mode
  - `<kernelDomain>` for the tenant named `user` under `tenancyMode: single`,
    and for nobody else

A host is `<subDomain>.<effectiveDomain>`. The one exception is the platform
tenant's desktop, whose `console` entry answers on `platform.<kernelDomain>`
itself (`exposureHostIn`).

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

### 4.1 The front door: the session first, then the access check

A route behind a session carries one `SecurityPolicy` with two parts: `oidc`,
the zone's sign-in session, kept by Envoy's OAuth2 filter, and `extAuth`, the
bouncer, which asks OpenFGA whether this person may reach this host.

Envoy Gateway's own order runs `ext_authz` before the OAuth2 filter. This
platform reverses it, with `filterOrder` on the edge `EnvoyProxy`
(`kernel/manifests/gateway/chart`), and installer step A-05 does not report
the edge installed without it. The reason is that the bouncer must judge a
request by a token somebody has validated, and in Envoy Gateway's order there
is none yet: the session is cookies, the OAuth2 filter encrypts them, and a
bouncer that ran first could only wave through what it could not read.

With the OAuth2 filter first, per request on a session route:

| The request | What the OAuth2 filter does | What the bouncer sees |
|---|---|---|
| Valid session | Removes the client's `Authorization` header, sets it to the session's access token | The token. Verifies it, asks the relation, sets `x-gentian-*` |
| Session whose access token has run out, refresh token present | Fetches new tokens from the realm, then as above with the new token; the new cookies go out with the response | The new token |
| No session, or the refresh failed | Redirects the browser to the realm | Nothing: the request does not reach it |
| `/oauth2/callback` | Completes the code flow and redirects | Nothing |
| `/oauth2/logout` | Signs out (§4.2) | Nothing |

The bouncer refuses any request on a session route that reaches it without a
token it can verify, whatever its path. Nothing in the operator's policies
lets a request past the OAuth2 filter without a session — no
`passThroughAuthHeader`, no `denyRedirect`, no CORS or health path — and if
something did, the answer would be `401`. A client cannot present a bearer of
its own on these routes: `forwardAccessToken` makes the filter drop the
incoming `Authorization` header before it writes its own. Command-line
clients do not come through the edge at all (`kubectl gentian` reaches the
director through the API server).

One route keeps the caller's own `Authorization` header: Keycloak's
administration console on `id.<kernelDomain>`, whose page calls the Admin REST
API with a token of its own. There `forwardAccessToken` is off, and the
filter hands the bouncer the session's ID token in `x-gentian-id-token`
(`forwardIDToken`), a header it clears of anything the client sent before it
sets it. The bouncer verifies it as an ID token issued to the zone's client
and removes the header before the backend.

Identity headers (`x-gentian-subject`, `-realm`, `-session`, `-email`,
`-name`) are set by the bouncer on every request it allows, replacing
client-sent ones, and no request reaches a backend on a session route any
other way. The edge's access token goes on to a backend only where its
exposure says `forwardToken`, and then in the `Authorization` header.

**What a backend receives of the session** is those headers, and that bearer
where it is forwarded. It does not receive the session's cookies. The OAuth2
filter decrypts the token cookies into the request it passes on, so without
more the backend would find the person's access token and ID token in its
`Cookie` header. On every request it allows on a session route the bouncer
therefore rewrites that header to the same cookies without the edge's, or
removes it when no other cookie is left:

- the two the zone's policy names, `gentian-<zone>-access` and
  `gentian-<zone>-id`, by exact name;
- the ones the filter names itself, by the fixed word each begins with:
  `RefreshToken-`, `OauthHMAC-`, `OauthExpires-`, `OauthNonce-`,
  `CodeVerifier-`, and `AccessToken-` and `IdToken-`, which the filter would
  use for the two token cookies if a policy did not name them. What follows
  the word is a hash of the policy's UID and, for the two that carry a sign-in
  in progress, an id of that sign-in; it changes when a policy is made again,
  so it is not stated.

The names reach the bouncer in the route table the operator writes
(`sessionCookies`, `sessionCookiePrefixes`); it guesses at none. An `ext_authz`
answer can replace a request header or remove it, not edit it, which is why
the whole header is rewritten. The app's own cookies are passed on as the
bouncer was shown them, in the same order. Two things follow from where the
bouncer stands. The OAuth2 filter has already rebuilt the header by then, one
`name=value` per cookie name, in no particular order. And the bouncer is shown
header values as UTF-8, so a cookie value that is not valid UTF-8 goes on with
the offending bytes replaced. The rewrite touches only the request to the
backend: the filter has read its cookies before the bouncer is asked, and
`Set-Cookie` on the way back is the filter's.

A bearer route has no session and its `Cookie` header is not touched.

Routes without a session policy are unchanged and never ask the bouncer:
perimeter surfaces (a tenant's DMZ, the concierge), the identity provider's
realm endpoints on `id.<kernelDomain>`, and the redirects. What
`id.<kernelDomain>` refuses is a route with `authorization: Deny`.

### 4.2 Sign-in, sign-out and the session cookies

- **Cookies.** Per host, not per zone (no `cookieDomain`): `HttpOnly`,
  `Secure`, `SameSite=Lax`, token cookies encrypted by the filter. One sign-in
  at the realm covers every host of the zone; each host gets its own cookies
  on a silent round trip. They are the edge's alone: the bouncer takes them
  out of the request before the backend (§4.1).
- **Sign-out** is `GET /oauth2/logout` on any host of the zone. The filter
  deletes that host's cookies and redirects to the realm's end-session
  endpoint (read from the issuer's discovery document) with `id_token_hint`,
  `client_id` and `post_logout_redirect_uri=https://<host>/`. Keycloak ends
  the realm session without asking and returns the browser to the host's
  front page, which is behind the session and so shows the realm's sign-in.
  `https://<host>/` is registered on the zone's client for every host of the
  zone (`validPostLogoutRedirectUris`, `tenant-default.yaml`).
- `/oauth2/sign-out` is the older path. The bouncer answers it with a redirect
  to `/oauth2/logout`.
- **What ends a session.** Ending the realm session ends it everywhere: other
  hosts' cookies cannot be refreshed, so each stops within one access-token
  lifetime (five minutes in a tenant realm). A right withdrawn in OpenFGA is
  refused within about two seconds, the bouncer's poll of the change log; its
  cached allows live five minutes at most. There is no back-channel logout.

### 4.3 Embedding

The desktop opens components in frames, so every component route carries a
frame policy, set at the edge and not by the component:

- remove upstream `X-Frame-Options`
- set `Content-Security-Policy: frame-ancestors 'self'` plus, by name, the
  desktop of the component's own tenant and the component's own other hosts
  (a file store and the editor it embeds)

| Component of | May be framed by |
|---|---|
| A tenant, tenancy `multi` | `console.<tenant>.<kernelDomain>` (or `console.<its own domain>`) |
| The user tenant, tenancy `single` | `console.<kernelDomain>` |
| The platform tenant (`admin.platform.<kernelDomain>`, …) | `platform.<kernelDomain>` |
| The kernel's consoles (`argocd.`, `headlamp.`, Keycloak's administration) | `platform.<kernelDomain>` |

No wildcard, and the platform's desktop is not a framer of any tenant's
component: platform administrators do not open tenants' apps.

Framing also depends on the session cookie, which is `SameSite=Lax`: a framed
page gets its session only when the framing page is on the same site (the
same registrable domain). So:

- A desktop and its apps under one registrable domain work —
  `console.acme.example.org` framing `cloud.acme.example.org`, or a tenant's
  own domain throughout.
- A frame across sites does not: the framed app has no cookie, is sent to
  sign in, and the sign-in cannot complete in the frame.
- A cluster domain that is itself a public suffix makes every host a site of
  its own, and nothing can be framed. Use a domain below one.

Keycloak's realm endpoints on `id.<kernelDomain>` carry a separate policy,
reconciled from tenant and domain state, naming the hosts whose pages embed
its session frames.

### 4.4 Versions

Envoy Gateway and its chart are pinned in `versions.yaml`. Envoy Gateway 1.9
is tested on Kubernetes 1.33 to 1.36, which makes **Kubernetes 1.33 the
minimum** for this release; pre-flight refuses an older cluster. The chart
installs the Gateway API v1.6.1 CRDs (experimental channel). A-05 applies the
chart's CRDs before the chart, because Helm does not upgrade CRDs
([operations.md §7.5](operations.md)).

On listeners whose certificates overlap — the kernel wildcard serves several —
Envoy Gateway offers HTTP/1.1 only, so that a browser cannot reuse one HTTP/2
connection for a host with a different policy.

---

## 5. Redirects and URL Control

The **kernel** apex -- the cluster's bare domain -- is the first thing anybody
typing the cluster's address meets, before any session. The concierge, a
component of the platform tenant, is published on it from that tenant's DMZ
(`tenant-platform-dmz`) on a listener of the perimeter Gateway, under either
tenancy mode. What a visitor gets there is the mode's
([iam.md §1.1a](iam.md)), and the kernel's routes are what differ
(`kernelFrontDoor` in `kernel_gateway_routes.go`):

| Host | `multi` | `single` |
|---|---|---|
| `<kernelDomain>/` | the concierge's form: asks for an e-mail address and sends the browser to its workspace's desktop | `302` to the user tenant's desktop, `https://console.<kernelDomain>/` (`console.<custom domain>` when a `TenantDomain` binds one) |
| `<kernelDomain>/branding/` | the cluster's brand, served by the concierge | the same |
| `www.<kernelDomain>` | `302` to the bare domain | `302` to the user tenant's desktop |
| `console.<kernelDomain>` | `302` to the bare domain (`kernel-console-redirect`) | the user tenant's desktop itself, routed by its component |
| `platform.<kernelDomain>` | the platform admin's desktop | the same |

On a single-tenancy cluster the redirect of the bare domain is two routes for
the two listeners the name can arrive on. `kernel-apex-perimeter-redirect`
attaches to the perimeter's listener for the bare domain and matches only `/`
and `/sign-in`: both are more specific than the concierge's whole-host route
on the same listener, so a person is sent on, and `/branding/` -- which every
desktop loads the cluster's brand from -- is still the concierge's.
`kernel-apex-redirect` is the same redirect on `https-wildcard`, for a cluster
where nothing is published on the bare domain. Both exist only once the user
tenant is Ready; until then the bare domain shows the concierge's form. A
browser sent to a name that is not published yet remembers that it is missing.

Nothing is forwarded by the page. The `concierge-lookup` ConfigMap the
operator keeps in the platform tenant's namespace holds one file per custom
domain and nothing else; there is no `_single.json`.

A **tenant** apex redirects to `https://console.<effectiveDomain>/`, path and
query kept. Two tenants have none: the platform tenant, whose apex is its
desktop, and the user tenant of a single-tenancy cluster, whose apex is the
cluster's.

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
