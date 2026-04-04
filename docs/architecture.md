# Gentian OS — Platform Architecture

**Version:** 2.0-draft
**Status:** Proposal

---

# Part I — Gentian OS

## 1. Vision

Gentian OS is a cloud-native OS. It allows users to install open-source applications on a Kubernetes cluster the same way a desktop OS lets users install software. Users don't configure databases, wire up authentication, or manage credentials. They click "install" and the platform handles the rest.

This requires the same abstraction a traditional OS provides: a **kernel** that manages shared resources and presents a uniform interface to applications. On a desktop, the kernel manages memory, filesystems, processes, and I/O. In Gentian OS, the kernel manages identity, storage, databases, mail, and networking — and exposes them to apps through a contract-based API.

The platform optimises for two dimensions: **easy onboarding of new applications** (adding an app to the catalogue is a single-file operation) and **easy provisioning of new tenants** (creating a tenant is a single declarative resource).

## 2. The Kernel

A traditional OS kernel sits between hardware and applications. It provides services that every application needs, so that applications don't implement their own TCP stack, filesystem driver, or process scheduler. The Gentian OS kernel does the same for cloud-native applications running on Kubernetes.

### 2.1 Kernel Functions

| OS Function | Traditional OS | Gentian OS Equivalent | Scope |
|---|---|---|---|
| Identity & permissions | UID/GID, PAM, login | OIDC provider + LDAP directory (SSO, user/group management, token exchange) | v1 |
| Filesystem | VFS, ext4, block devices | WebDAV (hierarchical files, locking, sharing) + S3 (object storage, bulk data) | v1 |
| Networking | TCP/IP stack, drivers | Kubernetes CNI + Ingress + NetworkPolicies | v1 |
| Process execution | Scheduler, init | Kubernetes workload scheduling + GitOps deployment | v1 |
| Secrets & keyring | Keychain, GNOME Keyring | Centralised secret store with tenant-scoped policies | v1 |
| Database services | — | Shared database clusters (SQL) with per-app-per-tenant isolation | v1 |
| Cache subsystem | Page cache, tmpfs | Shared Redis / Memcached with per-app isolation | v1 |
| Mail transport & storage | sendmail, Maildir | SMTP transport (MTA) + IMAP storage (MDA) + spam filtering | v1 (kernel extension) |
| Package manager | apt, App Store | App catalogue CRD + automated deployment pipeline | v1 |
| App-to-app permissions | Capabilities, Android intents | Contract-based integration bindings + OIDC token exchange (RFC 8693) | v1 |
| Window manager | Compositor, desktop env | Browser-based shell/portal with iframes, unified navigation, SSO session | v1 |
| Notifications | Notification daemon | Notification gateway aggregating across all apps | v1 |
| Init system / lifecycle | systemd | Thin orchestrator (install, upgrade, uninstall via existing operators) | v1 |
| Resource quotas | cgroups, ulimits | Tenant quota policies + Kubernetes ResourceQuotas + LimitRanges | v1 |
| IPC bus | D-Bus, Unix sockets | Message broker with per-tenant subject namespaces, standard event schemas | Future |
| Clipboard / intents | X11 clipboard, Android intents | Share-to intent system via message bus | Future |
| Config store | dconf, Windows registry | Per-tenant, per-app key-value configuration service | Future |
| Capability enforcement | SELinux, seccomp | Runtime permission enforcement via service mesh or API gateway | Future |

### 2.2 Kernel Extensions

Some kernel functions are **optional** — not every deployment needs them. These are modelled as **kernel extensions** that can be enabled per tenant or per cluster. Unlike apps (which are user-facing), kernel extensions provide infrastructure services consumed by other apps.

**Mail** is the primary kernel extension. Not every tenant needs self-hosted mail — some use external providers, others only need outbound SMTP for notifications. Mail is therefore not part of the core kernel but a deployable extension with four tenant modes:

| Mode | Transport | Storage | Client | Use case |
|---|---|---|---|---|
| `selfhosted` | Kernel extension | Kernel extension | App | Full self-hosted mail |
| `external` | Tenant's own | Tenant's own | App → external IMAP/SMTP | Tenant uses Gmail, existing mail server |
| `transport-only` | Kernel extension | External | App → external storage | Kernel handles SMTP relay |
| `disabled` | — | — | Apps send via SMTP relay only | Outbound-only (notifications) |

When deployed as a kernel extension, the mail stack (MTA, MDA, spam filter) can be provisioned per-tenant rather than shared, avoiding the scaling limitations of a single Postfix/Dovecot instance serving hundreds of tenant domains. This gives true isolation, independent scaling, and simpler operations at the cost of higher resource usage.

### 2.3 Kernel vs Userspace

The kernel provides services that are **shared, trusted, and always available**. Applications (userspace) consume these services through well-defined contracts. An app never provisions its own database, creates its own OIDC client, or manages its own mail server. It declares what it needs, and the kernel provisions it.

