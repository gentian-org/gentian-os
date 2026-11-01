---
audience: architect, operator, auditor
type: design
---

# Networking

How traffic is admitted inside the cluster: the network plugin, the
NetworkPolicy tiers of a tenant namespace, the kernel's rules, egress, and the
edge that real-time media takes. The edge itself -- Gateways, listeners,
routes, certificates and DNS -- is [routing.md](routing.md); the controls each
rule rests on, with their tests, are [security.md](security.md); the mail
edge is [mail.md](mail.md). The rules are AD-1, AD-6 and AD-9 in
[decisions.md](decisions.md).

## 1. The layers

Each layer answers one question and is enforced by one component. A request
passes them top to bottom; a lower layer never re-answers a higher one.

| # | Layer | Question | Enforced by | Identity it sees |
| --- | --- | --- | --- | --- |
| L0 | TLS, DNS, rate limit | is this traffic well-formed and within budget? | the Envoy listener; a local limit per client address on sign-in posts, to a realm's sign-in pages and to a sign-in sidecar's answer path (`BackendTrafficPolicy`); the publishing proxy's own limits per client address on a published entry ([security.md §2.14](security.md)) | none |
| L1 | Edge session | who is this, in which realm? | Envoy `SecurityPolicy.oidc`: one confidential client per tenant zone in that tenant's realm, the kernel realm for the platform tenant's zone and the kernel UIs ([routing.md §4.1](routing.md)) | the realm's token: `sub`, realm |
| L1′ | Perimeter | which paths, how much, and does a credential pass? | the publishing proxy in the tenant's DMZ, a named enforcement point: it forwards the declared paths and nothing else, holds a request to one reading of its path, a method, a size and a time, removes every identity header, `Cookie` and `Set-Cookie`, and removes `Authorization` unless the entry was approved as `authMode: app`, where the app alone checks it. It verifies no caller | none, or the credential the app issued |
| L2 | Reachability | may this person reach this component at all? | the ext-auth bouncer, asking OpenFGA `can_use` on the app (or the relation the route names) over the stored membership projection (AD-12); cached per `(sub, sid, route)`, evicted on OpenFGA's change log | the same token |
| L3 | App session and authorization | what may they do inside? | the app: the identity headers the bouncer sets, or its own silent single sign-on with its own client and audience and its own cookie, capped by the profile's `sessionMaxAge`. The edge token itself is forwarded only on a route that declares `forwardToken`; a route that declares `exchangeToken` is handed a token exchanged for that app alone (§1.1) | identity headers, or the app's own session |
| L5 | Network | may these two pods talk at all? | NetworkPolicy derived from `requires`, `integrations` and granted privileges (§2–§4); Kyverno for what a pod may be | ServiceAccount, namespace and pod labels |

Two things follow. **The edge decides reachability, the app decides
everything finer**: the Gateway never asks "may Alice edit document 4711", so
its OpenFGA load scales with logins × apps, not with requests. And **a
perimeter route skips L1–L2 by declaration**: its `authMode` is the whole of
its authentication, which is why the field is mandatory and `none` is a word
somebody wrote and somebody else approved.

### 1.1 What a backend is handed

A backend behind the session is told who is asking in the identity headers
(`x-gentian-subject`, `-realm`, `-session`, `-email`, `-name`), set by the
bouncer after it removed what the client sent. It believes them only as far as
it believes that nothing but the Gateway's Envoy pods reach it, which is the
network rule of §2 and nothing in the request.

The session's own token is handed to no app: it is valid at the director and
at every sibling. A route whose entry says `forwardToken` -- the desktop, which
relays to the director -- receives it. A route whose entry says
`exchangeToken` is handed a token of the person issued by the tenant's realm
for that app alone: the bouncer exchanges the session's token (RFC 8693)
through a client of the realm that exists for nothing else,
`gentian-edge-exchange`, which signs nobody in, has no token of its own and is
admitted to the exchange only for a session's token that names it. The bouncer
holds that client's secret for each realm (Secret `bouncer-exchange`); it is
the one platform component below the edge that holds a client secret. One
exchange serves a session for the token's lifetime, and a request for which no
exchanged token can be had is refused, never let through with the session's
token or with none (`internal/bouncer/exchange.go`).

## 2. Tenant namespaces

