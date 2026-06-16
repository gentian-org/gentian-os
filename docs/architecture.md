# Gentian OS — Platform Architecture

**Version:** 3.0-draft
**Status:** Proposal

> A standalone overview of the Gentian OS architecture. Deeper material
> on individual concerns is split into focused companion documents
> linked from each section.

---

## 1. What Gentian OS Is

Gentian OS is a **cloud-native operating system** for open-source
business applications. It runs on Kubernetes and exposes the same
"install / uninstall / use" experience to organisations that a desktop
OS exposes to a single user — except the "user" is a tenant
(organisation) and the "apps" are full multi-user products like
Nextcloud, OpenProject, OX App Suite, Element, or XWiki.

The design optimises for two things:

1. **Onboarding a new application** to the catalogue is a
   single-file change.
2. **Onboarding a new tenant** is a single declarative resource
   (`Tenant`) that triggers the entire provisioning pipeline.

For the rationale — why this gap exists in the open-source landscape
and why a kernel-style abstraction is the right answer — see
[design/cloud-os-rationale.md](design/cloud-os-rationale.md).

---

## 2. The OS Analogy

Gentian OS is structured like a traditional operating system, with
direct analogues for every layer:

| Traditional OS | Gentian OS |
|---|---|
| Syscall API (`open`, `socket`, `fork`) | **CRDs**: `Tenant`, `AppProfile`, `IntegrationBinding` |
| `libc` — friendly call → raw syscalls | **Crossplane Compositions** |
| Syscall dispatcher / VFS | **Crossplane Composition engine** |
| Loadable kernel modules / device drivers | **Crossplane providers** (`provider-helm`, `provider-vault`, `provider-kubernetes`, `provider-keycloak`, …) |
| Hardware (disks, NICs) | **External operators & APIs** (Keycloak, CloudNativePG, MinIO, OpenBao, cloud APIs) |
| File descriptor / process handle | **Managed Resource (MR) status** |
| Kernel scheduler / writeback | **Crossplane reconcile loop** |
| `init` / `systemd` | **ArgoCD** |
| Default mounts (`C:`, `/`, `~/`) | **Default-install kernel components** (Nextcloud, MinIO, Nubus, …) |

The CRDs are the syscall API. Crossplane is the kernel that implements
those syscalls. Providers are the device drivers. Compositions are
libc. ArgoCD is `init`. The full unpacking of this analogy and the
reasoning behind each mapping is in
[design/kernel.md](design/kernel.md).

---

## 3. Architecture at a Glance

Two control loops do all the work, with one shared secret store:

```mermaid
graph TD
    GIT[Git<br/>gentian-os / gentian-apps / gentian-deployments]
    AC[ArgoCD<br/>— deployment plane —<br/>Git sync · Drift · Rollback · Health]
    XP[Crossplane<br/>— provisioning plane —<br/>XR → MR composition<br/>Reconciliation]
    PROV[Crossplane Providers<br/>provider-helm / provider-kubernetes<br/>provider-vault / provider-keycloak]
    OB[OpenBao<br/>— secret store —]
    OP[Upstream Operators<br/>Keycloak · CloudNativePG<br/>MinIO · ESO · Reloader]

    GIT --> AC
    AC -- applies XRs, Compositions, Providers --> XP
    XP -- runs --> PROV
    PROV -- writes secrets/policies --> OB
    PROV -- creates operator CRs --> OP
    OP -- store credentials --> OB

    style XP fill:#e8f4f8,stroke:#2980b9
    style AC fill:#eafaf1,stroke:#27ae60
    style OB fill:#f5eef8,stroke:#8e44ad
    style OP fill:#fdf2e9,stroke:#e67e22
    style PROV fill:#fef9e7,stroke:#f39c12
```

| Tool | Role | Boundary |
|---|---|---|
| **ArgoCD** | Deployment plane: pulls Git, syncs all manifests, shows drift, supports rollback. | Never provisions infrastructure. |
| **Crossplane** | Provisioning plane: composes `Cluster` and per-tenant **`App`** claims into managed resources (helm `Release`, ESO, …). | Does not replace the gentian-os operator for identity, LDAP, ingress, or tenant namespace lifecycle. |
| **gentian-os operator** | Orchestration: reconciles `Tenant` CRs, provisions kernel functions via Jobs/CRs, creates `App` claims and `IntegrationBindings`. | Tenant apps deploy via Crossplane helm `Release`, not ArgoCD `Application` per app. |
| **OpenBao + ESO** | Single secret store, synced into Kubernetes Secrets that Helm charts consume via `existingSecret` references. | Secrets never touch Git or appear in CR specs. |

