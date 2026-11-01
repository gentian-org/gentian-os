# The Kernel and the Default Install

**Companion to:** [architecture.md](../architecture.md)

---

## 1. Kernel Functions

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
| Syscall API (`open`, `socket`, `fork`) — stable, declarative | **CRDs**: `Tenant`, `ComponentProfile`, `Component`, `IntegrationBinding` |
| `libc` — friendly call → raw syscalls | **Crossplane Compositions** — claim → managed resources |
| Syscall dispatcher / VFS | **Crossplane Composition engine** |
| Loadable kernel modules / drivers | **Crossplane providers** |
| Hardware behind the drivers | **External operators & APIs** |
| File descriptor / process handle | **Managed Resource (MR) status** |
| Scheduler / writeback thread | **Crossplane reconcile loop** |
| `init` / `systemd` | **Argo CD** |
| Default mounts (`C:`, `/`, `~/`) | **Default-install kernel components** |

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

Gentian OS ships a **kernel** that must be Ready before any tenant app
can run. Kernel components provide **platform primitives**; catalog
apps (Nextcloud, Collabora, mail, …) are installed per tenant from a
`ComponentProfile`, rendered by the operator
([app-catalogue.md](app-catalogue.md)).

| Kernel function | Default-install component | Desktop OS analogue | Standard contract exposed to apps |
|---|---|---|---|
| Identity & SSO | **Keycloak + OpenFGA** | `/etc/passwd` + PAM | OIDC issuer, groups that give access to apps, ReBAC graph |
| Object storage | **SeaweedFS** (S3) | Page cache, `/tmp`, `/var` | S3 endpoint + per-app bucket via kernel requirement |
| Relational data | **CloudNativePG / MariaDB** | Per-app SQLite / registry hive | `database` requirement (host + per-app DB + user) |
| Key-value cache | **Redis / Memcached** | Page cache, `tmpfs` | `cache` requirement |
| Edge routing | **Gateway API** (Envoy Gateway) | Network stack + firewall | TLS termination, HTTPRoute, tenant wildcards |
| Window manager | The **desktop** and the **administration console**, components installed for every tenant, ([gentian-ui](https://github.com/gentian-org/gentian-ui)), and the **App Store app** where a store is offered (from the store's catalog, implementing [store-contract.md](store-contract.md)) | Desktop shell, Start menu, Settings | Tiles (`spec.expose[].tile`) |
| Secret store | **OpenBao** | Keychain | Underlying store for ESO-backed Secrets |

**Not kernel** — installed from a catalog; what a tenant has installed
is listed in `Tenant.spec.apps`:

| Capability | Catalog profile(s) | Contract |
|---|---|---|
| Hierarchical files (WebDAV) | catalog file-store profiles | `file-store` |
| Collaborative editing | catalog WOPI providers | WOPI |
| Office editing | **Collabora** (catalog app) | embed |
| Self-hosted mail UI | mail app profiles in `gentian-apps` | SMTP/IMAP kernel requirements |

In Crossplane terms: the default install is the set of `Cluster`-XR
managed resources that must be Ready before any `Tenant` claim can
reach Ready. They are the **kernel devices** the syscall layer assumes
are present — formatted, mounted, addressable — at boot.

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
