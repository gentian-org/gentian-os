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
- **Gateways**: two, both the kernel's, in the edge namespace (`kernel-edge`):
  `authenticated`, for every host behind a session, and `perimeter`, for the
  hosts that take none. They terminate TLS for every external hostname —
  kernel hosts and tenant app hosts alike — and are merged into one Envoy
  deployment
- **HTTPRoute**: one route object per exposed app endpoint, living in the
  namespace that owns its backend
- **Envoy policy CRDs**: security, traffic, and header policy attached to
  Gateway/HTTPRoute resources

---

## 2. Resource Topology

### 2.1 The cluster Gateway

The edge is two Gateways in the edge namespace, `authenticated` and
`perimeter`, programmed into one Envoy deployment that owns the cluster's
external address (`ensureEdgeGateways`). Every externally reachable hostname
is served by it. Where this document says "the Gateway" it means that one
deployment; a route names the Gateway of the two whose listener it pins to.

One deployment, rather than one per tenant, is a requirement rather than a
simplification. A Gateway maps to an Envoy deployment with its own Service, and
each such Service claims the cluster's external address; two of them contend for
one address, and a client's TLS handshake succeeds or fails depending on which
one holds it at that moment.

Its listeners are:

| Gateway | Listener | Port | Hostname | Certificate |
| --- | --- | --- | --- | --- |
| `authenticated` | `https-wildcard` | 443 | none | kernel wildcard, `wildcard-tls` |
| `authenticated` | `https-tenant-<name>-wildcard` | 443 | `*.${effectiveDomain}` | `tenant-<name>-wildcard-tls` |
| `perimeter` | `http-redirect` | 80 | none | none — the ACME HTTP-01 answer, and a redirect to `https` for everything else |
| `perimeter` | `https-id` | 443 | `id.<kernelDomain>` | kernel wildcard |
| `perimeter` | `perimeter-<host, dots as dashes>` | 443 | one published host, exactly | the tenant's wildcard; the kernel's for a host that is the cluster's domain or one label under it |

A perimeter listener exists for each host a tenant has an approved perimeter
entry on (§7), and for the bare domain. It names the host exactly: merged
Gateways may not repeat a port and hostname, and the exact name is the more
specific match, so that one host leaves the session and the rest of the
zone stays behind it.

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

The apex carries a redirect to the tenant's desktop (§5), and it is kept
reachable by keeping it out of the tenant certificate.

Two tenants are not like the others.

The **platform tenant** has a listener like any tenant's, for
`*.platform.<kernelDomain>`, with a wildcard certificate of its own issued the
way every tenant's is. Its administration console is
`admin.platform.<kernelDomain>`, two labels under the cluster's domain, which
the cluster's own certificate (`<kernelDomain>` and `*.<kernelDomain>`) does
not name. Its desktop is `platform.<kernelDomain>` itself -- the zone's own
name, not `desktop.` under it -- which that certificate does name, so the
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
- Gateway listener: `https-tenant-<name>-wildcard` on the `authenticated` Gateway
- ReferenceGrant: permits the Gateway to read that certificate across the
  namespace boundary
- HTTPRoutes: in the tenant namespace, attached to the tenant's listener

The certificate stays under tenant ownership and is never copied into the kernel
namespace; the ReferenceGrant is the only thing that crosses the boundary, and it
is granted per tenant, for one Secret, in one direction.

### 2.3 Route model

For each app endpoint, Gentian OS creates an HTTPRoute with:

- `parentRefs` naming the `authenticated` Gateway in the edge namespace (the
  `perimeter` one for a published entry), with `sectionName` pinning it to
  one listener
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
tenant's desktop, whose `desktop` entry answers on `platform.<kernelDomain>`
itself (`exposureHostIn`).

| | `multi` | `single` |
| --- | --- | --- |
| A user tenant's hosts | `<label>.<tenant>.<kernelDomain>` | `<label>.<kernelDomain>`, for the one tenant `user` |
| The platform tenant's hosts | `platform.<kernelDomain>`, `<label>.platform.<kernelDomain>` | the same |
| With a domain of its own | `<label>.<custom domain>` | the same |

