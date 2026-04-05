# Gentian OS — Implementation Plan

**Date:** 2026-04-04
**Scope:** Build Gentian OS to match the architecture document — CRDs, thin orchestrator, kernel services, and deployment pipeline.
**Principle:** Every increment produces a working, testable artifact. The thin orchestrator is the backbone — most other work flows from it.

---

## Strategy

The implementation follows from the architecture's **priority gaps** (current-state.md). The biggest gap — and the one that blocks almost everything else — is the thin orchestrator. Rather than migrating files from `server/`, we build the platform as described in the architecture, pulling proven patterns from `server/` where they exist.

### Starting point

The `server/` repo is a working single-tenant deployment. It is **reference material**, not the migration source. We cherry-pick what works (HMAC-SHA256 derivation, Pattern A/B, Reloader, ApplicationSet patterns, Tofu modules) and rewrite what doesn't fit (flat secret paths, manual YAML wiring, no CRDs, no orchestrator).

### Build order

The architecture has clear dependency chains:

```
CRD definitions
    └── Thin orchestrator (watches CRDs)
            ├── Operator CRs (Keycloak, CloudNativePG, MinIO, ...)
            ├── ExternalSecret CRs
            ├── ArgoCD Application CRs
            └── IntegrationBinding CRs
                    └── MCP registry (future)
```

We build bottom-up: CRDs first, then the orchestrator reconcilers one kernel function at a time, then tenant lifecycle, then apps.

### Why a custom orchestrator? — Alternatives considered

The thin orchestrator is ~5000 LOC of new Go code. Before committing to that, three alternatives were evaluated:

| Alternative | What it does | Why not |
|---|---|---|
| **Crossplane** | Kubernetes-native infrastructure orchestration. Compositions map provider APIs to custom XRDs. | Crossplane Compositions are declarative (no conditional logic). The orchestrator needs conditional behaviour: "if both Nextcloud and OX are in the tenant's app list, create an IntegrationBinding." Crossplane can't express this without a custom provider — which is as much Go code as the orchestrator itself. Also adds a heavyweight control plane (Crossplane runtime + provider pods). |
| **Kratix** | Platform-as-a-Product framework. Promises define what a platform team offers; Pipelines reconcile them. | Kratix Pipelines are container-based (one container per reconciliation step). This adds image build/push overhead per reconciler change and makes debugging harder than a single Go binary with `envtest`. Kratix is also younger (fewer production references) and introduces its own CRDs/concepts on top of the ones we already need. |
| **Kyverno generate rules + ArgoCD ApplicationSets** | Policy-driven resource generation. Kyverno watches CRs and generates downstream resources (ExternalSecrets, Applications). | Works for simple "CR A exists → create CR B" patterns. Breaks down for sequenced multi-step provisioning (create database → wait for ready → store credentials in OpenBao → create ExternalSecret → create Application). Kyverno has no concept of ordering, status aggregation, or rollback. |

**Decision: custom orchestrator.** The provisioning logic requires conditional branching (Pattern A vs B, mail modes, optional integrations), ordered sequencing (identity before database before app deployment), and status aggregation across multiple operator CRs. These are the exact things `controller-runtime` is designed for. The alternatives either can't express the logic at all (Kyverno) or require as much custom code as the orchestrator itself (Crossplane provider, Kratix pipeline containers).

The risk of "building too much" is mitigated by the architecture's delegate-don't-implement principle: the orchestrator creates CRs for operators and Jobs, not provisioning logic. Most reconcilers are ~200 LOC of CR creation + status watching.

---

## Progress

| Increment | Title | Status | Notes |
|---|---|---|---|
| 0 | Project scaffolding | ✅ Done | go.mod, Makefile (`generate`, `manifests`, `build`, `test`, `lint`, `docker-build`), `internal/controller/` stub, `charts/gentian-os/` (Chart.yaml + values.yaml), `kernel/` (tofu modules/platform/tenant, bootstrap, appsets, manifest, eso, openbao, services, values), `scripts/` (5 bootstrapping scripts), Dockerfile (multi-stage distroless), CI pipeline (go/generate/lint/docker jobs). Deployment smoke test is a manual gate requiring a live cluster. |
| 1 | CRD definitions | ✅ Done | All three CRDs (AppProfile, Tenant, IntegrationBinding) generated. 18 tests pass. Spike: OX, Nubus, Nextcloud sample YAMLs validate. |
| 2 | Orchestrator skeleton + Tenant namespace reconciler | ⬜ Not started | |
| 3 | Identity reconciler (Keycloak realm + OIDC clients) | ⬜ Not started | |
| 4 | LDAP reconciler (UDM REST API — per-tenant OUs + bind accounts) | ⬜ Not started | |
| 5 | Database reconciler (CloudNativePG) | ⬜ Not started | |
| 6 | MariaDB reconciler | ⬜ Not started | |
| 7 | Storage reconciler (MinIO buckets + Nextcloud provisioning) | ⬜ Not started | |
| 8 | Cache reconciler (Redis ACLs + Memcached) | ⬜ Not started | |
| 9 | App deployment reconciler (ArgoCD Application / Tofu Workspace CRs) | ⬜ Not started | |
| 10 | Ingress + DNS reconciler | ⬜ Not started | |
| 11 | IntegrationBinding reconciler | ⬜ Not started | |
| 12 | OpenBao restructuring (multi-tenant secret paths) | ⬜ Not started | |
| 13 | Orchestrator Helm chart + observability | ⬜ Not started | |
| 14 | AppProfile update reconciler | ⬜ Not started | |
| 15 | gentian-deployments repo setup | ⬜ Not started | |
| 16 | Mail extension reconciler | ⬜ Not started | |
| 17 | Hardening + end-to-end tenant lifecycle tests | ⬜ Not started | |

---

## Increments

### Increment 0 — Project scaffolding

**Goal:** Go module, Makefile, CI, directory structure matching architecture §7 Repo 1.

**Deliverables:**
```
gentian-os/
├── api/v1alpha1/           # CRD Go types (empty stubs)
├── internal/               # Orchestrator source (empty)
├── config/crd/             # Generated CRD YAML (empty)
├── charts/
│   └── gentian-os/         # Helm chart for the orchestrator
├── kernel/
│   ├── tofu/               # OpenTofu modules (copied from server/)
│   ├── bootstrap/          # ArgoCD bootstrap Applications
│   ├── services/           # Kernel service Helm values + ExternalSecrets
│   ├── appsets/             # ApplicationSet definitions
│   └── manifest/           # Static Kubernetes manifests
├── scripts/                # Bootstrap + operational scripts
├── docs/
├── Makefile
├── Dockerfile
└── go.mod
```

**Actions:**
- `go mod init github.com/gentian-org/gentian-os`
- Add `controller-runtime`, `controller-gen`, `kustomize` as dependencies
- Makefile targets: `generate` (CRD YAML from Go types), `manifests`, `build`, `docker-build`, `test`, `lint`
- CI pipeline: `go vet`, `go test`, `golangci-lint`, `yamllint`, `shellcheck`, `tofu fmt -check`
- Copy kernel assets from `server/`: scripts, Tofu modules (openbao-paths, app, app-trust, infra-workspaces, keycloak-config), bootstrap Applications, kernel service configs (all apps from `server/apps/`), ApplicationSets, OpenBao/ESO configs, Helm values
- Adapt paths in copied files to match new directory structure

**Test:**
- `make build` succeeds. `make generate` produces no errors. CI green.
- **Deployment smoke test:** Deploy `gentian-os/kernel/` to the dev cluster using the bootstrap scripts. Verify all kernel services (ArgoCD, OpenBao, Keycloak/Nubus, PostgreSQL, MinIO, Redis, ESO, Reloader) reach a healthy state. This catches path errors, broken chart references, and ApplicationSet misconfigurations immediately — not months later in Increment 15.

---

### Increment 1 — CRD definitions (AppProfile, Tenant, IntegrationBinding)

**Goal:** Define the three core CRDs in Go with proper validation, defaulting, and generated YAML — matching architecture §4 and §12.

**Files:**
- `api/v1alpha1/types.go` — shared types (isolation mode, mail mode, deletion policy)
- `api/v1alpha1/appprofile_types.go` — AppProfile spec (kernelRequirements, provides, optionalIntegrations, chart, valueMapping, appSecrets, extraValues)
- `api/v1alpha1/tenant_types.go` — Tenant spec (domain, isolation, mail, quotas, deletionPolicy, apps list) + status (conditions, provisioned apps, phase)
- `api/v1alpha1/integrationbinding_types.go` — IntegrationBinding spec (contract, provider, consumer, capabilities, auth) + status (state, conditions, secretRef)
- `api/v1alpha1/groupversion_info.go` — scheme registration
- `api/v1alpha1/zz_generated.deepcopy.go` — generated

