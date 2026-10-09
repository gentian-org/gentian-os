# Gentian Cloud OS — Security Architecture

**Status:** Draft v0.3 · Architecture reference. The normative one-page rules are [security-principles.md](../security-principles.md); what is implemented versus target is §3.0, with the closing work in [plans/security-gap-closing.md](../plans/security-gap-closing.md) and the longer-dated items in [roadmap.md](../roadmap.md) §1.
**Scope:** Identity, authorization, and isolation for a fully cloud-based, Kubernetes-native sovereign cloud OS.

---

## 1. Purpose

Gentian needs an identity and access layer that is (a) simpler and more modern than traditional directory-centric stacks, (b) fully open-source and sovereignty-friendly, (c) first-class for **four principal types** — humans, AI agents, applications/workloads, and assets — and (d) designed so that a *compromised principal of any type does the least possible damage*.

This document explains the models behind the principles, the concrete Keycloak + OpenFGA architecture, and how application permissions are modeled (profile declaration, IntegrationBinding wiring, and AppGrant ReBAC layer). Where a section describes a control, §3.0 says whether the code has it.

The guiding idea, borrowed from Android's sandbox model: **least privilege is not a single access-control model — it is the intersection of several independent layers, each enforcing a different concern, so that breaching one layer does not collapse the others.**

---

## 2. Security principles

The nine normative rules are in [security-principles.md](../security-principles.md). This section is the reasoning they rest on.

### 2.1 Core principle — defense in depth as an intersection

Android's "a compromised app can do little" property does not come from one mechanism. It comes from stacking independent enforcement layers: kernel DAC (each app is its own UID, owns its own files), a capability manifest (declared + granted permissions), and SELinux MAC (a system-wide label policy that even root cannot override). An app's effective reach is the **intersection** of sandbox ∩ granted permissions ∩ mandatory policy.

Gentian ports the *layering*, not any single model. Each of the four classic access-control models is assigned to the layer it is best at:

| Layer | Model | Job | Android analogue |
|---|---|---|---|
| **Mandatory isolation backbone** | **MAC** | Tenant boundaries, default-deny egress, "agent ≤ human" ceiling — true regardless of any authZ config | SELinux + inet GID |
| **Authorization plane** | **ReBAC** | Fine-grained "who may touch which resource," sharing, delegation | Binder caller-UID checks + file ownership |
| **Conditioning** | **ABAC** | Time, device posture, risk, data classification | Runtime permission prompts |
| **Ergonomic grouping** | **RBAC** | Human-friendly bundles of grants | Permission groups |
| **Per-task grant** | **Capabilities** | Short-lived, attenuated, audience-bound authority for an app/agent | The permission manifest + grant |

**Rule:** MAC is the backbone, ReBAC is the authorization plane, ABAC conditions it, RBAC is an ergonomic veneer over ReBAC, and capability tokens are the per-task least-privilege grant. **Effective access = the intersection of all layers; the most restrictive layer wins.**

### 2.2 The composition principle

Android composes identity: `effectiveUID = androidUserId × 100000 + appId`. The same app in a work profile vs. a personal profile gets two different sandboxes.

Gentian generalizes this to a **principal chain**:

```
tenant T  →  human U  →  agent A  →  workload W
```

Each layer checks its slice:
- **Tenant T** isolation is **MAC** (namespaces, network policy).
- **Human U** rights are a **ReBAC** ceiling.
- **Agent A** task scope is a **capability ⊆ U's rights**.
- **Workload W** reachability is **SPIFFE/mesh** identity.

Effective permission = the intersection of the whole chain.

### 2.3 The derived-ceiling invariant (most important single idea)

An agent or app acting for a user must be **mathematically incapable of exceeding that user's rights**. This is expressed natively in ReBAC by *deriving* the agent's access through the user rather than granting it independently:

```
# OpenFGA-style schema sketch
type document
  relations
    define reader: [user, group#member]
    define acting_for: [user]
    define valid_task: [task]              # task object carries TTL via Condition
    define can_read: reader or (valid_task and can_read from acting_for)
```

The agent reads a document **only if** it is the user's agent (`acting_for`), the task is still valid (TTL), **and** the user can read it. Consequences:
- The "agent ≤ human" ceiling is an **invariant of the model**, not a policy someone must remember to configure.
- **Revocation is one tuple delete** on `acting_for` — it transitively collapses all downstream access.
- Full auditability: every action attributes to the agent identity *and* the delegating human.

### 2.4 Blast-radius containment per principal type

**Compromised human user** — ReBAC confines to actual relationships; ABAC forces step-up auth on sensitive ops; MAC guarantees the tenant boundary holds regardless of tuple errors; short sessions cap the window.

**Compromised agent** (the dangerous case — autonomous and prompt-injectable):
1. Each agent *instance* is its own principal — never a shared service account (the "each app is its own UID" port).
2. Holds only a task-scoped, time-boxed capability token.
3. Rights derived from the delegating human (§2.3) — can never exceed them.
4. **Default-deny egress** — the most underrated Android lesson. Most agent damage is exfiltration or calling out; a NetworkPolicy egress allowlist is the inet-GID.

**Compromised app/workload** — namespace isolation (the UID sandbox); SPIFFE/mTLS identity so it reaches only what mesh policy permits (Binder checks); default-deny egress; per-workload short-lived secrets (no shared God credential); admission control blocking privilege escalation at deploy time (SELinux confining even privileged domains).

### 2.5 The shared MariaDB server — what an app's account can do

One MariaDB server (`system-mariadb`) holds the databases of every tenant's MariaDB apps. An app gets one account on it, `<tenant>_<app>`@`%`, with a password of its own from the vault. As implemented (`internal/backup/mariadb.go`, verified against the pinned server image by `TestMariaDBAgainstAServer`):

- **It can** do anything inside its provisioned database. If its profile sets `allowDynamicDatabaseCreation`, it can also create, use and drop databases named `<database>_…`, and do anything inside them.
- **It cannot** read, write, create or drop a database of any other name — including one that differs only where its own name has an underscore, or only in case. It holds no privilege on the server: no `GRANT OPTION`, `CREATE USER`, `SUPER`, `PROCESS`, `FILE`, `RELOAD`, `SHUTDOWN`, `SHOW DATABASES` or replication privilege, so it cannot make or change accounts, pass its rights on, read other sessions, read or write the server's files, or see databases it has no rights on. Before this rule an app with `allowDynamicDatabaseCreation` held `ALL PRIVILEGES ON *.* WITH GRANT OPTION` — all of the above, on every tenant's data.
- **Two apps cannot be given overlapping rights.** A provisioned database's name can fall under another app's `<database>_` prefix, because hyphens in tenant and app names become underscores. Provisioning refuses the app that would create the overlap (operations.md §9.3).

What it does not cover:

- **No transport encryption is required** of an app's connection (no `REQUIRE SSL`, and the server does not set `require_secure_transport`); the account's host is `%`. Neither is new, and neither was changed.
- **No resource limits** are set on the account (`MAX_USER_CONNECTIONS` and the like): one app can exhaust the server's connections.
- **The network keeps an app to the server it declared, and no further.** A tenant's namespace denies egress by default. An app's `kernel-access` policy opens the server of the engine its profile declares, on that engine's port: `system-mariadb` on 3306 for a MariaDB app, `system-postgresql` on 5432 for a PostgreSQL one, and nothing of the other. So an app in a tenant's namespace that declared no MariaDB database cannot reach the MariaDB server at all. The server's side keeps out the rest: its own policy admits tenant namespaces and the platform's named clients, and no other pod of the cluster (§2.7). Among the apps that do reach the server, the account's rights above are the only boundary between tenants.
- **The admin credential** (`mariadb-admin`, root) is held by the operator's Jobs in `system-mariadb` and is not an app's.

### 2.6 What a declared store opens

A tenant's namespace denies egress by default. Policy `kernel-access-<app>` (`internal/kernel/netpolicy/kernel.go`) opens, for the app's pods, what its profile declares under `requires.services`:

| The profile declares | Opened |
| --- | --- |
| `database`, engine `postgresql` (the default) | `system-postgresql`, TCP 5432 |
| `database`, engine `mariadb` | `system-mariadb`, TCP 3306 |
| `cache`, engine `redis` (the default) | `system-cache`, TCP 6379 |
| `cache`, engine `memcached` | Nothing in the system tier. The tenant's own Memcached, TCP 11211, in the tenant's namespace (policies `tenant-cache-egress` and `-ingress`, which name only the apps that declared Memcached) |
| `storage.s3` | `system-s3`, TCP 9000 |
| `storage.files` alone | Nothing: another app serves the files |
| `mail` | `system-mail` and `system-mail-dmz`, every port. Not narrowed on the app's side: where an app's mail goes depends on the cluster's mail mode. The servers' side is: the proxy in the DMZ admits its three listeners, and the mail servers admit a tenant's pod on 587, 143 and 993 only (§2.8) |
| `identity` | The edge and the identity provider's namespace, every port. Not narrowed |
| `llm` | `system-llm`, TCP 4000: the model gateway, and nothing else in its namespace |