Where an entry answers is decided in one package, `internal/addresses`, which
the operator (to write the route and the listener) and the director (to show
an approver the address before it is approved) both import.

**A domain of its own.** `kubectl gentian tenants domain <name> [<domain> |
--remove]` binds a tenant to a custom domain or puts it back, in one commit by
the director (`PUT`/`DELETE /v1/clusters/{c}/tenants/{t}/domain`,
`can_configure` on the cluster). The director refuses a name that is not a
hostname, a domain that is the cluster's or under it, and one another tenant
holds (`422`). Binding is for the user tenants of a multi-tenancy cluster:
the director refuses a bind for any tenant when the tenancy mode is `single`
(the user tenant would leave the cluster's own addresses and give up the
main address), and for the platform tenant under either mode (`422`, with
the reason; nothing is committed). Removing a binding is refused to nobody,
so a tenant bound before this was asked can be put back. The operator still
follows a `TenantDomain` that reaches the cluster another way. Nobody checks
the DNS record or the certificate.

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

### 3.1 Address names an app cannot take

The label of a host is the profile's to state (`subDomain`, or the
component's name), and a catalogue's profile is not written by the platform.
Two routes that claim one host are resolved by the Gateway by age, with no
error anywhere. So the names the platform depends on are refused to
everything but the platform's own component for each. One list
(`internal/hostnames`), two tiers.

**Kept in every tenant**, on every cluster, under the tenant's domain
whatever it is -- `<tenant>.<kernelDomain>`, `<kernelDomain>` for the user
tenant of a single-tenancy cluster, or a custom domain:

<!-- reserved-names:platform:begin -->
| Label | Who may hold it | Why it is kept |
| --- | --- | --- |
| `desktop` | `desktop` | the tenant's desktop |
| `admin` | `admin-console` | the tenant's administration console |
| `store` | `app-store` | the App Store app |
| `console` | nothing | the desktop's former address, which people may still type |
| `platform` | nothing | reads as the platform administrator's desktop, platform.<cluster> |
| `id` | nothing | reads as the identity provider, id.<cluster> |
| `auth` | nothing | a name a sign-in page would have |
| `login` | nothing | a name a sign-in page would have |
| `signin` | nothing | a name a sign-in page would have |
| `sign-in` | nothing | a name a sign-in page would have |
| `sso` | nothing | a name a sign-in page would have |
| `account` | nothing | a name a page for one's account and password would have |
| `accounts` | nothing | a name a page for one's account and password would have |
<!-- reserved-names:platform:end -->

**The kernel's own hosts**, refused where a tenant's domain is the cluster's
-- the user tenant of a single-tenancy cluster -- to every component, the
platform's included:

<!-- reserved-names:kernel:begin -->
| Label | What answers there |
| --- | --- |
| `argocd` | the GitOps console |
| `corp` | a name the tunnel ingress publishes for the cluster |
| `headlamp` | the cluster console |
| `id` | the identity provider |
| `imap` | the cluster's mail host for reading mail |
| `llm` | the model gateway |
| `mail` | the cluster's mail host |
| `mail-egress` | the address the cluster's mail leaves from |
| `platform` | the platform administrator's desktop, and the platform tenant's zone below it |
| `www` | an alias of the cluster's main address |
<!-- reserved-names:kernel:end -->

In a tenant with a domain of its own these are not the kernel's hosts, and
`mail`, `www` or `llm` there is an app's business. The two that would help
deceive people under any domain (`id`, `platform`) are in the first table.

- **Matching.** The label itself, in any case, and anything below it
  (`x.admin`). Nothing else: there is no look-alike matching (`desktop1`,
  `desk-top`). An apex entry has no label and its own rule (§5). An add-on's
  entry served by its base has the base's host.
