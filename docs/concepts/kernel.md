---
audience: architect, operator
type: concept
---

# The Kernel and the Default Install

**Companion to:** [architecture.md](../overview/architecture.md)

---

## 1. Kernel Functions

Gentian OS is a cloud operating system: a kernel and a user land. The kernel
is built from Kubernetes primitives — scheduling, isolation, networking,
quotas — and from the kernel services: control, identity and authorization,
secrets, data, edge, admission, provisioning and GitOps, one `kernel-*`
namespace each (§6). Gentian OS is installed onto a running Kubernetes
cluster; from then on the cluster is part of the operating system. The user
of the OS is a tenant; apps are what it installs.

A traditional OS kernel mediates between hardware and applications by
providing a small, stable set of services every app needs. The Gentian
OS kernel does the same for cloud-native applications.

| OS Function | Traditional OS | Gentian OS Equivalent |
|---|---|---|
| Identity & permissions | UID/GID, PAM | **Keycloak** (OIDC/SSO) + **OpenFGA** (ReBAC) |
| Filesystem | VFS, ext4 | S3 object storage (SeaweedFS); hierarchical files via installable apps (e.g. Nextcloud WebDAV) |
| Networking | TCP/IP stack | K8s CNI + Gateway API edge + NetworkPolicies + kernel wildcard + per-tenant zone wildcards (DNS-01) |
| Process execution | Scheduler, init | K8s scheduling + GitOps deployment |
| Secrets keyring | Keychain | Centralized secret store with tenant-scoped policies |
| Database services | — | Shared SQL clusters with per-app-per-tenant isolation |
| Cache | Page cache, tmpfs | Shared Valkey / Memcached with per-app isolation |
| Mail (extension) | sendmail, Maildir | SMTP transport + IMAP storage (optional kernel extension; no spam filter) |
| Package manager | apt, App Store | Catalogs of `ComponentProfile` bundles, installed through the director; rendered by the operator |
| App-to-app permissions | Capabilities, Android intents | Contracts between apps: an `AppGrant` by the tenant's administrator opens the network path and gives the consumer a key of its own ([multi-tenancy.md §4](multi-tenancy.md)) |
| Window manager | Desktop env | The tenant desktop: tiles for what a person may open, opened in a frame or a tab, on one session |
| Notifications | Notification daemon | Notices an administrator publishes to a tenant's people, shown on the desktop |
| Init / lifecycle | systemd | Crossplane Compositions + Argo CD |
| Resource quotas | cgroups, ulimits | K8s ResourceQuotas + LimitRanges, per-tenant policies |

## 2. The OS Analogy in Detail

Each layer of a traditional OS has a direct counterpart in Gentian OS:

| Traditional OS | Gentian OS |
|---|---|
| Hardware | **Nodes, their disks, the load balancer, cloud APIs** — what the cluster runs on |
| Process scheduler | **The Kubernetes scheduler** |
| Process isolation — cgroups, ulimits | **Namespaces, ResourceQuotas, LimitRanges** |
| Network stack | **CNI, NetworkPolicies, the Gateway API edge** |
| Device drivers | **Operators and Crossplane providers** — CloudNativePG, Envoy Gateway, External Secrets, cert-manager, `provider-helm`, `provider-keycloak`, … |
| Syscall API (`open`, `socket`, `fork`) — stable, declarative | **CRDs**: `Tenant`, `ComponentProfile`, `Component`, `IntegrationBinding` |
| `libc` — friendly call → raw syscalls | **Crossplane Compositions** — claim → managed resources |
| Syscall dispatcher / VFS | **Crossplane Composition engine** |
| File descriptor / process handle | **Managed Resource (MR) status** |
| Writeback thread | **Crossplane reconcile loop** |
| `init` / `systemd` | **Argo CD** |
| `/etc/passwd` + PAM | **Keycloak + OpenFGA** |
| Keychain | **OpenBao** |
| Page cache | **Valkey / Memcached** |
| Default mounts (`C:`, `/`, `~/`) | **The default install** (§3) |

Two consequences worth calling out:

1. **POSIX is declarative too.** `O_CREAT | O_RDWR` is a desired-state
   flag, not a script. A `Tenant` CR is the same shape one layer up:
   "I want this resource, in this configuration." Declarative resource
   APIs are the form OS resource interfaces have always taken.
2. **Drivers must be pluggable.** Linux scales to thousands of devices
   via loadable kernel modules; you would never re-implement them per
   distribution. Cloud-OS-scale resource diversity (every managed
   service, every SaaS API) demands the same model — Crossplane
   providers play that role. A custom Go controller would be the
   equivalent of writing your own ext4 from scratch.

## 3. Default Install — the "Default Drives"

A desktop OS ships with default mounts: `C:`, `/`, `~/`. These are not
"apps" — they are the filesystem the OS exposes so every other app has
somewhere to read/write data through a standard API.