A tenant's namespace is closed by default and opened by declaration. The
operator writes every policy of the namespace from the tenant's installs,
bindings and grants (`internal/kernel/netpolicy`, `BuildDesired`); nobody
writes one by hand.

| Policy | Selects | Opens |
| --- | --- | --- |
| `tenant-isolation` (the baseline) | every pod of the namespace, an adapter's excepted | Ingress from the edge namespace -- its Envoy pods alone where the kernel's rules are on (§3) -- from the identity provider's namespace, which calls an app's back-channel logout address, and from the operator's namespace, which provisions into running apps. Egress to the cluster's DNS and to the API server. Nothing else in either direction |
| the export Job's policy | the tenant's backup and export Jobs | what a backup of the tenant's stores needs |
| an adapter's policies | an adapter's pods, which the edge, the identity provider and the operator must not reach | a floor of the adapter's own |
| `kernel-access-<app>` | the app's pods | Egress to each store the profile declares, on that store's port, and to the model gateway for `requires.services.llm` ([security.md §2.6](security.md)) |
| the app's in-app policy | the app's pods | the programs of one app reach each other |
| `component-<app>` | the app's pods | the egress rules of `requires.privileges.egress` that were granted by name on the install. A declared privilege nobody granted opens nothing |
| a contract's policies | consumer and provider of an `IntegrationBinding` | the path between two apps that share a contract, as far as the `AppGrant` allows |
| the cache policies | the apps that declare a Memcached cache | Egress to the tenant's Memcached instance on its port, and ingress at it. An app that declares Redis uses the shared cache store through `kernel-access-<app>` instead |

Network rules only ever add: a pod the baseline selects cannot be closed again
by another policy, which is why an adapter's pods are left out of the baseline
instead of being narrowed afterwards.

What follows for an app that believes the identity headers: another app of the
same tenant reaches it only where an integration between the two was granted,
and a pod of another tenant never. The rule holds only on a cluster whose
network plugin enforces NetworkPolicy; the installer refuses a cluster whose
plugin does not (§3).

## 3. The kernel's rules

Every kernel namespace refuses an ingress that an inventory of callers does
not list. The inventory is one file (`internal/kernel/kernelnet/inventory.yaml`)
-- per namespace, every pod set, every port, and for each port who is admitted
-- and the policies are generated from it, so no rule exists without a line
that says who it is for. OpenFGA admits the programs that ask it; the
bouncer's authorization port admits the Envoy proxies alone; the vault and its
seal admit their named clients; a tenant's pod reaches the director, the
usher, the custodian and the registrar and no other port of the control
namespace. The table is [security.md §2.13](security.md).

The rules are on by default: `KERNEL_NETWORK_POLICIES`, read by the installer,
is true when unset, after a check that the network plugin enforces
NetworkPolicy. The installer applies them with the namespaces, before a pod
exists in any of them. The operator is told the same switch, and the two rules
of its own that rest on the same fact follow it: the tenant baseline's edge
rule, which then admits the Envoy pods instead of the whole edge namespace,
and the publishing proxy's ingress rule, which then admits the Envoy pods and
nothing else.

**Egress is open in the kernel and system namespaces.** Argo CD, Crossplane
and its providers, cert-manager, external-dns, Keycloak, the Envoy proxies and
the operator reach the internet and the API server in ways that differ per
cluster. A pod there is stopped at another namespace's door by that
namespace's rules and on the way out by nothing. The exceptions are the edges
that face the internet: the mail proxy in `system-mail-dmz` and the TURN edge
in `system-turn-dmz` (§7), whose egress is restricted to what they relay to.

Plain `networking.k8s.io/v1` NetworkPolicy throughout, naming no address
block, so the rules mean the same on every plugin that enforces them. What a
NetworkPolicy cannot say -- the API server, the kubelet, the install host --
and what stands in its place is in [security.md §2.13](security.md).

## 4. The shared stores: one rule with two sides

A tenant's namespace denies egress by default and an app's
`kernel-access-<app>` policy opens the store its profile declares, on that
store's port. Each store -- PostgreSQL, MariaDB, Redis, SeaweedFS -- carries
the other side, a `store-ingress` policy on its server's pods: tenant
namespaces, the pods of its own namespace (the operator's provisioning and
destroy Jobs, and the steps of a backup and a restore that work on that
store), and by name the operator, for SeaweedFS the backup and restore pods of
the database and identity namespaces and, for PostgreSQL's instance manager,
CloudNativePG's operator. The server's side keeps out everything that is not a
tenant namespace or one of those; the tenant's side keeps out the apps that
declared no such store. Who reaches which port, and what stays closed:
[security.md §2.7](security.md).