- **Who the platform's own component is.** The profile named in the table,
  and only when no catalogue brought it: the director records an origin and
  a bundle on every profile it materialises
  (`gentianos.io/catalogue-origin`, `gentianos.io/profile-bundle`), refuses a
  catalogue's profile that states either itself, and refuses to materialise
  any catalogue's profile under a name the platform ships. `trustTier:
  platform` is not asked: it is a field any catalogue can write. And the
  label has to be that component's own: the desktop holds `desktop`, not
  `admin`.
- **Where it is refused.** The director refuses the install, the add-on
  selection or the import with `422` and the reason, before anything is
  committed. The operator refuses again where the component would be
  published: the Component is held whole with the condition `HostReserved`
  and a warning event, before any policy, release, route or listener is
  written for it. Both ask `hostnames.Check`.
- **What is not on the list.** `operations`: the Operations Console is a
  catalogue component, and reserving its label would refuse it. `www` and
  `api` in a tenant with its own domain, and every name that is an app's
  business (`mail`, `chat`, `files`, `docs`, `wiki`).

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
something did, the answer would be `401`. The one path of an app that takes no
session is not an exception written into a policy: it is a route of its own
that no policy names (below). A client cannot present a bearer of
its own on these routes: `forwardAccessToken` makes the filter drop the
incoming `Authorization` header before it writes its own. That includes an
app whose own page sends the app's API a bearer token of the app's: on an
ordinary entry the token does not arrive, and the app has to go by the
identity headers or by a session of its own. Command-line
clients do not come through the edge at all (`kubectl gentian` reaches the
director through the API server).

Two kinds of route keep the caller's own `Authorization` header. One is
Keycloak's administration console on `id.<kernelDomain>`, whose page calls the
Admin REST API with a token of its own. The other is an app's gateway entry
that declares `clientAuthorization: app` and that the tenant's perimeter
approver has approved (§7). On both, `forwardAccessToken` is off, so the
filter leaves the header as the browser sent it, and it hands the bouncer the
session's ID token in `x-gentian-id-token` (`forwardIDToken`), a header it
clears of anything the client sent before it sets it. The bouncer verifies
it as an ID token issued to the zone's client, asks the route's relation of
that person, and removes the ID-token header and the session's cookies
before the backend. It does not read the `Authorization` header and does not
remove it.

Keeping the header is not accepting it. The session is required on these
routes exactly as on every other: the filter still wants its own cookies on
every request and sends a request without them to sign in, whatever the
request carries in `Authorization`, because the one setting that would let a
bearer stand in for the session (`passThroughAuthHeader`) is set on no
policy. A request that reached the bouncer without the session's ID token
would be answered `401`. No token of the platform's reaches the app on such a
route.

A component has one policy for all its hosts. When one of them keeps the
header, the filter stops writing the edge's token into the header on all of
them; the bouncer's table then says per host whether the header is the app's
to keep (`keepClientToken`) or is still removed (`idTokenSession`). An entry
that forwards the edge's token (`forwardToken`) or asks for an exchanged one
(`exchangeToken`) and one that keeps the app's own cannot be in one profile:
the schema refuses it.

For up to about a minute after such an approval is given or withdrawn, the
policy and the bouncer's table can disagree, because they reach the Gateway
and the bouncer separately. In that window requests to that component are
refused with `401`; none is admitted that would not have been.

Identity headers (`x-gentian-subject`, `-realm`, `-session`, `-email`,
`-name`) are set by the bouncer on every request it allows, replacing
client-sent ones, and no request reaches a backend on a session route any
other way. The edge's access token goes on to a backend only where its
exposure says `forwardToken`, and then in the `Authorization` header. The
identity headers are not signed: a backend believes them because only the
Gateway's Envoy pods can reach it, which is a network rule
([security.md §2.13](security.md)) and nothing in the request.

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
realm endpoints on `id.<kernelDomain>`, and the redirects. Nobody checks the
caller of a perimeter surface at the edge: the proxy forwards for anyone, and
on an entry of `authMode: app` the app checks the credential it is passed
(§7). What
`id.<kernelDomain>` refuses is a route with `authorization: Deny`.

**The sign-in sidecar's answer path.** An app whose profile declares
`requires.services.identity.sidecar` has two more paths on its host
([iam.md §1.11](iam.md)):

| Path | Route | Session | Reaches |
|---|---|---|---|
| `/sso/login`, exactly | a rule of the app's own route | yes, and the bouncer's question | the sidecar, with the identity headers |
| each of the profile's `entryPaths`, exactly, GET | a rule of the app's own route | yes | nothing: a redirect to `/sso/login` |
| `/sso/acs`, exactly, POST | `<component>-<entry>-sso-acs`, on the same host and listener | **none** | the sidecar, with the identity headers removed |

`/sso/acs` has no session because the realm posts its answer there from
`id.<kernelDomain>`. For a tenant on a domain of its own that is another site,
and a browser sends `SameSite=Lax` cookies with no cross-site POST: behind the
session the OAuth2 filter would answer with a redirect to sign in and the
answer would be lost. The Gateway prefers the exact match over the app's `/`
prefix, so nothing else of the host is on that route; no `SecurityPolicy` names
it and the bouncer's table has no line for it. What guards it is the sidecar
([security.md §2.12](security.md)). The route is written and removed by the
component reconciler with the sidecar.

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
  cached allows live five minutes at most. The edge has no back-channel
  logout. An app is told only if its own OIDC client declares a
  `backchannelLogoutUrl`, which the realm then calls; otherwise a session it keeps of its own (a sign-in sidecar's
  lasts up to an hour, [iam.md §1.11](iam.md)) ends when the app ends it.
  The app is not reachable meanwhile, because the front door comes first.

### 4.3 Embedding

The desktop opens components in frames, so every component route carries a
frame policy, set at the edge and not by the component:

- remove upstream `X-Frame-Options`
- set `Content-Security-Policy: frame-ancestors 'self'` plus, by name, the
  desktop of the component's own tenant and the component's own other hosts
  (a file store and the editor it embeds)

| Component of | May be framed by |
|---|---|
| A tenant, tenancy `multi` | `desktop.<tenant>.<kernelDomain>` (or `desktop.<its own domain>`) |
| The user tenant, tenancy `single` | `desktop.<kernelDomain>` |
| The platform tenant (`admin.platform.<kernelDomain>`, …) | `platform.<kernelDomain>` |
| The kernel's consoles (`argocd.`, `headlamp.`, Keycloak's administration) | `platform.<kernelDomain>` |

No wildcard, and the platform's desktop is not a framer of any tenant's
component: platform administrators do not open tenants' apps.

Framing also depends on the session cookie, which is `SameSite=Lax`: a framed
page gets its session only when the framing page is on the same site (the
same registrable domain). So:

- A desktop and its apps under one registrable domain work —
  `desktop.acme.example.org` framing `cloud.acme.example.org`, or a tenant's
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
| `<kernelDomain>/` | the concierge's form: asks for an e-mail address and sends the browser to its workspace's desktop | `302` to the user tenant's desktop, `https://desktop.<kernelDomain>/` (`desktop.<custom domain>` when a `TenantDomain` binds one) |
| `<kernelDomain>/branding/` | the cluster's brand, served by the concierge | the same |
| `www.<kernelDomain>` | `302` to the bare domain | `302` to the user tenant's desktop |
| `desktop.<kernelDomain>` | `302` to the bare domain (`kernel-desktop-redirect`) | the user tenant's desktop itself, routed by its component |
| `platform.<kernelDomain>` | the platform admin's desktop | the same |

