---
audience: architect, operator, auditor
type: concept
---

# The Edge

Where traffic crosses the cluster's boundary, in both directions, and who
decides each crossing. This page is the map. The ways in are detailed in
[routing.md](routing.md), the rules between pods in
[networking.md](networking.md), and mail in [mail.md](mail.md).

## 1. The edge at a glance

| Direction | Carrier | Where | Carries | Decided by |
| --- | --- | --- | --- | --- |
| In | the Gateways `authenticated` and `perimeter`, one Envoy fleet behind the cluster's address | `kernel-edge` | HTTP(S) to apps, consoles and the identity provider | a profile's entry; for a host without a session, the tenant's perimeter approver |
| In, from inside only | the Gateway `inside`, an Envoy fleet with a cluster-internal address | `kernel-edge` | HTTP(S) to inside entries | a profile's entry and the tenant's perimeter approver |
| In | the mail edge | `system-mail-dmz` | SMTP, submission, IMAP | the Cluster claim |
| In | the TURN edge | `system-turn-dmz` | real-time media | a profile's `requires.services.turn` |
| Out | the Gateway `outgoing`, an Envoy fleet of its own | `kernel-edge` | TLS and HTTP to hosts named in a grant | a profile's request and the tenant's perimeter approver, a trusted domain, or the platform for a profile it ships |
| Out | a rule by address | the caller's own NetworkPolicy | what names no host or is not yet converted: mail to other servers, a database protocol, addresses chosen at run time | the same approver, shown as an address |
| Out | the kernel's own policies | each kernel namespace | what the kernel's components fetch | the kernel's inventory |

Envoy Gateway runs the three fleets. They share a controller and a namespace
and nothing else. Each has a GatewayClass of its own (`gentian-envoy`,
`gentian-envoy-inside`, `gentian-envoy-out`), and every rule that admits a
fleet names its class.

## 2. Ways in

One external address or one tunnel serves every public hostname. The
Gateways `authenticated` and `perimeter` are two policy domains on one fleet.
`authenticated` puts a session and the access check in front of a route.
`perimeter` serves the hosts that take no session, through a publishing proxy
in the tenant's DMZ ([routing.md §11](routing.md)).

An **inside entry** is a perimeter entry that the cluster's own pods reach and
the internet does not. Its route is on the Gateway `inside`, whose Service has
only a cluster-internal address. No public DNS record names it, and the
cluster's DNS maps the name to that address. A caller needs an egress grant
for the host, and the app checks every caller's credential.

Mail and real-time media do not pass the HTTP fleets. Each has a load
balancer of its own and relays only to its own servers
([mail.md](mail.md), [networking.md §7](networking.md)).

## 3. Ways out

### 3.1 The rule

A tenant's pod reaches three things without asking: the cluster's DNS, the
API server, and what its declarations opened inside the cluster. A
connection to anything outside takes the outgoing gateway or a rule by
address. Any other connection is refused by the pod's own NetworkPolicy.

A kernel namespace either reaches the internet (`egress: open` in the
inventory, for components that fetch from Git, registries, the certificate
authority or the DNS provider) or reaches only the cluster and the API server
(`egress: cluster`).

The gateway is the default for a tenant's component. A rule by address is
for a protocol the gateway cannot carry, and the approver sees it as such: an
address and a port, with no host to read.

### 3.2 The outgoing gateway

A component connects to an outside host as it always does. It is given no
proxy address and reads no setting.

- **A listener per host.** Each granted host has a listener of its own on the
  `outgoing` Gateway, on a port of its own, and a Service that maps the
  host's port to it. The listener passes a connection on only when the TLS
  server name is that host. The fleet resolves the host itself and connects
  to the address it found, so a caller cannot steer a connection by the name
  it claims.
- **The caller's host table.** A pod of a component that was granted a host
  finds the host's name in its own host table, pointing at that host's
  Service. The operator writes the entry into the component's pod templates
  when it renders the chart; the dispatcher writes it for a run. No other pod
  sees it, and the cluster's DNS is not changed.