The kernel's own PostgreSQL carries the same kind of policy: Keycloak's and
OpenFGA's namespaces, the registrar and the operator by label, tenant
namespaces for the desktop of the tenant that adopts the kernel realm, and its
own namespace. So do the two mail servers in `system-mail`, neither of which
faces the internet: 25, 587 and 993 are taken by a TCP proxy in
`system-mail-dmz` that holds no mail, no user, no certificate and no key, and
passes each connection on with the client's address in a PROXY-protocol
header, on ports of the servers that require the header and admit the proxy
alone ([security.md §2.8](security.md), [mail.md](mail.md)).

The model gateway is the same rule as a store. An app that declares
`requires.services.llm` is opened `system-llm` on the gateway's port by its
`kernel-access-<app>` policy; the gateway's own policy admits tenant
namespaces, the operator and the Gateway's Envoy pods (its console) on that
port, and the gateway's database, cache and mock model server each admit the
gateway alone ([security.md §2.9](security.md)). An app that did not declare
the gateway has no path to it and no key for it.

## 5. Three levels of exposure

Nobody writes a route. What is reachable without a session is declared at
three levels with different owners, cardinalities and lifetimes, and the
operator derives listeners, certificates, routes and DMZ proxies from them.
Keeping the levels apart is what makes self-service and control compatible.

| Level | Object | Declared by | How many | Lives |
| --- | --- | --- | --- | --- |
| The profile's entry | a `surface: perimeter` entry: name, host label, paths, deny paths, `authMode` | the app's author, in the catalog | a few per app | with the profile |
| The tenant's enablement | *this* entry of *this* install is published: owner, publish date, `reviewAt`, an optional `expiresAt` ([routing.md §7](routing.md)). The `authMode` is not restated: it is the entry's, and an approval cannot weaken it | the tenant's perimeter approver, `can_expose`, through the director | a handful per tenant | until withdrawn or expired; regular review is the control |
| The app-level object | a share link, a guest meeting, a public form | any user of the app, inside the app | thousands | as the app's own policy says |

A share never needs an administrator: the approver published *share links*
once, and the app issues them. What the platform never does is learn about
individual shares by routing: they are capabilities inside a declared surface,
not routes. So the platform's inventory of what can be reached anonymously --
hosts, path prefixes, modes -- is the enablements and the routes derived from
them, complete by construction, since nothing is routed without one. Which
objects an app has made public is the app's to say.

## 6. Certificates and DNS

**Every wildcard is issued by DNS-01.** ACME issues a wildcard by DNS-01 only.
The kernel's `*.<kernelDomain>` and each tenant's `*.<effectiveDomain>` -- a
domain of the tenant's own included -- are `Certificate`s against the DNS-01
`ClusterIssuer` `letsencrypt-dns01-<provider>` (`tenant_edge_tls.go`, default
`letsencrypt-dns01-cloudflare`). The zone of a tenant served on a domain of its
own is listed on the Cluster claim (`certificates.dnsZones`) so that the DNS
credential cert-manager and external-dns hold covers it ([routing.md §3a](routing.md)).
The HTTP-01 issuer is emitted beside the DNS-01 one, and the perimeter
Gateway's port-80 listener answers its challenges at
`/.well-known/acme-challenge/*`: with the realm endpoints it is one of exactly
two kernel-owned surfaces without a session.

Two different things travel under the word "wildcard". The certificate's
DNS-01 challenge is a **TXT** record at `_acme-challenge.<domain>`, written and
removed by cert-manager. A wildcard **address** record is separate and
unnecessary: external-dns publishes one record per routed hostname, so every
host resolves without one. Only a deployment that chooses to rely on a
wildcard address record has to care that a DNS wildcard matches a single
label.