The **default install** is what a cluster has before any catalog app is
installed: the kernel, the system engines that run on every cluster
(PostgreSQL and the object store, §7), and the user-interface components
the operator installs into each tenant. The kernel must be Ready before any
tenant app can run. Kernel components provide **platform primitives**;
catalog apps (Nextcloud, Collabora, mail, …) are installed per tenant from a
`ComponentProfile`, rendered by the operator
([app-catalog.md](app-catalog.md)).

| Kernel function | Default-install component | Desktop OS analogue | Standard contract exposed to apps |
|---|---|---|---|
| Identity & SSO | **Keycloak + OpenFGA** | `/etc/passwd` + PAM | OIDC issuer, groups that give access to apps, ReBAC graph |
| Object storage | **SeaweedFS** (S3) | Page cache, `/tmp`, `/var` | S3 endpoint + per-app bucket via kernel requirement |
| Relational data | **CloudNativePG / MariaDB** | Per-app SQLite / registry hive | `database` requirement (host + per-app DB + user) |
| Key-value cache | **Valkey / Memcached** | Page cache, `tmpfs` | `cache` requirement |
| Edge routing | **Gateway API** (Envoy Gateway) | Network stack + firewall | TLS termination, HTTPRoute, tenant wildcards |
| Secret store | **OpenBao** | Keychain | Underlying store for ESO-backed Secrets |