**A website on the main address (single-tenancy only).** The user tenant may
put a public website on the bare domain. The main address then behaves like
this:

| Address | no website | website approved, its proxy not up yet | website serving |
|---|---|---|---|
| `<kernelDomain>/` | `302` to the desktop | `302` to the desktop | the website |
| `<kernelDomain>/<any other path>` | the concierge (`404`) | the website's route (answers once the proxy is up) | the website |
| `<kernelDomain>/sign-in` and below | `302` to the desktop | the same | the same |
| `<kernelDomain>/branding/` | the concierge | the same | the same |
| `<kernelDomain>/.well-known/acme-challenge/`, `/.well-known/pki-validation/` | the concierge (`404`) | the same | the same |
| `www.<kernelDomain>` | `302` to the desktop | `302` to the desktop | `302` to the bare domain, path kept |
| `desktop.`, `admin.`, `platform.`, `id.<kernelDomain>` | unchanged | unchanged | unchanged |

The bare domain is the website's one name; `www` redirects to it, as `www`
redirects everywhere on this platform. Sign-in does not move: the desktop is
at `desktop.<kernelDomain>`, and `https://<kernelDomain>/sign-in` always leads
there, whatever the website does. A multi-tenancy cluster is not affected: its
main address stays the sign-in form.

*Who may.* Three things must all be true (`main_address.go`):