**Why two tools, not one:** ArgoCD's drift detection, UI, and rollback
work for *every* Kubernetes resource, not just MRs. Crossplane's
reconcile loop handles the slow, eventually-consistent external APIs
that Argo cannot reason about. They compose cleanly and a bug in one
does not break the other.

A full dependency-graph walk for a single `Tenant` claim is in
[design/app-catalogue.md](design/app-catalogue.md).

### 3.1 How provisioning works on the cluster today

This section matches a running dev cluster (e.g. kernel domain
`desk.gentian.org`, a provisioned tenant such as `demo` with Element). It is the
authoritative “today” view; §3’s diagram is the stable mental model.

Fresh installs leave **no tenants** in Git or on the cluster until a cluster
admin deploys a definition from `clusters/<cluster>/definitions/` into `tenants/`.

**Two planes, one Git truth for tenants**

| Layer | What runs it | What it does today |
|---|---|---|
| **Deployment** | ArgoCD | Syncs `gentian-os` kernel manifests, Crossplane XRDs/compositions, `gentian-deployments` env config, and the **`gentian-appprofiles`** Application (install step **15c**) — not per-tenant app installs |
| **Provisioning** | Crossplane + **gentian-os operator** | Operator reconciles each `Tenant`; Crossplane reconciles each `App` claim into `ExternalSecret` + helm `Release` |

**Tenant lifecycle (operator-led)**  
Applying a `Tenant` from `gentian-deployments` (e.g. `demo` with
`spec.apps: [element]`) triggers the operator in order:

1. Namespace `tenant-{name}`, quota, limit range, network policy, registry pull secret; replicate `gentian-staging-ca-tls` on ACME staging clusters  
2. Keycloak **realm** via Jobs in `platform-kernel` (realm → admin → browser-flow)  
3. LDAP OU, `managed-by-attribute-*` groups, and bind accounts via UDM REST Jobs (`dc=swp-ldap,dc=internal` on dev)  
4. Keycloak **LDAP group sync** → OpenDesk **OIDC pack** client Jobs (maps `managed-by-attribute-*` → client roles per `internal/oidc/packs/opendesk.yaml`)  
5. Post-unlock **LDAP user sync** and kernel admin re-enable  
6. Per-app databases, object storage, cache where required  
7. Mail secrets / virtual-domain registration when `spec.mail.mode` needs kernel mail (see [design/mail.md](design/mail.md))  
8. **`App` claim per `spec.apps` entry** — created by the operator (authoritative today)  
9. Per-tenant DNS-01 wildcard cert + per-app `Ingress` (`chat.demo.desk.gentian.org`, …)  
10. **`IntegrationBinding`** CRs when two installed apps match a contract in `spec.apps`

In parallel, the operator also ensures an **`XTenant`** composite
(`tenant-default` Composition). That path renders namespace, limits,
network policy, OpenBao policy, and **duplicate `App` claims** from the
same `spec.apps` list. The two paths are idempotent today; imperative
steps are **not** yet fully retired into the Composition (Phase 3b).

**App install flow** — not ArgoCD per app  
Catalogue entries live in **`gentian-apps/profiles/`** and reach the
cluster only via ArgoCD Application **`gentian-appprofiles`**. Installing
for a tenant means appending a profile name to **`Tenant.spec.apps`** in
`gentian-deployments`; the operator materialises namespace-scoped
`App` claims; Crossplane deploys charts via **`provider-helm` `Release`**
MRs. There is no third “app-of-apps” source for tenant apps.

**Identity / Keycloak — two mechanisms**

| Scope | Mechanism today | Git location |
|---|---|---|
| **Kernel / shared realm clients** (portal, static integrations) | **`provider-keycloak`** `Client` / scope MRs | `kernel/services/keycloak-config/` |
| **Per-tenant realms and OIDC clients** | Operator **shell Jobs** (`curl` + admin API) | Reconciler in `gentian-os` |

Some app Compositions (`app-default`, `app-element`, `app-ox`) also emit
`openidclient.keycloak.crossplane.io/Client` MRs for chart-specific
clients. That overlaps with operator Jobs for the same tenant — both
exist today; consolidation is planned (see [roadmap.md](roadmap.md)).

**Mid-term direction for `provider-keycloak`:** Prefer managing **all**
Keycloak objects (kernel + tenant + app clients) as Crossplane MRs once
realm lifecycle is declarative and drift-safe. Until then, keep the
provider for kernel `keycloak-config` and treat operator Jobs as the
source of truth for tenant realms. **Do not remove** `provider-keycloak`
without migrating `keycloak-config` and tenant client creation first.