One engine opens nothing of the other. The ports are the ones the app is handed with its credentials (`internal/controller/provisioner`), and a test holds the store charts to them. A namespace named in a profile's `gentianos.io/kernel-egress-namespaces` annotation is opened whole, beside these; a profile that declares `llm` has no reason left to name `system-llm` there, and one that does opens the gateway's database, cache and model servers to its own egress as well (the gateway's side, §2.9, still refuses it on those ports). This is egress from the tenant's namespace; who a store admits is §2.7, who the model gateway admits §2.9.

**The model gateway is a declared requirement.** An app whose profile declares `requires.services.llm` is given a key at the gateway, the Secret `llm-credentials-<app>` and the rule above; an app that does not is given none of the three, on any cluster (`internal/controller/model_access_reconciler.go`). The key is generated per tenant and app the way a database password is -- derived from the master by the vault's seeder, or random without one -- kept in the vault at the app's path, and known to the gateway by its hash; nothing about it follows from the tenant's and the app's names. A key of the earlier form, `sk-gentian-<tenant>-<app>`, is replaced at the gateway in the first pass that finds it, and removed outright for an app that does not declare the gateway or is no longer installed.

### 2.7 Who may reach a shared store

Each of the four shared stores carries one NetworkPolicy, `store-ingress`, on its server's pods. It is delivered by the engine's own chart (`templates/networkpolicy.yaml` in `kernel/data/tenant-postgres` and `kernel/services/infra-{mariadb,redis,minio}/manifests`), in the Argo CD sync that brings the values the server is installed from, so a server is never up without it. A connection is admitted from the sources below, on the port named, and from nowhere else.

| Server | Port | Admitted |
| --- | --- | --- |
| PostgreSQL, `system-postgresql` | 5432 | tenant namespaces; pods of `system-postgresql` itself (the role and destroy Jobs, the cluster's own replicas); the operator's pods in `kernel-control` (a tenant's usage history and notices). The dump and load of a backup and a restore run in `system-postgresql` itself, where the administrator's Secret is |
| | 8000 | CloudNativePG's operator in `kernel-data`, which asks each instance for its state |
| MariaDB, `system-mariadb` | 3306 | tenant namespaces; pods of `system-mariadb` itself (the setup and destroy Jobs, and the dump and load of a backup and a restore) |
| Redis, `system-cache` | 6379 | tenant namespaces; pods of `system-cache` itself (the ACL and destroy Jobs) |
| MinIO, `system-s3` | 9000 | tenant namespaces (apps, and the volume capture and restore pods, which run where the claim is); pods of `system-s3` itself (the bucket and destroy Jobs, the capture and restore of a bucket, a bundle's manifest); pods labelled `gentianos.io/component=tenant-export` in `system-postgresql`, `system-mariadb`, the identity namespace, `system-mail` (the copy of a tenant's mailboxes, beside the mail server's volume) and `kernel-data` (the desktop database of the tenant that adopts the kernel realm, beside the kernel's PostgreSQL), which are the steps of a backup and a restore that run beside their own service and write to or read from the bundle; the operator's pods in `kernel-control` (a bundle's manifest, download and import) |

Closed, deliberately: PostgreSQL's metrics port (9187) and MinIO's console (9001). Nothing the platform runs uses either; a cluster that brings a Prometheus adds a policy of its own selecting the same pods, since policies add up.

**The rule has two sides, and neither is enough alone.** A server cannot tell one tenant's app from another's pod, so its policy admits every namespace labelled `gentianos.io/tier=tenant`. It keeps out everything that is not a tenant namespace or a client named above: the kernel's other namespaces, the other system namespaces, a tenant's DMZ, a namespace outside the layout. What keeps out an app that declared no such store is the tenant's side (§2.6): `tenant-isolation` denies egress for every pod of a tenant's namespace -- the platform tenant's and a single-tenancy cluster's `user` tenant included, since both are Tenants and the same reconcile writes it -- and `kernel-access-<app>` opens one store's port for the app that declared it. Two things on that side are wider than a declaring app: a component with a database requirement (the desktop) is opened the whole of `system-postgresql`, which the server now narrows to 5432, and a capture pod (`gentian-tenant-export`) is opened the three stores a bundle can hold.

**A pod of the server's own namespace is admitted without a label.** The operator's Jobs there carry none on their pods, and a pod there can mount the store's admin Secret, so a label would keep nobody out. The boundary is who may create a pod in a system namespace.

What this does not cover:

- **The other servers.** The kernel's own PostgreSQL and the mail servers have a policy each (§2.8), and so has each server of the model gateway's namespace (§2.9); what those leave open is said there.
- **Kernel and system namespaces deny no egress.** A pod in one of them is kept from a store by the store's policy, from a kernel namespace by that namespace's rules (§2.13), and on the way out by nothing (gap G28).
- **A tenant's DMZ namespace has no default deny.** Its proxies carry their own policies -- out to the one component, in from the Gateway's Envoy pods (§2.14); the stores do not admit that tier.
- **A CNI that does not enforce NetworkPolicy** makes all of this a description. The kubelet's probes are unaffected either way: traffic from a pod's own node is not subject to a NetworkPolicy.

The clients are written down once, in `scripts/tests/store-clients.yaml`. `make test-store-network-policies` renders each policy and each engine and asserts every client is admitted, every entry it says to refuse is refused, and every port a server's pods declare is listed or recorded as closed; a Go test holds the file to the code that builds each client (`internal/controller/store_clients_test.go`). A new client of a store is added there first.

One switch turns the four off: `storeNetworkPolicies` on the bootstrap chart, which the installer sets from `STORE_NETWORK_POLICIES` (default `true`). Off leaves credentials as the only check on a connection.

### 2.8 Who may reach the kernel's PostgreSQL and the mail servers

Three more servers carry a NetworkPolicy of the same kind, each in its own chart and synced before the server it selects (sync wave -1). The same switch turns them off.

**`kernel-postgres`, in `kernel-data`** (`kernel/data/kernel-postgres/templates/networkpolicy.yaml`, policy `kernel-postgres-ingress`). It holds Keycloak's database, OpenFGA's, the registrar's record and the desktop's database of the tenant that adopts the kernel realm.

| Port | Admitted |
| --- | --- |
| 5432 | the authentication namespace (Keycloak); the authorization namespace (OpenFGA, whose migration runs as an init container of its own pod); in `kernel-control`, the registrar's and the operator's pods by label, and no other pod there -- the operator reads and writes the usage history and the notices of the tenant that adopts the kernel realm; tenant namespaces, for that tenant's desktop; pods of `kernel-data` itself (the cluster's replicas and join Jobs, and CloudNativePG's operator) |
| 8000 | CloudNativePG's operator, in `kernel-data`, which asks each instance for its state |

Closed: the metrics port (9187). Not admitted: Argo CD, Crossplane and its providers, the vault, the edge, the director, the usher, the custodian, the system namespaces, a tenant's DMZ.

- **The two identity namespaces are admitted whole.** Keycloak's and OpenFGA's pods are built by upstream charts the Suze claim installs at a version it may move, and a pod of either namespace can mount the database Secret there.
- **Tenant namespaces are admitted by tier, not by name.** Which tenant keeps its desktop's database here is decided by its realm (`componentDatabaseNamespace`), so no namespace can be named. The tenant's side is what narrows it, as for a store: egress is denied by default, and only that one tenant's desktop is opened `kernel-data`.
- **CloudNativePG's operator is not selected by this policy.** It runs in `kernel-data` beside the cluster; the namespace's own rule (§2.13) leaves its webhook port open to any source, because the API server calls it from no pod, and closes its metrics port.
- **Switched off, this one policy is kept and admits everything** rather than being removed: the Application that syncs the chart does not prune, so a policy that merely stopped being rendered would stay in force.

**Mail: Dovecot and Postfix in `system-mail`, and the proxy in front of them in `system-mail-dmz`** (`templates/networkpolicy.yaml` in `kernel/services/dovecot/manifests`, `kernel/services/postfix/manifests` and `kernel/services/mail-edge/manifests`; policies `dovecot-ingress`, `postfix-ingress` and `mail-edge`). They exist only on a cluster whose `mail.serviceMode` is `system`; a cluster that relays its mail runs none of the three and has none of the policies.

Nothing in `system-mail` is reachable from outside the cluster: both servers' Services are ClusterIP. The one load balancer for mail is in the DMZ, in front of a proxy (HAProxy, TCP only) that takes 25, 587 and 993 and opens each connection again to the server it belongs to. What the proxy is and is not is in [mail.md §9b](mail.md).

Every flow of the two namespaces. The first three rows are the only ones that begin outside the cluster.

| From | To | Port | Held by |
| --- | --- | --- | --- |
| Any address: mail servers and mail clients on the internet, through the load balancer | the proxy | 25, 587, 993 (the pod's 2525, 2587, 2993) | `mail-edge`, ingress, no source named |
| A tenant's app that declared mail, dialling `mail.<kernelDomain>` or `imap.<kernelDomain>` | the proxy | 587, 993 | the same rule; the app's side is `kernel-access-<app>` |
| Keycloak, sending a realm's mail to `mail.<kernelDomain>` | the proxy | 587 | the same rule |
| The proxy | Postfix | 10025 (inbound mail), 10587 (submission), TCP, each connection behind a PROXY-protocol header | `mail-edge` egress; `postfix-ingress` admits the proxy's pods, by the DMZ's name and their labels, and nothing else there |
| The proxy | Dovecot | 10993 (IMAPS), TCP, behind a PROXY-protocol header | `mail-edge` egress; `dovecot-ingress` likewise |
| The proxy | the cluster's resolver | 53, UDP and TCP | `mail-edge` egress, no destination named |
| A pod of a tenant namespace, by the Service's name | Postfix | 587 | `postfix-ingress`, namespaces labelled `gentianos.io/tier=tenant` |
| A pod of a tenant namespace, by the Service's name | Dovecot | 143, 993 | `dovecot-ingress`, the same |
| Postfix | Dovecot | 24 (LMTP), 12345 (authentication service) | `dovecot-ingress`, Postfix's pods |
| The installer's check, in `system-mail`, labelled `gentianos.io/purpose=verify` | Dovecot | 24, 143 | `dovecot-ingress` |
| Postfix | mail servers on the internet | 25 | no policy: egress from `system-mail` is not restricted |
| Postfix | the relay host, where the claim names one | the relay's port (587 unless set) | no policy |
| Postfix, Dovecot | the cluster's resolver | 53 | no policy |
| Dovecot | Keycloak, in `kernel-authentication`, to introspect a sign-in token | 8080 | no policy on Dovecot's side |
| The kubelet, from the pod's node | the proxy's health endpoint (8404), Dovecot's 24 | | not subject to a NetworkPolicy |

Closed, though something listens: Postfix's 25 (mail from outside arrives on 10025, and nothing inside the cluster is handed 25) and the proxy's health port. The proxy is the only pod of the two namespaces whose egress is restricted: it can reach the three PROXY-protocol ports and a resolver, and nothing else in any namespace or on the internet.

| Control | State |
| --- | --- |
| No Service of `system-mail` is a load balancer or a node port; the only listener that faces the internet is the proxy's, in `system-mail-dmz` | Built |
| The proxy holds no mail, no user database, no certificate and no key, mounts no Secret and carries no service-account token; it runs as an ordinary user with no capability on a read-only filesystem | Built |
| TLS ends at Postfix and Dovecot: the proxy passes STARTTLS and implicit TLS through unread | Built |
| The servers see the client's address, not the proxy's: each connection carries it in a PROXY-protocol header, which Postfix and Dovecot read on ports of their own and require there | Built; proven against the real images in local containers (`make test-mail-edge-lab`), not on a cluster |
| A pod of the cluster cannot give itself another address: the ports that believe a PROXY header admit the proxy's pods alone, and the ports pods use take no header | Built, and only as good as the CNI's enforcement of NetworkPolicy |
| Per client address: at most 20 connections at once and 60 a minute on 25 and on 587, 100 and 120 on 993, counted by each proxy replica; over that the connection is closed and logged | Built |
| The proxy can reach nothing but the three PROXY-protocol ports and DNS | Built |
| Egress from `system-mail` restricted | **Not built.** Postfix has to reach port 25 of any address; no policy says so and none denies the rest |
| The client's address behind a load balancer that is itself a proxy (an Octavia amphora) | **Built as a setting, off by default, and no claim field sets it.** `loadBalancer.proxyProtocol` on the mail-edge chart makes the proxy take a PROXY header from the load balancer's addresses and from nobody else. Without it, on such a load balancer, every client is the load balancer's address |
| Spam filtering, DNS blocklists, greylisting | **Not built.** Nothing scores mail; the proxy does not read it |
| Publishing 587 and 993 only where a perimeter approver enabled it ([networking.md §5](../plans/networking.md)) | **Not built.** All three listeners are on wherever the cluster runs its own mail |

What stays open, and why:

- **The proxy admits any source on its three ports.** A connection through the load balancer arrives from an address that is no pod's, and one from a pod that dialled the public name arrives from wherever the cluster's proxy mode puts it. The credential is the check on 587 and 993; on 25 it is Postfix's rule that it relays for nobody it does not know.
- **A broken-into proxy can lie about a client's address** to the two servers, on those three ports, and can do nothing else. It could name an address inside the pod range, which Postfix lets relay without a credential unless `submission.requireAuthentication` is set on the Postfix chart; with that set, an address buys nothing.
- **A tenant's pod reaches Postfix's 587 and Dovecot's 143 and 993 by the Service's name**, as before, now narrowed from any source to tenant namespaces. The operator hands out the public names, so these are a fallback; which pods use them cannot be read from this repository.
- **DNS egress names no destination**, as everywhere in this repository: where a cluster's resolver is differs by cluster.

The clients of all three are in the `servers` section of `scripts/tests/store-clients.yaml`, each marked proven or inferred; `make test-store-network-policies` holds the policies to it and holds the proxy's configuration, its egress rules and the servers' listeners to each other (`scripts/tests/server_network_policies.py`), and a Go test holds the table to the operator's code (`internal/controller/server_clients_test.go`). Where the operator writes each object a mail server mounts is held to the chart that mounts it by `internal/controller/mail_objects_test.go`.

### 2.9 Who may reach the model gateway

On a cluster that serves models, `system-llm` holds four pod sets that listen, and each carries a NetworkPolicy of its own. They are delivered with the gateway's chart (`kernel/services/llm/manifests/templates/networkpolicy.yaml`) in sync wave -1, before the servers, under the same switch as the stores' (`storeNetworkPolicies`).

| Server | Policy | Port | Admitted |
| --- | --- | --- | --- |
| The gateway (LiteLLM) | `llm-gateway-ingress` | 4000 | tenant namespaces; the operator's pods in `kernel-control` (an app's or a desktop's key, a tenant's team, a purge); and, only where the claim switches the console on (`llm.console.enabled`), the Gateway's Envoy pods in the edge namespace (the console `llm.<kernelDomain>`, a kernel console behind the kernel session) |
| Its PostgreSQL (CloudNativePG) | `llm-database-ingress` | 5432 | the gateway's pods; the cluster's own pods (a replica or join Job, when instances is raised) |
| | | 8000 | CloudNativePG's operator in `kernel-data` |
| Its Redis | `llm-cache-ingress` | 6379 | the gateway's pods |
| The mock model server (clusters without GPUs) | `llm-mock-ingress` | 8000 | the gateway's pods |

**The console has a switch, and it is off by default.** `spec.llm.console.enabled` on the Cluster claim decides whether `llm.<kernelDomain>` exists. Off, there is no route, no published host and no rule for the edge in `llm-gateway-ingress`: the gateway is reachable by its in-cluster clients alone, which is what "system services have no public route" asks. On, the route is composed behind the kernel session for whoever may configure the cluster (`can_configure`, `kernel_gateway_routes.go`), the edge's Envoy pods are admitted to port 4000, and the route carries the gateway's whole API as well as its pages; that is a departure from the rule which the cluster's owner takes by writing the setting. The operator reads the setting from the claim and falls back to nothing: a cluster whose claim does not state it has no console, including one upgraded from a version that always routed it, and the installer says so in one line. On this route the edge leaves the `Authorization` header to the console, whose pages send LiteLLM's own key there, and hands LiteLLM no token of the platform's; the session is shown to the bouncer by its ID token, as on Keycloak's administration console. The policy rule is rendered from the value the installer passes the gateway's chart, so after the claim changes it follows with the installer's next run; in between, a route without the rule answers nothing ([llms.md](llms.md)).

**The gateway's image is a named release.** `kernel/services/llm/manifests/values.yaml` names LiteLLM by tag and digest. `make lint-image-pins` fails on an image under `kernel/`, `charts/` or `crossplane/` that names `latest`, and on the gateway's image written without its digest, in the values and in the rendered chart. One `latest` is known and listed there: the fallback tag of a real vLLM instance.

The database's metrics port (9187) is closed. The rule for the gateway has the same two sides as a store's: its policy admits every tenant namespace, and the tenant's side (§2.6) opens port 4000 for the apps that declared the gateway and for no other. Unlike a store, no pod of `system-llm` itself is admitted to the gateway: nothing there is its client.

**The desktop is a client of the gateway**, for the assistant on it. Its profile declares the gateway as an app's does, optionally, and the operator serves it per tenant (`internal/controller/model_access_reconciler.go`): a key generated in the vault for this tenant's desktop alone, registered under the alias `<tenant>-desktop`, delivered in the Secret `llm-credentials-desktop` of the tenant's namespace, and written on the tenant's record so that deleting the tenant with its data removes it. The desktop's chart is told the gateway's address and that Secret's name; the key is in no release value and no Component. The desktop's pods reach port 4000 of `system-llm` by a rule of `component-desktop`, written once the key is delivered, and nothing else in that namespace. The platform tenant's desktop is served the same way -- its namespace is a tenant-tier namespace like any other. What this leaves as it was:

- **The key is the tenant's desktop's, not a person's.** Every member who can open the desktop calls models with it; the gateway sees one key per tenant's desktop and, at most, the name the desktop passes along with a request.
- **No budget is attached.** The gateway knows a team per tenant, and neither an app's key nor the desktop's is attached to it, so a tenant's spend is not capped at the gateway by this.
- **A profile below platform trust cannot have this.** The API server refuses a placed profile that declares the gateway at another tier, and the reconciler refuses one that asks for the key as a chart value.

The clients are the `store: llm` rows of `scripts/tests/store-clients.yaml`, each naming the server it connects to, and `make test-store-network-policies` holds the four policies to them.

Not covered: **real vLLM instances.** Their chart (`kernel/services/llm/chart`) names the v4 namespace and nothing in this repository installs it on the current layout, so there is no pod for a policy to select. A cluster that runs one in `system-llm` gets no policy for it from here. The gateway nevertheless offers a model for each instance the claim lists and calls it at `vllm-<name>-inference.system-llm`, port 8000 ([llms.md §6](llms.md)).

**A provider's token stays in its Secret.** The models the gateway offers are its configuration file, written from the Cluster claim; for a model of an external provider the file names an environment variable, and the gateway's container sets it at start from the Secret `llm-provider-credentials`, which the External Secrets Operator copies from `gentian-os/kernel/llm-providers`. The token is in no ConfigMap, not in git and not in the gateway's database. The gateway stores no model (`STORE_MODEL_IN_DB` is off), so its console and its API cannot add an endpoint for tenants' prompts to be sent to; the claim is the only place that names one.

### 2.10 A tenant's website on the cluster's main address

On a single-tenancy cluster the user tenant may publish a public website on
the cluster's bare domain ([routing.md §5](routing.md#5-redirects-and-url-control)).
It is the most visible page of the cluster and nobody is signed in on it.

| Control | State |
| --- | --- |
| Only the user tenant of a `single` cluster, one surface at a time | Built: the director refuses anything else (`409`), and the operator publishes nothing for it |
| Two people say so: the profile's author (`apex: true` on the entry) and the tenant's perimeter approver (`apex: true` on the request, `can_expose`) | Built |
| Recorded with owner, publish date and review date, like every published surface | Built (the exposure registry) |
| The approver is shown what is approved (address, paths, whether anybody signs in) before approving, and only what an installed app declares can be approved | Built: the director's read lists it, and refuses an approval of an app that is not installed or an entry that is not declared for the perimeter (`422`). The read is from git, so it says where an entry will answer, not whether it answers yet |
| Served from the tenant's DMZ by a proxy that passes no cookie, token or identity header in, and no `Set-Cookie` out | Built |
| `/branding/`, `/sign-in`, `/.well-known/acme-challenge/` and `/.well-known/pki-validation/` stay the platform's | Built, twice: route precedence at the Gateway, and `404` in the website's proxy |
| `https://<kernelDomain>/sign-in` always leads to sign-in | Built |
| The approver is told what a script on the website can do to sign-in, and acknowledges the rule below; who and when is recorded with the entry | Built: the director refuses the request without it (`400`) |
| A script on the website cannot disturb sign-in on the other addresses | **Not built.** See below |

**The finding.** Any page served on the bare domain can set a cookie for the
whole domain, and the browser then sends it to `desktop.`, `admin.`,
`platform.` and `id.<kernelDomain>` as well. A script in such a page cannot
read or forge anybody's session. It can do two things:

- stop a person from signing in, until they clear their cookies;
- plant a session of the script author's own, so that the person works in
  the author's account without noticing, and what they do or type there ends
  up with the author.

The same is true of every app host on a single-tenancy cluster; a public
website makes it more likely, because websites load scripts from other
parties and have more editors. Cookie names with the `__Host-` prefix would
close it, and are not available as configuration in Envoy Gateway 1.9.2 or
Keycloak. Detail and options:
[networking.md §8.7](../plans/networking.md#87-a-website-on-the-clusters-main-address).

**The rule.** A website goes on the main address only if the organisation
itself controls every script it runs: no third-party scripts (analytics,
embeds, widgets, anything loaded from another host) and no pages uploaded by
users.

**Why the platform cannot enforce it.** The platform sees a backend and the
paths it serves, not what the pages load or who may edit them, and both
change after approval without anything passing the platform. So the rule is
the approver's to keep. The director answers a request for the main address
with the warning and publishes only once the approver sends
`"acknowledgeMainAddressRule": true`, on the first publication and on every
review. The entry then records who acknowledged and when
(`apexAcknowledgedBy`, `apexAcknowledgedAt`), in the exposure registry in
git. An entry approved before this was asked has neither and stays
published; its next review is asked.

**What remains possible.** The acknowledgement is a person's word, not a
control. A site that breaks the rule, or one whose own scripts are replaced
by an attacker, can still do both things above to anybody who visits it. The
complete fix is to move the sign-in addresses to a domain that never serves
tenant content ([roadmap 1.37](../roadmap.md)).

### 2.11 No app at an address people trust as the platform's

People recognise the platform by its addresses: the desktop at
`desktop.<their domain>`, the administration console at `admin.`, sign-in at
`id.`. A profile states the label its component answers on, and a profile
from a catalogue is written by whoever publishes the catalogue. Without a
rule, an app whose profile says `subDomain: desktop` would claim the
desktop's host; the Gateway resolves two routes on one host by age, without
an error, so a tenant's people could be shown a look-alike desktop or
sign-in page under the address they trust.

| Control | State |
| --- | --- |
| One list of address names the platform keeps, with the reason for each ([routing.md §3.1](routing.md)) | Built (`internal/hostnames`); a test holds the documents to it |
| A reserved name is admitted only for the platform's own component for it, and only for that component's own label | Built |
| "The platform's own" is not what a profile says of itself: it is the profile's name together with no catalogue having brought it. The director records the origin of everything it materialises, refuses a profile that states an origin itself, and refuses any catalogue's profile under a name the platform ships | Built |
| The director refuses an install, an add-on or an import that would take a reserved name (`422`), before anything is committed | Built |
| The operator refuses again where the component would be published: held whole (`HostReserved`), before any policy, release, route or perimeter listener is written | Built |
| Applies under every domain a tenant can have, a custom domain included | Built |
| Names that only resemble a reserved one (`desktop1`, `desk-top`, `my-login`) | **Not built**, deliberately: no rule for it is well defined |
| An app's page imitating the platform's look under its own, permitted address | **Not covered.** The address is what is protected |

Whoever can write to the deployments repository or apply objects to the
cluster directly is not constrained by this: that is the cluster owner's
authority, and a ComponentProfile written there with no origin is taken for
what its name says.

**The installer is such a writer, and what it writes is held to a digest.**
At install it places the default profiles -- the Operations Console's, from
`<catalogue address>/profiles/operations-console.yaml`, by default at
`https://catalogue.aluvian.io` (`GENTIAN_STORE_CATALOGUE_URL`,
`GENTIAN_DEFAULT_PROFILES`) -- in the cluster's catalogue directory
(`_scaffold_default_profiles`; AD-14). A file is written only when it hashes
to the digest the same catalogue's `index.yaml` lists for the entry, or to a
digest the person installing pinned (`@sha256:<digest>`), which the index does
not override; a mismatch stops the install. It is written as the director
writes an install, with its bundle and with the catalogue as its origin, so it
is a catalogue's profile and not one "no catalogue brought"; an entry under a
name the platform ships is refused.

What that is worth, precisely. The index is served by the catalogue the file
comes from, over HTTPS, and is not signed: a digest from it catches a file
that changed without its index, not a catalogue that publishes a file and an
index that agree with each other. A pin is the person's own statement and
catches that too. Afterwards the operator compares the profile on the cluster,
and what its bundle brings, with that bundle before it rolls out the
Component it creates for a default profile, and holds the Component when they
differ (`internal/profilebundle/recorded.go`). The digest it uses is the one
recorded with the profile, the bundle's own: no second place states it, as
the tenant's manifest does for an install, so a profile replaced together
with its bundle is not caught. And the installer applies the
part of the director's bundle check that concerns kinds, names and metadata,
not the rules inside a companion's body.

---

### 2.12 What the sign-in sidecar trusts, and what remains weak

For an app that can do neither OIDC nor SAML, a program of the platform's stands beside it and
makes the app's session for a person the realm vouches for ([iam.md §1.11](iam.md),
[app-customization.md §2.3a](../app-customization.md)). It holds what its handler needs of the
app, which is usually the app's signing key and database. This section is what guards it.

**The one path with no session.** `/sso/acs` of the app's host takes no session and is not shown
to the bouncer: the realm posts its answer from its own address, and for a tenant on a domain of
its own a browser sends the front door's `SameSite=Lax` cookies with no such request. The route
matches that path exactly, by POST, removes the identity headers a client could send, and leads
to the sidecar alone. Every other path of the app keeps its session and its question.

**What the sidecar accepts there.** All of this, or the request is refused and the sign-in it
answered is spent:

| It checks | So that |
|---|---|
| Two signatures by a certificate of the realm: the response as a whole and the assertion in it. Everything it reads is read from the signed bytes | nothing posted is believed that the realm did not say |
| The issuer is the realm it was told about | another realm's people are not this tenant's |
| `Destination`, the assertion's `Recipient` and the audience are its own address and name | an answer made for another app is not accepted here |
| `InResponseTo` names a request this process sent, not answered before, at most five minutes old | nobody presents an answer the sidecar did not ask for, or one twice |
| The browser carries the cookie set when that request was sent | an answer obtained in one browser is not completed in another |
| The person the realm names is the person the front door admitted when the request was sent | only somebody who may use the app is signed in to it, and as themselves |
| The validity period; exactly one assertion, in clear, not presented before; no document type declaration | a stale, repeated or doctored message is not accepted |

**Who may use the app** is the front door's answer, not the sidecar's. `/sso/login` is behind the
session and the bouncer, so only a person with `can_use` on the app begins a sign-in, and the
answer has to be about that same person. A member of the realm without the right cannot begin
one, and an answer they obtain elsewhere answers no request of theirs.

**Who administers the app** is the realm's answer, in the assertion it signs: the one role of the
sidecar's client, granted to the tenant's `app-admins` group. The sidecar reads it from the signed
assertion — the assertion's own attribute `Role` with the value `gentian-app-admin` — and from
nothing a request says about itself; an answer changed after signing is refused whole. The client
has no other role in scope (`fullScopeAllowed` is off), so a role of that name elsewhere in the
realm, or at another app's client, is not listed. The handler gives the app's administrator role
to that person and takes it from anybody else at every sign-in, so a withdrawal is in force
within the hour a session lasts.

**Which code is handed the app's keys.** The handler is part of the app's bundle. It is run only
from a bundle of a catalogue of the whole cluster, for an install pinned to that bundle's digest;
the operator gives the sidecar the bundle's bytes and their sha256, and the sidecar loads no
other file. A tenant's own catalogue cannot bring one, and no sidecar runs for a tenant that signs
in in the kernel realm: the Component is held with `SignInSidecarRefused`. What it is handed is this app's own
database login and the secrets of this app the profile lists, read from this app's vault paths; a
profile names which, never where.

**What it may reach, and what may reach it.** Out: the realm's signing certificate, and what the
profile declared for the handler — the app's database server, the app's own pods — one port
each. It does not carry the app's label, so none of the app's other paths are its. In: the edge,
and the identity provider's namespace, which tells it of a sign-out (below); no pod of the
tenant.

**Signing out.** The sidecar's client at the realm names the sidecar's own Service as its SAML
single-logout address, `http://<app>-sign-in.<namespace>.svc.cluster.local:8081/sso/logout`, and
asks the browser for nothing: when a person signs out, the realm posts a signed `LogoutRequest`
there itself. No route carries `/sso/logout`, so nothing outside the cluster reaches it, and the
sidecar answers it only under that Service name — a request that arrived under the app's public
name is answered 404. What it accepts there, or it ends nothing:

| It checks | So that |
|---|---|
| A signature by a certificate of the realm over the request as a whole. Everything it reads is read from the signed bytes | nothing posted is believed that the realm did not say |
| It is a `LogoutRequest`, and its issuer is the realm it was told about | an answer or a request of another kind, or another realm's, signs nobody out |
| `Destination` is exactly its own sign-out address | a request made for another app's sidecar is not accepted here |
| Issued within the last two minutes and not in the future; not past its `NotOnOrAfter` where it has one; not presented before | a kept request is not presented later, or twice |
| It names one person, by e-mail address, in clear; a realm session this process signed somebody else in from is not named for them | the handler is told the person the realm means |
| Exactly one request in the post; no document type declaration | a doctored message is not accepted |

It then calls the handler's `onLogout` for that person, where the handler has one. The worst a
request could do that got past all of this is sign a person out of one app. It could not sign
anybody in: the path makes no session.

| Control | State |
|---|---|
| The checks above, each with a test; run against Keycloak 26.8.0 at the three kinds of address | Built (gentian-apps, `images/gentian-sidecar-sso-saml`) |
| The session-less route is one exact path, POST, to the sidecar; no policy names it and the bouncer has no line for it | Built; a test holds it |
| The handler is the pinned bundle's, from a cluster catalogue only | Built; held with `SignInSidecarRefused` otherwise |
| The handler is given only what the profile declared, of the app's own | Built |
| The sidecar's image is one build, named by tag and digest | Built |
| An app session lasts at most an hour and no longer than its realm session | Built in the sidecar; the handler has to give its token that lifetime |
| A sign-out at the realm ends the person's sessions in the app, on a request the realm signed and addressed to the sidecar inside the cluster | Built; tests in the sidecar, against Keycloak 26.8.0, and against Docmost and OpenProject. Activepieces 0.28.0 cannot end a session and keeps the hour |
| Who administers the app is read from the signed assertion alone; given and withdrawn at sign-in | Built; tests in the sidecar, against Keycloak 26.8.0, and against each of the three apps |

**What remains weak.**

- **The sidecar can become anybody in its app.** That is what it is for. Whoever takes it over,
  or changes the handler in a catalogue the cluster trusts and has an install moved to it, has
  every account of that app in that tenant. No other way of signing in has such a program.
- **The identity headers on `/sso/login` are believed.** They are the bouncer's and are not
  signed. A pod that reached the sidecar directly could begin a sign-in under a name it chose —
  and would then still need the realm's signed answer for that name, so it gains only the skipped
  check of who may use the app. The network policy is what keeps pods from reaching it.
- **The realm's certificate is fetched over plain HTTP inside the cluster.** Something that could
  answer in the identity provider's place there could sign answers of its own.
- **Requests to the realm are not signed.** The sidecar holds no key. Anybody can make the realm
  post an answer to the sidecar's address for whoever is at the browser; the sidecar refuses it,
  because it asked for none.
- **Within the hour, the app can show the previous person — where the sign-out did not end the
  session.** Anna signs out, Ben signs in at the same browser and opens a page of the app that is
  not an entry path: an app that still has Anna's session shows it. Docmost and OpenProject end
  it when the realm tells the sidecar. It is left in three cases: Activepieces, whose handler
  has nothing to end a session with; a sidecar that was not running at the moment the realm told
  it, because the realm tells once; and a realm session that ran out without a sign-out. Opening
  the app from its tile goes through the sign-in and gives Ben his own.
- **The sign-out request arrives over plain HTTP inside the cluster,** like the realm's
  certificate. It carries the person's address and the realm session's identifier and no secret:
  something that read it would learn who signed out, and presenting it again is refused.
- **Signing out at one device signs the person out of the app at all of them.** The handler is
  told who, not which browser. The other device is taken through the sign-in again, silently.
- **App Admin is one role per tenant.** Who holds it administers every app of the tenant that
  maps it, not one app. The per-app group of the authorization model
  (`gentian:tenant:<t>:app:<p>:admins`) is not built.
- **A withdrawn administrator keeps the role until their next sign-in,** at most an hour, in an
  app that does not ask again in between.
- **A person is an e-mail address to the app.** An address given to somebody else later is the
  same account. The person's permanent identifier is not what these apps key on.
- **Accounts are made and never removed.** A person removed at the platform cannot get in, and
  their account and what they made stay in the app.
- **A handler depends on the app's inside**: its tables, its token, its calls. A new version of
  the app can break it, or worse, change what a field means. Each has an end-to-end test against
  the real app that is to be run before the app's version moves; nothing enforces that it is.
- **The app's own protections for sign-in do not apply**: its second factor, its lock after
  failed attempts, its record of sign-ins. The realm's do.
- **Whether a vendor accepts this in place of its paid single sign-on** is a judgement per app,
  recorded in the app's `Customization` record, and not made here.

### 2.13 Who may reach a pod of a kernel namespace

Every kernel namespace refuses an ingress that nothing lists. The list is one file, `internal/kernel/kernelnet/inventory.yaml`: per namespace, every pod set, every port it listens on, and for each port who is admitted, with where in the repository that was read. The NetworkPolicies are generated from it (`kernel/security/network-policies/kernel-network-policies.yaml`, `make gen-kernel-network-policies`), so no rule exists without a line that says who it is for.

They are plain `networking.k8s.io/v1` NetworkPolicy and name no address block, so they mean the same on every network plugin that enforces NetworkPolicy. Each namespace has one policy, `kernel-ingress`, that selects every pod; rules are by the pod's port, because a plugin matches a connection after a Service's port has become the pod's.

| Namespace | Admitted from outside the namespace | Pods of the namespace reach each other |
| --- | --- | --- |
| `kernel-gitops` | Nothing by these rules. Argo CD's own manifest ships a policy per component, and those stand: the console (8080) stays open to any source, as upstream has it | No: a rule for the whole namespace would widen upstream's |
| `kernel-provisioning` | 9443 from any source (Crossplane's and its providers' webhooks, called by the API server) | Yes |
| `kernel-secrets` | The vault, 8200: the operator and the custodian by label, and the provisioning namespace (provider-vault). 10250 from any source (External Secrets' webhook) | Yes (External Secrets reads the vault) |
| `kernel-seal` | The seal, 8200: the secrets namespace | Yes |
| `kernel-data` | 9443 from any source (CloudNativePG's webhook). The kernel's PostgreSQL keeps its own policy (§2.8) | Yes |
| `kernel-authentication` | Keycloak, 8080: the edge namespace, the control namespace's programs, the provisioning namespace, tenant namespaces, the mail namespaces. Its management port (9000) and 8443: nobody | Yes (the realm Jobs) |
| `kernel-authorization` | OpenFGA, 8080: the control namespace's programs and the bouncer, by label. gRPC, metrics, playground: nobody | Yes |
| `kernel-control` | Director and usher (8080), custodian (9444), registrar (9445): tenant namespaces. The operator keeps its own policy: its listener admits the director and the usher | No: it would open the operator's listener to every pod here |
| `kernel-edge` | The Envoy proxies: any source, any port (they are the public listeners). The bouncer, 9001: the Envoy proxies alone; 8082 (the rights check): tenant namespaces. Envoy Gateway's configuration port, 18000: the Envoy proxies. 10250 from any source (cert-manager's webhook). An HTTP-01 solver, 8089: the Envoy proxies | No: it would open the bouncer to cert-manager and external-dns |
| `kernel-admission` | 9443 from any source (Kyverno's webhooks) | Yes |
| `kernel-observability` | Headlamp, 4466: the Envoy proxies. kube-oidc-proxy: nobody | Yes (Headlamp reaches kube-oidc-proxy) |

What follows from this: OpenFGA is reachable from the programs that ask it and from nothing else; the bouncer's authorization port takes the Gateway's Envoy pods alone; the vault and its seal take their named clients; a tenant's pod reaches the director, the usher, the custodian and the registrar, which check the person's token, and no other port of the control namespace.

**The tenant side is narrowed with it.** A tenant's `tenant-isolation` policy admits the whole edge namespace; with the switch on it admits that namespace's Envoy pods (`app.kubernetes.io/name=envoy`, the label Envoy Gateway puts on every proxy pod). cert-manager, external-dns and the bouncer share the namespace and then have no path to an app on which to send identity headers. The authentication and control namespaces are still admitted whole: Keycloak's pods are an upstream chart's, and besides the operator the registrar calls a tenant's desktop.

**What a NetworkPolicy cannot say, and what was done instead.**

- *The API server.* It is no pod, and not always on the pod's node; on a managed cluster it arrives through an agent. No portable rule names it, so every port it calls -- the admission and conversion webhooks of Crossplane and its providers, External Secrets, cert-manager, CloudNativePG, Kyverno and the operator -- admits any source. A webhook serves TLS and decides nothing by who connects; an open webhook port is what keeps an install alive on every cluster.
- *The kubelet.* A connection from the node a pod runs on is admitted whatever the policies say; that is NetworkPolicy's own rule, so probes need no rule and health ports are closed to everything else.
- *The install host.* The installer reaches the vault, the seal and Keycloak from the host, by the Service's address where the host routes to it and by `kubectl port-forward` where it does not (`scripts/lib/portforward.sh`); a port-forward enters the pod itself. Its one check through the API server's proxy, of the custodian, falls back to a port-forward as well (`E-03`).

**Egress is not restricted** in any kernel namespace. Argo CD, Crossplane and its providers, cert-manager, external-dns, Keycloak, the Envoy proxies and the operator reach the internet or the API server in ways that differ per cluster; CloudNativePG's instances call the API server; OpenFGA's database address is the claim's to decide. A pod of a kernel namespace is stopped at another kernel namespace's door by that namespace's rules, and on the way out by nothing (gap G28, the egress half).

**Not covered.** `system-mail` and `system-mail-dmz` are system namespaces and their rules are written with the mail servers (§2.8); nothing here selects a pod of them. `kube-system` and the load balancer's namespace are the cluster's own. The system and shared tiers are as §2.7 to §2.9 leave them.

**The switch, and why it is off.** `KERNEL_NETWORK_POLICIES`, read by the installer. `true` applies the file with the namespaces themselves, in the installer's first step -- before a pod exists in any of them on a fresh cluster, and with nothing else running to deliver it; anything else removes every policy labelled `gentianos.io/kernel-network-policy` and applies none. The operator is told the same switch, and its two rules that rest on the same fact -- the tenant side above, and the publishing proxy's own ingress rule (§2.14) -- follow it. **It is off by default.** The rules were derived from the repository and checked against it, and no cluster had run them when they were merged; a caller the inventory lacks shows as a timeout in the middle of an install. Turn it on once an install has succeeded ([install-reference.md §9](../install-reference.md)), and make it the default when a fresh install has passed with it on.

**How it is held.** `go test ./internal/kernel/kernelnet/` reads the generated policies with NetworkPolicy's own semantics and asserts that every caller the inventory names is admitted, that a list of flows a cluster cannot do without is admitted by the pods' real labels, and that a list of flows the rules exist to refuse is refused (a tenant's pod to OpenFGA, the vault, the seal, the bouncer's authorization port, the operator's listener; a publishing proxy to anything in the kernel; cert-manager to the bouncer). It also fails when a Go file or a template of the operator's chart gains an in-cluster address nobody classified. `make test-kernel-network-policies` renders the operator's chart and the bootstrap chart and fails on a port, a Service or a webhook in a kernel namespace that the inventory does not have, and on a pinned chart whose version is not the one the inventory was read at. No cluster ran these rules before they were merged: what they prove is that the list and the rules agree, and that the list agrees with what the repository deploys.

### 2.14 What a published entry is held to, and the limits at the edge

**Who may approve one.** Approving and withdrawing a perimeter entry is asked as `can_expose` on the tenant, never as `admin` (AD-6; `authz/model/v1/model.fga`). It is held by:

| Who | How | Withdrawn by |
| --- | --- | --- |
| The members of `gentian:tenant:<t>:perimeter` | The tenant's composition creates the group with the tenant, empty; the operator projects it as `perimeter_approver` | Taking the person out of the group |
| The cluster's administrator | The model: `admin from operated_by`, in every tenant the cluster operates. No tuple of its own, and no kernel-realm person in a tenant-realm group | The tenant withdrawing `operated_by`. Never for the platform tenant |
| The tenant's own administrators | **Only where the manifest says so**: `Tenant.spec.perimeter.adminsApprove`. Off by default and switched on by the cluster's administrator, except for the user tenant of a single-tenancy cluster, which the install, and the director's create route where the request does not say, create with it on. The operator writes the admins group into `perimeter_approver` while the manifest says so and deletes it when it does not, including one written by hand | Removing the line from the manifest |

The switch is set through the director by `can_configure` on the cluster alone, at tenant creation and on `PUT`/`DELETE /v1/clusters/{c}/tenants/{t}/perimeter-delegation`; there is no such route under a tenant, and an imported tenant does not carry it. It stands beside `spec.catalogue.delegated`, the switch for a tenant's administrators adding catalogues (AD-14), which is set the same way.

Who is in the perimeter group is changed through the registrar only by a caller who holds `can_expose` on the tenant. `can_manage_users` opens the route; for a caller without `can_expose` the registrar then refuses every write that would change who approves — the group's membership, the group itself, a new group of that name, and any write to an account in the group (address, password link, second factor, removal) — in the one place its writes to Keycloak leave from (`internal/registrar/identity/guard.go`). So a tenant's administrator who may not approve cannot become an approver by managing people. This is a rule in the registrar's code, like the one for the platform's role groups: its Keycloak credential could do all of it, and so can anybody with the realm's own administration console.

A perimeter entry is answered by a publishing proxy in the tenant's DMZ (AD-6). The proxy checks nobody, and holds every request to the following, whatever the profile says (`internal/controller/component_perimeter_config.go`):

| | |
| --- | --- |
| Methods | `TRACE`, `TRACK` and `CONNECT` are refused (405) |
| The path | Refused (400) when it has more than one reading: an empty segment (`//`), a dot segment plain or encoded (`/../`, `/%2e%2e/`), an encoded slash, backslash or NUL, a backslash, a path parameter (`;`). The proxy decides on the normalised path and sends the app the path as written, so only a path both read alike is let through |
| Declared prefixes | By whole segments: `/s` publishes `/s` and `/s/x`, not `/sx`. Anything else is 404 from the proxy |
| Denied paths | Compared without regard to case |
| A declared path | One with a character outside letters, digits and `/ . _ ~ -` is not rendered. If a *denied* path cannot be rendered the entry publishes nothing |
| Body | 10 MB (413) |
| Headers | 8 kB for the request line and for each header, 32 kB together (400/414); a header name with an underscore is dropped |
| Timeouts | 15 s to send the headers, 30 s between two reads of a body, 5 s to connect to the app, 60 s for the app to answer |
| Rate | 20 requests a second for each client address, with 200 more at once; then 429. On an entry that passes the caller's credential to the app: 5 a second, with 50 more at once |
| Concurrency | 100 requests at a time for each client address; then 429. On an entry that passes the credential: 20 |
| Identity | Every header the front door sets (`internal/bouncer`, `IdentityHeaders`: `x-gentian-subject`, `-realm`, `-session`, `-email`, `-name`, `-id-token`) is removed, with `Cookie`, `X-Forwarded-Access-Token` and the older `X-Auth-Request-*` names. `Authorization` is removed too, except on an entry declared `authMode: app` and approved as that, where it is passed on as the caller sent it. A test holds the proxy's list to the bouncer's, so a header added there is removed here |
| The connection | `X-Forwarded-For` and `X-Real-IP` are set to the one client address, `X-Forwarded-Proto` to `https`, `X-Forwarded-Host` to the host; `Forwarded`, `X-Forwarded-Port`, `-Prefix`, `-Server`, `X-Original-URL` and `X-Rewrite-URL` are removed |
| Answers | `Set-Cookie` and `X-Powered-By` are removed; `Server` names the proxy, without a version |

With the kernel's network rules on (§2.13) the proxy's pods admit the Gateway's Envoy pods and nothing else (`<name>-ingress` in the DMZ namespace), which is what makes the forwarded address the Gateway's word. With them off any pod of the cluster that can reach the proxy can state an address of its choosing: it is then counted, and shown to the app, under that one.

**Whose address.** The address the Gateway saw the connection come from, which Envoy appends to `X-Forwarded-For` -- unless the cluster is reached through the tunnel, where every connection comes from the tunnel's client and the address is read from `CF-Connecting-IP`, which Cloudflare sets and overwrites. Which of the two holds is asked of the cluster and not assumed (`edgeClientAddressHeader`): what the operator was told (`NETWORK_MODE`, `EDGE_INGRESS`), else the Gateway's own Service -- without an address outside the cluster (`ClusterIP`) nothing but the tunnel's client and the cluster's pods can reach the Gateway, so the header is the tunnel's word; with one, a caller could write it and it is not read. A load balancer or CDN that replaces the caller's address with its own makes every caller one address, and the limit then counts them together until the administrator names the header that carries the caller's (`PERIMETER_CLIENT_ADDRESS_HEADER`).

**Sign-in.** Two routes without a session take a credential, and each carries Envoy's local rate limit (`BackendTrafficPolicy.rateLimit.local`, `internal/controller/edge_rate_limit.go`): 60 POSTs a minute for each client address to a realm's sign-in pages (`/auth/realms/<realm>/login-actions/`), and the same to a sign-in sidecar's `/sso/acs`. Local means a bucket in each Envoy pod, so with two proxies an address may get twice that; it bounds how fast one address can post, and Keycloak's brute-force detection per account is what stops slow guessing. Pages, their assets and the token endpoint are not limited: the Gateway's own code exchange and apps' calls arrive at the token endpoint from a few addresses that stand for everybody.

**The numbers** are the platform's, and the cluster's administrator may change them through the operator's environment: `PERIMETER_MAX_BODY`, `PERIMETER_RATE_PER_SECOND`, `PERIMETER_RATE_BURST`, `PERIMETER_CONCURRENT_PER_CLIENT`, the three for an entry that passes the credential (`PERIMETER_CREDENTIAL_RATE_PER_SECOND`, `PERIMETER_CREDENTIAL_RATE_BURST`, `PERIMETER_CREDENTIAL_CONCURRENT_PER_CLIENT`, each held to the general one where it is set higher), `PERIMETER_CLIENT_ADDRESS_HEADER` (a header name, or `none`), `EDGE_SIGN_IN_POSTS_PER_MINUTE` (`0` turns that limit off). A profile cannot change any of them.

**What an entry's `authMode` means.** Two values are served on a perimeter entry, and neither is a check the platform makes of the caller.

- `none`: the declared paths, for anyone. `Authorization` and `Cookie` are removed, so no credential reaches the app. What still reaches it is the path, the query, the body and the other headers, so a link that carries its own secret, or a signature in a header of its own, can be checked by the app.
- `app`: the same, and the caller's `Authorization` header is passed to the app as sent. The callers are sync clients, mobile apps, API clients and webhooks that hold a credential the app itself issued; only the app can check it. The proxy does not look at it. It takes effect only when the tenant's perimeter approver approved the entry as that kind (`publicAppCredential` in the registry): an entry approved as a plain public address whose catalogue entry later declares `app` is taken down until it is approved again.

What `app` costs is said to the approver in the director's words before they approve, and is accepted (AD-1): the platform does not know or check who calls; an app password or token of a person who was removed from the tenant keeps working until the app itself revokes it; every request may be a guess at a password that lands on the app's own sign-in, which is why the rate is lower and why the app's own lock-out matters. Cookies pass in neither direction, so a client that needs the app's cookie on a public address does not work.

`jwt`, `bearer`, `basic` and `signature` are refused on a perimeter entry by the schema. They named a check at the edge that nothing made, and an entry carrying one read as protected when it was not.

**The same approver decides one thing that is not a public address.** An entry behind sign-in may declare `clientAuthorization: app`: the `Authorization` header is the app's own. Approved (`signInAppAuthorization` in the registry), the Gateway leaves the header as the browser sent it and the bouncer does not remove it. The session and the bouncer's question are required as on every entry — the OAuth2 filter admits no request on a bearer, and the bouncer is shown the session as an ID token in a header of its own ([routing.md §4.1](routing.md)) — and no platform token reaches the app. What changes for the app is that it now receives a header a signed-in person's browser chose, where before it received none; it must check it as it would on its own.

**Not built.** Limits on routes behind a session (sync clients and uploads have bursts of their own); a limit on the token endpoint; a cap on a WebSocket's duration; a global limit across Envoy pods, which needs a rate limit service; a web application firewall; the checks the withdrawn modes named (`basic`, `signature`, a bearer verified at the edge); cookies for a client on a public address; and credentials the platform issues and can revoke, which is what would end a removed person's access through an app password ([roadmap.md](../roadmap.md) 1.34).

**How it is held.** The proxy's rules are asked of the proxy: `TestTheProxyItself*` (`internal/controller/component_perimeter_nginx_test.go`) start the image the operator deploys with the configuration it renders, in front of a server that says back what it was sent, and send it some ninety paths written to leave a prefix or reach a denied path, every identity header in three spellings, oversized bodies and headers, and bursts from two addresses; and, for an entry that passes the credential, that `Authorization` arrives as sent on the declared paths and on no other, that `Cookie` and every identity header still do not, and that guesses from one address are answered 429 at the lower limit. They need docker and are skipped without it. The sign-in limits are validated against the pinned release's definitions, and `TestEnvoyGatewayAcceptsTheRateLimits` puts them through that release's own translator where its `egctl` is installed.

### 2.15 Telling an app that a person signed out

When a person signs out, the realm ends its session and the front door stops admitting that
browser within five minutes ([routing.md §4.2](routing.md)). What that leaves is each app's own
session: on a shared browser the next person, admitted as themselves, could be shown the previous
person's account. So the realm tells the app, and the app ends its session
([iam.md §1.12](iam.md)). For an app with its own OIDC client that is an OpenID Connect logout
token, posted to an address the platform registers with the client. This section is what that
address may be, who can reach it, and what is left. The sign-in sidecar's side is §2.12.

**Before.** A profile wrote a whole address (`backchannelLogoutUrl`), and it was registered as
written. Every profile wrote the app's public address, where every path is behind a session; the
realm has none, so its request was answered with a redirect to the sign-in and no notice ever
arrived. And the field was a request forgery waiting for a use: a catalogue entry could make the
identity provider send a POST to any address at all, inside the cluster or outside it. The field
is now refused.

**Now.** A profile states a path and which of its own entries serves it
(`requires.services.identity.oidc.backchannelLogout`). The platform's Composition builds
`http://<backend.service>.<namespace>.svc.cluster.local:<backend.port><path>` and registers it.

**(a) What the app must verify, and what happens if it does not.** A logout token is a JWT the
realm signs. Before it ends a session the app has to check the signature against the realm's
published keys, the issuer, that the audience is its own client, and that the token carries the
back-channel logout event; it then ends the session the token's `sid` names, or the sessions of
its `sub`. The platform cannot check that an app does. An app that ends a session on a token it
did not verify lets whoever can reach that path sign a person out of it — an annoyance, and
never a sign-in: the path makes no session. So the declaration is a review item: the profile's
customization record says what the app checks, and the app is run once against Keycloak with a
token signed by another key and an unsigned one. For the two that declare it and were run:
Nextcloud's `user_oidc` checks signature, audience, event, the absence of a nonce, and that it
holds a session for the token's `sid`, `sub` and issuer together; XWiki's OIDC authenticator
checks signature, issuer and audience against the keys the realm publishes, provided a provider
is configured, which the profile does. (Open WebUI checks the token as well, and cannot end the
session without Redis, so its declaration has no effect yet;
[roadmap.md](../roadmap.md) 2.27.)

**(b) Who can reach the path.** Nothing new. The address is the app's Service inside the
cluster, and what reaches an app's pods is what the tenant's network policies already admit
(`internal/kernel/netpolicy`):

- the identity provider's namespace — Keycloak, and the realm, client and group Jobs that run
  beside it — on every port, which is the path the notice takes;
- the edge: the Gateway's Envoy pods, and with `KERNEL_NETWORK_POLICIES` off every pod of the
  edge namespace;
- the control namespace: the operator and the programs beside it;
- pods of the same app; another app of the tenant that holds a granted contract with it; the
  app's sign-in sidecar, on the one port its profile declares;
- the node the pod runs on.

No other app of the tenant, no other tenant, and nothing outside the cluster. The same path at
the app's **public** address is where it always was: behind the session and the bouncer, so a
signed-in person who may use the app can post to it. That is unchanged, and it is why (a)
matters for an app that verifies nothing.

**(c) The realm cannot be pointed at another address.** The address is built, never written.
Each part is held to what it has to be, by the definition's rules and again by the Composition,
which registers nothing if one fails:

| Part | From | Held to |
|---|---|---|
| The Service | the `backend.service` of the entry the profile names | an entry of this profile, with no `backend.component` — this component's own Service — and a plain name: one DNS label, so nothing in it can end the host or begin a path |
| The namespace | the install's, from the claim the operator writes | not the profile's to say |
| The port | that entry's `backend.port` | a port number |
| The path | the profile | segments of letters, digits, `_`, `~`, `-`, with single dots inside: no query, no fragment, no `..`, no `//`, no `@` |
| Scheme, `.svc.cluster.local` | fixed | — |

So the realm is told to call a Service of that name in the component's own namespace and
nothing else: not another namespace, not a host outside the cluster. For the sign-in sidecar the
Service is the operator's own (`<app>-sign-in`) and the claim carries only the port.

*What this does not close.* The Service is the app's chart's to define. A chart that gave the
Service of that name hand-written endpoints instead of its own pods could make the realm post to
an address of the chart's choosing after all. What such a request can carry is fixed: a POST, a
plain path, and a body the chart does not choose (a logout token for its own client, which no
other client accepts). What admits the identity provider's namespace is every tenant's pods,
Keycloak's own database and the mail relay, and the operator's membership listener. This is the
position a chart already has towards the Gateway, which routes to the same Service; it needs a
chart written to do it, where the old field needed one line of a profile. The platform does not
check a Service's endpoints.

**(d) Plain HTTP inside the cluster.** The token is signed and is not a credential: it names the
issuer, the client, the person's identifier and the realm session, and it is good for ending
that session and nothing else. Something that could read the hop would learn who signed out of
what; something that could change it could only make it invalid. It is the same hop, and the
same absence of TLS, as an app's connection to its database.

**(e) The name the app is called by.** The app sees its Service's name in the `Host` header, not
its public one. An app that refuses a host it does not know needs that name among the hosts it
trusts. Trusting it lets the app answer a caller under that name; it does not make the app
reachable from anywhere it was not, because reaching it is the network policy's question and
the name is only what an admitted caller writes in a header. The risk a host list guards
against — a link in a mail built from a `Host` header an attacker chose — needs the attacker to
reach the app under that name, which no browser can. For Nextcloud as the catalogue configures
it no name had to be added at all: with `overwritehost` set it does not hold a request to its
list of trusted names, and XWiki answered under its Service's name as it is.

**(f) The identity provider's own egress.** The kernel's network rules restrict who may connect
to a kernel pod, not where one connects to: `kernel-authentication` is `egress: open` in the
inventory the rules are generated from (`internal/kernel/kernelnet/inventory.yaml`), which
names this flow among the reasons. Nothing had to be opened. If that namespace's egress is ever
restricted, this is a flow it must keep: from Keycloak to tenant namespaces, on each app's
backend port and on a sign-in sidecar's 8081.

**What the notice does not do.** Keycloak 26.8.0 posts it once, when a person signs out or an
administrator ends their session. It does not post again if the app was not there, and posts
nothing when a session only runs out. So it ends an app session at once in the ordinary case and
bounds nothing: an app's own session lifetime is still the bound, and for an app that cannot be
told it is the only thing there is.

| Control | State |
|---|---|
| The address the realm posts to is built from the profile's own entry, in the install's namespace; a profile names none | Built (`app-default.yaml`); render fixtures for an accepted declaration and two refused ones |
| The free-form address is refused, with a message naming the field that replaces it | Built (the definition's rules; a test holds each) |
| The catalogue's own profiles are admitted by those rules | Built; a test reads the published bundles |
| The notice arrives at the Service's address and ends the session; a token of another key, an unsigned one and a repeated one end nothing | Shown for Nextcloud (`nextcloud-base-ce`) and XWiki against their images and Keycloak 26.8.0 (gentian-apps, `e2e/oidc-sign-out`) |
| An app checks the token before it ends a session | **The app's**, reviewed per profile and recorded in its customization record; nothing enforces it |
| A Service's endpoints are the app's own pods | **Not checked** |
| The notice is sent again when it did not arrive, or when a session runs out | **Not done**: Keycloak does neither |

### 2.16 Who may open a mailbox with a sign-in token

Only on a cluster whose `mail.serviceMode` is `system`, and only for a tenant with mailboxes on it (`spec.mail.mode` `selfhosted`, the default there) in a realm of its own. A mail program presents either an app password or a person's access token (XOAUTH2). For a token, four things have to hold, and each is checked by a different party:

| Check | By | What makes it true |
| --- | --- | --- |
| The token names `gentian-dovecot` in its audience | Keycloak, when Dovecot introspects as `gentian-dovecot` | the realm's client scope `mailbox`, whose mapper adds that audience to an access token |
| The token carries the scope `mailbox` | Dovecot (`scope = mailbox`) | the client asked for `mailbox` at that sign-in, and has the scope |
| The client has the scope | Keycloak, at sign-in (`invalid_scope` otherwise) | the app's profile declares `requires.services.mail.imap.tokenSignIn`; the scope is an optional scope of that app's own client and of no other |
| The mailbox is the person's | Dovecot | the user is the token's `email` claim, and the address the mail program gave has to equal it |

So an app that declares it can open the mailbox of a person who signed in to it, with the token of the sign-in at which it asked for `mailbox`, for as long as that token is valid. It cannot open anybody else's, and its other tokens -- the ones it holds for its own session or relays to what it calls -- open none. An app that does not declare it cannot obtain such a token at all, and a token of another realm is introspected by a realm that does not know it.

- **The declaration is a grant, so it is explicit.** `mail.imap: {}` alone means the app reads mail and is given the server's address; it does not make its tokens mailbox keys.
- **Withdrawn with the declaration.** A profile that stops declaring it keeps its optional scopes without `mailbox`; tokens already issued stay valid until they expire.
- **A token with the scope is a mailbox key wherever it travels.** An app should ask for `mailbox` in a sign-in of its own for mail and not relay that token.
- **`gentian-dovecot` checks the audience like every other client.** The client attribute that exempts one client from the check is set nowhere (`TestNothingSwitchesTheIntrospectionAudienceCheckOff`).
- **Not served:** the kernel realm, and so the platform tenant, has no `mailbox` scope; a cluster that relays its mail has no Dovecot. The declaration is accepted there and grants nothing.
- **The network is not the gate.** A profile that declares `requires.services.mail` is opened `system-mail` (`kernel-access-<app>`), and Dovecot's IMAP ports admit every tenant's pods, and the proxy for everybody else (§2.8); the token is the check.

---

## 3. Architecture

### 3.0 Implementation status

Read from `internal/`, `crossplane/`, `kernel/` and the administration
console's backend (gentian-ui, `apps/admin-console`). A row changes only when
the code does.

| Control | Status | Where |
| --- | --- | --- |
| Keycloak per-tenant realms, kernel realm, OIDC for the desktops, the consoles and apps | Implemented | `suze.yaml` installs Keycloak; realms and clients come from `tenant-default.yaml` and `identity_reconciler.go` |
| Keycloak group → OpenFGA tuple sync | Implemented, from events | Membership is stored as `group#member` tuples, a projection the **operator** writes from Keycloak's signed event-listener statements (`membership_listener.go`, `internal/membership`). The poll on a timer is gone from the code. The operator is the writer AD-12 names; the director writes nothing to the store. Not built: nothing reconciles the stored memberships toward Keycloak, and a change of a person's groups ends none of their sessions |
| Keycloak's master administrator credential | **Held by the operator** | The operator reads the `keycloak-admin` Secret and hands it to the Jobs that configure realms, clients and groups (`identity_reconciler.go`, `internal/keycloak/shell_helpers.go`). The process that writes the rights store is therefore also the one that can change any identity. Narrowing it is **Target** |
| `AppGrant` → tuples | **Not built** | A grant opens the network path of a contract and gives the consumer a key (§3.4). It does not reach the rights store: `app_grant_reconciler.go` records the object and writes no tuple, the model's `contract` type has no writer, and nothing asks about it |
| Gateway ext-auth calling OpenFGA `Check` on every session route | Implemented | `internal/bouncer`, attached by `internal/controller/bouncer.go`; the session filter runs first and the bouncer refuses a request without a token it verified ([routing.md §4.1](routing.md)). Fails closed |
| Session cookies: per host, encrypted, `SameSite=Lax`; frame policy naming the tenant's own desktop | Implemented | `zoneSecurityPolicySpec`, `componentFramers` ([routing.md §4.2, §4.3](routing.md)) |
| Sign-out reaching the apps | **Implemented where an app can be told; otherwise bounded by the app** | Sign-out ends the realm session and the edge's cookies. The realm then tells an app inside the cluster: an app with its own OIDC client at the path its profile declares (`backchannelLogout`), at an address the platform builds from the entry's own Service (`app-default.yaml`); an app behind the sign-in sidecar through the sidecar's `/sso/logout` (`signin_sidecar.go`). Shown end to end for Nextcloud (`nextcloud-base-ce`), XWiki, Docmost and OpenProject. Not told, or told to no effect: Activepieces (an hour at most), Open WebUI, Element, Mathesar, Odoo, and `nextcloud-base-od` until it is shown — their sessions last as long as the app keeps them, though the front door refuses the person's next request. The realm tells once and not when a session only runs out (§2.15, [iam.md §1.12](iam.md)) |
| Identity headers to a backend, signed | **Target** | `x-gentian-*` are plain headers. What makes them the bouncer's word is that only the Gateway's Envoy pods reach the backend: a NetworkPolicy, and for the kernel side one that is off by default (§2.13). The same holds for a sign-in sidecar's `/sso/login` (§2.12) |
| An app's own bearer token through the front door | Implemented, **on approval** | An entry that declares `clientAuthorization: app` keeps the header once the tenant's perimeter approver approved it; the session and the bouncer's check stay required and no platform token reaches the app (`approvedClientAuthorization`, `internal/bouncer`; [routing.md §4.1](routing.md)). Every other session route still drops it, but for two kernel consoles that keep theirs by the kernel's own rule: Keycloak's administration console, and the model gateway's where the claim switches it on (§2.9). A client with no browser session cannot use a session route at all: it needs a public entry of `authMode: app` |
| The session's tokens stop at the edge: a backend gets its own cookies, the identity headers, and a bearer only where its exposure says `forwardToken` | Implemented | `internal/bouncer/cookies.go` rewrites the `Cookie` header without the edge's cookies on every allowed request of a session route; the names come from the route table ([routing.md §4.1](routing.md)) |
| Tenant namespace + NetworkPolicy default-deny egress | Implemented | `internal/kernel/netpolicy/` — tenant namespaces only |
| Per-app egress to the stores a profile declares | Implemented, mail and identity not narrowed | `internal/kernel/netpolicy/kernel.go`, policy `kernel-access-<app>`; see §2.6 |
| NetworkPolicy in the kernel namespaces | Implemented for ingress, **off by default**; egress open | With `KERNEL_NETWORK_POLICIES=true` each of the eleven kernel namespaces refuses an ingress nothing lists, from one inventory (`internal/kernel/kernelnet/inventory.yaml`) the policies are generated from; webhook ports admit any source, because no portable rule names the API server (§2.13). Applied by the installer with the namespaces. Off until a cluster has run them; without the switch a kernel pod is selected only by the policies its own chart delivers. No kernel namespace restricts egress (gap G28, the egress half) |
| NetworkPolicy in system and shared namespaces | **Partial**: ingress to twelve servers, egress from one | One on each shared store -- PostgreSQL, MariaDB, Redis, MinIO -- admitting tenant namespaces and the platform's named clients (§2.7). One each on Dovecot and Postfix, which no longer face the internet, and one on the proxy in the mail DMZ that does -- the one pod of these whose egress is restricted too (§2.8). One on each server of the model gateway's namespace (§2.9). The kernel's own PostgreSQL and the operator keep the policies their charts deliver, beside the kernel namespaces' rules |
| Publishing proxy: declared paths only, one reading of a path, every identity header removed, size, time and rate limits per client address | Implemented | `component_perimeter_config.go`; run against the proxy itself (§2.14) |
| Publishing proxy: passing the caller's credential to the app (`authMode: app`) | Implemented | Only for an entry approved as that kind; `Cookie` and identity headers still removed; a lower rate per client address (§2.14). The platform checks no caller: accepted (AD-1) |
| Publishing proxy: a caller check at the edge (`basic`, `signature`, `jwt`, `bearer`) | **Target**; the values are refused | The schema refuses them on a perimeter entry, so no entry claims a check that is not made (§2.14) |
| Publishing an entry: who may approve (`can_expose`) | Implemented | The perimeter group's members, the cluster's administrator in a tenant its cluster operates, and the tenant's administrators only by the cluster administrator's switch; the registrar holds back who is in the group (§2.14) |
| Publishing an entry: approval by the tenant's perimeter approver, of what an installed app declares | Implemented | The director lists every perimeter entry an installed app declares and refuses an approval of anything else, or with a main-address setting that is not the entry's (`internal/director/api/exposure_requests.go`, `internal/addresses`; [routing.md §5](routing.md)) |
| A website on the cluster's main address | Implemented; the cookie finding is open | §2.10: single-tenancy only, two people say so, the approver acknowledges the rule. A script there can still disturb sign-in on the other addresses |
| Address names the platform keeps | Implemented | `internal/hostnames`, asked by the director and the operator (§2.11) |
| Default profiles the installer places | Implemented at a stated digest, and compared with the recorded bundle at rollout | Written only when the file hashes to the digest its catalogue's index lists, or to a pin, with its bundle and origin (§2.11). The index is not signed. The operator holds the Component it creates for a default profile to the bundle recorded with the profile; nothing apart from the profile states that digest |
| Mail: one proxy faces the internet, the mail servers do not | Implemented; proven in local containers, not on a cluster | `kernel/services/mail-edge`, in `system-mail-dmz`: PROXY protocol, TLS passed through, limits per client address (§2.8). Egress from `system-mail` is open |
| A mailbox opened with a sign-in token | Implemented | Only for an app that declares `requires.services.mail.imap.tokenSignIn`, by the scope `mailbox` (§2.16) |
| Sign-in sidecar | Implemented, with the weaknesses listed | Only from a cluster catalogue's bundle pinned by digest, never in the kernel realm (§2.12). It can become anybody in its app |
| The rights check for a component (`requires.services.rights`) | Implemented | A key per component for one question at the bouncer -- may this person use that app of my tenant -- instead of the store's key (`rights_check.go`, `internal/bouncer/check.go`). Platform-trust profiles only |
| A removed person's mailbox | Implemented; proven in local containers, not on a cluster | Whoever removes the person chooses archive or delete; no default, and the registrar refuses a removal without the choice. The registrar writes the choice down (`MailboxRemoval`, the one kind it may write in the cluster) and has no access to `system-mail`. The operator acts only on an address of the tenant's mail domain that no person of any realm on that domain holds, after the address's mail passwords are gone ([mail.md §5c](mail.md)). Mail to the address is refused at `RCPT` (550, unknown recipient) while the record stands and nobody holds the address; a deleted mailbox's record is removed after 30 days, and the recipient policy decides again from then |
| A tenant's backup and deletion | Implemented | A bundle (format 3) holds what a deletion destroys, mailboxes included, and a deletion destroys the mailboxes. Rights recorded as granted are not written into a tenant made new for the restore (`TenantRestore.spec.intoNewTenant`); they are named instead ([data-lifecycle.md](data-lifecycle.md)) |
| Approval path for profile-declared egress | Implemented | `requires.privileges.egress` is a request. A rule reaches the NetworkPolicy only once it was granted by name on the install (`internal/security/privilege.go`, `GrantedEgressRules`). The tenant's administrator approves it (`can_approve_privilege`) and the director writes the grant as a commit (`internal/director/api/privileges.go`). The other two kinds are less far: a pod-security waiver takes effect when the cluster's allowlist names it (`PlatformSecurityPolicy`, `mac_waiver_reconciler.go`), whether or not the security officer granted it on the install, and a requested cluster role is recorded when granted and created by nothing |
| Pod-security admission (privileged, host ns, non-root, hostPath, caps, priv-esc) | Implemented | `kernel/security/kyverno/policies/` |
| Gateway rate limit | **Partial**: sign-in posts only | Envoy's local limit per client address on the identity provider's sign-in pages and on a sign-in sidecar's answer path (`edge_rate_limit.go`, §2.14). Routes behind a session, the token endpoint and WebSocket duration: **Target** |
| Service mesh, SPIFFE/SPIRE, workload identity | **Target**, with one exception | The operator's app-lifecycle listener admits its two callers, the director and the usher, by ServiceAccount: each presents a projected token for the audience `gentian-os-operator` and the operator asks the API server whose it is (`internal/applifecycle/auth.go`). Every other call between platform services still rests on a shared key or on the person's token |
| One credential per process at the rights store | **Target** | OpenFGA has one preshared key. The operator, the director, the bouncer, the usher, the custodian and the registrar all present it, and it can write |
| Agent identities, tokens on behalf of a person for an agent, `agent`/`task` types | **Target** | the model (`internal/director/authz/model.json`) has no such types |
| Human-identified secret writes (token exchange, no service token) | Implemented | `internal/custodian/` |
| Human-identified configuration writes | **Target** | the director verifies the person and commits; the operator's lifecycle API admits only the director's ServiceAccount to its commands and still trusts the `X-Gentian-Actor` name it passes |
| OpenBao policy per tenant | Implemented | `tenant-default.yaml` |
| OpenBao policy per (tenant, app) | **Target** | `app-default.yaml` composes none |
| Record of administrative changes | **Partial** | A change to declared state is a commit the director writes, naming the person, the relation and the object that allowed it; the administration console lists them (`/admin/changes`). A change to a person is recorded by the registrar with who asked and what allowed it (`internal/registrar/record`). A sign-in, a refused request and a secret read are recorded nowhere |
| Decision log, request-id correlation | **Target** | — |
| Commit signing and verification | Implemented, where the deployments repository names the keys | The director signs every commit (`internal/director/gitops/signing.go`); the installer signs its own with the break-glass key, found by its recorded id (`scripts/lib/signing.sh`). Argo CD verifies through the AppProject's `sourceIntegrity` and its keyring (step `B-10`). A repository without key ids renders no policy, and the director then commits unsigned and says so |
| Images named by release | **Partial** | No image under `kernel/`, `charts/` or `crossplane/` may name `latest`, and the model gateway's is held to tag and digest (`make lint-image-pins`, §2.9). Other images are pinned by tag; the Keycloak event listener's follows a branch tag |
| Image signature verification | **Target** | — |
| What the installer downloads | **Partial** | The OpenBao CLI is fetched from the release's address and held to a checksum (`make test-openbao-cli-download`). The default profiles are held to the digest their catalogue's index lists, or to a pin (`make test-default-profile-digest`); the index itself is unsigned (above) |
| Rotation rolling app workloads (Reloader) | Partial | annotation on the operator Deployment and a few kernel services (Keycloak, Redis, Dovecot). No composition adds it to a tenant app; an app is rolled only where its profile sets the annotation in its own chart values, as some catalogue profiles do (gap G13) |
| Admission guard against literal secrets in `Release.set` | **Target** | — |

Keycloak runs with its release's default features and none added; what that
leaves reachable, and why each newer capability stays inert, is listed in
[iam.md §1.10](iam.md). The identity host's public paths are
`/auth/realms/` and `/auth/resources/`, so a realm-level endpoint a new
Keycloak release adds is public unless the realm keeps it off: check that list
on every Keycloak upgrade.

### 3.1 Component roles

| Component | Role | License |
|---|---|---|
| **Keycloak** | Authentication authority + token issuer (*who you are*). **Per-tenant realms** (not Organizations-as-isolation); kernel realm brokers login; SAML/OIDC brokering; token exchange where §3.0 lists it. Service accounts for agents are target. See [iam.md](iam.md), [admin-console.md](admin-console.md). | Apache 2.0 |
| **OpenFGA** | ReBAC authorization PDP (*what you may do*). Today the model holds people, groups, the cluster, tenants, apps and revoked sessions. Agents, assets, Conditions, contextual tuples and the derived-ceiling schema are target. | Apache 2.0 |
| **Director** | Turns an authorised request into a signed commit to the deployments repository. It asks OpenFGA before each one and writes nothing there. Keycloak decides no permission; OpenFGA changes no identity. | Implemented (`cmd/director`) |
| **Operator, as the store's writer** | The authorization store is written by the operator and by nothing else on purpose: role-to-group assignments from the Cluster claim, tenants and apps from what Argo CD applied, and membership as `group#member` tuples from Keycloak's signed event-listener statements -- a projection, never edited in place. The design named the director for this (AD-2, AD-12); the code does not, so that the process holding the push credential holds no reason to write a relation. What enforces "on purpose" is weak: OpenFGA has one preshared key, every process that asks the store presents it, and it can write. | Implemented (`authz_projection_reconciler.go`, `membership_listener.go`); per-process store credentials are **Target** |
| **Provisioning bridge** | The operator makes an `IntegrationBinding` for each pair of installed apps that share a contract and turns an `AppGrant` into the network path and the keys of §3.4. Neither reaches the graph. It no longer polls Keycloak for group membership: that arrives as events (row above). The operator still holds Keycloak's master administrator credential, for the Jobs that configure realms. | **Partial** |
| **MAC backbone** | K8s namespaces per tenant, NetworkPolicy default-deny egress in those namespaces, Kyverno pod-security admission (implemented); the same default-deny in the platform tiers, service mesh + SPIFFE/SPIRE (target). | Apache 2.0 / OSS |
| **PEP** | Named enforcement points — Envoy Gateway ext-auth, the director, the custodian, the MCP gateway — calling OpenFGA `Check`, ideally over the OpenID **AuthZEN** Authorization API so PDPs stay swappable. The bouncer behind the Gateway, the director, the custodian, the usher and the registrar call `Check` today (§3.0), over OpenFGA's own API; AuthZEN and the MCP gateway are target. | OSS |
| **ITAM source of truth (optional)** | NetBox (best license fit) / GLPI / Snipe-IT feeding device & asset objects into the graph. Target: nothing is built. | Apache 2.0 / GPL / AGPL |

### 3.2 Design rationale

For a greenfield, cloud-only sovereign OS:

- **Keycloak-native identity.** Keycloak owns identities per tenant realm, backed by its own Postgres, and is the only place membership is changed. OpenFGA holds a projection of it, fed by Keycloak's event listener, to compute *may* — authority is separated, not copies (principle 2).
- **OpenFGA ReBAC** replaces coarse group-only RBAC. One relationship graph is meant to model humans, agents, apps, and assets — no role explosion. Today it holds people, groups, the cluster, tenants and apps (§3.0).
- **Layered isolation** (§2) — MAC backbone, identity, and authorization are independent enforcement planes.

### 3.3 Reference architecture

```mermaid
flowchart TD
    Shell["Gentian desktops<br>tenant desktop per tenant · platform console<br>= tenant-platform's desktop (AD-10)"]
    
    Users(("Humans /<br>Agents login"))
    
    subgraph Identity ["Authentication"]
        Keycloak["KEYCLOAK (IdP / AuthN)<br>realms, clients, service accounts"]
    end
    
    Director["OPERATOR (writes the store; the director only asks it)<br>membership projection from Keycloak events<br>+ structure: roles, tenants, installs<br>+ Integration Binding + ITAM conn."]
    
    AgentsWorkloads["Agents / Workloads"]
    Apps["Front door (Envoy Gateway + bouncer),<br>platform services, apps ◄── PEP"]
    
    OpenFGA["OPENFGA (ReBAC PDP)<br>user:* agent:* app:* group:* tenant:*<br>document/db:* device:* task:* contract:*<br>Conditions (TTL / ABAC) · derived-ceiling"]
    
    ITAM["ITAM source of truth (opt.)<br>(NetBox / GLPI / Snipe-IT)"]
    
    MAC["MAC BACKBONE (peer to all of the above, not inside it):<br>K8s namespaces/tenant · NetworkPolicy default-deny<br>egress · service mesh + SPIFFE · Kyverno/OPA admission"]
    
    Shell -->|"user and group administration"| Keycloak
    Users --> Keycloak
    Keycloak -.->|"OIDC / OAuth2 / SAML brokering<br>RFC 8693 Token Exchange → tokens"| Apps
    
    Keycloak -->|"event-listener feed: signed membership statements"| Director
    Keycloak -->|"(optional) SPIFFE/SPIRE → SVIDs (mTLS)<br>for autonomous workload agents"| AgentsWorkloads
    
    AgentsWorkloads -->|"acts via OBO token (≤ user)"| Apps
    
    Shell -->|"installs, grants<br>(commits by the director,<br>applied by Argo CD)"| Director
    Director -->|"writes every tuple"| OpenFGA
    Apps -->|"AuthZEN Check<br>+ contextual tuples: task TTL, acting_for,<br>device posture — never memberships"| OpenFGA
    
    ITAM -.->|"device/asset + contract-consumer edges"| OpenFGA
```

The diagram shows the target. Agents, SPIFFE, AuthZEN, contextual tuples and the ITAM feed are not built; §3.0 says what is.

**Decision flow (target):** (1) principal authenticates to Keycloak → OIDC token (agents via client-credentials or Token Exchange carrying `act`). (2) Keycloak's event listener pushes signed membership changes to the operator, which writes them as `group#member` tuples — a projection, never edited in place; the operator also writes structure (roles, tenants, installs) from the objects Argo CD applied from the director's commits, so a relation follows a commit only once it has been applied; `IntegrationBinding` reconciles cross-app credentials. (3) PEP receives request + token, calls OpenFGA `Check` (over AuthZEN), passing runtime facts — a task's TTL, `acting_for`, device posture — as contextual tuples. Memberships are already in the graph and no group travels in a token for a platform decision. (4) OpenFGA traverses the graph (principal → group/org → resource/device, plus task-scoped delegation with TTL Conditions, plus derived-ceiling) → allow/deny. (5) Independently, the MAC backbone enforces tenant isolation and egress *regardless* of the authZ result. (6) Sensitive ops use consistent reads; the Watch API streams tuple changes to an audit log. Today (1) runs for people, (2) runs as written, and (5) runs for tenant namespaces and, for the platform's namespaces, as far as §3.0 says. (3) and (4) run at the front door and at the platform's own services, with the relation and the object and no contextual tuples; agents, tasks, AuthZEN and (6) are target.

### 3.4 Application permissions — catalogue contracts and grants

What an app needs from the platform and from other apps is declared in its **`ComponentProfile`**. A declaration is a request and grants nothing. Access to another app is wired by an **`IntegrationBinding`** and limited by an **`AppGrant`**. This mirrors Android's manifest (`<uses-permission>` = intent) vs. the separate platform/user grant — **the app declares; it never grants itself access to another tenant or app.**

Field reference and deployment flow: [app-catalogue.md](app-catalogue.md).

#### Terminology — manifest language vs CRD fields

| Concept | `ComponentProfile` field | Type |
|---|---|---|
| Contracts the app **provides** to other apps | `spec.provides[]` | `{ name, protocol? }` |
| Contracts the app **may consume** from other apps | `spec.integrations[]` | `{ contract, provider?, capabilities? }` |
| Platform services (identity, database, object storage, cache, mail, model gateway, …) | `spec.requires.services` | Separate from contracts |
| What goes beyond the default posture (pod-security waiver, egress, cluster roles) | `spec.requires.privileges` | Requests, answered per install (§3.0) |
| Where the chart takes what was provided | `spec.package.valueMapping` | Chart value keys per service |

A contract is a name (`file-store`, `project-management`) two profiles agree on. Nothing in the cluster holds a contract's definition or checks a call against one.

#### Three layers — declaration, wiring, authorization

| Layer | Object | Scope | Written by | Status |
|---|---|---|---|---|
| **Declaration** | `ComponentProfile` | Cluster (one per catalogue entry) | The catalogue's maintainer; it reaches the cluster in a bundle fetched from a catalogue | **Implemented** |
| **Wiring** | `IntegrationBinding` | Tenant namespace, one per consumer and contract | The operator, when the consumer and a provider are both in `Tenant.spec.apps` | **Implemented** |
| **Grant** | `AppGrant` | Tenant namespace, one per consuming app | The director, as a commit, for a person who holds `can_grant` on the tenant; and the operator (below) | **Partial** |

Do not conflate them:

- **`requires.services`** — what the platform provides before the app runs (an OIDC client, a database, a mailbox, …). The credentials are written to OpenBao and reach the chart through `package.valueMapping` (§8). Not a contract between apps.
- **`provides` / `integrations`** — what the app offers to, or may use from, other apps of the same tenant. An absent provider is normal.
- **`IntegrationBinding`** — the record that a consumer and a provider of one contract are both installed, with the capabilities the consumer's profile asks for.

#### 1. Declaration — `ComponentProfile` (developer-authored, static)

`ComponentProfile` is **cluster-scoped** — one object per catalogue entry, shared by every tenant that installs it. It is the *upper bound* of what the app can ask for, not an authorization decision.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: demo-app                     # cluster-scoped
spec:
  classes: [app]
  launch: tile
  trustTier: certified
  version: "1.0.0"

  package:
    chart:
      repository: oci://registry.example/charts
      name: demo-app
      version: "1.0.0"
    valueMapping:                    # where what was provided goes in the chart's values
      oidc:
        issuerKey: "oidc.issuer"
        clientIdKey: "oidc.clientId"
        clientSecretKey: "oidc.clientSecret"
      database:
        hostKey: "database.host"
        nameKey: "database.name"
        userKey: "database.user"
        passwordKey: "database.password"

  requires:
    services:                        # platform services, not contracts
      identity:
        oidc:
          clientId: demo-app
          accessType: CONFIDENTIAL
      database:
        engine: postgresql
        databasePerTenant: true

  provides:                          # contracts this app serves to other apps
    - name: project-management
      protocol: http-json

  integrations:                      # contracts this app may consume, if a provider is installed
    - contract: file-store
      provider: file-store-app       # optional: the provider's profile name
      capabilities: [webdav:read, webdav:write]

  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: demo
      backend:
        service: demo-app
        port: 8080
      tile:
        displayName: "Demo App"
        logo: data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4=
        relation: can_launch
```

A tenant's administrator installs an app by its profile name (an entry in `Tenant.spec.apps`, written by the director) and cannot edit the profile.

#### 2. Wiring — `IntegrationBinding` (operator-authored, per tenant)

For each `spec.integrations[]` entry of an installed app the operator looks for a provider among the tenant's other installed apps: the profile the entry names in `provider`, or else any whose `spec.provides` carries the contract. Where it finds one it makes an **`IntegrationBinding`** in the tenant's namespace and removes it when either app goes:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: IntegrationBinding
metadata:
  name: demo--consumer-app--file-store     # <tenant>--<consumer>--<contract>
  namespace: tenant-demo
spec:
  contract: file-store
  provider:
    app: provider-app
    namespace: tenant-demo
  consumer:
    app: consumer-app
    namespace: tenant-demo
  capabilities: [webdav:read, webdav:write]
  auth:
    method: api-key
    vaultPath: gentian-os/tenants/demo/contracts/file-store
```

With the binding the operator writes a generated password to `vaultPath`, once per tenant and contract. A consumer whose profile maps the contract under `package.valueMapping.integrations` gets it, with an endpoint and a user name where the binding's reconciler knows them (`calendar` and `contacts` only), as chart values. No profile of the catalogue maps one today, and this delivery does not wait for a grant. The binding is topology and secret wiring, not a per-person permission.

#### 3. Grant — `AppGrant` (tenant-authored, per install)

An **`AppGrant`** names, for one consuming app, the capabilities of each contract it may use. The director writes it as a commit (`PUT /v1/tenants/{t}/grants/{app}`, relation `can_grant`).

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppGrant
metadata:
  name: demo-app
  namespace: tenant-demo
spec:
  app: demo-app
  consume:
    - contract: file-store
      granted: [webdav:read]             # webdav:write withheld
  allowConsumers:                        # the provider's side; read by nothing today
    - app: crm-app
      contract: project-management
      scope: [tasks:read]
```

What a grant does today (`tenant_network_policy.go`, `internal/kernel/netpolicy`, `contract_keys.go`):

- **The network path.** A tenant's namespace is closed in both directions. For a binding whose consumer was granted at least one capability, two NetworkPolicies let the consumer's pods reach the provider's and the provider's admit them. Without a grant neither exists.
- **A key per consumer.** The consumer gets a Secret `contract-key-<consumer>-<contract>` with a random key, and the provider a Secret `contract-callers-<provider>` with the SHA-256 of each granted consumer's key and its name, so a provider can tell which of its consumers is calling. Both go when the grant goes. Mounting the Secret, sending the key and checking it are the two apps' own work; the platform checks no call.

What it does not do:

- **Capabilities are not enforced.** Any granted capability opens the whole path to the provider's pods. The list is written on the policy as a label and nowhere else.
- **Nothing reaches the rights store.** The model has a `contract` type; nothing writes it and no enforcement point asks about a call between two apps (§3.0).
- **The operator grants by itself.** On each reconcile of a tenant the operator writes an `AppGrant` for every consumer that has a binding, carrying every capability the consumer's profile declares (`ensureAppGrants`), under the name the director's commit uses. A declared integration is therefore granted in full as soon as both apps are installed, and a narrower grant an administrator set is overwritten by the operator's on its next reconcile.

Revoking a grant removes the network path and both key Secrets in one reconcile. It does not change the shared password at `vaultPath`, which stays in the consumer's values until the app is rolled.

#### 4. Runtime authorization — computed at the PEP (per request)

**Today.** A person signs in at their tenant's realm. The front door asks the rights store whether that person may use the app (`can_use` on `app:<tenant>/<profile>`) on every request of a session route (§3.0). The right comes from the app's Keycloak group, `gentian:tenant:<t>:app:<profile>`, whose members the operator projects into the store. **App administrators** are the members of `gentian:tenant:<t>:app-admins`; the operator gives them the role each app's profile names in `spec.hooks.provisioning.privilegedRole` (see [app-profile-guide.md](https://github.com/gentian-org/gentian-apps/blob/main/docs/app-profile-guide.md) §6h). People and groups are administered in the [administration console](admin-console.md). Between two apps there is the network path and the key of step 3, and no check by the platform.

**Target:**

```
effective access = declared (ComponentProfile)
                 ∩ wired (IntegrationBinding exists + credentials valid)
                 ∩ granted (AppGrant subset)
                 ∩ acting-user ceiling (ReBAC)
                 ∩ conditions (ABAC)
```

The most restrictive layer wins (§2.1).

### 3.5 Agentic identity

Design only: none of this section is built (§3.0).

- Each agent is a **distinct first-class identity** — a dedicated Keycloak client/service account and an `agent:` object in OpenFGA — never a shared human credential.
- Tokens are short-lived: client-credentials for autonomous agents; **RFC 8693 Token Exchange** with `act` / `may_act` for on-behalf-of a user.
- Delegation lives in the graph via the **derived-ceiling** schema (§2.3); TTL enforced by OpenFGA **Conditions**; revocation = tuple delete.
- For agent/tool endpoints, adopt the **MCP authorization** model (OAuth 2.1 resource server: validate audience, require PKCE, RFC 8707 resource indicators, no token passthrough). Track **Cross-App Access / ID-JAG** so Keycloak can later mediate agent→app access centrally.
- Add **SPIFFE/SPIRE** only when autonomous in-cluster workload agents need secret-less mTLS identity — a layer *beneath* OAuth/ReBAC, not a replacement.

*Standards note (2025–2026): the industry is converging on extending OAuth/OIDC/SPIFFE rather than inventing agent-specific protocols (IETF WIMSE, `draft-klrc-aiagent-auth`, OpenID AIIM/AuthZEN, NIST agent-identity work). Architect for these primitives; treat the specs as still in flux.*

### 3.6 Physical assets & ITAM

Design only: the model has no `device` type and nothing feeds one.

Model devices as plain **resource objects**: `type device` with relations `owner`, `assigned_user`, `operator`, `maintainer`, inheriting org scope (`device:printer-3f#can_print@user:alice`; agents the same way via `#operator@agent:print-bot`). Evolve toward full ITAM only when inventory grows: add **NetBox** (Apache 2.0, best license fit) / **GLPI** / **Snipe-IT** as the asset source of truth, projected into OpenFGA tuples by the operator, the store's one writer. Keep this layer thin.

### 3.7 Automation (n8n-like workflows)

Design only. An automation app installed today is one app with one identity and its own credential store; the platform confines it as it does any app (namespace, default-deny network, granted egress) and does nothing below.

An automation platform is a textbook **confused deputy**: a central engine holding many services' credentials and combining them in flows. Dropped in unmodified it becomes the god-mode lateral-movement engine this architecture exists to prevent. The fix is to decompose it along the same seams as everything else — **never one principal, never a central credential vault.**

| n8n concept | Maps to | Enforcement |
|---|---|---|
| The n8n **platform** | A `ComponentProfile` and a tenant's install of it (§3.4) | `requires.services` + MAC-confined namespace + **default-deny egress**, opened only by granted `requires.privileges.egress` |
| Cross-app **connectors** | `integrations` → `IntegrationBinding` | Operator-wired credentials; per-step token exchange — not a shared vault |
| A **workflow** | First-class principal / agent instance (§3.5) — *one identity per workflow*, the "each app is its own UID" port | Own `workflow:` (or `agent:`) identity; no shared vault |
| A **workflow execution** | `task:` object with TTL Condition | `user → owns → workflow → executes_as → task(ttl)`; revocation = delete `acting_for` |
| **Credentials** | JIT short-lived scoped tokens | Requested per-step from Keycloak via **RFC 8693** token exchange — no stored long-lived secrets |
| A user-owned workflow | Derived-ceiling delegation (§2.3) | `workflow ≤ owning-user` — cannot touch what the owner can't |
| A system/scheduled workflow | Machine identity | Keycloak **client-credentials** + explicit narrow grant (no human ceiling) |
| Each **node/step** touching a resource | A PEP `Check` (§3.3) | Per-step authorization, each bounded by the ceiling and the egress allowlist |
| A step needing access beyond its grant | **Human-in-the-loop** approval | AuthZEN Access Request & Approval Profile → time-boxed elevated grant |

**The two inversions from vanilla n8n:** (1) replace the central credential vault with **just-in-time, scoped, attenuated tokens**; (2) replace the single platform identity with **one identity per workflow**, each running under the derived-ceiling. A "read CRM contacts → post to Slack" flow then becomes two PEP checks plus two egress-allowlist gates, every action attributed to (workflow identity + owning user) via the `act` chain — instead of one over-privileged deputy with standing access to everything.

---


## 4. Secrets topology

```mermaid
flowchart TD
    OpenBao["OpenBao (KV v2)<br>where credentials are kept"]
    ESO["External Secrets Operator<br>sync to K8s API"]
    K8sSecret["Kubernetes Secret<br>in the workload's namespace"]
    HelmRelease["Helm release<br>provider-helm for apps, Argo CD for kernel services"]

    OpenBao -->|read| ESO
    ESO -->|writes| K8sSecret
    K8sSecret --> HelmRelease
```

The credentials of the platform's services and of apps are kept in OpenBao and
reach a workload as a Kubernetes Secret that ESO writes. No secret is put in
Git, in a CR spec or in a ConfigMap.

Some secrets are Kubernetes Secrets that never pass through OpenBao: the
installer's copy of the master password and of the kernel credentials derived
from it, in `kernel-provisioning`, from which the `Cluster` Composition makes
the OpenBao paths (§7); the signing key of Keycloak's event listener, with its
public half in `kernel-control`; the keys of a granted contract (§3.4);
and TLS keys, which cert-manager holds.

## 5. Path Layout

All under the KV mount `secret`.

```
gentian-os/
├── kernel/                           # the platform's own; no tenant in the path
│   ├── internal/master-password      #   value, salt (§6)
│   ├── database/                     #   postgresql, mariadb, cnpg, portal-shell
│   ├── cache/redis
│   ├── storage/                      #   minio, registry
│   ├── identity/keycloak-bootstrap   #   Keycloak's bootstrap administrator
│   ├── authz/openfga                 #   the rights store's preshared key
│   ├── oidc/openbao                  #   OpenBao's own sign-in client
│   ├── mail/                         #   postfix, dovecot, smtp, relay
│   ├── dns/{provider}                #   DNS-01 credential (cloudflare, route53, …)
│   ├── edge/cf-tunnel
│   ├── repositories/{name}           #   deployments and source repositories
│   ├── signing/director              #   the director's commit-signing key
│   ├── backup/                       #   recipients, destination, identity
│   ├── llm, llm-providers            #   model gateway and its upstream keys
│   ├── licence-report
│   └── argocd/github-webhook
│
└── tenants/
    └── {tenant}/
        ├── apps/
        │   └── {app}/                #   an extension of an app: {app}-{extension}
        │       ├── oidc              #   issuer, client_id, client_secret
        │       ├── database          #   host, user, password, database name
        │       ├── s3                #   access_key, secret_key, bucket
        │       ├── cache             #   host, port, password
        │       ├── smtp              #   host, user, password
        │       ├── imap              #   host, port
        │       ├── llm               #   base URL and key at the model gateway
        │       └── internal/{name}   #   a secret the profile asks to have generated
        ├── repositories/
        │   └── {repository-name}     #   username, password of a declared repository
        ├── contracts/
        │   └── {contract-name}       #   shared credential of a binding (§3.4)
        └── backup/                   #   destination, identity
```

The path helpers are in `internal/kernel/secrets/paths.go`.

One path is outside this tree: `identity/portal-admin`, directly under the
mount, where the installer keeps three client secrets on a cluster with
`secretMode: random` (§6.3). No policy but an administrator's reaches it.

Who may read what:

- **A policy per tenant**, `<tenant>-tenant-policy` (`tenant-default.yaml`),
  covers `gentian-os/tenants/<tenant>/*` and nothing of another tenant.
- **ESO** reads through one `ClusterSecretStore`, `openbao`, whose policy
  `eso-read` (`cluster-default.yaml`) covers every tenant's `apps/`,
  `repositories/`, `contracts/` and `backup/` and all of `gentian-os/kernel/*`,
  the master password and the salt included. Only the backup identities are
  denied to it. The master password's path is not denied in the same way,
  because one `ExternalSecret` reads it: the probe that reports whether it was
  supplied (§6.1).
  What keeps a secret to its namespace is therefore which `ExternalSecret` the
  Compositions write, not the store.
- Per-`(tenant, app)` policies, so that no app can read a sibling app's paths,
  are a target — the layout above is shaped for them.

### 5.1 Repository pull credentials

A tenant's registry repository (`Repository`, `type: oci`, `spec.tenant` set
by the director from the authorised route) has its credential at
`gentian-os/tenants/{tenant}/repositories/{name}`. `repository-default`
materialises it as one Secret, `repository-{name}-pull`, through a
`ClusterExternalSecret` whose selector is exactly
`gentianos.io/tenant: {tenant}` and `gentianos.io/tier: tenant` — the
namespace that tenant's apps run in, not its DMZ and no other tenant's. The
Secret is a `dockerconfigjson` that also carries `username` and `password`,
so a kubelet and provider-helm read the same object.

The component reconciler names that Secret, never its content: as
`chart.pullSecretRef` on the Release of a chart whose address lies inside
exactly one repository the tenant declared (same host and port, the
repository's path a prefix by whole segments), and in `imagePullSecrets` /
`global.imagePullSecrets` of every chart the tenant installs, after
`registry-credentials`.

What this does not give:

- **The store is not the boundary.** ESO reads through the one
  `ClusterSecretStore`, whose role reads every tenant's `repositories/*`.
  What keeps a credential to its tenant is the Composition: the selector
  above, and its refusal to make a pull Secret for a tenant's repository
  whose path is outside that tenant's `repositories/`.
- **provider-helm is one process for all tenants**, and runs as
  cluster-admin. It keeps pulled charts in a cache keyed by chart name and
  version and stays logged in to a registry host once any Release has
  presented a credential for it. A chart one tenant pulled can therefore be
  installed by another whose profile names the same chart, until the
  provider restarts. The credential is not disclosed; the chart is.
- **An image already on a node** is started for any pod with
  `imagePullPolicy: IfNotPresent` without a pull, so without asking for a
  credential.
- **A chart must take `imagePullSecrets` or `global.imagePullSecrets`** as
  a value. One that takes neither pulls its images without the tenant's
  credential, as it does without the cluster's.
- A bearer credential yields no pull Secret: both readers need a username.

## 6. Secret Generation Mode

`secretMode` on the `Cluster` claim, in the deployments repository, selects how
the platform makes a credential that nothing has stored yet: the kernel's
credentials and each app's. Three are computed from the master password in
both modes (§6.3).

| Mode | Claim value | A generated credential is |
| --- | --- | --- |
| **Deterministic** (default) | `secretMode: derived` | Computed from the master password and a per-cluster salt. A rebuild with both yields the same values. |
| **Random** | `secretMode: random` | Drawn at random, once, and stored in OpenBao. Nothing reproduces it: the stored copy is the only one. |

Two readers act on it. The installer reads the claim (`SECRET_MODE`) when it
makes the kernel's credentials. The operator reads it from the
`gentian-cluster-config` ConfigMap, which the `Cluster` Composition writes
from the claim, each time it makes a credential for an app
(`ClusterSecretMode` in `internal/controller/cluster_config.go`).

**The mode is chosen at the first install. Changing it later converts
nothing.** A credential that exists stays as it is, in both directions; only
a credential made afterwards follows the new mode. A cluster switched to
`random` therefore still holds derived credentials, and one switched to
`derived` still holds random ones that no rebuild reproduces. One path is an
exception and makes a switch back to `derived` harmful: the installer
rewrites `gentian-os/kernel/llm` on every run, and under `derived` it writes
the derived keys over whatever was there, while the model gateway's database
still has the old password.

**Under `random`, OpenBao holds the only copy, and nothing backs OpenBao
up.** A tenant's bundle holds no stored credential in either mode
([data-lifecycle.md §5](data-lifecycle.md)), the recovery kit holds the
master password and not what was drawn at random, and the platform takes no
snapshot of OpenBao. What follows:

- *A tenant's backup, restore and import work as under `derived`.* A restore
  changes no stored credential and an import makes new ones, in both modes.
  The one kind of credential an app's restored data depends on, the app's own
  secrets, is derived in both modes for that reason (§6.3).
- *A cluster rebuilt from the recovery kit* gets new kernel credentials and
  new credentials for every app, where `derived` arrives at the old ones.
  The rebuild makes every database, bucket and client anew with them, so
  nothing is locked out.
- *OpenBao's storage lost on a cluster that keeps running* is the case
  `random` does not survive. Under `derived` the installer and the operator
  write the same values again, from the master password and the salt the
  recovery kit holds. Under `random` they write new ones, which the
  running databases, buckets and sign-in clients were not created with, and
  every service and app is locked out of its own store until each credential
  is set by hand. A cluster that runs `random` has to snapshot OpenBao itself
  and keep the unseal material from the recovery kit with the snapshot.

No person's password is derived or generated. The platform administrator and
each tenant's administrator set their own through a single-use activation link
([iam.md §1.4](iam.md)). The one administrator credential among the kernel's is
Keycloak's own bootstrap administrator (`identity/keycloak-bootstrap`), which
the operator uses (§3.0).

### 6.1 Deterministic mode (`derived`)

The installer derives each kernel credential in shell (`_derive` in
`scripts/lib/bootstrap.sh`, the same function in
`scripts/bootstrap/seed-openbao.sh`):

```bash
_derive() {   # <context> <purpose>
  echo -n "${1}:${2}" \
    | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" \
    | awk '{print $2}'    # 64 hex characters
}
```

The salt is 16 random bytes generated at the first install and stored beside
the master password.

Properties:

1. **One secret to protect** instead of hundreds — with the salt.
2. **Idempotent re-seeding** — rerunning the installer produces identical
   credentials, and an existing path is never overwritten (§7).
3. **Disaster recovery** — if OpenBao is lost, the kernel credentials can be
   regenerated from the master password **and the salt**. The salt lives only
   in OpenBao and in the recovery kit (`./install.sh --export-recovery-kit`),
   so the master password alone reproduces nothing.

The master password and the salt are written to
`gentian-os/kernel/internal/master-password` (steps `B-07-crossplane-secrets`
and `B-08-seed-secrets`), and to the Secret `gentian-os-master-password` in
`kernel-provisioning`, which the `Cluster` claim refers to.

**Per-app credentials are derived by the operator.** It reads the master
password and the salt once at start and derives the one credential a
requirement needs — HKDF-SHA256 with the credential's own OpenBao path as
salt (`internal/kernel/secrets`, `Deriver`, `Seeder`) — and writes it to that
app's path. An install Job of the platform's Compositions receives its
credential through its own `ExternalSecret` and never sees the master
password. An app uninstalled and installed again gets the same credentials.

What still reaches the master password, and should not:

- ESO's policy reads all of `gentian-os/kernel/*` (§5), so the store ESO uses
  can read this path too. One `ExternalSecret` does read it:
  `credreq-master-password`, the probe that tells the custodian and
  `make check-credentials` whether the master password has been supplied. It
  creates no Secret, but ESO fetches the value to answer. The path cannot be
  denied to ESO, as the backup key's is, until that one credential's presence
  is established some other way. The salt is a field of the same path, so it
  is readable wherever the master password is.
- The catalogue's Element profile brings a Composition of its own whose
  database Job reads the master password and derives in shell.

Narrowing both, and moving the master password to a KMS or HSM, are target.

The Secret `gentian-os-master-password` in `kernel-provisioning` is not made
by ESO: the installer writes it directly (step `B-07-crossplane-secrets`).

### 6.2 Random mode (`random`)

Each credential is generated independently, by the installer with

```bash
openssl rand -hex 32
```

and by the operator from `crypto/rand`. The master password is still stored
in OpenBao in this mode, and the operator still reads it at start; it is used
only for the three credentials that stay derived (§6.3).

Properties:

1. **Independent of the master password** — whoever learns the master
   password and the salt learns none of the credentials made at random. The
   three that stay derived (§6.3) are the exception.
2. **Independent of each other** — one credential can be changed without
   affecting another.
3. **Made once** — the first value stored at a path stays the path's value
   (§7). The operator hands on a random value only after reading it back from
   the path; where the read fails it reports an error and tries again, because
   a value it could not read back may not be the stored one.
4. **Requires a backup of OpenBao** — see the note at the top of this section.

### 6.3 Scope of each mode

- **Kernel credentials** — written at cluster install into
  `gentian-os/kernel/*`, derived or random as the mode says. Credentials a
  person supplies (a DNS token, a mail relay's password, a registry login) are
  stored as given, by the installer or, once the cluster runs, by the custodian.
- **Per-app credentials** — created by the operator when a tenant installs an
  app, in `gentian-os/tenants/<tenant>/apps/<app>/*`, and likewise derived or
  random as the mode says: the database password, the bucket's key pair, the
  cache password, the sign-in client's secret, the model gateway key and the
  password of a contract between two apps.
- **Where the operator cannot learn the mode** — the ConfigMap unreadable, or
  holding a value that is neither mode — it makes no credential and the
  reconcile is tried again. A ConfigMap that is not there yet, or that was
  written before it carried the mode, reads as `derived`, which is what every
  cluster did before the operator read the mode. Under `derived`, the
  operator generates random values only when it finds no master password at
  start.

Still derived under `random`, because making each random needs a decision
that has not been taken:

- **An app's own secrets** (`spec.appSecrets`, stored at
  `…/apps/<app>/internal/<name>`). An app encrypts and signs its data with
  them, and a bundle carries no stored credential. An app purged and installed
  again, a tenant deleted and imported again under its name, and a cluster
  rebuilt from the recovery kit all read the data a bundle brings back only
  because the secret comes out the same. A random one would have to travel in
  the bundle, and a bundle deliberately holds none
  ([data-lifecycle.md §5](data-lifecycle.md)).
- **The key the Keycloak event listener signs with.** The installer computes
  the key pair from the master password on every run and writes the two
  halves to two Kubernetes Secrets; neither half is in OpenBao.
- **The kernel realm's own mail login**, on a cluster that runs its own mail
  server. It is asked for twice in one run and held only in Kubernetes
  Secrets.

Outside the `gentian-os/` tree: under `random` the installer keeps the client
secrets of Argo CD, Headlamp and the model gateway's console at
`identity/portal-admin`, directly under the KV mount, and reads them back
from there or from the Kubernetes Secret each is mounted from before it draws
a new one. Under `derived` they are computed on every run and are in no vault
path.

## 7. Write-Once Protection

Nothing the platform generates overwrites a credential that is already there.

- **Kernel paths the `Cluster` Composition makes** (`database/postgresql`,
  `database/mariadb`, `cache/redis`, `storage/minio`,
  `identity/keycloak-bootstrap`, `authz/openfga`, `mail/postfix`,
  `mail/dovecot`, `oidc/openbao`) are managed with

  ```yaml
  managementPolicies: ["Observe", "Create"]
  ```

  so Crossplane creates the path on first reconcile and observes it afterwards.
  The installer adds keys a later release introduced to an existing path, and
  only the missing ones.
- **Kernel paths the installer seeds** are written only where the path does not
  exist (`kv_put_once`). Credentials a person supplies are written as given on
  each run. `gentian-os/kernel/llm` is written on every run, because it also
  carries addresses that follow the settings; under `random` the keys it
  already holds are read first and written back unchanged, and the run stops
  if the path cannot be read.
- **Per-app paths** are written by the operator with OpenBao's check-and-set
  (`cas=0`, `PutOnce`), which refuses a second write.

Changing a credential is therefore a deliberate act in OpenBao (§9). This
protects against the state-drift reset that locks out running apps.

## 8. Two Secret Delivery Patterns

Both begin with an `ExternalSecret` that ESO turns into a Kubernetes Secret.

| Pattern | Mechanism | Used by |
|---|---|---|
| **A** | The chart is told the Secret's name (`existingSecret` or its equivalent) and reads it itself | Kernel services whose charts support it |
| **B** | provider-helm reads single keys of the Secret into chart values (`set[].valueFrom.secretKeyRef`) | Every app installed from a profile: `app-default` writes the Secret `<app>-sensitive-values` and maps its keys to the value names in the profile's `package.valueMapping` |

Both keep secrets out of Git and CR specs. With B the value ends up in the
Helm release's own stored values, which is a Secret in the app's namespace.

A Kyverno (or `validatingAdmissionPolicy`) admission policy that rejects
any `Release` MR putting a literal secret value into `set:` instead of
`valueFrom:` is the intended structural guard rail. It is
not yet in `kernel/security/kyverno/policies/` (target).

## 9. Credential Rotation and Pod Restart

There is no rotation command and no schedule. A credential is changed by
writing the new value in OpenBao; ESO copies it into the Kubernetes Secret
within its refresh interval (one hour for an app's Secret), and **Stakater
Reloader** rolls a workload annotated with `reloader.stakater.com/auto: "true"`
whose Secret changed.

The operator Deployment and a few kernel services carry the annotation.
`app-default` adds it to no tenant Release, so an app is rolled only where its
profile sets the annotation in its own chart values; otherwise it needs a
manual roll (target: annotate every Release). The service the credential
belongs to — a database role, a Keycloak client — has to be given the new
value as well; nothing does that for a changed path.

### Rotation in `random` mode

One kernel credential can be changed on its own, by hand as above.
Rotation driven by an annotation or a schedule is not built
([roadmap.md](../roadmap.md)).

### Rotation in `derived` mode

A changed value in OpenBao holds, because nothing overwrites it (§7), but it
is no longer what the master password reproduces: a rebuild from the master
password and the salt returns the old one. Changing the master password
changes nothing on a running cluster for the same reason. Where a credential
must be rotated and survive a rebuild, use `random` mode and a backup of
OpenBao. Per-app credentials are derived in both modes (§6.3), so the same
holds for them everywhere.

## 10. Secret Flow Sequence

```mermaid
sequenceDiagram
    participant Inst as Installer
    participant XP as Crossplane
    participant Op as Operator
    participant OB as OpenBao
    participant ESO as ESO
    participant PH as provider-helm
    participant Pod as Workload

    Inst->>XP: Secrets in kernel-provisioning (master password, kernel credentials)
    XP->>OB: create kernel/* paths once (Observe, Create)
    Inst->>OB: seed the remaining kernel/* paths
    Note over Op: a tenant installs an app
    Op->>OB: derive and write tenants/{t}/apps/{app}/* once
    XP->>ESO: ExternalSecret from the app's Composition
    ESO->>OB: read the app's paths
    ESO->>PH: Secret {app}-sensitive-values
    PH->>Pod: Helm release with the values
    Note over Pod: a value is changed in OpenBao
    ESO->>PH: Secret data changes
    Note over Pod: Reloader rolls the workload, where annotated
```

## 11. What Never Touches Git

- The master password and its salt. On the cluster they are in OpenBao and in
  one Secret in `kernel-provisioning`; off it, in the recovery kit.
- Any value under `gentian-os/kernel/*` or
  `gentian-os/tenants/*/**`.
- Any TLS private key.
- The DNS provider's API token.

Everything else (claims, profiles, Compositions, manifests) is
plaintext-safe and committed to Git.

### 11.1 Matrix service accounts (Element / UVS)

Tenant **users** sign in to Element over OIDC only, at their tenant's realm
(`id.<cluster domain>/auth/realms/<tenant>`). Nothing in this repository or in
the catalogue's Element profile creates a Matrix service account or stores a
password for one. An add-on that needs a local Synapse account for a service
brings the account and its secret itself, declared under the profile's
`secrets`, and must not be a person's credential.

## 12. TLS and certificates

Gentian OS terminates TLS at the edge (Envoy Gateway listeners) with
certificates from cert-manager. The `Cluster` claim's
`certificates.issuerMode` names the issuer: `acme-dns01` (the default, and the
one that can issue wildcards), `acme-http01`, `private-ca` or `self-signed`.
The kernel's hosts (`platform.<cluster domain>`, `id.<cluster domain>`) are
covered by the kernel wildcard, `wildcard-kernel-tls`; each tenant zone
receives a certificate of its own. See
[multi-tenancy.md](multi-tenancy.md) §3 for the DNS-01 layout and ACME
rate-limit guidance.

### 12.1 Development (ACME staging)

Set `certificates.acmeEnv: staging` on the `Cluster` claim before the install.
The installer applies the Let's Encrypt **staging** `ClusterIssuer`s (step
`A-09-cluster-issuers`) and writes `ACME_STAGING: "true"` to the
`gentian-kernel-services` ConfigMap in `kernel-control` (step
`B-01-bootstrap-apps`). Staging certificates are **not** trusted by browsers
or by default system CA bundles.

An app that calls `https://id.<cluster domain>/…` from inside the cluster
therefore has to be given the issuer's chain. The same mechanism serves a
cluster whose issuer is `self-signed` or `private-ca`.

| Mechanism | Purpose | Limitation |
|---|---|---|
| Secret `gentian-trust-anchor-tls`, key `ca.crt` | Mozilla's CA bundle plus the issuer chain of the kernel wildcard. The operator builds it in `kernel-edge` and copies it into each tenant namespace (`internal/kernel/trustanchor`) | For clients that honour `SSL_CERT_FILE` or `--cacert` |
| Same Secret, key `node-extra-ca.crt` | The issuer chain only, for Node.js through `NODE_EXTRA_CA_CERTS` | The platform does not set that variable: a profile that needs it sets it in its own chart values. Do not point it at `ca.crt` (duplicate Mozilla CAs break verification) |
| Same Secret, key `truststore.jks` | A Java truststore of the same bundle | Its password is a fixed default |
| `app-default` | Where `ACME_STAGING` is `true` or the cluster names a trust anchor, mounts the Secret at `/opt/gentian-trust-anchor` in every app installed from a profile, sets `REQUESTS_CA_BUNDLE`, `SSL_CERT_FILE` and `DENO_CERT` (as `extraEnvVars` and in `values.environment`), and appends `javax.net.ssl.trustStore*` to `javaOpts` when the profile declares an OIDC client or already has `javaOpts` | A chart that takes none of these values is not reached |

An app whose runtime honours none of these needs its own answer, in its own
profile. The catalogue's Element profile is one: its Composition sets
Synapse's OIDC provider with discovery off, the browser's
`authorization_endpoint` on the public identity host, and the token and
userinfo endpoints on Keycloak's in-cluster address (`KEYCLOAK_INTERNAL_URL`
from `gentian-kernel-services`), so Synapse never opens TLS to the edge. It
does so on every cluster, not only on staging, and also sets Synapse's
`use_insecure_ssl_client_just_for_testing_do_not_use` on the provider.

Re-issue and refresh the trust bundle:

```bash
./install.sh --only A-09-cluster-issuers,C-03-wildcard-cert
# the operator rebuilds gentian-trust-anchor-tls and copies it into tenant namespaces
```

### 12.2 Production

Production clusters **must** use Let's Encrypt **production** issuers (or another
publicly trusted CA at both ingress and origin). Concretely:

1. Leave `certificates.acmeEnv` at `production`, its default.
2. Check that `gentian-kernel-services` in `kernel-control` has
   `ACME_STAGING: "false"`.
3. **Do not** rely on `gentian-trust-anchor-tls` or on an insecure client flag:
   `app-default` mounts the bundle only where the cluster is on staging or
   names a trust anchor of its own.
4. Verify `https://id.<cluster domain>/auth/realms/<tenant>/.well-known/openid-configuration`
   presents a chain trusted by standard clients before rolling apps that sign
   in over OIDC.
5. Prefer stable DNS-01 credentials and avoid reinstall loops that re-issue many
   wildcards per week (see [multi-tenancy.md](multi-tenancy.md) rate-limit table).

With production certificates an in-cluster OIDC client trusts
`id.<cluster domain>` through the normal system CA store; no custom CA mount
is required.

### 12.3 Cloudflare tunnel / orange-cloud

When traffic is proxied at Cloudflare, **edge TLS** and **origin TLS** are
independent. Origin certificates from cert-manager still matter for in-cluster
and direct-origin callers. Enable **Total TLS**
(or equivalent) at the edge so multi-label tenant hostnames
(`chat.demo.<cluster domain>`) receive edge certificates — the kernel wildcard alone is
not sufficient. See [multi-tenancy.md](multi-tenancy.md) §3.


## 13. Licensing & sovereignty summary

- **In use, Apache 2.0:** Keycloak, OpenFGA, Kyverno, Envoy Gateway, Crossplane, Argo CD, cert-manager, External Secrets Operator.
- **In use, MPL 2.0:** OpenBao.
- **Considered, not used:** SpiceDB, Ory, OPA, NetBox, Cilium, SPIRE (Apache 2.0); Zitadel v3+, Permify, Snipe-IT (AGPL-3.0, copyleft — disclose service-side modifications); GLPI (GPL).
- **Recommendation:** the Keycloak + OpenFGA core is fully Apache 2.0, so a *modified* IdP/authZ engine can be shipped and operated without a copyleft obligation on the modifications. Both are self-hosted, which keeps the identity layer independent of any vendor. Zitadel remains the alternative if native multi-tenancy outweighs the AGPL constraint and you don't need to *consume* upstream SAML.

---

## 14. Open questions / caveats

- **Dual-write consistency:** membership lives in Keycloak and is projected into OpenFGA from events, which leaves a window and, if an event is lost, a difference nothing repairs: no reconciliation of the stored memberships is built (§3.0). Consistent reads for sensitive checks are target.
- **Agent-identity standards are in flux (2025–2026):** ID-JAG, the IETF agent-auth draft, OIDC-A, NIST guidance are early. Architect for OAuth/OIDC/SPIFFE primitives, not any single proprietary agent framework.
- **ReBAC schemas need extension for advanced delegation** (runtime sessions, agent-to-agent, workflow-scoped authority) — active research. The model has no `agent` or `task` type yet (§3.5).
- **Operational cost:** Keycloak (JVM) + OpenFGA + OpenBao + the MAC backbone is more moving parts than a single binary. Budget DevOps capacity; a mesh and SPIRE add further weight if adopted.