1. The profile's entry is a perimeter one with `apex: true`. The author says
   the surface is a website for a bare domain.
2. The tenant's perimeter approver published it **and** said `apex: true` on
   the request (`can_expose`, the same relation as every published surface).
   The entry is in the exposure registry with its owner, publish date and
   review date, marked `apex: true`.
3. The cluster's mode is `single`, the tenant is the user tenant on the
   cluster's own domain and is Ready, and no other surface holds the address.

*The rule, and the acknowledgement.* A website goes on the main address only
if the organisation itself controls every script it runs: no third-party
scripts, no pages uploaded by users. A script on the bare domain can set
cookies that the browser also sends to `desktop.`, `admin.`, `platform.` and
`id.<kernelDomain>`; it cannot read a session, but it can stop people from
signing in and can sign a person in to an account its author chose
([security.md §2.10](security.md)). The platform cannot check what a site
loads, so the approver is told and says so: the director refuses a request
with `apex: true` (`400`, with the warning) until it also carries
`"acknowledgeMainAddressRule": true`, on the first publication and on every
review. The registry entry records who acknowledged and when
(`apexAcknowledgedBy`, `apexAcknowledgedAt`). The operator publishes by
`apex` alone, so an entry approved before the acknowledgement was asked
keeps serving and is asked at its next review.

*What the approver sees.* The director's read of a tenant's registry
(`GET /v1/tenants/{t}/exposures`, `can_view`) also lists every perimeter
entry an installed app declares, approved or not, with the address it is
published at, its paths, its `authMode` and its kind, and with them every
entry behind sign-in that asks to keep the app's own `Authorization` header
(§7). The address is resolved by the
function the operator publishes it with (`internal/addresses`). An approval
of an app that is not installed in the tenant, or of an entry its profile
does not declare for the perimeter, is refused (`422`) and nothing is
committed. So is an approval whose `apex` is not the entry's: `apex: true`
for an entry the profile does not declare for the main address, or no `apex`
for one it does. The operator would publish nothing for either. An entry
recorded that way earlier stays, is listed without an address, can be
withdrawn, and is renewed only with the setting the entry has.

One surface holds the main address at a time. The director refuses a second
request with `409` and names the holder. If two entries reach the cluster
anyway, the one published first keeps the address. The component's
`MainAddress` condition says which case applies (`Published`, `NotRequested`,
`MultiTenancy`, `NotTheUserTenant`, `OwnDomain`, `Expired`, `ReservedPath`,
`TenantNotReady`, `HeldByAnother`, `NotAMainAddressEntry`).

*The platform's paths.* The website can never answer these:

| Path | Why the platform keeps it |
|---|---|
| `/branding/` | the consoles load the cluster's brand from here |
| `/sign-in` and everything below | must always lead to sign-in |
| `/.well-known/acme-challenge/` | answering it proves control of the domain to a certificate authority; a website that could answer it could get a certificate for the cluster's address |
| `/.well-known/pki-validation/` | the same, for authorities that use this path |