**Simpler / more future-proof alternatives (not implemented)**

- Single reconcile owner: finish **Phase 3b** — move identity, LDAP,
  ingress, and mail into `tenant-default` / app Compositions so only
  Crossplane + one operator “orchestrator” remain. Step-by-step plan:
  [crossplane-convergence.md](crossplane-convergence.md).  
- App catalogue via OCI artefact + `Cluster` XR instead of a separate
  ArgoCD Application (still one sync path).  
- Replace point-to-point `IntegrationBinding` Jobs with composition
  steps gated on `App` Ready (see app-catalogue §8b).

Placeholder semantics (`${TENANT_DOMAIN}` vs `${KERNEL_DOMAIN}`) are
documented in [gentian-apps/app-profile-guide.md](../../gentian-apps/app-profile-guide.md) §2.

---

## 4. The Four User-Facing CRDs

The platform exposes four custom resources to humans, separated by who
owns them. Everything else is generated.

### 4.1 `AppProfile` (cluster-scoped) — the app catalogue entry

Declares **what an app is**: its kernel requirements (does it need a
database? OIDC? S3? mail?), the capabilities it exposes to other apps
(file storage? project management? MCP server?), the upstream Helm
chart, a typed `valueMapping` that tells the platform how to wire
kernel-provided values into the chart's `values.yaml`, and optional
branding tokens and integration hooks. Adding a new app to the catalogue
is one YAML file in `gentian-apps`. Cluster admins publish `AppProfile`
CRs; tenant admins consume them by name.

### 4.2 `Tenant` (cluster-scoped) — the customer

Declares **who** uses the platform: an optional vanity domain, an
isolation mode (namespace), resource quotas, mail mode, a deletion
policy, and **`spec.apps`** — the list of catalogue profiles to install
for this tenant (e.g. `element` — Jitsi is deployed as an Element sidecar). Creating a `Tenant`
provisions kernel-layer infrastructure: namespace, RBAC, OpenBao
policies, LDAP entries, DNS/TLS, and the Keycloak realm. The operator
then creates one **`App` claim per `spec.apps` entry**; Crossplane
deploys the Helm charts.

### 4.3 `App` (namespace-scoped) — the tenant's app installation

Declares **which app a tenant wants installed**: a reference to an
`AppProfile` by name and optional per-installation overrides (replica
count, branding tokens, enabled integrations). `App` claims live in the
tenant's namespace (`tenant-{name}`), so RBAC limits write access to the
tenant admin — the cluster admin never needs to be involved in
installing or uninstalling a tenant's applications.

A Crossplane Composition processes each `App` claim by fetching the
referenced `AppProfile` (via `function-extra-resources`) and emitting:
- One `ExternalSecret` that renders a `sensitive-values.yaml` file from
  per-tenant OpenBao paths (OIDC credentials, database password, S3
  keys, etc.), consuming the `valueMapping` from the profile.
- One `helm.crossplane.io/Release` MR that deploys the chart into the
  tenant namespace with `valuesFrom` pointing at the rendered secret.
- Zero or more `kubernetes.crossplane.io/Object` MRs for extra
  Kubernetes resources the chart does not ship (RBAC, NetworkPolicies,
  `ConfigMap` patches).

This model allows the Kubernetes API to act as the app store: tenant
admins install apps with `kubectl apply`, browse the catalogue with
`kubectl get appprofiles`, and watch install progress via the `App`
claim's `.status.conditions`. A web UI or CLI is a thin wrapper over
this API — no separate store backend is required.

### 4.4 `IntegrationBinding` (namespace-scoped) — the cross-app contract

When two `App` claims in the same tenant namespace declare matching
provider/consumer contracts (e.g., OX App Suite consumes `file-store`
provided by Nextcloud), the platform generates an `IntegrationBinding`
that provisions shared credentials, configures OIDC token exchange
(RFC 8693), and tracks health. Bindings are owned by the constituent
`App` claims and garbage-collected when either app is uninstalled.

Schema details, value-mapping rules, contract definitions, and worked
examples are in [design/app-catalogue.md](design/app-catalogue.md).

---

## 5. The Kernel and the Default Install

Like a desktop OS, Gentian OS ships with a default install — components
that must exist before any tenant app can run, because they back the
**kernel functions** every app assumes are available:

| Kernel function | Default-install component | Desktop OS analogue |
|---|---|---|
| Identity & SSO | **Nubus** (Keycloak + UCS LDAP) | `/etc/passwd` + PAM |
| Hierarchical files | **Nextcloud** (WebDAV) | `C:` drive / home directory |
| Object storage | **MinIO** (S3) | Page cache, scratch space |
| Relational data | **CloudNativePG** + **MariaDB Operator** | Per-app SQLite / registry |
| Cache | **Redis** + **Memcached** | Page cache / `tmpfs` |
| Mail (extension) | **Postfix + Dovecot + Rspamd** | Built-in mail spool |
| Window manager | **Gentian Portal** ([gentian-ui](https://github.com/gentian-org/gentian-ui)) | Desktop shell / Start menu |
| Notifications | **Notification Gateway** | Notification daemon |
| Secrets keyring | **OpenBao** | Keychain |
| Pod restart on secret rotation | **Stakater Reloader** | (no equivalent) |

These are not "apps" the user picks à la carte — they are the **kernel
devices** that must be Ready before a `Tenant` claim can reach Ready.
Tenants can later swap implementations (Nextcloud → Seafile, MinIO →
AWS S3) without breaking apps that program against the contract,
exactly the way a desktop user can replace `C:` with a different
volume without breaking `CreateFile()`.

The full kernel function list, the default-drive analogy, and the
"kernel extensions" mechanism (optional shared services like mail) are
in [design/kernel.md](design/kernel.md).

---

## 6. Multi-Tenancy, Domains and Security

Multiple tenants share one cluster:

- **Default isolation** is one Kubernetes namespace per tenant
  (`tenant-{name}`), with NetworkPolicies, ResourceQuotas, and
  LimitRanges. Identity, data, and mail are isolated through dedicated
  Keycloak realms, per-app database users, MinIO bucket policies,
  Redis ACLs, and (for mail) per-domain DKIM keys.
- **Domains** use a two-plane model: a per-cluster wildcard
  (`*.<kernel_domain>`) covers **kernel UIs only**; each tenant app zone
  gets its own wildcard (`*.<effectiveDomain>`) via DNS-01. Default
  effective domain depends on **`TENANCY_MODE`**: `multi` →
  `<tenant>.<kernelDomain>`; `single` → `<kernelDomain>` (flat URLs, one
  `Tenant` named `default`). Set `spec.domain` only for a customer vanity
  domain (e.g. `acme.com`). See [design/multi-tenancy.md](design/multi-tenancy.md) §3.
- **App-to-app calls** go through OIDC token exchange, with the
  `IntegrationBinding` defining which exchanges are permitted.
- **Database isolation:** each app within each tenant gets its own
  database user with grants limited to its own database — no cross-app
  or cross-tenant access is possible.

Full details — isolation modes, RBAC, NetworkPolicies, OIDC trust
chain, mail security (DKIM/SPF/DMARC), the domain model and TLS
issuance flow — are in
[design/multi-tenancy.md](design/multi-tenancy.md).

### 6.1 TLS certificate provisioning

For each tenant with edge-routed apps, the **gentian-os controller** ensures:

1. One cert-manager `Certificate` per tenant for `*.<effectiveDomain>` and
   `<effectiveDomain>` (DNS-01), stored as `tenant-{name}-wildcard-tls`.
2. One Gateway API `HTTPRoute` per app host, referencing the tenant Gateway
   listener TLS secret:
   `{subDomain}.{effectiveDomain}` → `Service:{servicePort}`, all referencing
   that TLS secret on the tenant Gateway listener or Ingress TLS block.

`effectiveDomain` is `Tenant.spec.domain` when set; otherwise it follows
`TENANCY_MODE` (`multi` → `<tenant>.<kernelDomain>`; `single` →
`<kernelDomain>`). The issuer is configured cluster-wide via
`TENANT_DNS01_CLUSTER_ISSUER` (Helm: `tenantDNS01ClusterIssuer`).
`AppProfile.spec.ingress.clusterIssuer` is reserved for a possible future
per-host HTTP-01 mode; the operator does not read it today.

The **kernel** wildcard (`*.<kernelDomain>`, DNS-01 at install) covers
platform hostnames only (`portal`, `id`, Argo CD, …) on
`kernel-public-gateway` and is never replicated into tenant namespaces.

When traffic is proxied through Cloudflare, an optional operator adapter
ensures `*.<effectiveDomain>` CNAME records so Total TLS can mint edge
certs for multi-level tenant hostnames. Origin TLS remains cert-manager in
the tenant namespace. See [design/multi-tenancy.md](design/multi-tenancy.md) §3.

### 6.2 CORS and iframe embedding

Gentian OS sidesteps most browser CORS restrictions by design:

- **Apps run in iframes** inside the Gentian Portal on `portal.<kernelDomain>`.
  Each app is served on `{sub}.<effectiveDomain>` (cross-origin). The operator
  sets `frame-ancestors` so the portal can embed them.
- **OIDC token exchange is server-side.** The browser never calls the identity
  provider directly from an app's origin — the OIDC redirect flow terminates at
  the app's server, not at a JS `fetch()`.
- **Cross-origin API calls** that the Gentian shell will make on behalf of the
  browser may be declared in `spec.browserProxy` (planned in
  [gentian-ui/gentian-ui-architecture.md](../../gentian-ui/gentian-ui-architecture.md)):
  proxy paths under `/api/apps/{name}/…` with forwarded bearer tokens. This is
  not required for apps whose UI only talks to its own origin.

The remaining app-side requirement is the **`frame-ancestors` CSP header**:
by default browsers block iframe embedding unless the embedded page explicitly
permits it. The gentian-os controller injects this on every edge route it
creates:

- Envoy `BackendTrafficPolicy` / `HTTPRoute` `ResponseHeaderModifier` filters
  (see [design/gateway.md](design/gateway.md)).

For standard AppProfile apps (Element, Jitsi, OpenProject, …) it clears upstream
`X-Frame-Options` and `Content-Security-Policy`, then sets a single
`frame-ancestors 'self' https://portal.<kernel_domain>` policy — many charts
only emit `frame-ancestors 'self'`, and **appending** a second CSP header
leaves both active so browsers still block the portal iframe. CryptPad
(`pad` / `pad-sandbox` subdomains) **appends** a second header instead so
upstream `script-src` without `'unsafe-eval'` stays intact. Per-tenant portal hostnames are not used;
tenants authenticate via the kernel portal. CryptPad's additional
`pad-sandbox.<tenant>` route instead allows `https://pad.<tenant>` and
`https://portal.<kernel_domain>` because CSP checks the full ancestor chain when
the portal embeds CryptPad in a window and CryptPad embeds the sandbox. Apps with
extra edge snippet needs (e.g. CryptPad `sub_filter`) keep those lines;
frame-ancestors is still injected on each route according to its role.

**IdP (`id.<kernel_domain>`) is the inverse case.** Portal-embedded apps (e.g.
`chat.<tenant>.<kernel>`) load Keycloak OIDC pages inside the app iframe. The
Keycloak proxy route must allow both `https://portal.<kernel_domain>` and
`https://*.<tenant-effective-domain>` (CSP allows only one `*.` label, so
`https://*.<kernel_domain>` does not cover `chat.demo.<kernel>`). The
**KeycloakPlatformReconciler** (gentian-os operator) owns frame-ancestors policy
on the Keycloak IdP HTTPRoute and re-converges it when tenants change or Helm
drifts; Nubus Helm values only strip
upstream framing headers via `server-snippet`.

---

## 7. Secrets and Credentials

All secrets live in **OpenBao** and are synced into Kubernetes Secrets
by **External Secrets Operator (ESO)**. Helm charts consume them via
standard `existingSecret` references; for charts that lack
`existingSecret` support, `provider-helm` injects values via
`valuesFrom: secretKeyRef`. Either way, **secrets never appear in Git
or in CR specs**.

Two key properties:

1. **Deterministic seeding.** Kernel credentials are derived from a
   single master password via **HKDF-SHA256** (see `internal/kernel/secrets`),
   so re-seeding produces identical values and full disaster recovery is
   possible from the master password alone.
2. **Write-once protection.** Crossplane manages KV paths with
   `managementPolicies: [Observe, Create]` — the platform creates
   secrets on first reconcile and never overwrites live credentials.

### 7.1 Two Secret Delivery Patterns

All upstream Helm charts fall into one of two categories, both served
by the same ESO → K8s Secret pipeline:

| Pattern | Mechanism | When to use |
|---|---|---|
| **Pattern A** | ESO syncs OpenBao → K8s Secret; chart references it via `existingSecret` | Charts with native `existingSecret` support. This covers **all current kernel apps**: Nubus, Nextcloud, PostgreSQL, MariaDB, Keycloak bootstrap, Redis, MinIO. |
| **Pattern B** | ESO syncs OpenBao → K8s Secret; `provider-helm` `spec.valuesFrom` maps individual keys to Helm value paths | Charts that accept secrets as plain values but have no structured `existingSecret` field. |

In both patterns:
- Secrets are RBAC-restricted K8s Secrets, never written to Git or CR specs.
- `provider-helm` manages the full Helm release lifecycle as a Crossplane
  Managed Resource — drift detection, upgrade, and rollback are all visible
  in ArgoCD.
- etcd encryption at rest applies to the K8s Secrets.

**Pattern A example** (Nubus — already supported upstream):
```yaml
# ExternalSecret (ESO) pulls from OpenBao → creates k8s Secret nubus-credentials
# provider-helm HelmRelease references it:
spec:
  values:
    postgresql:
      auth:
        existingSecret: nubus-credentials
        secretKeys:
          adminPasswordKey: postgresql-admin-password
```

**Pattern B example** (hypothetical chart with no existingSecret):
```yaml
# Same ExternalSecret creates k8s Secret my-app-secrets
# provider-helm HelmRelease references individual keys:
spec:
  valuesFrom:
    - kind: Secret
      name: my-app-secrets
      valuesKey: admin-password
      targetPath: app.adminPassword
```

### 7.2 OpenBao Bootstrap

OpenBao itself must be configured before ESO or Crossplane can
authenticate to it — a one-time bootstrap. The `install.sh` script
performs this via `bao` CLI calls directly:

```bash
bao secrets enable -path=secret kv-v2
bao auth enable kubernetes
bao write auth/kubernetes/config kubernetes_host="$K8S_HOST"
bao policy write eso-read <(cat kernel/bootstrap/eso-policy.hcl)
bao write auth/kubernetes/role/eso ...
```

After this bootstrap, all further OpenBao configuration (additional
policies, roles for new services) is managed as Crossplane Managed
Resources via `provider-vault`, which can authenticate using the
already-configured Kubernetes auth backend.

The OpenBao path layout, ESO sync flow, derivation algorithm, rotation
mechanics (Stakater Reloader), and credential-leak guard rails are in
[design/security.md](design/security.md).

---

## 8. Repository Structure

Three Git repositories, separated by rate of change:

```
gentian-os/              # The OS itself (versioned artifact)
├── crossplane/
│   ├── xrds/            # Tenant, App, Cluster XRDs
│   ├── compositions/    # Pipelines that fan out into MRs
│   ├── functions/       # Composition functions (valueMapping, auto-ready, …)
│   └── providers/       # Provider configs
├── kernel/              # Static manifests not provisioned by an XR
└── docs/

gentian-apps/            # The catalogue (versioned artifact)
├── profiles/            # One AppProfile YAML per app
└── contracts/           # Contract schema definitions

gentian-deployments/     # Per-cluster state (the only repo specific to a cluster)
└── <env>/
    ├── kernel/          # Operator Helm values, image updater
    └── tenants/
        ├── kustomization.yaml
        └── instances/<tenant>/
            └── tenant.yaml   # Tenant CR with spec.apps[] (tenant-admin managed)
```

`gentian-os` and `gentian-apps` publish versioned OCI artifacts;
`gentian-deployments` references them by version. ArgoCD syncs kernel
manifests, the **`gentian-appprofiles`** Application (profiles only), and
tenant YAML. The **gentian-os operator** creates in-cluster `App` claims
from `Tenant.spec.apps`. Adding an app to the catalogue is a PR to
`gentian-apps/profiles/` (synced by `gentian-appprofiles`); installing an
app for a tenant appends a profile to `spec.apps` in
`gentian-deployments` (see
[gentian-deployments/README.md](../../gentian-deployments/README.md)).

---

## 9. The Mail Kernel Extension

Mail is **optional** — not every tenant needs self-hosted mail. It is
modelled as a **kernel extension**: shared infrastructure (one Postfix,
one Dovecot, one Rspamd) with tenant-scoped configuration (per-tenant
SASL credentials, per-domain DKIM keys, isolated mailbox paths).

On the dev cluster today, Postfix (and Dovecot when enabled) run in
**`gentian-dev`** as helm Releases `postfix-dev` /
`dovecot-dev` — in-cluster SMTP is
`postfix-dev.gentian-dev.svc.cluster.local:587`, not
`postfix.platform-kernel.svc.cluster.local`.

**Install-time vs per-tenant:** `MAIL_SERVICE_MODE` in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`
(`external` or `kernel`) decides whether the installer deploys kernel
mail and how Postfix relays. **`Tenant.spec.mail.mode`** (`selfhosted`,
`external`, `transport-only`, `disabled`) decides what the operator
provisions for each organisation. See [design/mail.md](design/mail.md).

Each tenant picks a mode:

- `selfhosted` — full mail stack, shared infrastructure.
- `external` — tenant uses Gmail / its own server.
- `transport-only` — kernel relays SMTP, storage is external.
- `disabled` — outbound notifications only.

Configuration model, isolation guarantees, blast-radius trade-offs and
the per-tenant opt-out (dedicated mail stack for high-value tenants)
are in [design/mail.md](design/mail.md).

---

## 9b. The Office Kernel Extension

Nextcloud Office (powered by Collabora) is **optional** — not every
tenant needs collaborative document editing. It is modelled as a
**kernel extension**: one shared Collabora instance serves all tenants
via the WOPI protocol. The extension is declared per-tenant with a
single flag:

```yaml
office:
  enabled: true
```

Collabora is deployed as a kernel service in the platform namespace
(not in per-tenant namespaces), so the ingress hostname
`office.<domain>` is unique and shared across all tenants. Nextcloud's
`wopi_url` points to the shared in-cluster service URL and is
configured once at the platform level.

---

## 9c. The Diagrams Kernel Extension (CryptPad)

Nextcloud diagram editing (diagrams.net via the openincryptpad app) uses a
**shared CryptPad kernel service**, modelled like Collabora in §9b. One
instance at `pad.<kernel_domain>` (+ `pad-sandbox.<kernel_domain>` for the
crypto sandbox origin) serves all tenants; Nextcloud embeds it from
`files.<kernel_domain>`.

There is **no AppProfile or portal tile** — users open diagrams from Nextcloud
Files. Configuration matches OpenDesk: `restrictRegistration: true`,
`availablePadTypes: ["diagram"]`, `enableEmbedding: true`, no OIDC.

The nextcloud-management chart enables the CryptPad app when the kernel service
is deployed (`feature.apps.cryptpad.enabled: true`). Per-tenant CryptPad
AppProfiles are not used.

---

## 10. Backup, DR and Observability

Backup is **per subsystem** — each kernel component uses the
industry-standard tool for its data type (pgBackRest for PostgreSQL,
MinIO replication or Restic for buckets, dsync for Dovecot, Keycloak
realm export, OpenBao Raft snapshots, Velero for K8s state). The
per-tenant isolation model enables **single-tenant restore** without
touching others.

Observability is built into the CRD model: `kubectl get tenants`,
`kubectl get integrationbindings`, and `crossplane trace tenant/<name>`
show the entire dependency graph. Crossplane's standard metrics expose
reconcile latency, error counts, and per-MR readiness; ESO and ArgoCD
provide the rest.

The full backup matrix, tenant-restore procedure, DR drill, and
metrics catalogue are in [design/operations.md](design/operations.md).

---

## 11. Kernel and App Image Updates

### 11.1 Operator Image (gentian-os controller)

The `gentian-os` operator image is managed by **ArgoCD Image Updater**
through the `gentian-os` ArgoCD Application registered at install time.
The update chain is:

1. A CI push to `develop` (or `main` / a version tag) triggers the
   GitHub Actions `docker` job, which builds and pushes a new image to
   `ghcr.io/gentian-org/gentian-os:<branch>` (and a short-SHA tag).
2. `argocd-image-updater` polls GHCR every two minutes. The
   **`ImageUpdater` CR** deployed as Source 4 of the `gentian-os`
   Application tells it which Application to watch and which image to
   track (`newest-build` strategy).
3. When a new digest is detected, the updater patches the `image.tag`
   Helm parameter directly on the ArgoCD Application (`write-back-method:
   argocd`).
4. ArgoCD detects the parameter change, runs `helm upgrade`, and performs
   a rolling restart of the operator Deployment — no manual
   `kubectl rollout restart` needed.

The `ImageUpdater` CR lives in
`<gentian-deployments>/<env>/kernel/image-updater.yaml`. It is deployed
into the cluster by ArgoCD as part of the `gentian-os` Application sync,
**not** by a separate step in `install.sh`. This means it only exists
once ArgoCD has synced the Application.

Environment policies:

| Environment | Strategy | Tracks |
|---|---|---|
| dev | `newest-build` | Latest push to `develop` |
| staging | `newest-build` | Latest push to `staging` |
| prod | `semver` | Semver tags `v*` only |

### 11.2 Install-time bootstrap

`install.sh Step 15` uses a **two-phase** approach to avoid a
chicken-and-egg problem (ArgoCD can't sync the chart if the CRDs aren't
established yet):

- **Phase 1 — direct Helm install**: CRDs are applied and the operator
  is installed immediately via `helm upgrade --install`. Subsequent
  install steps that depend on CRDs or the webhook proceed without
  waiting for ArgoCD.
- **Phase 2 — ArgoCD handoff**: The `gentian-os` Application is rendered
  from `kernel/bootstrap/gentian-os-application.yaml.tmpl` (using the
  active `GENTIAN_DEPLOYMENTS_REPO`/`BRANCH`/`ENV` variables) and
  applied with `kubectl apply`. ArgoCD adopts the already-running
  resources via `ServerSideApply` and deploys the `ImageUpdater` CR on
  the first sync. From this point, all future upgrades — including image
  rollouts — are git-driven and fully automatic.

### 11.3 App images

App (tenant-facing) images update through the same `newest-build` /
`semver` mechanism applied per-AppProfile; each tenant picks up the new
chart version on the next ArgoCD sync triggered by the ImageUpdater.

---

## 12. The AI Layer (Future)

The Gentian Portal hosts an AI assistant (planned) that uses three kernel
services — identity, an MCP (Model Context Protocol) registry, and OIDC
token exchange — to discover what apps are installed and act across
them on behalf of the user. Apps expose capabilities by declaring an
MCP endpoint in their AppProfile; the registry is populated
automatically on install.

The MCP-based capability discovery model, cross-app workflow examples,
the AI-assisted AppProfile generator, and the implementation roadmap
are in [design/agentic-ai.md](design/agentic-ai.md).

---

## 13. Operational Roles

Three roles, three scopes:

| Role | Scope | Kubernetes primitives | Cannot do |
|---|---|---|---|
| **Cluster admin** | Cluster + kernel | `Tenant`, `AppProfile`, `Cluster` XRs; all verbs | Bypass GitOps in prod, perform tenant business actions |
| **Tenant admin** | One tenant namespace | `App` claims (create/delete/get/list in `tenant-{name}`); read `AppProfile` catalogue | Touch kernel, write outside own namespace |
| **Tenant user** | Day-to-day app use | Use installed apps with SSO | Install/uninstall, see admin surfaces |

Tenant admin RBAC is namespace-scoped: they hold `create`/`delete`
verbs on `apps.gentianos.io` in their own namespace and read-only on
`appprofiles.gentianos.io` cluster-wide. They cannot read `Tenant`
CRs or touch another tenant's namespace. The current model is
GitOps-driven (tenant admins edit `Tenant.spec.apps` in
`gentian-deployments` via `kubectl gentian apps install/uninstall`). Permissions, audit, and the future tenant-self-service flow
are in [design/multi-tenancy.md](design/multi-tenancy.md#roles).

---

## 14. Why This Architecture Scales

- **Adding an app to the catalogue = one YAML file** in `gentian-apps`.
  No code, no Composition change for typical apps; the generic `App`
  Composition reads the `AppProfile` via `function-extra-resources`.
- **Installing an app for a tenant = one entry in `Tenant.spec.apps`** in
  `gentian-deployments`; the operator materialises `App` claims. Tenant
  admins do this themselves; cluster admins are not involved.
- **Adding a tenant = one `Tenant` CR.** The operator and Crossplane
  reconcile kernel and app resources in parallel where dependencies allow.
- **Adding a cluster = one Argo App-of-Apps + one `Cluster` XR.** The
  same Compositions serve every environment; differences are
  per-environment values files.
- **Adding a kernel capability = one provider.** The driver model
  scales the way Linux kernel modules scale: pluggable, independently
  versioned, no kernel fork needed.
- **AI-friendly.** The platform's full state is queryable via the K8s
  API; AI agents see exactly the same model that operators see.

---

## 15. Document Map

| Topic | Document |
|---|---|
| Why a cloud OS at all | [design/cloud-os-rationale.md](design/cloud-os-rationale.md) |
| Kernel functions, default install, OS analogy details | [design/kernel.md](design/kernel.md) |
| Tenants, isolation, domains, network/identity security | [design/multi-tenancy.md](design/multi-tenancy.md) |
| AppProfile schema, IntegrationBindings, contracts, deployment flow | [design/app-catalogue.md](design/app-catalogue.md) |
| OpenBao, ESO, TLS, deterministic seeding, rotation | [design/security.md](design/security.md) |
| Identity and Access Management (IAM) and Roles | [design/iam.md](design/iam.md) |
| Mail kernel extension | [design/mail.md](design/mail.md) |
| Backup, DR, observability, image updates | [design/operations.md](design/operations.md) |
| Agentic AI / MCP integration | [design/agentic-ai.md](design/agentic-ai.md) |
