---
audience: app-author, operator
type: guide
---

# Using the edge: ways in and ways out

Every way traffic enters or leaves the cluster: which one to choose, what a
profile declares, who approves it, and how to check it. The why is in
[edge.md](../concepts/edge.md); the routes in
[routing.md §7](../concepts/routing.md) and §11; the network rules in
[networking.md §2](../concepts/networking.md) and §3; bridges in
[bridges.md](../concepts/bridges.md).

Placeholders: `<t>` is the tenant, `<app>` the install name (the
Component's name in `tenant-<t>`), `<entry>` an `expose[]` name, `<name>` an
`egress[]` name.

## 1. Decision table

| I want to ... | Way | The profile declares | Who approves | Where it shows |
| --- | --- | --- | --- | --- |
| let people reach the app after sign-in | Gateway `authenticated` | `expose[]`: `surface: gateway`, `authMode: oidc` | nobody: always on | an `HTTPRoute` in `tenant-<t>` |
| publish paths to anyone, no sign-in | Gateway `perimeter`, a proxy in `tenant-<t>-dmz` | `surface: perimeter`, `authMode: none` | the tenant's perimeter approver (kind `public`) | `kubectl gentian exposures requests --tenant <t>` |
| publish paths whose callers present the app's own credential | as above | `surface: perimeter`, `authMode: app` (`signInCookies` for the app's browser sign-in) | the tenant's perimeter approver (kind `publicAppCredential`, `publicAppSignIn`) | as above |
| let pods of this cluster, and nothing outside, call the app | Gateway `inside`, the same DMZ proxy | `surface: inside`, `authMode: app`, `subDomain` | the tenant's perimeter approver (kind `insideAppCredential`); each caller also needs an egress grant for the host | as above, marked `insideOnly` |
| let a program call the app with a token of the tenant's realm | Gateway `authenticated`, host of its own | `surface: gateway`, `authMode: bearer`, `subDomain`, `clientRedirectURIs`; the profile is `trustTier: platform` | the tenant's perimeter approver (kind `tokenEntry`) | as above |
| reach named hosts outside | outgoing gateway | `requires.privileges.egress[]` with `hosts` | the tenant's perimeter approver, a trusted domain, or the platform for a profile it ships | the Component's `status.outgoingHosts` |
| reach hosts the installation chooses | outgoing gateway | `egress[]` with `hostsAtApproval: true` | the tenant's perimeter approver, who names the hosts | as above |
| reach destinations chosen at run time, or speak a protocol that names no host (SMTP, a database protocol) | a rule by address in `component-<app>` | `egress[]` with `rule` | the tenant's perimeter approver, or the platform for a profile it ships | the `component-<app>` NetworkPolicy |
| call an outside tool server through an agent bridge | the bridge's grant, given to its callers | the bridge's own `egress[]`; the bridge is packaged as `api` with a `baseUrl` | whoever answers the bridge's request | the caller's `status.outgoingHosts` |
| keep people's sign-ins at an outside service | the liaison | the bridge's `outside.signIn.oauth.issuer` | nobody further: it follows the bridge's answered requests | the liaison's `status.outgoingHosts` |
| resolve names, call the API server | the tenant baseline `tenant-isolation` | nothing | nobody: every tenant pod has it | the `tenant-isolation` NetworkPolicy |

Everything else leaving a tenant's pod is refused by its own NetworkPolicy.

## 2. Ways in

### 2.1 Behind sign-in (`surface: gateway`)

```yaml
spec:
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      backend: {service: myapp, port: 8080}
```

- **Approval.** None. The entry is on as soon as the app runs.
- **Created.** An `HTTPRoute` on the Gateway `authenticated` in `kernel-edge`
  at `<subDomain or component name>.<tenant's domain>`, with the zone's
  session and the bouncer's `can_use` check in front of it.
- **The app's own `Authorization` header.** Add
  `clientAuthorization: app`. That is a request (kind
  `signInAppAuthorization`); until it is approved the header is removed.

### 2.2 Public, no sign-in (`surface: perimeter`, `authMode: none`)