This separation has the same benefits as in a traditional OS: apps are simpler (they don't reimplement infrastructure), isolation is enforced centrally (the kernel controls who can access what), and onboarding new apps is fast (the kernel contract is stable and documented).

### 2.4 Multi-Tenancy

Gentian OS supports multiple tenants (organisations) on a single cluster. Each tenant gets isolated identity (their own authentication realm), isolated data (their own databases, storage buckets, mailboxes), and isolated networking (namespace-level network policies). The kernel services are shared but tenant-scoped: one identity provider serves all tenants via separate realms, one database cluster hosts all tenant databases with separate credentials.

Each tenant's apps are deployed into a **dedicated namespace** (`tenant-{name}`). Kubernetes RBAC, ResourceQuotas, LimitRanges, and NetworkPolicies enforce isolation at the platform level. At the application level, isolation is enforced through separate Keycloak realms, PostgreSQL databases, MinIO bucket policies, and Redis ACLs.

For deployments requiring stronger isolation — regulated industries, hostile multi-tenancy, or customer-facing PaaS — each tenant can optionally run inside a **vCluster** (virtual Kubernetes cluster, Apache 2.0 licensed), providing a dedicated API server per tenant while still sharing the kernel infrastructure.

| Isolation level | Mechanism | Best for |
|---|---|---|
| Namespace-per-tenant (default) | K8s RBAC, ResourceQuotas, NetworkPolicies | Trusted internal tenants, cost efficiency |
| vCluster-per-tenant (optional) | Dedicated K8s API server per tenant | External customers, regulated environments |

### 2.5 Contracts and Integration Bindings

In a traditional OS, applications communicate through well-defined IPC mechanisms: D-Bus interfaces, Unix sockets, shared memory, clipboard protocols. Each has a contract — a specification of what data can be exchanged and how. The Gentian OS equivalent is the **contract system**, which governs how apps integrate with each other and with the kernel.

A contract defines a capability that one app provides and another consumes. For example, the `file-store` contract specifies that a provider offers WebDAV read/write access, and a consumer can use it to browse and edit files. The `central-navigation` contract specifies that a provider (the portal) accepts navigation link registrations from consumer apps.

**Point-to-point contracts (IntegrationBindings):** These are explicit, declared in the AppProfile under `optionalIntegrations`. When the orchestrator reconciles a tenant and finds that both the provider and consumer of a contract are in the tenant's app list, it auto-generates an `IntegrationBinding` CR. The binding provisions shared credentials (stored in OpenBao), configures authentication (typically OIDC token exchange so the consumer can act on behalf of the user), and tracks health through status conditions (credential validity, provider reachability). Bindings are owned by the Tenant CR and garbage-collected on deletion.

> **Future direction — capability enforcement:** In the current architecture, contracts are trust-based — an app declaring `webdav:read` is trusted not to attempt `webdav:write`. Future versions may enforce capabilities at the network layer using a service mesh (e.g., Istio authorization policies) or an API gateway that inspects requests against the declared capabilities. This would move from an Android-style "declared permissions" model to a runtime enforcement model.

> **Future direction — broadcast contracts (IPC bus):** A message broker (e.g., NATS) with per-tenant subject namespaces and standard event schemas (CloudEvents) could enable pub/sub between apps. This is out of scope for the initial implementation because existing openDesk applications do not natively produce or consume broker events. It would require webhook adapters or sidecar containers per app. The IntegrationBinding contract system is sufficient for the point-to-point integrations that openDesk apps currently support.

---

# Part II — The Architecture Triangle

## 3. Three Tools, Three Jobs

Gentian OS is built on three tools — the **triangle** — each doing exactly one job with no overlap.

| Tool | Role | What it does | What it never does |
|------|------|-------------|-------------------|
| **Thin orchestrator** (Go) | Provisioning plane | Coordinates tenant lifecycle by creating CRs for existing operators, wires secrets, manages IntegrationBindings | Directly call external APIs, deploy Helm charts |
| **OpenTofu** (via Tofu Controller) | Infrastructure plane | Static kernel provisioning, secret seeding, external resources (DNS, cloud LBs, TLS CAs), Helm releases for secrets-hostile charts (Pattern B) | React to runtime events, manage tenant lifecycle |
| **ArgoCD** | Deployment plane | Helm chart deployment, drift detection, rollback, health monitoring, sync status | Provision infrastructure, generate credentials |

```mermaid
graph TD
    OT[OpenTofu\nvia Tofu Controller\n\nSeeds kernel infra + OpenBao\nManages external resources]
    GC[Thin Orchestrator\nprovisioning plane\n\nCreates CRs for operators\nWires secrets via ESO\nManages IntegrationBindings]
    AC[ArgoCD\ndeployment plane\n\nHelm charts / Drift detection\nRollback / Health status\nSelf-healing]
    OB[OpenBao\nsecrets]
    OP[Existing Operators\n\nKeycloak Operator\nCloudNativePG / MinIO Operator\nESO / NATS Operator]

    OT -- seeds kernel credentials --> GC
    GC -- creates operator CRs --> OP
    OP -- store credentials --> OB
    GC -- creates ArgoCD Application CRs --> AC
    AC -- reads ExternalSecrets --> OB

    style OT fill:#e8f4f8,stroke:#2980b9
    style GC fill:#fef9e7,stroke:#f39c12
    style AC fill:#eafaf1,stroke:#27ae60
    style OB fill:#f5eef8,stroke:#8e44ad
    style OP fill:#fdf2e9,stroke:#e67e22
```

### 3.1 Thin Orchestrator — Delegate, Don't Implement

The orchestrator's job is sequencing, wiring, and status aggregation — not provisioning. Wherever possible, it creates CRs for existing operators rather than calling external APIs directly. Instead of importing `pgx` and running `CREATE DATABASE`, it creates a `Database` CR that CloudNativePG reconciles.

**Pragmatic exception (v1):** Where an operator cannot manage the underlying service — e.g., Keycloak bundled inside Nubus, or LDAP managed by UDM — the orchestrator creates idempotent **Jobs** that call REST APIs. These Jobs behave like operator CRs from the orchestrator's perspective: they are Kubernetes resources with status conditions that the reconciler watches. When the underlying service is eventually unbundled (e.g., standalone Keycloak managed by Keycloak Operator), the Jobs are replaced with operator CRs without changing the reconciler's external interface.

This reduces the custom code surface dramatically. Each operator is maintained by its upstream community, handles retries and idempotency internally, and is tested independently. The orchestrator only needs to know the CRD schemas of the operators it coordinates (or the Job spec for exceptional cases), not the internal APIs of every backend system.

| Kernel resource | Provisioning method (v1) | Target (post-Nubus) | CR/resource created by orchestrator |
|---|---|---|---|
| PostgreSQL database + user | CloudNativePG operator | — (already target) | `Database`, `Role` |
| MariaDB database + user | MariaDB Operator or SQL Job | MariaDB Operator | `MariaDBDatabase` or `Job` |
| Keycloak realm + OIDC client | **Job** (Keycloak Admin REST API) | Keycloak Operator | `Job` → `KeycloakRealmImport`, `KeycloakClient` |
| MinIO bucket + IAM user | MinIO Operator or admin API Job | MinIO Operator | `Tenant` (MinIO) or `Job` |
| LDAP bind account | **Job** (UDM REST API) | Direct LDAP or operator | `Job` with UDM CLI |
| NATS account + user | NATS Operator (or nsc CLI via Job) | NATS Operator | NATS account JWT |
| Redis ACL user | Redis Operator or `redis-cli` Job | Redis Operator | Operator-specific CR or `Job` |
| Secrets sync to K8s | External Secrets Operator | — (already target) | `ExternalSecret` |
| Postfix + Dovecot (mail ext.) | Helm chart via ArgoCD | — | `Application` (per-tenant if selfhosted) |
| ArgoCD Application | — (direct CR creation) | — | `Application` |

> **Future direction — saga pattern:** The current orchestrator uses Kubernetes' built-in retry (requeue on failure) and relies on operator idempotency. At scale, a formal saga pattern with compensating transactions would provide explicit rollback: if Keycloak client creation succeeds but database creation fails, the saga would delete the Keycloak client rather than leaving orphaned resources. This can be implemented using a workflow engine (e.g., Temporal) or explicit phase tracking in the Tenant CR status.

### 3.2 Why This Split

- **The orchestrator never deploys workloads.** It creates CRs for operators and ArgoCD Applications. A provisioning bug doesn't break running deployments.
- **ArgoCD never provisions infrastructure.** It deploys Helm charts and continuously reconciles them. A deployment bug doesn't corrupt databases or credentials.
- **OpenTofu never reacts to runtime events.** It manages static kernel infrastructure through plan/review/apply cycles. Changes to the kernel are deliberate and reviewed.

A bug in one tool doesn't break the others. OpenTofu state corruption doesn't affect running tenant apps. An orchestrator crash doesn't break deployed workloads. An ArgoCD issue doesn't corrupt databases.

### 3.3 Tool Responsibility Matrix

| Concern | Thin orchestrator | OpenTofu | ArgoCD |
|---|---|---|---|
| Tenant provisioning | **Best fit** — fast, reactive, conditional | Too slow (batch model) | Wrong tool — no provisioning logic |
| App install/uninstall | **Best fit** — orchestrated, ordered | Awkward — state per app per tenant | Deploys the workload, not the infra |
| Kernel infrastructure | Overkill — this is static | **Best fit** — plan/review/apply | Can deploy Helm charts for kernel services |
| External resources (DNS, cloud) | Wrong tool — no providers | **Best fit** — huge provider ecosystem | Wrong tool — not infra provisioning |
| Credential rotation | **Best fit** — continuous, event-driven | Wrong model — batch | Picks up new secrets via ESO sync |
| App deployment (Helm charts) | Wrong tool | **Fallback** for charts without `existingSecret` support (Pattern B — `set_sensitive`) | **Best fit** — drift detection, rollback, sync |
| App upgrades across tenants | Triggers ArgoCD by updating Application CRs | Could update chart versions but no rollback | **Best fit** — rolling upgrades, health monitoring |
| Drift detection | Delegated to operators | Only on next plan/apply run | **Best fit** — continuous for all deployed manifests |
| Rollback | Delegated to operators | State-based, risky | **Best fit** — built-in revision history per Application |
| Visibility / dashboard | Custom metrics only | State files, no live view | **Best fit** — full UI with health, sync, logs |

## 4. CRD Abstraction Model

The platform uses four CRDs that form a layered abstraction. Two are authored by humans (AppProfile, Tenant); two are generated by the orchestrator (IntegrationBinding, ArgoCD Application).

### 4.1 AppProfile (cluster-scoped) — The App Catalogue

Defines what an app **is**: its kernel requirements, the capabilities it provides, optional peer integrations, and its Helm chart reference. Written once per app type. This is the abstraction that makes adding new apps a single-file operation.

The AppProfile uses a **schema-based value mapping** rather than freeform Go templates. Each kernel requirement type (OIDC, database, S3, SMTP, cache) has a well-defined schema that the orchestrator validates at admission time and renders programmatically. The mapping declares which Helm value keys should receive which kernel-provided values:

```yaml
valueMapping:
  oidc:
    issuerKey: "oidc.issuer"
    clientIdKey: "oidc.clientId"
    clientSecretKey: "oidc.clientSecret"
  database:
    hostKey: "database.host"
    nameKey: "database.name"
    userKey: "database.user"
    passwordKey: "database.password"
  s3:
    endpointKey: "s3.endpoint"
    bucketKey: "s3.bucket"
    accessKeyKey: "s3.accessKey"
    secretKeyKey: "s3.secretKey"
```

This approach has three advantages over freeform templates: the schema can be validated at admission time (a typo is caught before reconciliation, not during), the orchestrator knows exactly which secret paths to create ExternalSecrets for, and the mapping is testable without a running cluster.

For apps with non-standard value structures, an `extraValues` field allows arbitrary YAML that is merged into the rendered values, providing an escape hatch without compromising the typed path for common cases.

Updating an AppProfile's chart version propagates to all tenants: the orchestrator lists affected tenants via a label index, updates their ArgoCD Applications, and ArgoCD rolls out the upgrade.

### 4.2 Tenant — The Customer

Represents an organisation. Specifies a domain, isolation boundaries (namespace, LDAP OU, database prefix, S3 prefix, Keycloak realm), mail configuration, resource quotas, a deletion policy, and a list of desired apps by profile name. Creating a Tenant CR triggers the full provisioning and deployment pipeline.

The deletion policy is configurable per tenant: `Retain` (default) revokes access credentials but keeps databases, storage buckets, mailboxes, and LDAP entries intact — safe for compliance and data recovery. `Delete` drops everything, intended for development and test tenants.

### 4.3 IntegrationBinding — The Cross-App Contract

Auto-generated by the orchestrator when both the provider and consumer of a contract exist in a tenant's app list. Each binding provisions shared credentials (stored in OpenBao), configures the authentication method (e.g., OIDC token exchange), and continuously tracks health through status conditions: whether credentials are valid, when they were last rotated, and whether the provider is reachable. Bindings are owned by the Tenant CR and garbage-collected on deletion.

### 4.4 ArgoCD Application — The Deployment Handoff

Generated by the orchestrator, one per app per tenant. Contains the chart reference from the AppProfile, rendered Helm values with secret references pointing to Kubernetes Secrets synced by ESO, and an owner reference to the Tenant CR. This is where the provisioning plane ends and the deployment plane begins.

## 5. Secret Management — OpenBao + External Secrets Operator

All secrets flow through OpenBao. OpenTofu seeds the kernel secrets (database root credentials, identity provider endpoints). Existing operators and the orchestrator create tenant-scoped secrets (OIDC client credentials, per-app database users, S3 keys, SMTP credentials, DKIM keys). **External Secrets Operator (ESO)** syncs OpenBao paths into Kubernetes Secrets, which Helm charts reference via standard `existingSecret` patterns.

### 5.1 Secret Seeding — Deterministic Derivation

Kernel secrets are seeded deterministically from a single **master password** using HMAC-SHA256 derivation. Each credential is derived from a `(context, purpose)` pair:

```bash
derive_password() {
    echo -n "${context}:${purpose}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary | sha1sum | awk '{print $1}'
}
```

This gives three properties: (1) **one secret to protect** instead of hundreds, (2) **idempotent re-seeding** — rerunning the seeding script produces identical credentials, and (3) **disaster recovery** — if OpenBao is lost, all kernel credentials can be regenerated from the master password alone without requiring backup restoration.

The seeding script uses a `kv_put_once()` guard that only writes a secret path if it does not already exist, preventing accidental overwrites of live credentials on re-runs.

### 5.2 OpenTofu Secret Lifecycle Guard

All OpenBao secrets managed by OpenTofu use `lifecycle { ignore_changes = [data_json] }`. This ensures Tofu creates secrets on first apply but **never overwrites live credentials** on subsequent applies — protecting against the most dangerous Terraform anti-pattern where state drift causes unintended credential resets.

### 5.3 Two Secret Delivery Patterns

Not all upstream Helm charts support `existingSecret` references. The platform uses two delivery patterns based on chart capabilities:

| Pattern | Mechanism | When to use |
|---|---|---|
| **Pattern A** (preferred) | ESO syncs OpenBao → Kubernetes Secret; chart references via `existingSecret` | Charts with `existingSecret` support (Redis, MinIO, Intercom Service) |
| **Pattern B** (fallback) | Tofu Controller reads from OpenBao and injects via Helm `set_sensitive` | Charts without `existingSecret` support (Nubus, OX App Suite, Postfix, Dovecot) |

Pattern B keeps secrets out of Git and the ArgoCD UI — they remain in memory during Helm apply. The trade-off is reduced ArgoCD visibility (Tofu-managed releases appear as opaque resources). New apps should prefer Pattern A; Pattern B is a pragmatic fallback for upstream charts that cannot be modified.

**Upstream contribution strategy:** The long-term goal is to eliminate Pattern B entirely by contributing `existingSecret` support to the upstream Helm charts that currently require it (Nubus, OX App Suite, Postfix, Dovecot). Each successful upstream merge allows migrating that app from Pattern B to Pattern A — one configuration change in the AppProfile’s `deploymentMethod` field, zero orchestrator code changes. This reduces Tofu Controller dependencies and improves ArgoCD visibility. Track upstream PR status per app.

### 5.4 Credential Rotation and Pod Restart

Credential rotation is passive: the orchestrator rotates credentials in OpenBao, and ESO automatically syncs the new values into Kubernetes Secrets. However, ArgoCD does not restart pods when only a Secret's *content* changes (it watches manifests, not data). **Stakater Reloader** bridges this gap — workloads annotated with `reloader.stakater.com/auto: "true"` are automatically rolled when a referenced Secret or ConfigMap changes. This is triggered via annotation (`kubectl annotate tenant acme-corp gentianos.io/rotate-credentials=all`).

**Secret path structure:**

```
gentian-os/
├── kernel/                              # Seeded by OpenTofu, read-only to apps
│   ├── identity/                        #   oidc_issuer, ldap_host, admin creds
│   ├── database/                        #   root credentials per engine
│   ├── storage/                         #   S3 admin credentials
│   ├── mail/                            #   MTA/MDA admin credentials
│   ├── cache/                           #   Redis/Memcached admin credentials
│   └── messaging/                       #   reserved for future IPC bus
│
└── tenants/
    └── {tenant-name}/
        ├── apps/
        │   └── {app-name}/
        │       ├── oidc                 #   client_id, client_secret
        │       ├── database             #   user, password, database name
        │       ├── s3                   #   access_key, secret_key, bucket
        │       ├── ldap                 #   bind_dn, bind_password, base_dn
        │       ├── smtp                 #   user, password
        │       ├── imap                 #   host, port, credentials
        │       └── cache                #   host, port, password
        ├── contracts/
        │   └── {contract-name}/         #   endpoint, auth, shared credentials
        └── mail/
            └── {domain}/               #   DKIM private key, domain config
```

Secrets never appear in Git, ArgoCD Application CRs, or ConfigMaps. OpenBao policies are generated per tenant+app, granting read access only to the paths that app needs.

**Secret flow:**

```mermaid
sequenceDiagram
    participant OT as OpenTofu
    participant OR as Orchestrator
    participant OP as Operators
    participant OB as OpenBao
    participant ESO as External Secrets Operator
    participant AC as ArgoCD

    OT->>OB: Seed kernel/ credentials (HMAC-SHA256 derived from master password)
    Note over OR: Tenant CR applied
    OR->>OP: Create operator CRs (DB, OIDC, bucket...)
    OP->>OB: Store provisioned credentials
    OR->>ESO: Create ExternalSecret CRs
    ESO->>OB: Sync secrets to K8s Secrets
    OR->>AC: Create ArgoCD Application CR
    AC->>AC: Deploy Helm chart (Pattern A: refs existingSecrets)
    Note over OT,AC: Pattern B (fallback): Tofu Controller deploys via set_sensitive for charts without existingSecret support
```

## 6. Deployment Layers

### Layer 000 — Bootstrap

A one-time init script (or Ansible playbook) that installs ArgoCD and Tofu Controller onto a bare cluster. This is the only component not managed by GitOps — everything else flows from here. Future sublayers (e.g., 001 for cert-manager, 002 for sealed-secrets) can be added as bootstrap dependencies grow.

**Safety guards on stateful bootstrap components:** ArgoCD Applications for OpenBao, Tofu Controller, and other stateful infrastructure must be deployed with `prune: false` and `finalizers: []`. This prevents ArgoCD from ever deleting these resources if their manifests are temporarily removed from Git — accidentally pruning OpenBao would be catastrophic. Self-healing (`selfHeal: true`) is still enabled to reconcile drift in values.

### Layer 100 — Kernel (OpenTofu + ArgoCD)

OpenTofu provisions the kernel infrastructure: namespaces, OpenBao instance and mounts, secret seeding, ArgoCD AppProject, CRD registration. ArgoCD deploys the kernel workloads as Helm charts: identity provider, database operators, object storage, cache, shell UI, notification gateway. The Gentian OS orchestrator and External Secrets Operator are deployed here too.

Sublayers allow sequencing within the kernel:

| Layer | Contents | Depends on |
|-------|----------|------------|
| 100 | Namespaces, OpenBao, cert-manager, ESO, Stakater Reloader | Layer 000 |
| 110 | Identity (Keycloak Operator, UCS LDAP) | 100 |
| 120 | Databases (CloudNativePG, MariaDB Operator) | 100 |
| 130 | Storage (MinIO Operator, Nextcloud) | 110, 120 |
| 140 | Cache (Redis Operator, Memcached) | 100 |
| 150 | Shell UI, Notification Gateway | 110 |
| 160 | Gentian OS Orchestrator | All of the above |

### Layer 100e — Kernel Extensions

Optional kernel extensions deployed per-tenant or per-cluster as needed.

| Layer | Contents | Depends on |
|-------|----------|------------|
| 100e-mail | Mail stack (Postfix, Dovecot, Rspamd) | 110 (LDAP), 120 (optional DB for Rspamd) |

### ArgoCD Application Discovery — Root ApplicationSet

Kernel and app deployments are discovered via a **root ApplicationSet** that watches a directory of ApplicationSet manifests. Adding a new deployment layer = adding one YAML file to the directory; ArgoCD auto-discovers it without modifying the root. This provides a fully declarative, Git-visible deployment pipeline.

ApplicationSets use **matrix generators** with explicit environment × app lists rather than Git directory generators. This makes the deployment matrix predictable, auditable, and avoids surprising auto-discovery of stray files:

```yaml
generators:
  - matrix:
      generators:
        - list:
            elements:
              - env: dev
              - env: staging
        - list:
            elements:
              - app: openproject
                nsPrefix: tenant
              - app: nextcloud
                nsPrefix: tenant
```

The orchestrator extends this pattern for tenant-scoped apps by updating the app list within a tenant's ApplicationSet when a Tenant CR changes, rather than creating individual Application CRs directly. This preserves Git-visible deployment state while keeping the orchestrator's role limited to list management.

### Layer 200 — Apps (Orchestrator + ArgoCD)

User-installable applications. The orchestrator reacts to Tenant CRD changes, creates operator CRs (databases, OIDC clients, buckets), creates ExternalSecret CRs, and creates ArgoCD Application CRs. ArgoCD deploys the Helm charts, monitors health, and handles upgrades.

Sublayers can distinguish app categories:

| Layer | Contents | Example |
|-------|----------|---------|
| 200 | Core productivity apps | OX App Suite, Collabora |
| 210 | Communication apps | Element/Matrix, Jitsi |
| 220 | Project & knowledge apps | OpenProject, XWiki |
| 230 | Custom / third-party apps | Tenant-specific installations |

## 7. Repository Structure

The platform is organised into three Git repositories, separated by rate of change and concern. The OS definition changes when the platform evolves. The app catalogue changes when an app is added or upgraded. The deployment state changes when a tenant is created or an environment is configured. Mixing them would mean a tenant creation commit triggers CI pipelines for the entire platform.

### Repo 1 — `gentian-os` (the OS)

The platform itself: orchestrator source code, CRD definitions, OpenTofu modules for kernel seeding, Helm chart for the orchestrator, and kernel layer definitions. This repo produces versioned artifacts — a container image and a Helm chart — published to an OCI registry.

```
gentian-os/
├── api/v1alpha1/                # CRD type definitions
├── internal/                    # Orchestrator source code
├── config/crd/                  # Generated CRD manifests
├── charts/
│   └── gentian-os-orchestrator/ # Helm chart for the orchestrator
├── kernel/
│   ├── tofu/                    # OpenTofu modules (secret seeding, namespaces)
│   └── applications/            # ArgoCD Application manifests for kernel services
│       ├── 100-openbao.yaml
│       ├── 110-keycloak.yaml
│       ├── 120-cloudnativepg.yaml
│       ├── 130-minio.yaml
│       └── ...
├── docs/
└── Makefile
```

### Repo 2 — `gentian-apps` (the app catalogue)

One AppProfile YAML per app. Each profile wraps an upstream Helm chart with the schema-based value mapping and kernel requirements. This repo does not contain upstream charts — it references them by OCI URL and version.

```
gentian-apps/
├── profiles/
│   ├── openproject.yaml
│   ├── nextcloud.yaml
│   ├── ox-appsuite.yaml
│   ├── element.yaml
│   ├── collabora.yaml
│   ├── xwiki.yaml
│   └── jitsi.yaml
├── contracts/                   # Contract schema definitions
│   ├── file-store.yaml
│   ├── filepicker.yaml
│   └── central-navigation.yaml
└── tests/
    └── validate-profiles.sh     # Schema validation for all profiles
```

### Repo 3 — `gentian-deployments` (the cluster state)

The only repo specific to a running cluster. Contains Tenant CRs, environment-specific OpenTofu variables, and the ArgoCD App of Apps that references the OS and catalogue by version. This repo does **not** fork or submodule the OS repo — it references published artifacts by version, the same way `apt` references a package version without forking Debian.

```
gentian-deployments/
├── production/
│   ├── bootstrap/
│   │   └── install.sh             # Layer 000 — one-time ArgoCD + Tofu Controller
│   ├── kernel/
│   │   ├── values-production.yaml # Environment-specific kernel overrides
│   │   └── tofu.tfvars            # OpenTofu variables (domain, root creds ref)
│   ├── app-of-apps.yaml           # ArgoCD Application pointing at gentian-os + gentian-apps
│   └── tenants/
│       ├── acme-corp.yaml         # Tenant CR
│       ├── beta-inc.yaml
│       └── new-customer.yaml
├── staging/
│   └── ...
└── dev/
    └── ...
```

### How the Three Repos Connect

ArgoCD watches all three sources. The `app-of-apps.yaml` in the deployment repo ties everything together:

```mermaid
graph TD
    AOA[app-of-apps.yaml\nin gentian-deployments/production/]
    OS[gentian-os repo\nOCI registry v2.0.0]
    CAT[gentian-apps repo\nGit tag v1.3.0]
    DEP[gentian-deployments repo\nproduction/tenants/]
    AC[ArgoCD]

    AOA --> OS
    AOA --> CAT
    AOA --> DEP
    AC -- watches --> AOA
    AC -- pulls orchestrator chart --> OS
    AC -- syncs AppProfile CRs --> CAT
    AC -- syncs Tenant CRs --> DEP
```

Upgrading the OS means bumping a version string in the deployment repo. Adding a new app means committing an AppProfile to the catalogue repo. Creating a tenant means adding a Tenant YAML to the deployment repo. Each change flows through ArgoCD independently.

### Upstream Helm Charts

AppProfiles reference upstream charts directly by OCI URL (e.g., `oci://charts.openproject.org/openproject:14.2.0`). ArgoCD pulls from upstream — no chart management needed. If an upstream chart must be patched (which should be avoided), create a thin wrapper chart in a separate `gentianos-charts` repo that depends on the upstream chart and adds overrides.

## 8. Security Model

**Network boundaries:** Tenant namespaces can reach kernel services but not other tenant namespaces. NetworkPolicies enforce this at the CNI level. IntegrationBindings create scoped app-to-app rules within a tenant.

**OIDC trust chain:** The identity provider is the single trust anchor. Each tenant gets a dedicated Keycloak realm with independent user pools, branding, and password policies. Apps authenticate users via OIDC. App-to-app calls use token exchange (RFC 8693) — app A presents its token and receives a scoped token for app B. The IntegrationBinding configures which exchanges are permitted.

**Mail security:** DKIM private keys are stored in OpenBao and injected into the mail extension at deployment time. SPF and DMARC records are generated and surfaced in the Tenant status for DNS configuration. SMTP submission requires SASL authentication — no open relay. Dovecot authenticates IMAP sessions against the tenant's LDAP directory.

**Database isolation:** Each app within each tenant gets its own database (`{prefix}_{app}`) with a dedicated user that has grants limited to that database only. No cross-tenant or cross-app database access is possible.

> **Future direction — database scaling:** At very high tenant counts (500+), database-per-tenant-per-app produces thousands of databases. Two scaling strategies are available: (1) **schema-per-app** within a tenant database, reducing count by the number of apps per tenant — requires apps to support configurable schema names; (2) **multiple PostgreSQL clusters** with tenant sharding, distributing load across independent database instances.

## 9. Backup and Migration

### 9.1 Backup Strategy

Backup in Gentian OS is **per-subsystem** — each kernel component uses the industry-standard backup tool for its data type, orchestrated centrally.

| Data type | Tool | Scope | Method |
|---|---|---|---|
| PostgreSQL databases | **pgBackRest** or CloudNativePG built-in backup | Per-database (tenant-scoped restores) | WAL archiving + base backups to S3 (MinIO or external) |
| MariaDB databases | **Mariabackup** or MariaDB Operator backup CRD | Per-database | Full + incremental to S3 |
| S3 / MinIO buckets | **MinIO replication** or **Restic** | Per-bucket (tenant-scoped) | Cross-site replication or snapshot to external S3 |
| Dovecot mailboxes | **dsync** (Dovecot's native replication) or **Restic** | Per-tenant mail domain | Filesystem-level backup or Dovecot-native sync |
| Keycloak realms | **Keycloak realm export** (JSON) | Per-realm (tenant-scoped) | Scheduled export to S3, versioned |
| OpenBao secrets | **OpenBao snapshots** (`bao operator raft snapshot`) | Full vault | Raft snapshots to S3, encrypted |
| Kubernetes resources | **Velero** | Per-namespace (tenant-scoped) | CRD state, ConfigMaps, Secrets (encrypted) |
| LDAP directory | **slapcat** or UDM backup API | Per-OU (tenant-scoped) | LDIF export to S3 |

**Velero** serves as the cross-cutting backup orchestrator for Kubernetes-native resources (CRDs, ConfigMaps, namespace metadata). It can trigger pre/post-backup hooks that coordinate with the application-specific tools listed above.

### 9.2 Tenant-Scoped Restore

The per-tenant isolation model (separate databases, buckets, realms, namespaces) enables **single-tenant restore** without affecting other tenants. Restoring tenant `acme-corp` means:

1. Restore the PostgreSQL databases matching `acme_*` from pgBackRest.
2. Restore the MinIO buckets matching `acme-corp-*`.
3. Restore the Dovecot mailboxes for the `acme.example.com` domain.
4. Re-import the Keycloak realm `acme-corp` from the JSON export.
5. Restore the namespace `tenant-acme-corp` via Velero.

The orchestrator can automate this sequence by responding to a `RestoreTenant` CR (future work).

### 9.3 Tenant Migration

Moving a tenant between clusters follows the same backup/restore pattern: backup on the source cluster, restore on the target cluster, update DNS. The key requirement is that OpenBao secrets for the tenant are either migrated or re-provisioned. The orchestrator handles re-provisioning naturally — applying the Tenant CR on the target cluster triggers the full provisioning pipeline, and existing data is picked up from the restored databases and buckets.

### 9.4 Disaster Recovery

For full-cluster DR, the recovery sequence follows the deployment layers: restore Layer 000 (bootstrap ArgoCD + Tofu Controller), apply Layer 100 (kernel infrastructure from OpenTofu state + ArgoCD Git repo), restore data (OpenBao snapshots, database backups, S3 replication), then apply Layer 200 (tenant CRs from Git trigger re-provisioning). GitOps ensures that the desired state of all workloads is recoverable from the Git repository; only the stateful data requires backup restoration.

---

# Part III — Agentic AI Integration

## 10. AI-Native Platform Design

Traditional workplace suites integrate apps through static configurations — hardcoded webhooks, manual API wiring, point-to-point integrations. Gentian OS is designed to support an **agentic AI layer** that discovers app capabilities dynamically and orchestrates cross-app workflows on behalf of users. This is enabled by the **Model Context Protocol (MCP)**, which provides a standardised way for AI agents to discover and invoke the capabilities that applications expose.

### 10.1 MCP as the Capability Discovery Layer

The existing contract system (IntegrationBindings) handles data-level integration: WebDAV file access, OIDC token exchange, shared credentials. But contracts are static declarations — an AppProfile says "I provide `project-management` via `http-json`" without describing what operations are available, what parameters they take, or what data they return.

MCP complements contracts by making capabilities **self-describing and machine-invocable**. An app that exposes an MCP server allows an AI agent to call `tools/list` and receive structured descriptions of every operation: create a task, list milestones, assign a user, attach a file. The contract stops being "this app does project management" and becomes "here are the specific things this app can do, with typed parameters and return values."

The two mechanisms serve different purposes and coexist:

| Mechanism | Purpose | Example |
|---|---|---|
| IntegrationBinding | Data-level plumbing between apps | Nextcloud provides WebDAV to OX App Suite |
| MCP endpoint | Capability discovery and AI-driven invocation | AI assistant creates an OpenProject task on behalf of a user |

### 10.2 MCP as a Kernel Requirement

Apps that expose an MCP server declare it in their AppProfile as a kernel requirement:

```yaml
kernelRequirements:
  mcp:
    enabled: true
    endpoint: /mcp
    auth: oidc    # MCP calls authenticated via the user's OIDC token
```

The orchestrator provisions the MCP endpoint, registers it in the **MCP registry** (a lightweight kernel service that tracks which apps expose MCP endpoints per tenant), and wires authentication so that the AI assistant can call app MCP servers on behalf of the logged-in user using the same OIDC token exchange mechanism already in the architecture.

The MCP registry is populated automatically — no manual registration needed. When the orchestrator provisions an app that declares `mcp: enabled`, the registry entry is created. When the app is uninstalled, the entry is removed.

### 10.3 Shell AI Assistant

The Univention Portal (the shell/window manager) gains an AI assistant that can act across all installed apps. The assistant uses three kernel services:

- **Identity** — knows who the user is, what groups they belong to, what permissions they have.
- **MCP registry** — discovers which apps are installed and what each can do.
- **OIDC token exchange** — authenticates MCP calls on behalf of the user.

This enables natural-language workflows like "create a project called Q3 Planning in OpenProject and invite the marketing team" — the assistant looks up the marketing group in LDAP, calls OpenProject's MCP server to create the project, and adds the group members. No custom integration code, no webhooks, no NATS events.

### 10.4 Cross-App Agent Orchestration

Once multiple apps expose MCP endpoints, an AI agent can orchestrate workflows that span apps without any point-to-point integration:

- "When a new invoice arrives in OX Mail, create a task in OpenProject, attach the PDF from Nextcloud, and notify the finance channel in Element."
- "Summarise yesterday's Element chat in the marketing channel and post it to the project wiki in XWiki."
- "Find all files in Nextcloud shared with external users and list them as issues in OpenProject for review."

This addresses the NATS/IPC gap from a different angle. Instead of building an event bus that existing apps don't natively speak (which would require webhook adapters or sidecar containers per app), the AI agent becomes the integration layer. The agent watches for triggers (new email, file upload, task completion) and orchestrates responses via MCP calls. The "bus" is the agent, not a message broker.

### 10.5 AI-Assisted Platform Operations

Beyond user-facing workflows, AI agents can assist platform operators:

- **AppProfile generation** — point an agent at an upstream Helm chart's `values.yaml` and documentation, and it generates the AppProfile with the correct `valueMapping` and `kernelRequirements`. This turns "adding an app to the catalogue" from manual YAML authoring into an AI-assisted task.
- **Tenant provisioning assistant** — an agent that helps administrators create tenants by asking questions ("How many users? Do you need self-hosted mail? Which apps?") and generating the Tenant CR.
- **Health monitoring and diagnosis** — an agent that watches Prometheus metrics and CRD status conditions, proactively suggests scaling, and diagnoses provisioning failures by reading operator CR statuses and correlating error patterns.

### 10.6 Implementation Roadmap

| Phase | Scope | Depends on |
|---|---|---|
| **v1** | Shell AI assistant with MCP registry as kernel service. MCP adapters for 2–3 core apps (OpenProject, Nextcloud). | Kernel (identity, shell UI), OIDC token exchange |
| **v2** | MCP adapters for remaining openDesk apps. Cross-app workflow orchestration. | v1 MCP registry, app-specific MCP server development |
| **v3** | AI-assisted AppProfile generation. Operator health monitoring agent. | Stable AppProfile schema, observability stack |

The primary engineering effort is building MCP servers (or adapters) for each openDesk app. The kernel infrastructure (registry, auth wiring, shell integration) is lightweight. The strategic bet is that MCP becomes the standard protocol for AI-to-app interaction — its adoption by Anthropic, its open specification, and its growing ecosystem of community-built servers support this direction.

---

# Part IV — Case Study: openDesk

## 11. openDesk Overview

openDesk is a sovereign workplace suite developed by ZenDiS (Centre for Digital Sovereignty) for the German public sector. It integrates open-source applications into a unified browser-based workspace as an alternative to Microsoft 365. The architecture described in Parts I and II was designed to repackage openDesk for scalable, multi-tenant operation.

### 11.1 openDesk Components Mapped to Gentian OS

| OS Function | openDesk Component | Role | Layer | Status |
|---|---|---|---|---|
| Identity & permissions | **Nubus** (Keycloak + UCS LDAP) | OIDC provider, user/group directory | Kernel | Existing — needs Keycloak Operator CRs |
| Filesystem | **Nextcloud** | WebDAV file access, locking, sharing | Kernel | Existing — needs AppProfile + value mapping |
| Networking | Kubernetes CNI + Ingress | NetworkPolicies per tenant | Kernel | Existing — orchestrator generates policies |
| Process execution | Kubernetes + ArgoCD | Workload scheduling, GitOps deployment | Kernel | Existing — ArgoCD Application CRs |
| Secrets & keyring | **OpenBao** + **ESO** | Secret store + sync to K8s Secrets | Kernel | Existing — needs ExternalSecret CRs |
| Database services | **PostgreSQL** (CloudNativePG) + **MariaDB** | Per-tenant-per-app databases | Kernel | Existing — CloudNativePG CRs |
| Cache subsystem | **Redis** + **Memcached** | Per-app caching (Redis ACLs, Memcached SASL) | Kernel | Existing — needs operator CRs |
| Package manager | AppProfile CRD + ArgoCD | App catalogue + deployment pipeline | Kernel | **To build** (orchestrator) |
| App-to-app permissions | IntegrationBinding CRD | Contract-based bindings + OIDC token exchange | Kernel | **To build** (orchestrator) |
| Window manager | **Univention Portal** | App launcher, SSO session, unified navigation | Kernel | Existing — needs AppProfile |
| Notifications | **Notification Gateway** | Cross-app notification aggregation | Kernel | **To build** |
| Init system / lifecycle | **Thin orchestrator** | Install, upgrade, uninstall via operator CRs | Kernel | **To build** (orchestrator) |
| Resource quotas | Kubernetes ResourceQuotas + LimitRanges | Per-tenant CPU, memory, storage limits | Kernel | Existing — orchestrator applies quotas |
| Mail (kernel extension) | **Postfix + Dovecot + Rspamd** | SMTP transport, IMAP storage, spam filtering | Kernel ext. | Existing — deploy as per-tenant extension |
| IPC bus | — | Event-driven pub/sub (e.g., NATS) | — | **Out of scope** (v1) |
| Clipboard / intents | — | Share-to / cross-app data transfer | — | **Out of scope** (v1) |
| Config store | — | Per-tenant, per-app key-value settings | — | **Out of scope** (v1) |
| Capability enforcement | — | Runtime permission checks via service mesh | — | **Out of scope** (v1) |
| Groupware / mail client | **OX App Suite** | Webmail, calendar, contacts | App | Existing — needs AppProfile |
| Collaboration | **Collabora Online** | Document editing (via Nextcloud) | App | Existing — needs AppProfile |
| Chat & video | **Element** (Matrix) + **Jitsi** | Messaging, video conferencing | App | Existing — needs AppProfile |
| Project management | **OpenProject** | Tasks, timelines, agile boards | App | Existing — needs AppProfile |
| Wiki | **XWiki** | Knowledge management | App | Existing — needs AppProfile |

### 11.2 Namespace Layout

```mermaid
graph LR
    subgraph Kernel
        PK[platform-kernel\nKeycloak / MinIO\nPostgreSQL / MariaDB\nRedis / Memcached / OpenBao\nNextcloud / Univention Portal]
    end
    subgraph System
        PS[platform-system\nArgoCD / Tofu Controller\nGentianOS Orchestrator\nESO / cert-manager]
    end
    subgraph Tenants
        TA[tenant-acme-corp\nAcme Corp apps\n+ mail extension if selfhosted]
        TB[tenant-beta-inc\nBeta Inc apps]
    end

    TA -- NetworkPolicy allowed --> PK
    TB -- NetworkPolicy allowed --> PK
    TA -. NetworkPolicy denied .- TB

    style PK fill:#e8f4f8,stroke:#2980b9
    style PS fill:#f5eef8,stroke:#8e44ad
    style TA fill:#eafaf1,stroke:#27ae60
    style TB fill:#eafaf1,stroke:#27ae60
```

### 11.3 openDesk Technology Stack

| Concern | Tool | Role |
|---------|------|------|
| Kernel infrastructure | **OpenTofu** (via Tofu Controller) | Static provisioning, secret seeding, external resources |
| Secrets & sync | **OpenBao** + **External Secrets Operator** | Secret storage + sync to K8s Secrets |
| Provisioning | **Thin orchestrator** (Go + controller-runtime) | Tenant lifecycle via operator CRs |
| App deployment | **ArgoCD** | Helm chart deployment, drift detection, rollback |
| Identity | **Nubus** (Keycloak + UCS LDAP) | OIDC, user/group directory, SSO |
| Mail (extension) | **Postfix + Dovecot + Rspamd** | Per-tenant mail stack |
| Groupware | **OX App Suite** | Mail client, calendar, contacts |

## 12. CRD Definitions for openDesk

### 12.1 AppProfile (with schema-based value mapping)

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppProfile
metadata:
  name: openproject            # cluster-scoped
spec:
  displayName: "OpenProject"

  kernelRequirements:
    identity:
      oidc: true
      ldap: { sync: true, interval: 1h }
    database:
      engine: postgresql
      databasePerTenant: true
    storage:
      s3: { bucketPerTenant: true }
      files: { protocol: webdav, capabilities: [read, write] }
    cache:
      engine: memcached
    mail:
      smtp: { auth: cram-md5, port: 587 }

  provides:
    - name: project-management
      protocol: http-json

  optionalIntegrations:
    - contract: file-store
      provider: nextcloud
      capabilities: [webdav:read, webdav:write]
    - contract: central-navigation
      provider: portal
      capabilities: [navigation:register]

  chart:
    repository: oci://registry.gentianos.io/charts
    name: openproject
    version: "14.2.0"

  # Schema-based value mapping — validated at admission time
  valueMapping:
    oidc:
      issuerKey: "oidc.issuer"
      clientIdKey: "oidc.clientId"
      clientSecretKey: "oidc.clientSecret"
    database:
      hostKey: "database.host"
      nameKey: "database.name"
      userKey: "database.user"
      passwordKey: "database.password"
    s3:
      endpointKey: "s3.endpoint"
      bucketKey: "s3.bucket"
      accessKeyKey: "s3.accessKey"
      secretKeyKey: "s3.secretKey"
    smtp:
      hostKey: "smtp.host"
      userKey: "smtp.user"
      passwordKey: "smtp.password"
    cache:
      hostKey: "cache.host"
      portKey: "cache.port"
    ldap:
      hostKey: "ldap.host"
      baseDnKey: "ldap.baseDn"
      bindDnKey: "ldap.bindDn"
      bindPasswordKey: "ldap.bindPassword"

  # Escape hatch for non-standard values
  extraValues:
    smtp:
      port: 587
```

### 12.2 Tenant

```yaml
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: acme-corp
spec:
  displayName: "ACME Corporation"
  domain: acme.gentianos.example.com
  adminEmail: admin@acme.example.com

  isolation:
    mode: namespace              # namespace | vcluster
    namespace: tenant-acme-corp
    ldapOU: "ou=acme-corp"
    keycloakRealm: acme-corp
    databasePrefix: acme_
    s3Prefix: acme-corp-

  mail:
    mode: selfhosted             # selfhosted | external | transport-only | disabled
    domain: acme.example.com
    quotaPerUser: 5Gi
    rateLimit: 100/h

  quotas:
    maxApps: 20
    storage: 100Gi
    cpu: "8"
    memory: 16Gi

  deletionPolicy: Retain         # Retain | Delete

  apps:
    - profile: nextcloud
    - profile: ox-appsuite
    - profile: element
    - profile: openproject
      config:
        replicas: 2
    - profile: xwiki
    - profile: notes
```

### 12.3 IntegrationBinding (auto-generated)

```yaml
apiVersion: gentianos.io/v1alpha1
kind: IntegrationBinding
metadata:
  name: acme-corp-filepicker
  namespace: tenant-acme-corp
  ownerReferences:
    - kind: Tenant
      name: acme-corp
spec:
  contract: filepicker
  provider: { app: nextcloud, namespace: tenant-acme-corp }
  consumer: { app: ox-appsuite, namespace: tenant-acme-corp }
  capabilities: [webdav:read, webdav:write, ocs:shares]
  auth:
    method: oidc-token-exchange
    vaultPath: gentianos/tenants/acme-corp/contracts/filepicker
status:
  state: Ready
  conditions:
    - type: CredentialsValid
      status: "True"
      lastRotation: "2026-04-01T00:00:00Z"
    - type: ProviderReachable
      status: "True"
      lastProbeTime: "2026-04-02T10:30:00Z"
  secretRef:
    name: contract-filepicker
```

### 12.4 ExternalSecret (generated by orchestrator)

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: openproject-db
  namespace: tenant-acme-corp
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: gentianos-openbao
    kind: ClusterSecretStore
  target:
    name: openproject-db-credentials
    creationPolicy: Owner
  data:
    - secretKey: username
      remoteRef:
        key: gentianos/tenants/acme-corp/apps/openproject/database
        property: user
    - secretKey: password
      remoteRef:
        key: gentianos/tenants/acme-corp/apps/openproject/database
        property: password
```

### 12.5 ArgoCD Application (generated by orchestrator)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: acme-corp-openproject
  namespace: argocd
  labels:
    gentianos.io/tenant: acme-corp
    gentianos.io/app: openproject
  ownerReferences:
    - kind: Tenant
      name: acme-corp
spec:
  project: gentianos-tenants
  source:
    repoURL: oci://registry.gentianos.io/charts
    chart: openproject
    targetRevision: "14.2.0"
    helm:
      valuesObject:
        oidc:
          issuer: "https://keycloak.gentianos.example.com/realms/acme-corp"
          existingSecret: openproject-oidc-credentials
        database:
          host: "postgresql.platform-kernel.svc.cluster.local"
          name: "acme_openproject"
          existingSecret: openproject-db-credentials
        s3:
          endpoint: "https://minio.platform-kernel.svc.cluster.local"
          bucket: "acme-corp-openproject"
          existingSecret: openproject-s3-credentials
        smtp:
          host: "postfix.tenant-acme-corp.svc.cluster.local"
          port: 587
          existingSecret: openproject-smtp-credentials
  destination:
    server: https://kubernetes.default.svc
    namespace: tenant-acme-corp
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=false
```

## 13. OpenTofu — Kernel Seeding for openDesk

```hcl
resource "kubernetes_namespace" "kernel" {
  metadata { name = "platform-kernel" }
}

resource "vault_kv_secret_v2" "kernel_identity" {
  mount = vault_mount.gentianos.path
  name  = "kernel/identity"
  data_json = jsonencode({
    oidc_issuer  = "https://keycloak.${var.domain}/realms/gentianos"
    admin_api    = "https://keycloak.platform-kernel.svc.cluster.local:8443"
    ldap_host    = "openldap.platform-kernel.svc.cluster.local"
    ldap_port    = 389
    ldap_base_dn = var.ldap_base_dn
  })
}

resource "vault_kv_secret_v2" "kernel_database" {
  mount = vault_mount.gentianos.path
  name  = "kernel/database"
  data_json = jsonencode({
    postgresql_host      = "postgresql.platform-kernel.svc.cluster.local"
    postgresql_port      = 5432
    postgresql_root_user = var.pg_root_user
    postgresql_root_pass = var.pg_root_password
    mariadb_host         = "mariadb.platform-kernel.svc.cluster.local"
    mariadb_port         = 3306
    mariadb_root_user    = var.mdb_root_user
    mariadb_root_pass    = var.mdb_root_password
  })
}

resource "kubernetes_manifest" "argocd_project" {
  manifest = {
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "AppProject"
    metadata   = { name = "gentianos-tenants", namespace = "argocd" }
    spec = {
      sourceRepos  = ["oci://registry.gentianos.io/charts"]
      destinations = [{ server = "*", namespace = "tenant-*" }]
    }
  }
}

resource "helm_release" "gentianos_orchestrator" {
  name       = "gentianos-orchestrator"
  namespace  = "platform-system"
  repository = "oci://registry.gentianos.io/charts"
  chart      = "gentianos-orchestrator"
  version    = var.orchestrator_version
}
```

## 14. Orchestrator Reconciliation Logic

### 14.1 On Tenant Create/Update

```
func (r *TenantReconciler) Reconcile(ctx, req) (Result, error) {

    tenant := fetch Tenant CR

    1. Provision tenant-level resources (via operator CRs):
       ├── Create namespace (or vCluster if isolation.mode == vcluster)
       ├── Create KeycloakRealmImport CR → Keycloak Operator provisions realm
       ├── Create LDAP OU via UDM Job
       ├── Create MinIO Tenant prefix via MinIO Operator CR
       ├── Apply ResourceQuotas + LimitRanges
       └── If mail.mode == selfhosted:
           ├── Create ArgoCD Application for per-tenant mail stack
           ├── Generate DKIM keypair, store in OpenBao
           └── Surface DNS records (DKIM, SPF, DMARC) in tenant status

    for each app in tenant.spec.apps:

        2. Fetch AppProfile from cluster cache (informer)

        3. Create operator CRs for app-scoped resources:
           ├── CloudNativePG Database CR → operator creates DB + user
           ├── MinIO bucket CR → operator creates bucket + IAM
           ├── KeycloakClient CR → operator registers OIDC client
           ├── LDAP bind account via UDM Job
           ├── Redis ACL user via operator CR or Job
           └── Dovecot mailbox accounts (if mail.mode == selfhosted)

        4. Create ExternalSecret CRs:
           → ESO syncs gentianos/tenants/{tenant}/apps/{app}/*
              into K8s Secrets in tenant namespace

        5. Generate and apply OpenBao policy for tenant+app

        6. Resolve optional integrations:
           ├── Is the provider app in this tenant's app list?
           ├── If yes → create IntegrationBinding CR
           └── Create ExternalSecret for contract credentials

        7. Create NetworkPolicies:
           ├── Allow app → kernel services (per kernelRequirements)
           ├── Allow app → app (per IntegrationBindings)
           └── Deny all other cross-namespace traffic

        8. Render valueMapping with resolved context:
           ├── Map kernel endpoints to chart value keys
           ├── Reference existingSecret names from ESO
           └── Merge extraValues

        9. Create/update ArgoCD Application CR:
           ├── Chart reference from AppProfile
           ├── Rendered values (existingSecret references)
           ├── Destination: tenant namespace (or vCluster endpoint)
           ├── Sync policy: automated, self-healing
           └── Owner reference → Tenant CR

    10. Update Tenant status with per-app readiness

    return ctrl.Result{RequeueAfter: 5 * time.Minute}
}
```

### 14.2 On Tenant Delete

```
1. ArgoCD Applications are garbage-collected via ownerReferences
   (ArgoCD finalizer handles Helm uninstall)
2. Revoke OpenBao credentials and delete policies
3. Delete ExternalSecret CRs → ESO cleans up K8s Secrets
4. Based on tenant.spec.deletionPolicy:
   ├── Retain: revoke access but keep databases, buckets, mailboxes
   └── Delete: delete operator CRs → operators drop databases, buckets, etc.
5. Delete Keycloak realm CR → operator removes realm
6. Remove LDAP OU via UDM Job
7. Remove per-tenant mail stack (if selfhosted) → ArgoCD deletes
8. Delete tenant namespace (or vCluster)
9. Remove Tenant finalizer
```

### 14.3 On AppProfile Update

```
1. List all Tenants referencing this profile (via label index)
2. For each tenant:
   ├── Re-render valueMapping if mapping changed
   ├── Update ArgoCD Application with new chart version
   └── ArgoCD handles the rolling upgrade
3. Update AppProfile status with affected tenant count
```

## 15. Building the Orchestrator

### 15.1 Project Structure (Kubebuilder scaffold)

```
gentianos-orchestrator/
├── api/
│   └── v1alpha1/
│       ├── appprofile_types.go
│       ├── tenant_types.go
│       ├── integrationbinding_types.go
│       └── zz_generated.deepcopy.go
├── internal/
│   ├── controller/
│   │   ├── tenant_controller.go
│   │   ├── tenant_controller_test.go
│   │   ├── appprofile_controller.go
│   │   └── binding_controller.go
│   ├── orchestration/
│   │   ├── tenant_provisioner.go      # creates operator CRs in sequence
│   │   ├── app_provisioner.go         # creates per-app operator CRs
│   │   ├── externalsecret_builder.go  # generates ExternalSecret CRs
│   │   ├── networkpolicy_builder.go   # generates NetworkPolicy CRs
│   │   └── mail_extension.go          # manages per-tenant mail stack
│   ├── rendering/
│   │   └── valuemapping.go            # schema-based value rendering
│   └── argocd/
│       └── application.go
├── config/
│   ├── crd/
│   ├── rbac/
│   └── manager/
├── test/
│   ├── e2e/
│   │   ├── tenant_lifecycle_test.go
│   │   ├── mail_extension_test.go
│   │   └── contract_wiring_test.go
│   └── integration/
│       ├── orchestration_test.go
│       └── rendering_test.go
├── cmd/
│   └── main.go
├── Dockerfile
├── Makefile
└── go.mod
```

### 15.2 Operator Dependencies

The orchestrator depends on these operators being deployed in Layer 100:

| Operator | CRDs used by orchestrator | Purpose |
|---|---|---|
| CloudNativePG | `Cluster`, `Database`, `Role` | PostgreSQL provisioning |
| Keycloak Operator | `KeycloakRealmImport`, `KeycloakClient` | OIDC + realm management |
| MinIO Operator | `Tenant` (MinIO) | Bucket + IAM provisioning |
| External Secrets Operator | `ExternalSecret`, `ClusterSecretStore` | OpenBao → K8s Secret sync |
| Redis Operator (optional) | Operator-specific CRs | Redis ACL management |
| NATS Operator (future) | Account, User CRs | Reserved for future IPC |

### 15.3 Key Interfaces

```go
// The orchestrator does not implement provisioners directly.
// It creates CRs for existing operators and tracks their status.
type ResourceOrchestrator interface {
    // Create all operator CRs needed for a tenant
    ProvisionTenant(ctx context.Context, tenant *v1alpha1.Tenant) error
    // Create all operator CRs needed for an app within a tenant
    ProvisionApp(ctx context.Context, tenant *v1alpha1.Tenant,
                 app *v1alpha1.AppProfile) error
    // Clean up operator CRs for a tenant
    DeprovisionTenant(ctx context.Context, tenant *v1alpha1.Tenant) error
    // Check if all operator CRs have reached Ready status
    CheckReadiness(ctx context.Context, tenant *v1alpha1.Tenant) (bool, error)
}
```

## 16. Observability

### Built-in via CRD Status

```bash
# Tenant health at a glance
kubectl get tenants
NAME          STATUS         APPS   READY   MAIL         AGE
acme-corp     Ready          6      6/6     selfhosted   30d
beta-inc      Ready          4      4/4     external     15d
new-customer  Provisioning   5      3/5     selfhosted   2m

# Integration contract health
kubectl get integrationbindings -A
NAMESPACE          NAME                  CONTRACT             STATUS   AGE
tenant-acme-corp   acme-filepicker       filepicker           Ready    30d
tenant-acme-corp   acme-file-store       file-store           Ready    30d
tenant-acme-corp   acme-central-nav      central-navigation   Ready    30d

# ArgoCD sync status
kubectl get applications -n argocd -l gentianos.io/tenant=acme-corp
NAME                       SYNC     HEALTH    STATUS
acme-corp-nextcloud        Synced   Healthy   Running
acme-corp-openproject      Synced   Healthy   Running
acme-corp-ox-appsuite      Synced   Healthy   Running
```

### Prometheus Metrics (exported by orchestrator)

| Metric | Description |
|--------|-------------|
| `gentianos_tenants_total` | Total number of tenants |
| `gentianos_tenant_apps_total` | Apps per tenant |
| `gentianos_provisioning_duration_seconds` | Time to provision a tenant |
| `gentianos_reconcile_errors_total` | Failed reconciliations by type |
| `gentianos_credentials_age_seconds` | Age of oldest credential per tenant |
| `gentianos_integration_bindings_status` | Binding health by contract type |
| `gentianos_externalsecrets_sync_status` | ESO sync health per tenant |
| `gentianos_operator_cr_ready_total` | Operator CRs in Ready state |
| `gentianos_operator_cr_failed_total` | Operator CRs in Failed state |
