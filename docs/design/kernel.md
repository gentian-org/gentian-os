# The Kernel and the Default Install

**Companion to:** [architecture-crossplane.md](../architecture-crossplane.md)

---

## 1. Kernel Functions

A traditional OS kernel mediates between hardware and applications by
providing a small, stable set of services every app needs. The Gentian
OS kernel does the same for cloud-native applications.

| OS Function | Traditional OS | Gentian OS Equivalent | Scope |
|---|---|---|---|
| Identity & permissions | UID/GID, PAM | OIDC provider + LDAP directory (SSO, user/group, token exchange) | v1 |
| Filesystem | VFS, ext4 | WebDAV (hierarchical files, locking, sharing) + S3 (object storage) | v1 |
| Networking | TCP/IP stack | K8s CNI + Ingress + NetworkPolicies + per-cluster kernel domain (hybrid wildcard / HTTP-01 TLS) | v1 |
| Process execution | Scheduler, init | K8s scheduling + GitOps deployment | v1 |
| Secrets keyring | Keychain | Centralised secret store with tenant-scoped policies | v1 |
| Database services | — | Shared SQL clusters with per-app-per-tenant isolation | v1 |
| Cache | Page cache, tmpfs | Shared Redis / Memcached with per-app isolation | v1 |
| Mail (extension) | sendmail, Maildir | SMTP transport + IMAP storage + spam filtering | v1 |
| Package manager | apt, App Store | App catalogue CRD + automated deployment pipeline | v1 |
| App-to-app permissions | Capabilities, Android intents | Contract-based bindings + OIDC token exchange (RFC 8693) | v1 |
| Window manager | Desktop env | Browser-based shell/portal with iframes, unified nav, SSO session | v1 |
| Notifications | Notification daemon | Notification gateway aggregating across apps | v1 |
| Init / lifecycle | systemd | Crossplane Compositions + ArgoCD | v1 |
| Resource quotas | cgroups, ulimits | K8s ResourceQuotas + LimitRanges, per-tenant policies | v1 |
| IPC bus | D-Bus, sockets | Message broker with per-tenant subjects (CloudEvents) | Future |
| Clipboard / intents | X11 clipboard | Share-to intent system | Future |
| Config store | dconf, registry | Per-tenant, per-app key-value config service | Future |
| Capability enforcement | SELinux, seccomp | Runtime checks via service mesh / API gateway | Future |

## 2. The OS Analogy in Detail

Each layer of a traditional OS has a direct counterpart in Gentian OS:

| Traditional OS | Gentian OS |
|---|---|
| Syscall API (`open`, `socket`, `fork`) — stable, declarative | **CRDs**: `Tenant`, `AppProfile`, `IntegrationBinding` |
| `libc` — friendly call → raw syscalls | **Crossplane Compositions** — claim → managed resources |
| Syscall dispatcher / VFS | **Crossplane Composition engine** |
| Loadable kernel modules / drivers | **Crossplane providers** |
| Hardware behind the drivers | **External operators & APIs** |
| File descriptor / process handle | **Managed Resource (MR) status** |
| Scheduler / writeback thread | **Crossplane reconcile loop** |
| `init` / `systemd` | **ArgoCD** |
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

Several Gentian OS components play exactly this role: nominally apps
with their own UIs, but their primary job is to **be the default
backing store for a kernel function** so any installable app can rely
on a standard contract instead of bringing its own backend.

| Kernel function | Default-install component | Desktop OS analogue | Standard contract exposed to apps |
|---|---|---|---|
| Hierarchical file storage | **Nextcloud** (WebDAV) | `C:` drive / home directory | `file-store` (WebDAV read/write, OCS shares) |
| Object storage | **MinIO** (S3) | Page cache, `/tmp`, `/var` | S3 endpoint + per-app bucket via kernel requirement |
| Identity directory | **Nubus** (Keycloak + UCS LDAP) | `/etc/passwd` + PAM | OIDC issuer + LDAP base DN |
| Relational data | **CloudNativePG / MariaDB** | Per-app SQLite / registry hive | `database` requirement (host + per-app DB + user) |
| Key-value cache | **Redis / Memcached** | Page cache, `tmpfs` | `cache` requirement |
| Mail | **Postfix + Dovecot + Rspamd** | Built-in mail spool | SMTP submit + IMAP endpoint + DKIM (kernel extension) |
| Window manager | **Univention Portal** | Desktop shell, Start menu | `central-navigation` contract |
| Notification surface | **Notification Gateway** | Notification daemon, Action Center | Notifications contract (future) |
| Secret store | **OpenBao** | Keychain | Underlying store for ESO-backed Secrets |

In Crossplane terms: the default install is the set of `Cluster`-XR
managed resources that must be Ready before any `Tenant` claim can
reach Ready. They are the **kernel devices** the syscall layer assumes
are present — formatted, mounted, addressable — at boot.

A tenant can later swap implementations (Nextcloud → Seafile, MinIO →
external S3) without breaking apps that program against the contract,
the way a desktop user can replace `C:` without breaking
`CreateFile()`.

## 4. Kernel Extensions

Some kernel functions are **optional** — not every deployment needs
them. These are modelled as **kernel extensions**: shared
infrastructure with tenant-scoped configuration, enableable per cluster.

The primary extension today is **mail**. Not every tenant runs
self-hosted mail; some use Gmail, others only need outbound SMTP for
notifications. Mail is therefore not part of the core kernel but a
deployable extension. See [mail.md](mail.md) for the configuration
modes, isolation guarantees, and trade-offs.

## 5. Capability Enforcement (Future)

Today, contracts between apps are **trust-based** — an app that
declares `webdav:read` is trusted not to attempt `webdav:write`.
Future versions may enforce capabilities at the network layer using a
service mesh (Istio authorization policies) or an API gateway that
inspects requests against declared capabilities. This would move from
an Android-style "declared permissions" model to runtime enforcement,
the same way SELinux extended Linux's discretionary file permissions.