```yaml
spec:
  expose:
    - name: website
      surface: perimeter
      authMode: none
      subDomain: www
      paths: [/, /shop]
      denyPaths: [/admin]
      backend: {service: myapp, port: 8080}
```

`apex: true` (no `subDomain`, `authMode: none` only) asks for the bare name
instead.

### 2.3 Public, app's own credential (`surface: perimeter`, `authMode: app`)

```yaml
spec:
  expose:
    - name: api
      surface: perimeter
      authMode: app
      subDomain: api
      paths: [/api/]
      backend: {service: myapp, port: 8080}
```

The caller's `Authorization` header passes to the app as sent; the app checks
it, the platform does not. Every other header that names a person, and every
cookie, is removed. For an app that signs a browser in with a cookie of its
own, add `signInCookies: {names: [...], paths: [...]}` (kind
`publicAppSignIn`).

### 2.4 Inside the cluster only (`surface: inside`)

```yaml
spec:
  expose:
    - name: internal-api
      surface: inside
      authMode: app          # the only mode allowed
      subDomain: api-int     # required
      paths: [/v1/]
      backend: {service: myapp, port: 8080}
```

No `apex`, no alias. A caller declares the host as egress (§3.1):

```yaml
requires:
  privileges:
    egress:
      - name: internal-api
        hosts: [api-int.acme.example.org]
        reason: "Sends orders to the tenant's ERP through its inside entry."
```

The grant opens the inside fleet's pods to the caller, not the outgoing
gateway (§4).

### 2.5 Token entry (`surface: gateway`, `authMode: bearer`)

```yaml
spec:
  trustTier: platform
  expose:
    - name: tokens
      surface: gateway
      authMode: bearer
      subDomain: mcp
      clientRedirectURIs: [http://127.0.0.1/callback]
      backend: {service: myapp, port: 8080}
```

No `forwardToken`, `exchangeToken` or `tile`. The token is one of the
tenant's realm, made out to this host.

### 2.6 Approving an entry

Whoever holds `can_expose` on the tenant approves: the tenant's perimeter
approver, the cluster's perimeter approver, or the cluster's administrator in
a tenant the cluster operates. In the administration console: the
**Perimeter** page. On the command line:

```bash
kubectl gentian exposures requests --tenant <t>        # every entry, its state, kind, address, paths
kubectl gentian exposures approve <app> <entry> --tenant <t> --expires 2027-06-30 --reason "Public shop"
kubectl gentian exposures list --tenant <t>            # what is approved: live, reviewDue, expired
kubectl gentian exposures withdraw <app> <entry> --tenant <t>
```

| Flag | Meaning |
| --- | --- |
| `--expires <date>` | the approval ends then; a bare `YYYY-MM-DD` is the end of that day, UTC. Without it the approval stays until withdrawn |
| `--reason <text>` | an optional comment recorded with the approval |
| `--yes` | confirm without typing `<app>/<entry>`; needed when standard input is not a terminal |
| `--acknowledge-main-address-rule` | required for an entry on the cluster's main address |

Approving again is a review and replaces the expiry and the reason. Every
approval has a review date a year out at the latest. The approval is
committed to the tenant's `spec.exposures` in git (`exposures.yaml`) and
copied to the Component's `spec.exposures`. An alias directly under the
cluster's domain is the cluster administrator's:
`kubectl gentian exposures alias <app> <entry> --tenant <t> --host <label>.<cluster domain>`
([routing.md §7](../concepts/routing.md)).

### 2.7 What an approved perimeter or inside entry creates

| Object | Namespace | Name |
| --- | --- | --- |
| publishing proxy: Deployment (container `proxy`), ConfigMap, Service, HTTPRoute | `tenant-<t>-dmz` | `perimeter-<app>-<entry>` (a hash when longer than 63 characters), label `app.kubernetes.io/name: perimeter-proxy` |
| the proxy's NetworkPolicies | `tenant-<t>-dmz` | `perimeter-<app>-<entry>-egress`, `-ingress` |
| a listener for exactly the host | `kernel-edge` | on Gateway `perimeter` (`perimeter-…`), or on Gateway `inside` (`inside-…`) for an inside entry |
| DNS | — | public entry: the record follows from the route (external-dns), or the tenant makes it for a domain the platform does not manage. Inside entry: no public record (`external-dns.alpha.kubernetes.io/controller: none`); the CoreDNS hairpin maps the host to the inside Service's ClusterIP |