**external-dns owns records; the operator does not write them.** On a
static-ip cluster its `gateway-httproute` source reads hostnames off
`HTTPRoute`s and takes the target from the Gateway's status address, and the
operator publishes nothing. On a tunneled cluster the Gateway never gets an
address, so the operator publishes a `DNSEndpoint` naming the hostnames and
the tunnel's target and external-dns reconciles that
(`edge_dnsendpoint.go`): not the operator writing DNS, the operator declaring
intent for the component that owns it. Mail and the TURN edge publish their
load balancers' addresses the same way.

## 7. Real-time media

A conference has two parts, and the platform treats them differently. Getting
media through NAT is TURN's job, and every conferencing app needs the same
thing from it, so it is one system service. Forwarding media between the
participants of a call is a selective forwarding unit's, and every app brings
its own, so the media server belongs to the app. Media never passes the
Gateway: WebRTC goes to the TURN edge over UDP, and the controls there are the
credential's lifetime, the app's room membership and the short list of peers
the edge relays to.

### 7.1 The TURN edge

- **Where.** `system-turn-dmz`, tier `system-dmz`, with no inner part: TURN
  holds no data, so the whole service is edge (AD-9), the way the mail edge is
  in `system-mail-dmz`. The namespace is composed on demand: the first
  component that declares `requires.services.turn` puts the engine `turn` into
  the operator's set (`system_engines.go`), and it stays until a person purges
  it.
- **What runs.** STUNner's daemon, `stunnerd`, standalone, from a
  configuration the operator writes (`turn_edge.go`). There is no STUNner
  operator, no resource definition of STUNner's and no permission outside the
  namespace. The configuration is the one thing that names the server
  software.
- **How it is reached.** A LoadBalancer Service of its own, not a listener of
  the Gateway's, for the reason the mail edge has one: UDP 3478, TCP 3478, and
  TURN over TLS on TCP 443 for networks that let nothing else out.
  `externalTrafficPolicy: Local` keeps the client's address, which STUN
  reports back to the client. One name for the cluster, `turn.<kernelDomain>`;
  the operator publishes it from the Service's address as a `DNSEndpoint`, and
  the name's certificate is issued by DNS-01 into the namespace. Apart from
  the shared secret, that key is the only secret the namespace holds.
- **Where it relays to.** The relay is headless: a relayed connection may
  reach the edge's own pods, which is how two participants behind NAT reach
  each other through one public port, and nothing else in the cluster. Two
  things enforce that: the daemon's list of peers, which is the headless
  Service of its own replicas, and the namespace's policy, whose egress admits
  the edge's own pods and DNS and nothing of the cluster or the internet.
- **Credentials.** Time-limited TURN credentials (the TURN REST scheme): the
  username is `<expiry>:<id>`, the password is the HMAC-SHA1 of the username
  under a shared secret, and the app mints one per session. One secret for
  the cluster, minted once into the vault and delivered to the edge and to
  every component that declares the requirement.
- **Tunnel mode.** A tunnel carries no UDP and no TURN. A component that
  declares the requirement on a tunnel cluster is not installed; it waits
  under `SystemEngineNotReady` with the reason named.

### 7.2 The contract

`requires.services.turn` names no implementation. A component that declares
it receives the Secret `turn-credentials-<component>` in its namespace
(`turn_credentials.go`):

| Key | Value |
| --- | --- |
| `TURN_SERVER` | `turn.<kernelDomain>` |
| `TURN_PORT`, `TURNS_PORT` | `3478`, and `443` for TURN over TLS |
| `TURN_URIS` | `turn:…:3478?transport=udp`, `turn:…:3478?transport=tcp` and `turns:…:443?transport=tcp`, comma-separated |
| `STUN_URI` | `stun:turn.<kernelDomain>:3478` |
| `TURN_SECRET` | the shared secret |

A chart mounts the Secret by that name, so the secret is never a release
value. The Secret is removed once nothing the component runs declares the
requirement. A requirement an add-on declares is served to the component the
add-on activates into, because that is where the add-on runs: the Secret is
named after the base. Nextcloud Talk's add-on declares it as `turn: {}`, and
the base mounts `turn-credentials-nextcloud-base-ce` while the add-on is
selected. A credential minted in the strict form above is accepted by STUNner,
coturn and the other TURN servers that implement the scheme, so replacing
STUNner changes the operator's edge and nothing a profile says. What it does
change is the network: a server that needs a range of relay ports needs that
range open on the load balancer, or host networking.