- **Two network rules.** The caller's `component-<app>` policy opens the
  fleet's pods on the ports of its granted hosts. The fleet's own policy
  admits, per port, the pods of the components granted that host.
- **A host the cluster serves itself is not carried.** Inside the cluster
  such a name resolves to the edge's private address, which the fleet may
  not reach. The operator reads those names from the cluster's DNS and
  leaves them off the gateway. The caller resolves the name to the edge, or
  to the inside Gateway for an inside entry, and its policy opens that
  fleet's pods on the port the host's port forwards to.
- **TLS is not opened.** It runs from the component to the host outside. The
  gateway sees the host and the volume, never the content.

A pod with no grant has no entry and no open port. The name resolves to the
host's real address, and the pod's policy refuses the connection.

### 3.3 Requests and approval

A profile requests hosts by name, with a reason an approver can judge:

```yaml
requires:
  privileges:
    egress:
      - name: microsoft-graph
        hosts: [graph.microsoft.com, login.microsoftonline.com]
        reason: "Reads and writes the calendars of people who connected their account."
```

- **A host is one name.** Port 443 unless the entry says otherwise. A pattern
  is not a host.
- **One answer per request.** The hosts of one request are answered together
  by the tenant's perimeter approver. A change to them waits for a new
  answer.
- **Hosts named at approval.** A request marked `hostsAtApproval` names no
  hosts of its own: its installation chooses them, such as the operator's
  own ERP. The approver names them, and the grant opens those and no others.
- **Trusted domains.** `PlatformSecurityPolicy.spec.trustedDomains` lists the
  domains the cluster trusts. A request whose every host is in a trusted
  domain, or under one, counts as granted. The cluster's administrator can
  delegate a list of its own to a tenant (`Tenant.spec.trustedDomains`).
  A domain taken off a list makes the requests it answered pending again.
- **Profiles the platform ships.** Their requests by host are answered by the
  platform, with the release that ships them.
- **The liaison.** The component that keeps people's sign-ins at outside
  services asks for nothing. It reaches the sign-in hosts the tenant's
  bridges name, each once that bridge's own requests are answered.

### 3.4 The record

The gateway writes one JSON line per connection to its log: host, port,
time, bytes in each direction, how it ended, and the caller's pod address.
Calls to hosts the cluster serves itself, and rules by address, are not in
it.

## 4. Names

| Asked by | Name | Answered |
| --- | --- | --- |
| any pod | a Service of the cluster | by the cluster's DNS |
| any pod | a public hostname of this cluster | by the cluster's DNS with the edge's address inside the cluster ([routing.md §10](routing.md)) |
| any pod | an inside entry's host | by the cluster's DNS with the inside Gateway's address; nothing outside the cluster answers it |
| a pod with a grant | a granted host outside | by the pod's own host table, with the host's Service on the outgoing fleet |
| any pod | any other name | by the cluster's DNS with the real address, which the pod's policy does not open |

A tenant's pod sends DNS to the cluster's DNS and to no other address
([networking.md §2](networking.md)).

## 5. What the edge does not do

- **It carries no UDP out.** A client that prefers HTTP/3 falls back to TCP.
- **It holds no credential.** What a component presents to an outside
  service, for itself or for a person, is the component's or the liaison's
  ([bridges.md](bridges.md)).
- **It does not see inside TLS.** The record is host and volume.

## 6. Failure domains

| What fails | Effect |
| --- | --- |
| The inbound fleet | no app, console or sign-in is reachable from outside; calls out continue |
| The inside fleet | inside entries are unreachable; everything else continues |
| The outgoing fleet | no tenant component reaches a granted host outside; apps, sign-in, calls inside the cluster and the kernel's own fetches continue; the installer restores the fleet |
| Envoy Gateway's controller | the fleets keep serving what they were last told; new routes and new grants wait |
| The mail edge or the TURN edge | that protocol alone |