The Gateway `inside` (class `gentian-envoy-inside`, Service `ClusterIP`)
exists while at least one inside entry is approved. Installer step A-05
installs the class: `./install.sh --only A-05`.

### 2.8 Checking a way in

```bash
kubectl gentian exposures requests --tenant <t>
kubectl -n tenant-<t>-dmz get deploy,svc,httproute -l app.kubernetes.io/name=perimeter-proxy
kubectl -n kernel-edge get gateway perimeter inside -o yaml     # listeners and their status
curl -sI https://<host>/<path>
```

## 3. Ways out

### 3.1 The three forms

Each entry of `requires.privileges.egress` has a `name`, a `reason` (at least
ten characters, read by the approver) and exactly one of `hosts`,
`hostsAtApproval: true` or `rule`. A profile has at most one request with
`hosts`.

**Named hosts.**

```yaml
requires:
  privileges:
    egress:
      - name: microsoft-graph
        hosts: [graph.microsoft.com, login.microsoftonline.com, files.example.org:8443]
        reason: "Reads and writes the calendars of people who connected their account."
```

One name per host, lower case, port 443 unless stated; a pattern is not a
host. The connection is TLS that opens with the host's name. A name ending
in `.svc`, `.local`, `.internal`, `.localhost`, `.home.arpa` or `.lan` is
refused (`EgressHostRefused`).

**Hosts the installation chooses.**

```yaml
      - name: erp
        hostsAtApproval: true
        reason: "Synchronizes invoices with the ERP this organization runs."
```

The approver names the hosts, at most 64. No trusted domain and no platform
release answers this form, and `apps install --approve-egress` does not
approve it.

**A rule by address.**

```yaml
      - name: smtp-relay
        rule:
          to:
            - ipBlock: {cidr: 203.0.113.25/32}
          ports:
            - {protocol: TCP, port: 587}
        reason: "Hands outgoing mail to the organization's own relay."
```

A `NetworkPolicyEgressRule`. It goes into the component's own NetworkPolicy,
not through the outgoing gateway, and is not in the gateway's log.

### 3.2 Who answers

| Answer | Covers | How |
| --- | --- | --- |
| the tenant's perimeter approver (`can_approve_privilege` on the tenant) | every form | console **Perimeter** page, or `kubectl gentian apps approve` (§3.3) |
| a trusted domain | a `hosts` request whose every host is the domain or under it | `PlatformSecurityPolicy` `spec.trustedDomains`, or the tenant's own list (§3.4) |
| the platform | `hosts` and `rule` requests of a profile the platform ships (not from any catalog) | the release that ships it |

### 3.3 Approving, declining, withdrawing

```bash
kubectl gentian apps approve <app> --tenant <t>                     # what the app waits for (read-only)
kubectl gentian apps approve <app> egress/<name> --tenant <t> [--reason "<comment>"] [--expires <RFC 3339>]
kubectl gentian apps approve <app> egress/<name> --tenant <t> --host erp.example.org --host files.example.org:8443
kubectl gentian apps decline <app> egress/<name> --tenant <t> --reason "<why, at least ten characters>"
kubectl gentian apps withdraw <app> egress/<name> --tenant <t>
kubectl gentian approvals list                                       # everything pending, every tenant
```

- `--host` is required for a `hostsAtApproval` request and refused for any
  other privilege. Each is `name` or `name:port`.
- `--reason` is an optional comment on an approval; on a decline it is
  required.
- A decline commits nothing: the request stays pending, and the app shows who
  declined and why.
- An approval is committed to the tenant's `spec.privileges` in git
  (`privileges.yaml`), with the approver, time, reason, expiry and, for
  `hostsAtApproval`, `hosts`. The operator copies it to the Component's
  `spec.privileges`.