**The user interfaces are in the default install and are not kernel
services.** The **desktop** and the **administration console** are installed
into every tenant, the **sign-in page** (the concierge) into the platform
tenant, and the **App Store app** into a tenant where a store is offered
(from the store's catalog, implementing [store-contract.md](store-contract.md)).
They are built in [gentian-ui](https://github.com/gentian-org/gentian-ui),
described by `ComponentProfile`s this repository's chart ships, and installed
by the operator like any other component: they run in a tenant namespace,
under its quota and policies, and hold no kernel credential. They are the
window manager of the analogy — desktop shell, Start menu, Settings — and
what they show are tiles (`spec.expose[].tile`).

**Not kernel** — installed from a catalog; what a tenant has installed
is listed in `Tenant.spec.apps`:

| Capability | Catalog profile(s) | Contract |
|---|---|---|
| Hierarchical files (WebDAV) | catalog file-store profiles | `file-store` |
| Collaborative editing | catalog WOPI providers | WOPI |
| Office editing | **Collabora** (catalog app) | embed |
| Self-hosted mail UI | mail app profiles in `gentian-apps` | SMTP/IMAP kernel requirements |

In Crossplane terms: the kernel and the always-on engines are the set of
`Cluster`-XR managed resources that must be Ready before any `Tenant`
claim can reach Ready. They are the **kernel devices** the syscall layer
assumes are present — formatted, mounted, addressable — at boot.

A tenant can swap implementations (e.g. external S3 instead of
SeaweedFS) without breaking apps that program against the contract,
the way a desktop user can replace `C:` without breaking
`CreateFile()`.

## 4. Kernel Extensions

Some kernel functions are **optional** — not every deployment needs
them. These are modeled as **kernel extensions**: shared
infrastructure with tenant-scoped configuration, enableable per cluster.

The primary extension is **mail**. Not every tenant runs
self-hosted mail; some use Gmail, others only need outbound SMTP for
notifications. Mail is therefore not part of the core kernel but a
deployable extension. See [mail.md](mail.md) for the configuration
modes, isolation guarantees, and trade-offs.

## 5. Capability Enforcement

Contracts between apps are **trust-based** — an app that
declares `webdav:read` is trusted not to attempt `webdav:write`.

## 6. Namespaces

Five categories of namespace, each with one authority and one policy set. The
list as built is [`kernel/namespaces.yaml`](../../kernel/namespaces.yaml):
the installer creates from it, the operator selects on it
(`internal/layout`) and `make lint-namespace-layout` checks the Cluster
composition against it. The decisions behind the layout are AD-7 to AD-11
([decisions/index.md](../decisions/index.md)).

### 6.1 Categories

| Category | Contains | Created by | Authority | Compromise is bounded by |
| --- | --- | --- | --- | --- |
| `kernel-<function>` | the services the OS is made of, including the operators: GitOps, provisioning, secrets, seal, data, authentication, authorization, control, edge, admission, observability | `install.sh`, before anything else exists; then Argo CD | break-glass platform administrator only | nothing: this is the trust root |
| `system-<function>` | one engine per namespace, named by the function a component's `requires.services` entry asks for: `system-postgresql`, `system-mariadb`, `system-cache` (Valkey), `system-s3` (SeaweedFS), `system-authorization` (the app instance of OpenFGA and the arbiter), `system-mail` (Postfix with the DKIM signer and the per-tenant keys, Dovecot), `system-llm`; no public route | the Cluster claim; the unconditional ones on every cluster, the rest on demand (§7) | platform administrator | other system functions; every tenant boundary |
| `system-<function>-dmz` | the internet-facing edge of a system service whose protocol needs one: a stateless listener holding one credential to its backend and no data. `system-mail-dmz` is a proxy on `:25`, `:587` and `:993` (`kernel/services/mail-edge`) in front of Postfix and Dovecot; `system-turn-dmz` is the TURN relay, all edge, no inner part | the Cluster claim | platform administrator | the service's backend; every tenant |
| `tenant-<t>` | the tenant's instances of class `app` and `adapter`, its desktop and administration console, and what the operator creates around them: quota, network policies, Services, ExternalSecrets, Jobs, HTTPRoutes | the operator, per tenant | tenant administrator through the director | every other tenant; the kernel; system services beyond declared requirements |
| `tenant-<t>-dmz` | the tenant's perimeter: one publishing proxy per `surface: perimeter` entry a perimeter approver has enabled ([app-catalog.md §17.5](app-catalog.md)) | the operator, from the tenant's enabled entries | perimeter approver through the director ([security/design.md §2.14](../security/design.md)) | the tenant's own instances |

`shared-<app>` is the namespace of a `shared-app` instance, a planned class not
yet implemented ([app-catalog.md §13.5](app-catalog.md)).

**`tenant-platform` is a tenant namespace.** The platform is a tenant whose
realm is the kernel realm (AD-10): its desktop is the platform administrators'
desktop, its tiles the administration apps, its members the platform
administrators. It runs under the same quota, policy and authority as any
tenant and holds no kernel credential: the kernel realm's confidential client
is on the `authenticated` Gateway in `kernel-edge`, never in a tenant
namespace, so a compromised platform desktop yields administrator *sessions*,
bounded by OpenFGA and the director. Its two exceptions are in the operator:
the realm is adopted rather than created, and the tenant cannot be deleted.

A Gateway is a **kernel resource**, in `kernel-edge`: created and reconciled by
the operator, configured from the Cluster claim. A tenant gets *listeners* on
it and owns `HTTPRoute`s only; under merged Gateways listener uniqueness is
class-wide, so a tenant that could create a Gateway could claim another
tenant's hostname ([routing.md §2.1](routing.md)).

### 6.2 Rules

**A new namespace** inside a category needs a different exposure, credential
set, upgrade owner or quota than its neighbor. None of the four: same
namespace. System engines are one namespace each because quotas, backup
policies and managed-service claims differ per engine, and separating stateful
services after the fact is a data migration.

**Names** are `<tier>-<qualifier>`. Kernel and system namespaces are named for
the function, not the product (`kernel-provisioning`, not the software behind
it), so the software can change. For tenants the tenant name is the qualifier
and `-dmz` a suffix, so a tenant's namespaces list and sort together. A
tenant name is at most 38 characters: its desktop's Helm release,
`tenant-<t>-desktop`, must fit Helm's 53, and its purge Job,
`s3-delete-<t>-gentian-backup`, the 63 of a pod label
(`MaxTenantNameLength` in `api/v1alpha1/tenancy.go`). A cluster has
one stage, so no name carries a stage suffix.

**Labels.** Every namespace carries `gentianos.io/tier: kernel | system |
system-dmz | shared | tenant | tenant-dmz | tenant-runs` and `gentianos.io/function:
<function>`; a tenant namespace carries `gentianos.io/tenant: <t>`. Most
policies select on the labels; some kernel network policies select on names
(`kernel/security/network-policies/kernel-network-policies.yaml`). The two
namespaces the platform provides and gentian-os does not create, `kube-system`
and `metallb-system`, are labeled `tier: kernel` and keep their names; the
label is their exemption from the baseline policies.

**Fresh installs.** The layout is what a fresh install creates. A stateful
kernel component is never renamed in place: a cluster that carries other names
keeps them and adopts the labels, or is rebuilt from its recovery kit.

**Perimeter mode.** A perimeter entry is a capability a profile declares; a
proxy exists in `tenant-<t>-dmz` only for an entry a perimeter approver
enabled, with that tenant's credential, and nothing is published by default.
Each published host gets its own listener on the `perimeter` Gateway, named for
the host exactly, so that one host leaves the session and the rest of the zone
stays behind it ([routing.md §2.1](routing.md)). The proxy checks no caller:
`authMode: none` publishes the declared paths to anyone and passes no
credential; `authMode: app` passes the caller's own `Authorization` header to
the app, which alone checks it ([security/design.md §2.14](../security/design.md)). Shared and
public are independent: nothing is published through a tenant's DMZ except
what that tenant enabled.

## 7. System engines

A system engine is an Argo CD application from `kernel/appsets`, in its own
`system-<function>` namespace, composed by the Cluster claim. PostgreSQL and
the object store are on every cluster: the kernel's UIs keep their data on the
one, and every backup, bundle and export is written to the other. MariaDB, the
cache, the authorization store and the TURN edge are started on demand
(`internal/controller/system_engines.go`): the operator keeps, per cluster, the
set of engines something declares in `requires.services`, the composition
composes each one the set names, and a component that declared an engine waits
until it answers. An engine is never taken out of the set because the last
component that declared it went: its data may be all that is left of a removed
app. A person asks for it to go, and the engine is purged. Mail and LLM are
switched on by the claim (`mail.serviceMode: system`, `llm.enabled`), and a
cluster that relays mail through a provider composes no mail namespace at all.