Everything else under `/.well-known/` is the website's (a Matrix delegation,
`security.txt`). `/oauth2/` is not on this host: no session lives here.

They are kept twice. At the Gateway each reserved path is a longer match than
the website's route, so it wins (Envoy Gateway orders Exact, then regular
expression, then prefix, longer first). And the website's own proxy answers
`404` for them, so even a moment of mixed routes cannot hand them over. A
profile that declares a path inside a reserved one is not published at all
(`ReservedPath`).

*How the routes change.* Three writers ask one function, `mainAddressHolder`,
so they agree:

- The website's component writes its route in `tenant-user-dmz`, on the
  perimeter Gateway's listener for the bare domain. No `ReferenceGrant` is
  needed: the route's backend is its own proxy in the same namespace.
- The concierge's route steps back from the whole host to the platform's
  paths (`/branding/` and the two `/.well-known/` paths). Otherwise two routes
  would match `/` equally and their age would decide.
- The kernel's redirect drops the front page and keeps `/sign-in`, once the
  website's proxy has a pod that answers. Until then the front page still
  leads to the desktop, so the main address never shows an error while a
  website is coming up.

Withdrawing the exposure (or its expiry) undoes all three on the next
reconcile after Argo CD applies the commit: usually within a minute or two.
The main address is then what it was.

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

A **tenant** apex redirects to `https://desktop.<effectiveDomain>/`, path and
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
3. **NetworkPolicies** allowing ingress to tenant workloads from the edge
   namespace -- from its Envoy pods alone where the kernel's network rules are
   on ([security.md §2.13](security.md)) -- and egress to the edge namespace
   for an app that declared `identity`, so in-cluster hairpin DNS overrides
   for kernel hostnames reach the programmed edge routes
   ([security.md §2.6](security.md)).
4. **Identity and token exchange controls** at app/service layers via
   IntegrationBindings and OIDC policy.

No tenant route may target backends in another tenant namespace unless explicitly
allowed by policy resources.

---

## 7. App Catalogue Contract