- `apps install <app> --tenant <t> --approve-egress` approves the build's
  `hosts` and `rule` requests with the install, for the tenant's perimeter
  approver.

### 3.4 Trusted domains

| List | Edited by | How |
| --- | --- | --- |
| the cluster's: `PlatformSecurityPolicy` `default`, `spec.trustedDomains` | the cluster's administrator | by hand in `clusters/<cluster>/kernel/claims/platform-security-policy.yaml` of the deployments repository; the director carries the field across its own writes |
| the tenant's: `Tenant` `spec.trustedDomains.domains` | the tenant's perimeter approver, while `spec.trustedDomains.delegated` is true | console **Perimeter** → **Trusted domains**, or the commands below |

```bash
kubectl gentian tenants delegate-trusted-domains <t> on       # the cluster's administrator
kubectl gentian trusted-domains list --tenant <t>
kubectl gentian trusted-domains add example.org --tenant <t>  # example.org and every name under it
kubectl gentian trusted-domains remove example.org --tenant <t>
kubectl gentian tenants delegate-trusted-domains <t> off      # the list stays and answers nothing
```

A domain covers whole labels: `example.org` covers `api.example.org`, not
`notexample.org`. Removing a domain makes every request it answered pending
again.

### 3.5 What an approval sets in motion

For `hosts` and `hostsAtApproval`:

1. The Component lists the hosts in `status.outgoingHosts`.
2. In `kernel-edge`, each host gets a listener on a Gateway `outgoing-<n>`
   (class `gentian-envoy-out`, EnvoyProxy `gentian-outgoing`, ports
   20000–27999, 64 per Gateway), a `TLSRoute` matching the host's name, and
   a Service `out-<hash>` that maps the host's port to the listener. Every
   object carries the label `gentianos.io/outgoing` and the annotation
   `gentianos.io/outgoing-host-name`.
3. Until the listener exists the Component reads `OutgoingGatewayPending`.
4. The operator writes a `hostAliases` entry (host → the Service's
   ClusterIP) into the component's pod templates; the dispatcher does the
   same for a run. The pods roll.
5. The `component-<app>` NetworkPolicy opens the outgoing fleet's pods on
   the host's listener port; the host's own policy `out-<hash>` in
   `kernel-edge` admits the component's pods on that port. The fleet's policy
   `outgoing-fleet` limits what the fleet itself reaches: DNS, Envoy Gateway,
   and public addresses on the granted hosts' ports.

For `rule`: the rule is added to `component-<app>`.

On withdrawal or expiry, or when a trusted domain goes, the request is pending
again and the Component reads `PrivilegesPending`. A host no Component lists
loses its listener and Service.

## 4. Choosing hosts or a rule

| The destination is ... | Declare |
| --- | --- |
| known by name when the profile is written | `hosts` |
| chosen by the installation (its own ERP, bucket, mail server) | `hostsAtApproval: true` |
| chosen at run time, or reached by a protocol that names no host | `rule` |

- **A host this cluster serves itself** (a public host of the cluster or an
  inside entry) is declared under `hosts` like any other. It is not carried
  by the outgoing gateway and not listed in `status.outgoingHosts`: the pod
  resolves it through the CoreDNS hairpin to the edge's or the inside
  fleet's Service, and `component-<app>` opens that fleet's pods on the port
  the host's port forwards to.
- **A `hostsAtApproval` grant opens only the hosts it names**, so a grant
  given without hosts leaves the request pending until it is approved with
  `--host`.

## 5. Checking and troubleshooting

All commands below are read-only.