**Design decisions:**
- AppProfile is **cluster-scoped** (one per app type, not per tenant)
- Tenant is **cluster-scoped** (owns a namespace)
- IntegrationBinding is **namespace-scoped** (lives in the tenant's namespace, owned by Tenant CR)
- `valueMapping` uses typed sub-schemas per kernel requirement (oidc, database, s3, smtp, cache, ldap) — not freeform templates
- `appSecrets` declares app-internal secrets (admin passwords, session keys, cluster tokens) the orchestrator generates via HMAC-SHA256 and injects — separate from kernel-provided `valueMapping` secrets
- Validation via `kubebuilder:validation` markers (required fields, enum constraints, regex patterns)

**Required validation — valueMapping spike (must pass before Increment 2):**

Before writing reconcilers that depend on the `valueMapping` schema, validate it against the three hardest apps in `server/`. The spike takes the existing Tofu `set_sensitive` blocks and `values-sensitive.yaml.tftpl` files and attempts to express them as AppProfile `valueMapping` + `extraValues`.

**Spike findings (from `server/` analysis):**

OX App Suite has 12 secret values injected via `templatefile()`. Some map cleanly to `valueMapping` schemas (OIDC, MariaDB, Redis, MinIO/S3). But several are **app-internal secrets** that don't correspond to any kernel requirement:

| Value path | Kernel requirement? | valueMapping fit? |
|---|---|---|
| `appsuite.core-mw.masterPassword` | No — OX admin password | `extraValues` only |
| `appsuite.core-mw.hzGroupPassword` | No — Hazelcast cluster secret | `extraValues` only |
| `appsuite.core-mw.basicAuthPassword` | No — OX internal API auth | `extraValues` only |
| `appsuite.core-mw.jolokiaPassword` | No — JMX monitoring auth | `extraValues` only |
| `global.appsuite.cookieHashSalt` | No — OX cookie signing salt | `extraValues` only |
| `global.appsuite.shareCryptKey` | No — OX sharing encryption | `extraValues` only |
| `global.appsuite.sessiondEncryptionKey` | No — OX session encryption | `extraValues` only |
| `appsuite.core-mw.secretProperties["com.openexchange.oidc.clientSecret"]` | Yes — OIDC | valueMapping (but nested path) |
| `global.mysql.auth.password` | Yes — MariaDB | valueMapping |
| `appsuite.core-mw.redis.auth.password` | Yes — Cache | valueMapping |
| `appsuite.core-mw.propertiesFiles[...].com.openexchange.filestore.s3...secretKey` | Yes — S3 | valueMapping (but deeply nested filesystem path key) |
| `appsuite.core-mw.propertiesFiles[...].bindDNPassword` | Yes — LDAP | valueMapping (but deeply nested) |

**Conclusions:**
1. **5 of 12 secrets** are kernel requirements and fit `valueMapping` (OIDC, MariaDB, Redis, S3, LDAP) — but some require deeply nested key paths like `appsuite.core-mw.propertiesFiles./opt/open-xchange/etc/ldapauth.properties.bindDNPassword`
2. **7 of 12 secrets** are app-internal and can only go through `extraValues` — but they're still secrets, so they need ExternalSecret references, not plain values
3. **Nubus is worse:** 30 `set_sensitive` values, most are internal (NATS passwords, LDAP search user passwords, provisioning API passwords)
4. The `valueMapping` schema works for the **kernel-provided** secrets but needs an **app-internal secrets** mechanism

**Schema revision needed:** Add an `appSecrets` field to AppProfile that declares app-internal secrets the orchestrator must generate and inject. These are not kernel requirements — they're random passwords the app needs for internal operation. The orchestrator generates them (HMAC-SHA256), stores them in OpenBao, and syncs them via ExternalSecret:

```yaml
# In AppProfile spec
appSecrets:
  - name: admin_password
    valuePath: "appsuite.core-mw.masterPassword"
  - name: hz_group_password
    valuePath: "appsuite.core-mw.hzGroupPassword"
  - name: cookie_hash_salt
    valuePath: "global.appsuite.cookieHashSalt"
```

This keeps `valueMapping` clean (typed schemas for kernel resources) while handling the reality that most complex charts have 5–10 internal secrets that don't map to any kernel function.

For **Pattern B apps** (where `existingSecret` isn't supported), the `appSecrets` values are injected via Tofu Controller `set_sensitive` alongside the kernel-provided secrets — the `deploymentMethod: tofu-controller` path handles both.

**Test:**
- `make generate` produces CRD YAML under `config/crd/`
- `kubectl apply --dry-run=server -f config/crd/` succeeds on a test cluster
- Unit tests verify deepcopy, defaulting, and validation
- Example CRs from architecture §12.1–12.3 validate against the generated schema
- **Spike**: hand-write `ox-appsuite.yaml`, `nubus.yaml`, and `nextcloud.yaml` AppProfile CRs using real value paths from `server/` — all must validate against the generated CRD schema. If any value path can't be expressed, fix the CRD before proceeding.

---

### Increment 2 — Orchestrator skeleton + Tenant namespace reconciler

**Goal:** A running controller-runtime binary that watches Tenant CRs and creates tenant namespaces with RBAC, ResourceQuotas, LimitRanges, and NetworkPolicies — the simplest useful reconciler.

**Files:**
- `internal/controller/tenant_controller.go` — main reconciler
- `internal/controller/tenant_controller_test.go` — envtest-based tests
- `cmd/main.go` — entry point, scheme registration, controller manager setup

**Reconciler logic (Tenant → namespace):**
1. Ensure namespace `tenant-{name}` exists with labels `gentianos.io/tenant: {name}`
2. Apply ResourceQuota from `spec.quotas`
3. Apply LimitRange (sensible defaults)
4. Apply NetworkPolicy: allow egress to `platform-kernel` namespace, deny ingress from other tenant namespaces
5. Set Tenant status condition `NamespaceReady: True`
6. On Tenant deletion with `deletionPolicy: Retain` — keep namespace, remove orchestrator-owned resources. With `Delete` — delete namespace (cascades all resources)

**Test:**
- envtest: create Tenant → namespace exists with correct labels, quotas, network policies
- envtest: delete Tenant with Retain → namespace preserved
- envtest: delete Tenant with Delete → namespace deleted
- `make docker-build` produces a container image
- Deploy to dev cluster, create a test Tenant, verify namespace appears

---

### Increment 3 — Identity reconciler (Keycloak realm + OIDC clients)

**Goal:** Orchestrator provisions Keycloak realm and OIDC clients per tenant using Keycloak Operator CRs — matching architecture §3.1.

**Prerequisites:** Keycloak Operator installed on cluster.

**Nubus compatibility decision:** The current deployment uses **Nubus**, which bundles Keycloak + UCS LDAP + UDM + Portal into a single Helm chart. The Keycloak Operator cannot manage the Keycloak instance inside Nubus — it manages standalone Keycloak instances. Two paths are available:

| Path | Approach | Trade-off |
|---|---|---|
| **A — Keep Nubus** | Provision realms/clients via Keycloak REST API (Jobs or lightweight controller). UDM REST API for LDAP. | Preserves existing UCS stack. Orchestrator needs REST API client code instead of pure CR creation. |
| **B — Replace Nubus** | Deploy standalone Keycloak (Operator-managed) + standalone UCS LDAP. Migrate UDM functions to direct LDAP provisioning. | Clean operator-native path. Requires validating all Nubus-dependent features still work. |

**Recommended: Path A for v1.** Nubus is deployed and working. The orchestrator uses Jobs that call the Keycloak Admin REST API for realm/client provisioning and the UDM REST API for LDAP provisioning. Keycloak Operator CRs become a future migration target once Nubus is decomposed. This avoids a risky Nubus replacement while still delivering tenant provisioning.

**Reconciler logic (Tenant → identity):**
1. Create a `Job` that calls the Keycloak Admin REST API to create tenant realm `{tenant-name}` with standard realm settings (token lifetimes, login theme, password policy)
2. For each app in `spec.apps`, look up the AppProfile's `kernelRequirements.identity.oidc`
3. If OIDC required: create a `Job` that provisions a Keycloak client in the tenant realm with redirect URIs derived from `{tenant-domain}/{app-path}`
4. Store OIDC client credentials in OpenBao at `gentian-os/tenants/{tenant-name}/apps/{app-name}/oidc`
5. Create `ExternalSecret` CR to sync OIDC credentials into the tenant namespace
6. For apps with `optionalIntegrations` using `oidc-token-exchange`, configure token exchange policies between provider and consumer clients
7. Set Tenant status condition `IdentityReady: True`

**Delete path:** On Tenant deletion with `deletionPolicy: Delete` — create a Job that deletes the Keycloak realm via REST API (cascades all clients). With `Retain` — revoke client secrets but keep the realm and user accounts intact.

**Design note:** Using Jobs instead of Keycloak Operator CRs is a pragmatic choice for Nubus compatibility. The Jobs are idempotent (check-before-create) and the orchestrator tracks completion via Job status conditions. When Nubus is eventually decomposed into standalone components, the Jobs can be replaced with Keycloak Operator CRs without changing the reconciler's external interface.

**Test:**
- envtest + mock Keycloak API: verify Jobs are created with correct realm, client config
- Integration test on dev cluster: create Tenant with one app → Keycloak realm exists, OIDC client works, ExternalSecret syncs
- Verify OIDC login flow end-to-end with a test app

---

### Increment 4 — LDAP provisioner (UDM REST API)

**Goal:** Provision per-tenant LDAP organisational units, bind accounts, and groups via the UDM REST API — required by multiple apps (OX App Suite LDAP auth, OpenProject LDAP sync, Nextcloud LDAP backend).

**Prerequisites:** Nubus deployed with UDM REST API accessible from kernel namespace.

**Reconciler logic (Tenant → LDAP):**
1. For each tenant, create a `Job` that calls the UDM REST API to:
   - Create OU `ou={tenant-name},dc=...` under the tenant's LDAP subtree
   - Create a bind account `cn=app-{app-name},ou={tenant-name},dc=...` for each app requiring LDAP (`kernelRequirements.identity.ldap`)
   - Create default groups (e.g., `users`, `admins`) under the tenant OU
2. Store LDAP bind credentials in OpenBao at `gentian-os/tenants/{tenant-name}/apps/{app-name}/ldap`
3. Create `ExternalSecret` CR to sync LDAP bind credentials into tenant namespace
4. Set status condition `LDAPReady: True`
5. On tenant deletion with `deletionPolicy: Delete`: remove OU and all child entries via UDM REST API

**Design note:** Architecture §3.1 specifies "LDAP bind account via UDM REST API (via Job)." The Jobs are idempotent — they check for existing entries before creating. The UDM REST API is preferred over direct LDAP writes because Nubus validates and indexes entries through UDM.

**Test:**
- envtest: Tenant with LDAP-requiring app → Job created with correct UDM API calls
- Integration: create Tenant → LDAP OU exists, bind account can authenticate, base DN is correct
- Verify apps can bind: Nextcloud LDAP backend connects with provisioned credentials

---

### Increment 5 — Database reconciler (CloudNativePG)

**Goal:** Orchestrator provisions per-app-per-tenant PostgreSQL databases via CloudNativePG operator CRs — replacing the current Tofu Controller Helm-based PostgreSQL deployment.

**Prerequisites:** CloudNativePG operator installed on cluster.

**Reconciler logic (Tenant + AppProfile → database):**
1. For each app in `spec.apps` where AppProfile declares `kernelRequirements.database.engine: postgresql`:
2. Create CloudNativePG `Database` CR: `{tenant-prefix}_{app-name}` on the shared PostgreSQL cluster
3. Create CloudNativePG `Role` CR: dedicated user with grants limited to that database
4. Store credentials in OpenBao at `gentian-os/tenants/{tenant-name}/apps/{app-name}/database`
5. Create `ExternalSecret` CR to sync database credentials into tenant namespace
6. Set per-app status condition `DatabaseReady: True`

**Delete path:** On Tenant deletion with `deletionPolicy: Delete` — delete CloudNativePG `Database` and `Role` CRs (operator drops the database). With `Retain` — revoke the Role's login privilege but keep the database and data.

**Migration path from `server/`:** The current setup uses Tofu Controller to deploy PostgreSQL via Helm (Pattern B, chart v2.1.2). CloudNativePG replaces this with operator CRs for database lifecycle management. The shared PostgreSQL cluster itself is still deployed as a kernel service (Layer 120).

**Data migration strategy:** For the existing dev cluster, two options:
- **Greenfield (recommended for dev):** Deploy a new CloudNativePG cluster alongside the existing Helm-based PostgreSQL. The orchestrator provisions new databases on CloudNativePG. Old databases remain on the Helm-based instance until all apps are migrated, then decommission it.
- **In-place (for production):** Use `pg_dump`/`pg_restore` to migrate databases from the Helm-based instance to CloudNativePG. Requires a maintenance window per tenant. Document the procedure in `docs/runbooks/`.

**Test:**
- envtest: Tenant with database-requiring app → Database + Role CRs created
- Integration: create Tenant → connect to provisioned database with provisioned credentials
- Verify isolation: tenant A cannot access tenant B's database

---

### Increment 6 — MariaDB reconciler

**Goal:** Orchestrator provisions per-app-per-tenant MariaDB databases — required by OX App Suite (a core openDesk app that uses MariaDB, not PostgreSQL).

**Prerequisites:** MariaDB deployed as a kernel service (Layer 120). Optionally, MariaDB Operator installed for CR-based provisioning.

**Reconciler logic (Tenant + AppProfile → MariaDB database):**
1. For each app in `spec.apps` where AppProfile declares `kernelRequirements.database.engine: mariadb`:
2. **If MariaDB Operator is available:** create `MariaDBDatabase` CR + user CR with grants limited to that database
3. **If no operator:** create a `Job` that runs `CREATE DATABASE` + `CREATE USER` + `GRANT` SQL via the MariaDB client CLI (idempotent — checks existence first)
4. Store credentials in OpenBao at `gentian-os/tenants/{tenant-name}/apps/{app-name}/database`
5. Create `ExternalSecret` CR to sync database credentials into tenant namespace
6. Set per-app status condition `DatabaseReady: True`

**Delete path:** On Tenant deletion with `deletionPolicy: Delete` — delete MariaDB database and user (via operator CR deletion or `DROP DATABASE`/`DROP USER` Job). With `Retain` — revoke user privileges but keep the database.

**Design note:** Architecture §3.1 lists MariaDB Operator as the preferred path, with "direct SQL via Job" as the fallback. Since the MariaDB Operator ecosystem is less mature than CloudNativePG, the Job path is the pragmatic starting point. The reconciler abstracts the provisioning method — switching to operator CRs later requires no changes to the AppProfile or Tenant CR schemas.

**Test:**
- envtest: Tenant with MariaDB-requiring app (e.g., OX App Suite) → Job or CR created
- Integration: create Tenant → connect to provisioned MariaDB database with provisioned credentials
- Verify isolation: tenant A cannot access tenant B's MariaDB databases

---

### Increment 7 — Storage reconciler (MinIO S3 + Nextcloud WebDAV)

**Goal:** Orchestrator provisions per-tenant S3 buckets and Nextcloud users/groups.

**Reconciler logic (Tenant + AppProfile → storage):**
1. For apps requiring S3 (`kernelRequirements.storage.s3`): create MinIO bucket `{tenant-prefix}-{app-name}`, IAM user with scoped policy, store credentials in OpenBao
2. For apps requiring WebDAV (`kernelRequirements.storage.files.protocol: webdav`): provision via Nextcloud OCS API (create group for tenant, configure share permissions)
3. Create ExternalSecret CRs for all storage credentials
4. Set status condition `StorageReady: True`

**Delete path:** On Tenant deletion with `deletionPolicy: Delete` — delete MinIO bucket and IAM user, remove Nextcloud group and shared folders via OCS API. With `Retain` — revoke IAM credentials but keep bucket contents and Nextcloud data.

**Design note:** MinIO Operator CRs for bucket provisioning if available; otherwise use a lightweight Job that calls the MinIO admin API. This is one of the cases where the orchestrator may use a Job rather than an operator CR, since MinIO Operator's bucket management is limited.

**Test:**
- Integration: create Tenant with S3-requiring app → bucket exists, credentials work, isolation enforced

---

### Increment 8 — Cache reconciler (Redis ACLs + Memcached)

**Goal:** Orchestrator provisions per-app Redis ACL users and Memcached instances — required by OX App Suite (Redis), Element/Synapse (Redis), and OpenProject (Memcached).

**Prerequisites:** Redis deployed as a kernel service (Layer 140). Memcached available as a kernel service or deployable per-tenant.

**Reconciler logic (Tenant + AppProfile → cache):**
1. For apps requiring Redis (`kernelRequirements.cache.engine: redis`):
   - **If Redis Operator is available:** create operator CR for a dedicated ACL user with a scoped key prefix `{tenant-prefix}:{app-name}:*`
   - **If no operator:** create a `Job` that runs `ACL SETUSER` via `redis-cli` (idempotent — checks existence first)
   - Store Redis credentials in OpenBao at `gentian-os/tenants/{tenant-name}/apps/{app-name}/cache`
   - Create `ExternalSecret` CR to sync cache credentials into tenant namespace
2. For apps requiring Memcached (`kernelRequirements.cache.engine: memcached`):
   - Deploy a per-tenant Memcached instance via ArgoCD Application CR (Memcached has no native multi-tenancy — isolation requires separate instances)
   - Store connection details in OpenBao
   - Create `ExternalSecret` CR
3. Set per-app status condition `CacheReady: True`

**Delete path:** On Tenant deletion with `deletionPolicy: Delete` — delete Redis ACL users (operator CR or `ACL DELUSER` Job), delete per-tenant Memcached deployments (via ArgoCD Application CR deletion). With `Retain` — disable Redis ACL user but keep Memcached instance running (for data recovery).

**Delete path for Ingress (Increment 10):** On Tenant deletion — delete Ingress resources, cert-manager Certificate CRs, and DNS Tofu workspace CR (Tofu removes DNS records). These are always deleted regardless of `deletionPolicy` since they are ephemeral routing resources, not tenant data.

**Design note:** Redis supports ACL users (Redis 6+) for per-app isolation on a shared instance. Memcached has no authentication or namespace isolation, so per-tenant instances are the only safe option. The cost is acceptable — Memcached is lightweight (~64MB per instance).

**Test:**
- Integration: create Tenant with Redis-requiring app → ACL user exists, app can read/write only its key prefix
- Integration: create Tenant with Memcached-requiring app → Memcached deployed in tenant namespace
- Isolation: tenant A's Redis ACL user cannot access tenant B's keys

---

### Increment 9 — App deployment reconciler (ArgoCD Application CRs)

**Goal:** The orchestrator creates ArgoCD Application CRs per app per tenant — the deployment handoff described in architecture §4.4.

**Reconciler logic (Tenant + AppProfile → ArgoCD Application):**
1. For each app in `spec.apps`, look up the AppProfile
2. Render Helm values using `valueMapping` schema:
   - OIDC: point at ExternalSecret-synced credentials
   - Database: host, name, ExternalSecret ref
   - S3: endpoint, bucket, ExternalSecret ref
   - SMTP: host, port, ExternalSecret ref (if mail mode allows)
   - Cache: host, port
   - LDAP: host, baseDn, ExternalSecret ref
3. Merge `extraValues` from AppProfile
4. Merge per-tenant app `config` overrides (e.g., `replicas: 2`)
5. Create `Application` CR in `argocd` namespace with:
   - `spec.source`: chart ref from AppProfile
   - `spec.destination.namespace`: `tenant-{name}`
   - Owner reference to Tenant CR
   - Labels: `gentianos.io/tenant`, `gentianos.io/app`
6. ArgoCD takes over from here — deploys, monitors, self-heals
7. Watch Application CR status → reflect sync/health in Tenant status

**This is the core of the orchestrator.** It replaces the current ApplicationSet + manual values approach with a programmatic, per-tenant deployment pipeline.

**Pattern B strategy:** Not all upstream Helm charts support `existingSecret` references (architecture §5.3). The orchestrator handles both patterns:

| Pattern | Orchestrator action | Apps |
|---|---|---|
| **Pattern A** (preferred) | Create ArgoCD `Application` CR with `existingSecret` refs in Helm values | Redis, MinIO, Intercom Service, OpenProject, Collabora |
| **Pattern B** (fallback) | Create Tofu Controller `Terraform` CR with `set_sensitive` injecting secrets from OpenBao | Nubus, OX App Suite, Postfix, Dovecot |

For Pattern B apps, the orchestrator creates a `Terraform` CR instead of an ArgoCD `Application` CR. The Tofu Controller reads secrets from OpenBao and injects them via `set_sensitive` during Helm apply. This means Pattern B apps appear in the Tofu Controller dashboard rather than ArgoCD — the AppProfile declares which pattern to use via a `deploymentMethod` field (`argocd` or `tofu-controller`). The orchestrator routes accordingly.

**Long-term goal — eliminate Pattern B by contributing upstream:** Pattern B is a pragmatic workaround, not the target state. The plan is to contribute `existingSecret` support to upstream Helm charts (Nubus, OX App Suite, Postfix, Dovecot) and migrate each app from Pattern B to Pattern A as patches are merged. Each successful upstream contribution removes one Tofu Controller dependency, simplifies the orchestrator (ArgoCD Application CR instead of `Terraform` CR), and improves visibility (app appears in ArgoCD UI instead of Tofu Controller). Track upstream PRs in the AppProfile's `deploymentMethod` field — when a chart gains `existingSecret` support, flip from `tofu-controller` to `argocd` and remove the Tofu module.

**Test:**
- envtest: Tenant with 3 apps → 3 Application CRs with correct values
- Integration: create Tenant → ArgoCD deploys apps → apps are accessible
- Verify value mapping: OIDC issuer points at correct realm, database name matches prefix convention

---

### Increment 10 — Ingress reconciler (per-tenant routing + DNS)

**Goal:** Orchestrator provisions per-tenant Ingress resources and (optionally) DNS records — required for tenant apps to be reachable at `{tenant-domain}`.

**Reconciler logic (Tenant → ingress):**
1. For each app deployed in the tenant namespace, create an `Ingress` resource:
   - Host: `{app-slug}.{tenant-domain}` or `{tenant-domain}/{app-path}` (depending on routing strategy in Tenant CR)
   - TLS: reference cert-manager `Certificate` CR or wildcard cert
   - Backend: Service created by the app's Helm chart
2. Create a cert-manager `Certificate` CR for `*.{tenant-domain}` (wildcard) or per-app certificates
3. **DNS provisioning (if enabled):** Create an OpenTofu workspace (via Tofu Controller `Terraform` CR) that provisions DNS records for `{tenant-domain}` using the appropriate provider (Cloudflare, AWS Route53, etc.). The Tofu module is parameterised by the tenant domain and app list.
4. Surface DNS records that need manual creation (for providers without Tofu support) in the Tenant CR status
5. Set status condition `IngressReady: True`

**Design note:** Architecture §3 assigns DNS to OpenTofu ("best fit — huge provider ecosystem"). The orchestrator creates a Tofu Controller CR per tenant for DNS, keeping the boundary clean: orchestrator handles Ingress resources (K8s-native), OpenTofu handles DNS records (cloud-provider-specific). For clusters without external DNS requirements (e.g., dev/staging with `*.nip.io`), the DNS step is skipped.

**Test:**
- Integration: create Tenant → Ingress resources exist, TLS certificate issued, app reachable at `{app}.{tenant-domain}`
- Verify: second tenant gets independent ingress with no route conflicts
- DNS test (if provider configured): `dig {app}.{tenant-domain}` resolves correctly

---

### Increment 11 — IntegrationBinding reconciler

**Goal:** Auto-generate IntegrationBinding CRs when both provider and consumer of a contract are in a tenant's app list — architecture §4.3.

**Reconciler logic:**
1. For each app in tenant's app list, check AppProfile's `optionalIntegrations`
2. For each integration, check if the `provider` app is also in the tenant's app list
3. If both exist: create IntegrationBinding CR with credential provisioning (OpenBao path, OIDC token exchange config)
4. If provider is removed from tenant: garbage-collect the IntegrationBinding (via owner reference)
5. Track health: periodically verify credentials are valid and provider is reachable
6. Surface binding status in Tenant CR status

**Test:**
- envtest: Tenant with Nextcloud + OX App Suite → `filepicker` IntegrationBinding created
- envtest: remove OX App Suite from tenant → IntegrationBinding deleted
- Integration: verify token exchange works between bound apps

---

### Increment 12 — OpenBao path restructuring

**Goal:** Migrate OpenBao secret paths from flat `gentian/{env}/...` to hierarchical `gentian-os/kernel/...` and `gentian-os/tenants/{name}/apps/{app}/...` — architecture §5.

**Actions:**
- Update `tofu/modules/openbao-paths/` to create the architecture's secret tree structure
- Update `seed-openbao.sh` to seed kernel credentials under `gentian-os/kernel/` paths
- Update orchestrator to write tenant secrets under `gentian-os/tenants/{name}/apps/{app}/`
- Update all `ExternalSecret` CRs (both orchestrator-generated and kernel static ones) to reference new paths
- Generate per-tenant OpenBao policies: read-only access scoped to `gentian-os/tenants/{tenant}/apps/{app}/*`

**Migration on dev cluster:**
- Seed new paths → update ExternalSecrets → verify all secrets sync → remove old paths

**Test:**
- `bao kv list gentian-os/kernel/` shows expected tree
- `bao kv list gentian-os/tenants/test-tenant/apps/` shows per-app paths
- All ExternalSecrets report `SecretSynced`
- Policy test: tenant-scoped token can read own paths, cannot read other tenant paths

---

### Increment 13 — Kernel services as Helm chart + observability

**Goal:** Package the orchestrator + CRDs as a Helm chart (`charts/gentian-os/`) with built-in observability — so a deployment repo can install it with `helm install` and immediately get metrics and status visibility.

**Chart contents:**
- CRD manifests (from `config/crd/`) — including printer columns for `kubectl get tenants` (STATUS, APPS, READY, MAIL, AGE) and `kubectl get integrationbindings` (CONTRACT, STATUS, AGE)
- Orchestrator Deployment, ServiceAccount, ClusterRole, ClusterRoleBinding
- ConfigMap for orchestrator settings (OpenBao address, ArgoCD namespace, defaults)
- Prometheus ServiceMonitor for `gentianos_*` metrics
- Grafana dashboard ConfigMap (optional, enabled via `grafana.dashboards.enabled`)

**Prometheus metrics (architecture §16):**
- `gentianos_tenants_total` — total number of tenants
- `gentianos_tenant_apps_total` — apps per tenant
- `gentianos_provisioning_duration_seconds` — time to provision a tenant (histogram)
- `gentianos_reconcile_errors_total` — failed reconciliations by type
- `gentianos_credentials_age_seconds` — age of oldest credential per tenant
- `gentianos_integration_bindings_status` — binding health by contract type
- `gentianos_externalsecrets_sync_status` — ESO sync health per tenant
- `gentianos_operator_cr_ready_total` — operator CRs in Ready state
- `gentianos_operator_cr_failed_total` — operator CRs in Failed state

Metrics are exported via the controller-runtime `/metrics` endpoint. Each reconciler (Increments 2–11) instruments its reconcile loop — this increment packages and exposes them.

**Values:**
- `image.repository`, `image.tag`
- `openbao.address`, `openbao.authPath`
- `argocd.namespace`, `argocd.project`
- `defaults.isolation.mode` (namespace or vcluster)
- `metrics.serviceMonitor.enabled` (default: true)

**Test:**
- `helm template` renders valid YAML (including ServiceMonitor)
- `helm install --dry-run` succeeds on test cluster
- End-to-end: install chart → create Tenant CR → full provisioning pipeline runs
- `kubectl get tenants` shows STATUS, APPS, READY, MAIL columns
- Prometheus scrapes orchestrator metrics endpoint → `gentianos_tenants_total` > 0

---

### Increment 14 — AppProfiles for kernel apps + AppProfile update reconciler

**Goal:** Write AppProfile CRs for the always-on kernel apps and implement the AppProfile update reconciler (architecture §14.3) — so that bumping a chart version in an AppProfile propagates to all tenants using that profile.

**Profiles to create (from server/ reference):**
- `nubus.yaml` — Keycloak + UCS LDAP (identity kernel service)
- `postfix.yaml` — Mail MTA (kernel extension)
- `dovecot.yaml` — Mail MDA (kernel extension)
- `intercom-service.yaml` — Cross-app notifications

**Note:** Nextcloud is a kernel service deployed once in `platform-kernel` (not per-tenant). It does **not** get an AppProfile — per-tenant Nextcloud provisioning (groups, folders, sharing) is API-based via OCS REST API Jobs in Increment 7. Nextcloud's kernel deployment is managed by the Layer 100 ApplicationSet, not the orchestrator.

**Note:** OX App Suite is a user-installable app, not a kernel service. Its AppProfile belongs in `gentian-apps` (see Apps-B below), not here.

**Each profile declares:**
- `kernelRequirements` (which kernel functions it needs)
- `chart` reference (OCI URL + version)
- `valueMapping` (typed schema for Helm values)
- `provides` (what contracts it offers)
- `optionalIntegrations` (what peer integrations it supports)

**AppProfile update reconciler (`internal/controller/appprofile_controller.go`):**
- Watch AppProfile CRs for changes (chart version bump, valueMapping update)
- On update: list all Tenants referencing this profile via a label index
- For each affected tenant: re-render valueMapping and update the ArgoCD Application CR (or Tofu Controller `Terraform` CR for Pattern B apps) with the new chart version and values
- ArgoCD handles the rolling upgrade per tenant
- Update AppProfile status with affected tenant count and rollout progress
- This is the mechanism that makes "adding an app to the catalogue is a single-file operation" actually work at scale

**Source:** Reverse-engineer from `server/apps/{app}/` Helm values + `server/tofu/tenant/keycloak-config/` to extract the actual value keys each chart expects.

**Test:**
- All profiles validate against the AppProfile CRD schema
- `kubectl apply -f profiles/` succeeds
- Orchestrator can render correct Helm values from each profile's `valueMapping`
- AppProfile update test: bump chart version in a profile → all tenants' ArgoCD Applications updated → ArgoCD rolls out new version
- Rollout tracking: AppProfile status reflects how many tenants have been updated

---

### Increment 15 — Deployment repo setup (gentian-deployments)

**Goal:** Create the `gentian-deployments` repo structure — architecture §7 Repo 3. This replaces `server/` as the deployment source of truth.

**Structure:**
```
gentian-deployments/
├── dev/
│   ├── bootstrap/
│   │   └── install.sh
│   ├── kernel/
│   │   ├── values-dev.yaml          # Env-specific kernel overrides
│   │   └── tofu.tfvars              # OpenTofu vars (domain, master password ref)
│   ├── app-of-apps.yaml             # ArgoCD Application → gentian-os chart
│   └── tenants/
│       └── dev-tenant.yaml          # First Tenant CR
└── README.md
```

**The `app-of-apps.yaml`** ties gentian-os + gentian-apps + gentian-deployments together as described in architecture §7. It points at the gentian-os Helm chart by OCI version.

**Test:**
- Bootstrap a fresh dev cluster using only `gentian-deployments/dev/bootstrap/install.sh`
- Apply `app-of-apps.yaml` → ArgoCD installs orchestrator → create Tenant CR → full stack provisions
- This is the **end-to-end validation** that the architecture works

---

### Increment 16 — Mail kernel extension (per-tenant)

**Goal:** Model Postfix + Dovecot as a kernel extension that can be provisioned per-tenant in the four modes described in architecture §2.2.

**Reconciler logic (Tenant → mail extension):**
1. Read `spec.mail.mode` from Tenant CR
2. `selfhosted`: deploy per-tenant Postfix + Dovecot via ArgoCD Application CRs, provision DKIM keys in OpenBao, generate SPF/DMARC records in Tenant status
3. `external`: create ExternalSecret with tenant-provided SMTP/IMAP credentials
4. `transport-only`: deploy shared Postfix relay, no Dovecot
5. `disabled`: configure apps for outbound-only SMTP relay

**Test:**
- Create tenant with `mail.mode: selfhosted` → Postfix + Dovecot deployed in tenant namespace
- Create tenant with `mail.mode: disabled` → no mail stack, apps can still send notifications
- Send/receive test mail in selfhosted mode

---

### Increment 17 — Multi-tenant isolation hardening

**Goal:** Validate and harden the isolation model — architecture §2.4 and §8.

**Actions:**
- NetworkPolicy audit: tenant-to-tenant denied, tenant-to-kernel allowed, IntegrationBinding-scoped app-to-app rules
- Database isolation audit: verify tenant A cannot access tenant B's databases
- S3 isolation audit: verify bucket policies enforce tenant boundaries
- Keycloak realm isolation: verify token from realm A is rejected by app in realm B
- ResourceQuota enforcement: verify tenant cannot exceed declared limits
- Optional: vCluster-per-tenant mode for `isolation.mode: vcluster`

**Test:**
- Automated isolation test suite: create 2 tenants → attempt cross-tenant access at every layer → all denied
- Penetration-style tests: try to escalate from tenant namespace to kernel namespace
- **End-to-end deletion test:** create Tenant with 3+ apps → verify all resources provisioned → delete Tenant with `deletionPolicy: Delete` → verify: Keycloak realm deleted, LDAP OU removed, PostgreSQL/MariaDB databases dropped, MinIO buckets deleted, Redis ACL users removed, Memcached instances deleted, Ingress/DNS cleaned up, ExternalSecrets removed, ArgoCD Applications garbage-collected, namespace deleted
- **Retain test:** create Tenant → delete with `deletionPolicy: Retain` → verify: databases, buckets, and mailboxes still exist but credentials revoked, namespace preserved

---

## Increment Summary

| # | Name | Effort | Blocks | Key deliverable |
|---|---|---|---|---|
| 0 | Project scaffolding | Small | — | Go module, Makefile, CI, kernel assets from server/, **deployment smoke test** |
| 1 | CRD definitions | Medium | 2–11 | AppProfile, Tenant, IntegrationBinding types |
| 2 | Tenant namespace reconciler | Medium | 3–11 | First working reconciler |
| 3 | Identity reconciler (Keycloak/Nubus) | Large | 9, 11 | Per-tenant realms + OIDC clients via Keycloak REST API |
| 4 | LDAP provisioner (UDM REST API) | Medium | 9 | Per-tenant OUs, bind accounts, groups |
| 5 | Database reconciler (CloudNativePG) | Medium | 9 | Per-app-per-tenant PostgreSQL databases |
| 6 | MariaDB reconciler | Medium | 9 | Per-app-per-tenant MariaDB databases (OX App Suite) |
| 7 | Storage reconciler (MinIO + Nextcloud) | Medium | 9 | Per-tenant buckets + WebDAV |
| 8 | Cache reconciler (Redis + Memcached) | Medium | 9 | Per-app Redis ACLs + per-tenant Memcached |
| 9 | App deployment reconciler | Large | — | ArgoCD Application / Tofu CRs from AppProfiles (Pattern A + B) |
| 10 | Ingress reconciler | Medium | — | Per-tenant Ingress resources + DNS via Tofu |
| 11 | IntegrationBinding reconciler | Medium | — | Auto-wired cross-app contracts |
| 12 | OpenBao path restructuring | Medium | — | Architecture-compliant secret tree |
| 13 | Orchestrator Helm chart + observability | Small | — | Installable via `helm install`, Prometheus metrics, printer columns |
| 14 | AppProfiles + update reconciler | Medium | — | Kernel app profiles (Nubus, mail, intercom) + AppProfile update propagation |
| 15 | Deployment repo (gentian-deployments) | Medium | 13, 14 | End-to-end validation |
| 16 | Mail kernel extension | Medium | 3 | Per-tenant mail modes |
| 17 | Multi-tenant isolation hardening | Medium | 2–8 | Security validation |

Increments 0–1 are prerequisites. Increments 2–11 build the orchestrator one reconciler at a time — each is independently testable. Increment 12 can run in parallel with 2–11. Increments 13–15 integrate everything into a deployable system. Increments 16–17 harden and extend.

---

## Day-2 Operations

The increments above cover initial deployment. This section documents the operational flows that keep the platform running after day 1.

### App upgrades across tenants

When an AppProfile's `chart.version` is bumped (e.g., OpenProject 14.2.0 → 15.0.0 in `gentian-apps`):

1. ArgoCD syncs the updated AppProfile CR to the cluster
2. The AppProfile update reconciler (Increment 14) lists all Tenants referencing this profile
3. For each tenant: update the ArgoCD Application CR (or Tofu Controller `Terraform` CR) with the new chart version
4. ArgoCD performs a rolling upgrade per tenant — health checks gate progression
5. AppProfile status shows rollout progress (`updated: 47/50 tenants`)

**Canary strategy:** For high-risk upgrades, manually update one test tenant's `spec.apps[].config.chartVersion` override first. Once validated, bump the AppProfile version (which updates all remaining tenants).

### Tenant config changes

When a Tenant CR is updated (apps added/removed, quotas changed):

- **Adding an app:** The orchestrator provisions all required infrastructure (database, OIDC client, S3 bucket, etc.) and creates the ArgoCD Application CR. Existing apps are unaffected.
- **Removing an app:** The orchestrator deletes the ArgoCD Application CR (ArgoCD uninstalls the Helm release). Per the `deletionPolicy`: `Delete` drops the database/bucket, `Retain` revokes credentials but keeps data. IntegrationBindings involving the removed app are garbage-collected.
- **Changing quotas:** The namespace reconciler updates ResourceQuotas and LimitRanges. Running pods are unaffected until they restart.
- **Changing mail mode:** The mail reconciler (Increment 16) transitions between modes — e.g., deploying or removing the per-tenant mail stack.

### Orchestrator upgrades

When the gentian-os Helm chart is bumped to a new version in `gentian-deployments`:

1. ArgoCD detects the version change and deploys the new orchestrator
2. If CRDs changed: `controller-gen` ensures new fields have defaults; existing CRs remain valid
3. The new orchestrator reconciles all existing Tenants — idempotent, so no-op for unchanged tenants

**CRD evolution strategy:** Use `v1alpha1` during development. When the schema stabilises, introduce `v1beta1` with a conversion webhook that migrates `v1alpha1` CRs. Serve both versions simultaneously until all CRs are migrated. Never remove a served version without a deprecation cycle.

### Credential rotation

Triggered via annotation: `kubectl annotate tenant acme-corp gentianos.io/rotate-credentials=all`

1. The orchestrator regenerates credentials in OpenBao for the specified scope (`all`, or a specific app name)
2. ESO detects the change and syncs new secrets to Kubernetes
3. Stakater Reloader restarts affected pods
4. The annotation is cleared after rotation completes

### Pattern B elimination via upstream contributions

Pattern B (Tofu Controller `set_sensitive`) is a workaround for Helm charts without `existingSecret` support. The long-term strategy is to contribute `existingSecret` support upstream:

| App | Upstream project | Status | Target |
|---|---|---|---|
| Nubus | Univention | — | Add `existingSecret` for Keycloak admin, LDAP bind, PostgreSQL credentials |
| OX App Suite | Open-Xchange | — | Add `existingSecret` for MariaDB, Redis, S3, SMTP credentials |
| Postfix | Docker-mailserver or custom chart | — | Add `existingSecret` for SASL, DKIM, relay credentials |
| Dovecot | Docker-mailserver or custom chart | — | Add `existingSecret` for LDAP bind, TLS credentials |

Each successful merge allows flipping `deploymentMethod` from `tofu-controller` to `argocd` in the AppProfile — one PR per app, zero orchestrator changes. Track progress in the AppProfile's `metadata.annotations`.

---

## Architecture Coverage Matrix

Which architecture concepts are addressed by which increment — and which are not yet covered by this plan.

### Covered

| Architecture section | Concept | Increment | Notes |
|---|---|---|---|
| §2.1 Kernel Functions — Identity | OIDC provider + LDAP, per-tenant realms | 3, 4 | Keycloak REST API Jobs + UDM REST API Jobs |
| §2.1 Kernel Functions — Filesystem | WebDAV + S3, per-tenant buckets | 7 | MinIO + Nextcloud provisioning |
| §2.1 Kernel Functions — Networking | NetworkPolicies per tenant | 2 | Created by namespace reconciler |
| §2.1 Kernel Functions — Process execution | Kubernetes + ArgoCD GitOps | 0, 9 | Kernel assets + ArgoCD Application CRs |
| §2.1 Kernel Functions — Secrets & keyring | OpenBao + ESO, tenant-scoped policies | 0, 12 | Copied from server/, restructured |
| §2.1 Kernel Functions — Database services | CloudNativePG, per-app-per-tenant DBs | 5 | Replaces Tofu Helm-based PostgreSQL |
| §2.1 Kernel Functions — Database services (MariaDB) | MariaDB per-app-per-tenant DBs | 6 | MariaDB Operator or SQL Jobs (OX App Suite) |
| §2.1 Kernel Functions — Cache | Redis ACLs + Memcached per-tenant | 8 | Redis ACL provisioning + Memcached deployment |
| §2.1 Kernel Functions — Mail | Per-tenant Postfix + Dovecot, 4 modes | 16 | Kernel extension reconciler |
| §2.1 Kernel Functions — Package manager | AppProfile CRD + orchestrator pipeline | 1, 9, 14 | CRD + reconciler + profiles |
| §2.1 Kernel Functions — App-to-app permissions | IntegrationBinding + OIDC token exchange | 1, 11 | CRD + reconciler |
| §2.1 Kernel Functions — Init system / lifecycle | Thin orchestrator | 2–11 | Built incrementally |
| §2.1 Kernel Functions — Resource quotas | Per-tenant ResourceQuotas + LimitRanges | 2 | Namespace reconciler |
| §2.2 Kernel Extensions | Mail as optional per-tenant extension | 16 | Four tenant modes |
| §2.4 Multi-Tenancy | Namespace-per-tenant isolation | 2, 17 | Namespace reconciler + hardening |
| §2.5 Contracts | IntegrationBinding auto-generation | 11 | Contract reconciler |
| §3 Architecture Triangle | Orchestrator + OpenTofu + ArgoCD | 0–11 | Triangle fully implemented |
| §3.1 Thin Orchestrator | Delegate to operators, don't implement | 2–11 | Core of the plan (Jobs for Nubus-managed services) |
| §3.1 Thin Orchestrator — LDAP | LDAP bind account via UDM REST API | 4 | UDM Jobs for per-tenant OUs and bind accounts |
| §4.1 AppProfile CRD | Cluster-scoped app catalogue | 1, 14 | Types + kernel app profiles |
| §4.1 AppProfile CRD — appSecrets | App-internal generated secrets (HMAC-SHA256) | 1, 9 | Declared in AppProfile, generated + injected by orchestrator |
| §4.2 Tenant CRD | Organisation resource | 1, 2 | Types + namespace reconciler |
| §4.3 IntegrationBinding CRD | Cross-app contract | 1, 11 | Types + reconciler |
| §4.4 ArgoCD Application (generated) | Per-app-per-tenant deployment | 9 | App deployment reconciler |
| §5.1 Secret Seeding | HMAC-SHA256 derivation | 0 | Copied from server/ |
| §5.2 Tofu Lifecycle Guard | `ignore_changes` on secrets | 0 | Copied from server/ |
| §5.3 Pattern A + B | Two secret delivery patterns | 0, 9 | Copied from server/; orchestrator routes by `deploymentMethod` |
| §5.4 Credential Rotation | Reloader + ESO | 0 | Copied from server/ |
| §5 Secret Path Structure | Per-tenant `gentian-os/tenants/{name}/apps/{app}/...` | 12 | OpenBao restructuring |
| §5 Per-tenant OpenBao Policies | Read-only scoped to own paths | 12 | Generated during restructuring |
| §6 Layer 000 Bootstrap | ArgoCD + Tofu Controller install | 0 | Scripts copied from server/ |
| §6 Layer 100 Kernel | Kernel workloads via ArgoCD/Tofu | 0 | Kernel services copied from server/ |
| §6 Layer 100e Kernel Extensions | Mail stack | 16 | Per-tenant mail reconciler |
| §6 Root ApplicationSet | Meta-deployer + matrix generators | 0 | Copied from server/ |
| §6 Layer 200 Apps | Orchestrator-managed tenant apps | 9 | App deployment reconciler |
| §6 Ingress / DNS | Per-tenant routing + TLS + DNS records | 10 | Ingress reconciler + Tofu DNS module |
| §7 Repo 1 `gentian-os` | OS definition repo | 0, 13 | Scaffolding + Helm chart |
| §7 Repo 3 `gentian-deployments` | Cluster state repo | 15 | Deployment repo setup |
| §8 Security Model — Network boundaries | Tenant-to-tenant deny | 2, 17 | NetworkPolicies + hardening |
| §8 Security Model — OIDC trust chain | Per-tenant realms, token exchange | 3, 11 | Identity + IntegrationBinding |
| §8 Security Model — Database isolation | Per-app-per-tenant databases | 5, 17 | CloudNativePG + hardening |
| §8 Security Model — Zero-trust secrets | All secrets via OpenBao | 0, 12 | Existing + restructured |
| §12 CRD Definitions | AppProfile, Tenant, IntegrationBinding, ExternalSecret, Application | 1 | Go types matching §12 examples |
| §13 OpenTofu — Kernel Seeding | OpenBao path seeding, secret tree | 0, 12 | Copied then restructured |
| §14 Orchestrator Reconciliation Logic | Create/Update/Delete tenant flows | 2–11 | Built incrementally |
| §14.2 Tenant Deletion | Full deletion pipeline (9-step sequence) | 2–8, 10, 17 | Delete-path in each reconciler + end-to-end test in Inc 17 |
| §14.3 AppProfile Update | Chart version propagation to all tenants | 14 | AppProfile update reconciler |
| §15 Building the Orchestrator | Kubebuilder scaffold, project structure | 0, 13 | Scaffolding + Helm chart |
| §16 Observability — Prometheus | `gentianos_*` metrics from orchestrator | 13 | 9 metrics exported via controller-runtime, ServiceMonitor in chart |
| §16 Observability — CRD Status | `kubectl get tenants` shows health | 1, 13 | Printer columns in CRD types, status conditions in reconcilers |

### Not yet covered (future work)

| Architecture section | Concept | Why not covered | Depends on |
|---|---|---|---|
| §2.1 Window manager | Contract-based portal navigation registration | Univention Portal works as-is | IntegrationBinding (Increment 11) |
| §2.1 Notifications | Cross-app notification gateway | Intercom Service exists but gateway not designed | Architecture design needed |
| §2.4 vCluster isolation | vCluster-per-tenant mode | Optional; namespace mode is default | Increment 17 (optional) |
| §7 Repo 2 `gentian-apps` | App catalogue repo | **Covered below** in gentian-apps plan | Orchestrator + AppProfile CRD |
| §8 Mail security | DKIM keys in OpenBao, SPF/DMARC automation | Part of mail extension | Increment 16 (partial) |
| §9 Backup Strategy | pgBackRest, Velero, OpenBao snapshots | Independent workstream; no orchestrator dependency | Can start anytime |
| §9.2 Tenant-Scoped Restore | RestoreTenant CR | Future CR | Backup strategy + orchestrator |
| §9.3 Tenant Migration | Cross-cluster tenant move | Future capability | Backup + orchestrator |
| §9.4 Disaster Recovery | Full-cluster DR sequence | Documented in architecture; operational procedure | Backup strategy |
| §10.1 MCP Discovery Layer | MCP complements IntegrationBindings | Phase after core orchestrator | Increment 11 + MCP ecosystem |
| §10.2 MCP Kernel Requirement | `mcp: enabled` in AppProfile | AppProfile schema supports it; no registry yet | AppProfile CRD (Increment 1) |
| §10.3 Shell AI Assistant | Portal AI assistant via MCP | Requires MCP registry + adapters | MCP registry |
| §10.4 Cross-App Agent Orchestration | AI agent as integration layer | Requires MCP servers per app | MCP adapters |
| §10.5 AI-Assisted Operations | AppProfile generation, health monitoring | Requires stable AppProfile schema + observability | Increments 1, 14 + observability |

---

## What comes from `server/`

The `server/` repo is reference material. These assets are copied into `gentian-os/kernel/` during Increment 0 and adapted:

| Source (`server/`) | Target (`gentian-os/`) | Adaptation needed |
|---|---|---|
| `scripts/*.sh` | `scripts/` | Update paths |
| `openbao/`, `eso/` | `kernel/openbao/`, `kernel/eso/` | Update secret paths (Increment 12) |
| `values/reloader.yaml`, `values/tofu-controller.yaml` | `kernel/values/` | Minimal |
| `argocd/bootstrap/`, `argocd/install/`, `argocd/projects/`, `argocd/repos/` | `kernel/bootstrap/`, `kernel/argocd/` | Update repo URLs |
| `apps/*` (all kernel services) | `kernel/services/` | Reference for AppProfile value mapping |
| `appsets/*.yaml` | `kernel/appsets/` | Update paths + repo URLs |
| `tofu/modules/openbao-paths/` | `kernel/tofu/modules/openbao-paths/` | Update path structure (Increment 12) |
| `tofu/modules/app/`, `tofu/modules/app-trust/` | `kernel/tofu/modules/app/`, `kernel/tofu/modules/app-trust/` | May be replaced by orchestrator (Increments 3, 11) |
| `tofu/tenant/infra-workspaces/`, `tofu/tenant/keycloak-config/` | `kernel/tofu/tenant/` | May be replaced by orchestrator |
| `manifest/keycloak-service-alias.yaml` | `kernel/manifest/` | Minimal |

**Key insight:** Several Tofu modules (`app/`, `app-trust/`, `infra-workspaces/`, `keycloak-config/`) do things that the orchestrator will eventually replace (OIDC client provisioning, token exchange setup, Pattern B Helm releases). They are copied as **transitional scaffolding** — they keep the kernel running while the orchestrator reconcilers are built. Once Increments 3–11 are complete, these Tofu modules become dead code.

---

## Transitional architecture

During development, the system runs in a hybrid mode:

```
Phase 1 (Increments 0–1):
  Kernel runs from gentian-os/kernel/ via ArgoCD (same as server/ but relocated)
  No orchestrator yet — Tofu modules handle provisioning
  CRDs installed but nothing watches them
  Deployment smoke test validates kernel health

Phase 2 (Increments 2–11):
  Orchestrator deployed, reconcilers added one at a time
  Each new reconciler replaces a Tofu module's responsibility
  Both can coexist — orchestrator creates CRs, Tofu modules are idempotent

Phase 3 (Increments 12–17):
  Orchestrator is the primary provisioning plane
  Tofu modules reduced to kernel-only infra (OpenBao seeding, external resources)
  Full tenant lifecycle via Tenant CRs
  gentian-deployments repo is the single entry point
```

### Kernel vs. orchestrator lifecycle boundary

**Kernel services (Layer 100) are permanently ApplicationSet-managed.** The orchestrator does not deploy or manage kernel services — it consumes them. Nubus, CloudNativePG clusters, MinIO, Redis, ESO, Reloader, and the orchestrator itself are deployed via ArgoCD ApplicationSets (copied from `server/` in Increment 0 and maintained in `gentian-os/kernel/`). This is not transitional — it is the permanent architecture.

**The orchestrator manages only Layer 200 (tenant-scoped resources).** When a Tenant CR is created, the orchestrator provisions identity, databases, storage, cache, and app deployments within the tenant namespace. It creates per-tenant ArgoCD Application CRs and Tofu Controller CRs, but never touches kernel ApplicationSets.

This matches architecture §6: Layer 100 is "OpenTofu + ArgoCD" managed, Layer 200 is "Orchestrator + ArgoCD" managed. The orchestrator sits at Layer 160 — deployed as part of the kernel, managing everything above it.

---

## Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Nubus decomposition complexity | Keycloak Operator cannot manage Nubus-bundled Keycloak | Path A (v1): use Keycloak REST API via Jobs. Path B (future): decompose Nubus into standalone components |
| CloudNativePG learning curve | Database provisioning regression | Keep Tofu-based PostgreSQL as fallback during transition |
| MariaDB Operator maturity | Less mature than CloudNativePG | Start with SQL Jobs; migrate to operator CRs when stable |
| Orchestrator bugs corrupt tenant state | Data loss | Orchestrator never deletes operator-managed resources directly; `deletionPolicy: Retain` is default |
| Scope creep on CRD design | Delays | Start with minimal viable CRDs; extend via v1alpha2 later |
| Tofu ↔ orchestrator conflict | Both try to manage same resources | Clear ownership boundary: orchestrator owns tenant-scoped resources; Tofu owns kernel-scoped resources |
| OpenBao path migration breaks running services | Secret sync failures | Dual-write during migration: populate both old and new paths, switch ExternalSecrets, then remove old paths |

---

## Prerequisites

- [ ] Go 1.22+ development environment
- [ ] `controller-runtime` v0.18+ / `controller-gen` available
- [ ] Dev cluster with ArgoCD, Tofu Controller, OpenBao, ESO (current `server/` setup works)
- [ ] Nubus deployed with Keycloak Admin REST API and UDM REST API accessible from kernel namespace
- [ ] CloudNativePG operator installable
- [ ] MariaDB deployed as kernel service (for OX App Suite)
- [ ] OCI registry for publishing orchestrator container image + Helm chart
- [ ] `gentian-deployments` repo created (can be empty until Increment 15)
- [ ] Agreement on CRD API version strategy (`v1alpha1` → `v1beta1` → `v1`)

---

## gentian-apps — App Catalogue Migration Plan

This section covers architecture §7 Repo 2: building the `gentian-apps` repository as the declarative app catalogue. This work can start once AppProfile CRDs exist (Increment 1) and is fully useful once the orchestrator's app deployment reconciler works (Increment 9).

### Current state

The `gentian-apps` repo contains only `LICENSE` and `README.md`. User-installable app configs currently live in `server/apps/` (OX App Suite) with more planned (Collabora, Element, Jitsi, OpenProject, XWiki).

### Target structure (architecture §7 Repo 2)

```
gentian-apps/
├── profiles/
│   ├── ox-appsuite.yaml         # Groupware (mail client, calendar, contacts)
│   ├── collabora.yaml           # Document editing (via Nextcloud integration)
│   ├── element.yaml             # Chat (Matrix)
│   ├── jitsi.yaml               # Video conferencing
│   ├── openproject.yaml         # Project management
│   └── xwiki.yaml               # Wiki / knowledge management
├── contracts/
│   ├── file-store.yaml          # WebDAV read/write (provider: Nextcloud)
│   ├── filepicker.yaml          # File selection UI (provider: Nextcloud)
│   ├── central-navigation.yaml  # Portal link registration (provider: Univention Portal)
│   ├── project-management.yaml  # Task/timeline API (provider: OpenProject)
│   └── chat.yaml                # Messaging API (provider: Element)
├── tests/
│   └── validate-profiles.sh     # Schema validation against AppProfile CRD
├── LICENSE
└── README.md
```

### App migration increments

#### Apps-A — Scaffold and contract definitions

**Goal:** Set up repo structure, CI, and define the contract schemas that apps reference.

**Actions:**
- Create `profiles/`, `contracts/`, `tests/` directories
- Write contract YAML files — each defines a capability name, protocol, and expected interface
- CI pipeline: validate all YAML files against the AppProfile CRD schema (requires CRD from gentian-os Increment 1)
- Add `validate-profiles.sh` that runs `kubectl apply --dry-run=server` against a test cluster or uses `kubeconform`

**Test:** CI green, contract schemas valid.

---

#### Apps-B — OX App Suite AppProfile (first real app)

**Goal:** Convert the existing `server/apps/ox-appsuite/` into an AppProfile — the first user-installable app in the catalogue.

**Actions:**
- Reverse-engineer `server/apps/ox-appsuite/` Helm values and `server/tofu/tenant/keycloak-config/` to extract:
  - Which kernel requirements OX App Suite needs (OIDC, database/MariaDB, S3, SMTP, IMAP, cache/Redis)
  - Which Helm value keys receive kernel-provided values
  - Which contracts it provides and consumes
- Write `profiles/ox-appsuite.yaml` with:
  - `kernelRequirements`: identity (oidc, ldap), database (mariadb), storage (s3), cache (redis), mail (smtp, imap)
  - `chart`: OCI reference to upstream OX App Suite Helm chart
  - `valueMapping`: typed schema mapping kernel values → Helm value keys
  - `optionalIntegrations`: filepicker (Nextcloud), central-navigation (Portal)
- Verify the orchestrator can render correct Helm values from this profile

**Source material:** `server/apps/ox-appsuite/`, `server/tofu/tenant/infra-workspaces/` (Pattern B values for OX)

**Test:**
- Profile validates against AppProfile CRD
- Orchestrator dry-run: create Tenant with `apps: [{profile: ox-appsuite}]` → rendered ArgoCD Application has correct values
- Deployed OX App Suite works identically to current server/ deployment

---

#### Apps-C — Collabora AppProfile

**Goal:** Add Collabora Online as the second user-installable app.

**Actions:**
- Write `profiles/collabora.yaml`:
  - `kernelRequirements`: identity (oidc — via Nextcloud integration)
  - `optionalIntegrations`: file-store (Nextcloud — Collabora is an editor, not standalone)
  - `chart`: upstream Collabora chart
  - `valueMapping`: Nextcloud integration URL, WOPI settings
- This is a simpler profile than OX since Collabora integrates through Nextcloud, not directly with the kernel

**Test:** Profile valid, Collabora deploys and integrates with Nextcloud for document editing.

---

#### Apps-D — Communication apps (Element + Jitsi)

**Goal:** Add chat and video conferencing to the catalogue.

**Actions:**
- Write `profiles/element.yaml`:
  - `kernelRequirements`: identity (oidc), database (postgresql — for Synapse homeserver), cache (redis)
  - `provides`: chat contract
  - `optionalIntegrations`: central-navigation (Portal)
  - `chart`: Element/Synapse chart
- Write `profiles/jitsi.yaml`:
  - `kernelRequirements`: identity (oidc)
  - `optionalIntegrations`: central-navigation (Portal), chat (Element — for Jitsi links in chat)
  - `chart`: Jitsi chart

**Test:** Both profiles valid, apps deploy per-tenant, SSO works.

---

#### Apps-E — Productivity apps (OpenProject + XWiki)

**Goal:** Add project management and wiki to the catalogue.

**Actions:**
- Write `profiles/openproject.yaml` (reference: architecture §12.1):
  - `kernelRequirements`: identity (oidc, ldap sync), database (postgresql), storage (s3, webdav), cache (memcached), mail (smtp)
  - `provides`: project-management contract
  - `optionalIntegrations`: file-store (Nextcloud), central-navigation (Portal)
- Write `profiles/xwiki.yaml`:
  - `kernelRequirements`: identity (oidc, ldap sync), database (postgresql), storage (s3)
  - `optionalIntegrations`: central-navigation (Portal)

**Test:** Both profiles valid, apps deploy per-tenant with correct database and storage isolation.

---

### App migration summary

| Increment | App(s) | Depends on (gentian-os) | Effort |
|---|---|---|---|
| Apps-A | Scaffold + contracts | Increment 1 (CRDs) | Small |
| Apps-B | OX App Suite | Increment 9 (app reconciler) | Medium |
| Apps-C | Collabora | Increment 9 | Small |
| Apps-D | Element + Jitsi | Increment 9 | Medium |
| Apps-E | OpenProject + XWiki | Increment 9 | Medium |

Apps-A can start as soon as the AppProfile CRD exists. Apps-B through Apps-E can be built in parallel once the orchestrator's app deployment reconciler (Increment 9) is functional. Each profile is an independent PR.

### Where `server/` content ends up

| Content | Current location | Target | When |
|---|---|---|---|
| OX App Suite Helm values | `server/apps/ox-appsuite/` | `gentian-apps/profiles/ox-appsuite.yaml` | Apps-B |
| OX ExternalSecrets | `server/apps/ox-appsuite/secrets/` | Generated by orchestrator (not in gentian-apps) | Increment 9 |
| OX Tofu config (Pattern B) | `server/tofu/tenant/infra-workspaces/` | Replaced by orchestrator + AppProfile | Increment 9 |
| OX Keycloak config | `server/tofu/tenant/keycloak-config/` | Replaced by orchestrator identity reconciler | Increment 3 |
| OX ApplicationSet entry | `server/appsets/30-opendesk.yaml` | Replaced by orchestrator ArgoCD Application generation | Increment 9 |
| Environment-specific overrides | `server/values/env/` | `gentian-deployments/` | Increment 15 |

Once all apps have AppProfiles and the orchestrator handles deployment, `server/` becomes empty and can be archived.