A profile declares each entry point under `expose[]` (`ExposureSpec`): a
name, a `surface` (`gateway`, behind the zone's session, or `perimeter`, with
none), a mandatory `authMode` (`oidc` on the gateway; `none` or `app` on the
perimeter), the host label (`subDomain`, else the
component's name; `apex` for the bare domain, §5), `paths` and `denyPaths`,
and the backend Service and port. It has no field for a timeout, a body size
or a rate: those are the platform's.

The platform renders this into HTTPRoute and policy resources, so profiles
stay controller-agnostic and do not encode implementation-specific
annotation keys.

**A perimeter entry is published only once it is approved.** Declaring it is
a request. The tenant's perimeter approver (`can_expose`) approves it: a
member of the tenant's group `gentian:tenant:<t>:perimeter`, which nothing
creates and the tenant's admins do not hold by default, so a tenant's admin
makes the group and joins it before anything can be approved. The director commits it to the tenant's registry in git with its owner, its
publish date and its review date; the operator then gives it a proxy in the
tenant's DMZ, a listener for exactly its host and a route (§2.1).

| | Command line | Director |
| --- | --- | --- |
| What the tenant's apps ask for, and what is approved | `kubectl gentian exposures requests --tenant <t>` | `GET /v1/tenants/{t}/exposures`, `entries[]`: state (`requested`, `approved`, `reviewDue`, `expired`, `unmatched`), `kind` and `kindLabel`, address, paths, `authMode`, and in the director's words what approving allows (`access`) and the limit that applies (`rateLimit`) |
| What is approved | `kubectl gentian exposures list --tenant <t>` | the same read: `live`, `reviewDue`, `expired`, each entry with its `kind`; `kinds` gives the words for each |
| Approve, or review again | `kubectl gentian exposures approve <app-instance> <entry> --tenant <t> [--expires <date>] [--reason <text>]` | `PUT /v1/tenants/{t}/exposures/{inst}/{name}` |
| Withdraw | `kubectl gentian exposures withdraw <app-instance> <entry> --tenant <t>` | `DELETE` on the same path |

The administration console shows the same list per app, with Approve, Review
and Withdraw for whoever may. An approval of an app that is not installed, of
an entry its profile does not declare for the perimeter, or with a
main-address setting that is not the entry's, is refused (`422`) and nothing
is committed (§5). The read is from git: it says where an entry will answer,
not whether it answers yet.

**Three kinds of entry take an approval**, and the registry records which
one was approved (`kind`):

| Kind | Declared as | Once approved |
| --- | --- | --- |
| `public` | `surface: perimeter`, `authMode: none` | The declared paths are published for anyone. No credential reaches the app |
| `publicAppCredential` | `surface: perimeter`, `authMode: app` | The declared paths are published for anyone, and the caller's `Authorization` header is passed to the app as sent. The app alone checks it; the platform does not know who calls |
| `signInAppAuthorization` | `surface: gateway`, `authMode: oidc`, `clientAuthorization: app` | Nothing is published. Behind sign-in, the `Authorization` header is left as the app's own page sent it (§4.1) |

An approval holds for the kind it was given for. If the app's catalogue
entry later declares another kind, the platform does nothing for the entry
— a public address is taken down, a kept header is removed again — and the
read lists it as `requested` with the earlier approval beside it, until it
is approved as what it is now. An approval request may name the kind the
approver was shown (`"kind"` in the body, which the command line and the
console send); the director refuses it with `409` if the entry declares
another. Before the third kind is approved the entry is served like any
other behind sign-in, and the Component's `ClientAuthorization` condition
says that the app's own calls will fail until then.

**What a published entry is held to.** The proxy forwards the declared paths
and nothing else, lets a path through only if it has one reading, removes
every identity header and `Cookie` on the way in and `Set-Cookie` on the way
out, removes `Authorization` unless the entry was approved as
`publicAppCredential`, and limits each client address: 10 MB a body,
20 requests a second with 200 more at once, 100 at a time — and 5 a second,
50 more at once, 20 at a time on an entry that passes the credential,
because every request to it may be a guess at a password. The numbers are
the cluster administrator's to change and no profile's. Sign-in posts are
limited at the Gateway, 60 a minute for each client address. The full list,
and whose address is counted: [security.md §2.14](security.md).

**It checks no caller.** On an entry of `authMode: app` the credential in
`Authorization` reaches the app and the app checks it; the approver is told
that the platform does not know or check who calls, and that an app password
or token of a person removed from the tenant keeps working until the app
itself revokes it. A client that needs the app's cookies on a public address
does not work: cookies pass in neither direction. `jwt`, `bearer`, `basic`
and `signature` named a check at the edge that nothing makes; the schema
refuses them on a perimeter entry until they are built.

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
names resolve to the ClusterIP of the edge's Envoy Service, which is found by
the GatewayClass it serves and not by a namespace's name.

A `mail.<kernelDomain>` line already in the block is left as it is. Nothing
in this repository writes one: the cluster's mail names are published as DNS
records for the mail edge's load balancer (see [mail.md](mail.md)).

The gateway-platform controller updates the hairpin block when the Envoy
Service or routing mode changes and rolls CoreDNS. Tenant NetworkPolicies are
refreshed when the edge Service changes so egress to the edge stays allowed.

The block holds `<kernelDomain>` itself, and a CoreDNS `hosts` entry answers
every query type for its name. In-cluster lookups of the zone apex's SOA and NS
records therefore return empty, so nothing in the cluster can discover the zone's
authoritative nameservers through cluster DNS. cert-manager needs exactly that
to confirm a DNS-01 challenge record has propagated. The installer therefore
runs it with `--dns01-recursive-nameservers-only` against the resolvers in the
Cluster claim's `certificates.dns01RecursiveNameservers` (public resolvers by
default; `cluster` keeps cluster DNS for zones only an internal server knows).
A-05 reconciles those flags on an existing Helm release as well as a new one.