| Symptom | Run | Look for |
| --- | --- | --- |
| the app does not start | `kubectl -n tenant-<t> get component <app> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'` | reason `PrivilegesPending` (message: `waiting for approval of egress/<name>`), `EgressHostRefused` (a host that is not outside), `OutgoingGatewayPending` (the listener is missing; on a cluster without class `gentian-envoy-out`, run A-05) |
| which requests wait | `kubectl -n tenant-<t> get component <app> -o jsonpath='{.status.pendingPrivileges}'` or `kubectl gentian apps approve <app> --tenant <t>` | `egress/<name>` entries |
| which hosts are granted | `kubectl -n tenant-<t> get component <app> -o jsonpath='{.status.outgoingHosts}'` | the host; absent for a host the cluster serves itself |
| the pod resolves the host's real address | `kubectl -n tenant-<t> get pod <pod> -o jsonpath='{.spec.hostAliases}'` | the host, at the ClusterIP of `out-<hash>` in `kernel-edge` |
| the listener exists | `kubectl -n kernel-edge get svc,tlsroute -l gentianos.io/outgoing -o custom-columns=NAME:.metadata.name,HOST:.metadata.annotations.gentianos\.io/outgoing-host-name` | the host |
| the connection is refused at the pod | `kubectl -n tenant-<t> get networkpolicy component-<app> -o yaml` | an egress rule to pods with `gateway.envoyproxy.io/owning-gatewayclass: gentian-envoy-out` on the listener port; for `rule`, the address and port |
| the connection reaches the gateway and fails | `kubectl -n kernel-edge logs -l gateway.envoyproxy.io/owning-gatewayclass=gentian-envoy-out -c envoy --prefix` | one JSON line per connection (below) |
| an entry does not answer | `kubectl -n kernel-edge logs -l gateway.envoyproxy.io/owning-gatewayclass=gentian-envoy -c envoy --prefix`, then `kubectl -n tenant-<t>-dmz logs deploy/perimeter-<app>-<entry> -c proxy` | the request's `status`, `flags` and `upstream`; the inside fleet's class is `gentian-envoy-inside` |
| a person is refused behind sign-in | `kubectl -n kernel-edge logs -l app.kubernetes.io/component=bouncer` | lines with `"audit":true`, `"source":"bouncer"`: `outcome` and `detail.reason` (`no-session`, `unpublished`, `refused`, `allowed`) ([audit-events.md §3](../security/audit-events.md)) |
| a cluster name resolves wrong | `kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' \| sed -n '/BEGIN gentian-hairpin/,/END gentian-hairpin/p'` | the host at the edge's or the inside Service's ClusterIP. Some distributions name the ConfigMap differently: the CoreDNS Deployment's volume names it |

**The outgoing gateway's log.** Fields: `time`, `host` (the TLS server
name), `port` (the listener), `client` (the calling pod's address),
`bytes_received`, `bytes_sent`, `duration_ms`, `flags`, `details`,
`upstream` (the address the fleet connected to). Find the caller with
`kubectl get pod -A -o wide | grep <client>`. `flags` are Envoy's response
flags: `-` for none; `NFCF` (no filter chain found) when the connection
opened with a name the listener does not serve; `UF` (upstream connection
failure) or `UH` (no healthy upstream) when the host outside is unreachable.
The log is not forwarded as audit events.

## 6. For agents

An agent that writes a profile or asks for access follows these rules.

1. **Declare the narrowest form.** Behind sign-in before perimeter, inside
   before perimeter, `authMode: none` only for content meant for anyone.
   `hosts` before `hostsAtApproval`, both before `rule`.
2. **Never ask for a rule when the names are known.** A rule is for a
   protocol without a host name or a destination chosen at run time. The
   approver reads it as addresses only.
3. **Put every host in the one `hosts` request**, with the port where it is
   not 443, and no pattern.
4. **Give a reason an approver can judge**: what the app does there, for
   whom, and with what data. At least ten characters; one sentence.
5. **An approval is a person's.** An agent does not run `apps approve`,
   `exposures approve`, `trusted-domains add`, `apps install --approve-egress`
   or `--yes` on a person's behalf. It reports what waits and who answers.
6. **Read whether a request is pending** with
   `kubectl -n tenant-<t> get component <app> -o jsonpath='{.status.pendingPrivileges}'`,
   `kubectl gentian apps approve <app> --tenant <t>` (without a request named:
   read-only), or `kubectl gentian exposures requests --tenant <t>` (state
   `requested`). The Ready condition's reason says what holds the app (§5).
