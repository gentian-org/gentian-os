# Gentian OS — Implementation Plan

**Date:** 2026-04-05
**Scope:** Build Gentian OS to match the architecture document — CRDs, thin orchestrator, kernel services, and deployment pipeline.
**Principle:** Every increment produces a working, testable artifact. The thin orchestrator is the backbone — most other work flows from it.

---

## Strategy

CRDs first, then orchestrator reconcilers one kernel function at a time, then tenant lifecycle, then apps. The `server/` repo is reference material — we cherry-pick proven patterns (HMAC-SHA256 derivation, Pattern A/B, Reloader, ApplicationSet patterns) and rewrite what doesn't fit.

```
CRD definitions → Thin orchestrator → Operator CRs / ExternalSecrets / ArgoCD Apps / IntegrationBindings
```

The orchestrator delegates to existing operators (CloudNativePG, MinIO, ESO, etc.) and uses idempotent Jobs where no operator exists (Keycloak Admin REST API, UDM REST API). Most reconcilers are ~200 LOC of CR creation + status watching.

---

## Completed Increments

| # | Title | Status | Key deliverable |
|---|---|---|---|
| 0 | Project scaffolding | ✅ Done | Go module, Makefile, CI, `kernel/` assets from `server/`, Dockerfile, deployment smoke test |
| 1 | CRD definitions | ✅ Done | AppProfile, Tenant, IntegrationBinding Go types + generated YAML. 13 tests |
| 2 | Tenant namespace reconciler | ✅ Done | Namespace + ResourceQuota + LimitRange + NetworkPolicy + Delete/Retain policy. 8 envtest tests |
| 3 | Identity reconciler | ✅ Done | Keycloak realm + OIDC client Jobs via Admin REST API. 5 envtest tests. 26 total |
| 4 | LDAP reconciler | ✅ Done | UDM OU + bind account Jobs via UDM REST API. 6 envtest tests. 32 total |
| 5 | Database reconciler (PostgreSQL) | ✅ Done | CloudNativePG `Database` CRs + psql role Jobs. 5 envtest tests. 37 total |
| 6 | MariaDB reconciler | ✅ Done | Idempotent `CREATE DATABASE / CREATE USER / GRANT` Jobs. 4 envtest tests. 41 total |
| 7 | Storage reconciler | ✅ Done | MinIO S3 buckets via `minio/mc` Job + Nextcloud OCS API Job. 5 envtest tests. 46 total |
| 8 | Cache reconciler | ✅ Done | Redis ACL users via `redis-cli` Job + per-tenant Memcached ArgoCD Application. 5 envtest tests. 51 total |
| 9 | App deployment reconciler | ✅ Done | ArgoCD Application CRs per app per tenant. Pattern A + B routing. `valueMapping` rendering. Orphan cleanup: removing an app from `spec.apps` deletes the corresponding CR. `destroyResourcesOnDeletion: true` on Terraform CRs. Label-based listing in `deleteAppDeployment()`. 7 envtest tests. 58 total |
| 10 | Ingress reconciler | ✅ Done | Per-app Ingress CRs + cert-manager wildcard Certificate CR. 5 envtest tests. 63 total |
| 11 | IntegrationBinding reconciler | ✅ Done | Auto-generates bindings when provider + consumer both in tenant app list. 4 envtest tests. 67 total |
| 12 | OpenBao restructuring | ✅ Done | `gentian-os/kernel/` and `gentian-os/tenants/{name}/apps/{app}/` path hierarchy. 67 total |
| 13 | Helm chart + observability | ✅ Done | `charts/gentian-os/` with CRDs, Deployment, RBAC, ServiceMonitor, Grafana dashboard. Prometheus metrics. Printer columns. 67 total |
| 14 | AppProfiles + update reconciler | ✅ Done | 5 AppProfile YAMLs (element, jitsi, openproject, xwiki, ox-appsuite). All Pattern B. 67 total |
| 15 | Deployment repo (gentian-deployments) | ✅ Done | `gentian-deployments/dev/` — bootstrap, app-of-apps, dev-tenant, values, env vars. 67 total |
| 16 | Mail kernel extension | ✅ Done | Shared Postfix/Dovecot via kernel ConfigMaps. 4 modes: selfhosted, external, transport-only, disabled. 7 envtest tests. 78 total |
| 17 | Isolation hardening tests | ✅ Done | Cross-tenant NetworkPolicy, ingress/egress rules, ResourceQuota, LimitRange, end-to-end Delete + Retain. 7 envtest tests. 95 total |
| 18 | Single-line domain config | ✅ Done | Single `domain` variable. 41 `_base.yaml` → `${domain}` template. Eliminated per-file hostname repetition. 95 total |
| 19 | App Store controller + catalogue API | ✅ Done | `AppCatalogue` singleton CR + `AppStoreReconciler`. `TenantValidator` webhook (maxApps quota + AppProfile existence). `kubectl-gentian` plugin (list/install/uninstall via Git commit to `gentian-deployments`). 6 envtest tests. 101 total |
| 20 | Collabora → kernel service (Nextcloud Office) | ✅ Done | Migrated Collabora from per-tenant AppProfile to shared kernel service in `kernel/services/collabora/`. Single ingress `office.<domain>`. `Tenant.spec.office.enabled` flag replaces `spec.apps: [{profile: collabora}]`. WOPI URL updated to kernel service in-cluster address. 101 total |
| 21 | Element AppProfile in gentian-apps | ✅ Done | `profiles/element.yaml` in `gentian-apps`. Removed from `gentian-os/config/samples/`. `extraValues` aligned with opendesk `values-element.yaml.gotmpl` + `values-synapse.yaml.gotmpl` (E2EE, OIDC, SMTP, ratelimits, security context). Ingress `chat.<domain>` → element:80. 101 total |
| 21a | Kernel secret seeder (OpenBao write path) | ✅ Done | Shared `internal/kernel/secrets` package: HKDF-SHA256 deriver, KV v2 client, canonical path builder, seeder. Master password is fetched at operator startup from `secret/gentian-os/kernel/internal/master-password` via the `gentian-os-operator` Kubernetes auth role; when present, every kernel reconciler (identity, ldap, database, mariadb, storage, cache, mail, apps) derives and writes the credentials it provisions into `gentian-os/tenants/{t}/apps/{a}/{category}` and `…/internal/{name}`. Tenant is included in the derivation salt for tenant-scoped secrets but omitted for kernel-shared paths (`gentian-os/kernel/{category}/{name}`) so shared services keep a single value across tenants. Provisioning Jobs (Keycloak, psql, mariadb, mc, redis-cli, UDM) consume the derived password via env var and apply it idempotently (`ALTER ROLE`, `ALTER USER`, `ACL SETUSER`, etc.) so the live backend password always equals the OpenBao value. Bootstrap (`scripts/seed-openbao.sh` + `install.sh`) writes the master password and configures OpenBao auth so a fresh cluster works without manual steps. |
| 22 | Jitsi AppProfile in gentian-apps | ✅ Done | `profiles/jitsi.yaml` in `gentian-apps` with `extraValues` aligned with `opendesk/helmfile/apps/jitsi/values-jitsi.yaml.gotmpl`. `kernelRequirements: oidc`. `appSecrets`: `jwt_app_secret`, `jicofo_auth_password`, `jicofo_component_secret`, `jvb_auth_password`. Hybrid-matrix-token auth scheme for Prosody. Ingress `meet.<domain>`. Jigasi disabled by default. TURN credentials injected from kernel path at deploy time. Optional Element video-conferencing IntegrationBinding. No sample to remove from `gentian-os` (sample was never added). |
---

## Day-2 Operations

The increments above cover initial deployment. This section documents the operational flows that keep the platform running after day 1.

### App upgrades across tenants

When an AppProfile's `chart.version` is bumped (e.g., OpenProject 14.2.0 → 15.0.0 in `gentian-apps`):

1. ArgoCD syncs the updated AppProfile CR to the cluster
2. The AppProfile update reconciler (Increment 14) lists all Tenants referencing this profile
3. For each tenant: update the ArgoCD Application CR or Terraform CR with the new chart version
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

Triggered via annotation: `kubectl annotate tenant gtn-demo gentianos.io/rotate-credentials=all`

1. The orchestrator regenerates credentials in OpenBao for the specified scope (`all`, or a specific app name)
2. ESO detects the change and syncs new secrets to Kubernetes
3. Stakater Reloader restarts affected pods
4. The annotation is cleared after rotation completes

### Pattern B elimination via upstream contributions

Pattern B (`set_sensitive`) is a workaround for Helm charts without `existingSecret` support. The long-term strategy is to contribute `existingSecret` support upstream:

| App | Upstream project | Status | Target |
|---|---|---|---|
| Nubus | Univention | — | Add `existingSecret` for Keycloak admin, LDAP bind, PostgreSQL credentials |
| OX App Suite | Open-Xchange | — | Add `existingSecret` for MariaDB, Redis, S3, SMTP credentials |
| Postfix | Docker-mailserver or custom chart | — | Add `existingSecret` for SASL, DKIM, relay credentials |
| Dovecot | Docker-mailserver or custom chart | — | Add `existingSecret` for LDAP bind, TLS credentials |

Each successful merge allows flipping `deploymentMethod` from `crossplane` to `argocd` in the AppProfile — one PR per app, zero orchestrator changes. Track progress in the AppProfile's `metadata.annotations`.

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
| §2.1 Kernel Functions — Database services | CloudNativePG, per-app-per-tenant DBs | 5 | CloudNativePG operator CRs |
| §2.1 Kernel Functions — Database services (MariaDB) | MariaDB per-app-per-tenant DBs | 6 | MariaDB Operator or SQL Jobs (OX App Suite) |
| §2.1 Kernel Functions — Cache | Redis ACLs + Memcached per-tenant | 8 | Redis ACL provisioning + Memcached deployment |
| §2.1 Kernel Functions — Mail | Per-tenant Postfix + Dovecot, 4 modes | 16 | Kernel extension reconciler |
| §2.1 Kernel Functions — Package manager | AppProfile CRD + orchestrator pipeline | 1, 9, 14 | CRD + reconciler + profiles. App Store: Inc 19 |
| §2.1 Kernel Functions — App-to-app permissions | IntegrationBinding + OIDC token exchange | 1, 11 | CRD + reconciler |
| §2.1 Kernel Functions — Init system / lifecycle | Thin orchestrator | 2–11 | Built incrementally |
| §2.1 Kernel Functions — Resource quotas | Per-tenant ResourceQuotas + LimitRanges | 2 | Namespace reconciler |
| §2.2 Kernel Extensions | Mail as optional per-tenant extension | 16 | Four tenant modes |
| §2.4 Multi-Tenancy | Namespace-per-tenant isolation | 2, 17 | Namespace reconciler + hardening |
| §2.5 Contracts | IntegrationBinding auto-generation | 11 | Contract reconciler |
| §3 Architecture Triangle | Orchestrator + ArgoCD + Crossplane | 0–11 | Triangle fully implemented |
| §3.1 Thin Orchestrator | Delegate to operators, don't implement | 2–11 | Core of the plan (Jobs for Nubus-managed services) |
| §3.1 Thin Orchestrator — LDAP | LDAP bind account via UDM REST API | 4 | UDM Jobs for per-tenant OUs and bind accounts |
| §4.1 AppProfile CRD | Cluster-scoped app catalogue | 1, 14 | Types + kernel app profiles |
| §4.1 AppProfile CRD — appSecrets | App-internal generated secrets (HMAC-SHA256) | 1, 9 | Declared in AppProfile, generated + injected by orchestrator |
| §4.2 Tenant CRD | Organisation resource | 1, 2 | Types + namespace reconciler |
| §4.3 IntegrationBinding CRD | Cross-app contract | 1, 11 | Types + reconciler |
| §4.4 ArgoCD Application (generated) | Per-app-per-tenant deployment | 9 | App deployment reconciler |
| §5.1 Secret Seeding | HMAC-SHA256 derivation | 0 | Copied from server/ |
| §5.2 Lifecycle Guard | Write-once protection on secrets | 0 | Crossplane `managementPolicies: [Observe, Create]` |
| §5.3 Pattern A + B | Two secret delivery patterns | 0, 9 | Copied from server/; orchestrator routes by `deploymentMethod` |
| §5.4 Credential Rotation | Reloader + ESO | 0 | Copied from server/ |
| §5 Secret Path Structure | Per-tenant `gentian-os/tenants/{name}/apps/{app}/...` | 12 | OpenBao restructuring |
| §5 Per-tenant OpenBao Policies | Read-only scoped to own paths | 12 | Generated during restructuring |
| §6 Layer 000 Bootstrap | ArgoCD + Crossplane install | 0 | Scripts copied from server/ |
| §6 Layer 100 Kernel | Kernel workloads via ArgoCD/Crossplane | 0 | Kernel services copied from server/ |
| §6 Layer 100e Kernel Extensions | Mail stack | 16 | Per-tenant mail reconciler |
| §6 Root ApplicationSet | Meta-deployer + matrix generators | 0 | Copied from server/ |
| §6 Layer 200 Apps | Orchestrator-managed tenant apps | 9 | App deployment reconciler |
| §6 Ingress / DNS | Per-tenant routing + TLS + DNS records | 10 | Ingress reconciler |
| §7 Repo 1 `gentian-os` | OS definition repo | 0, 13 | Scaffolding + Helm chart |
| §7 Repo 3 `gentian-deployments` | Cluster state repo | 15 | Deployment repo setup |
| §8 Security Model — Network boundaries | Tenant-to-tenant deny | 2, 17 | NetworkPolicies + hardening |
| §8 Security Model — OIDC trust chain | Per-tenant realms, token exchange | 3, 11 | Identity + IntegrationBinding |
| §8 Security Model — Database isolation | Per-app-per-tenant databases | 5, 17 | CloudNativePG + hardening |
| §8 Security Model — Zero-trust secrets | All secrets via OpenBao | 0, 12 | Existing + restructured |
| §12 CRD Definitions | AppProfile, Tenant, IntegrationBinding, ExternalSecret, Application | 1 | Go types matching §12 examples |
| §13 Kernel Seeding | OpenBao path seeding, secret tree | 0, 12 | Copied then restructured |
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
| §7 Repo 2 `gentian-apps` | App catalogue repo | **Covered below** in App Store plan | Inc 19–25 |
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

## gentian-apps — App Store and App Catalogue

This section covers architecture §7 Repo 2 and the "Package manager" kernel function (§2.1): building `gentian-apps` as a runtime app catalogue with install/uninstall semantics — analogous to the Android Play Store or `apt`. The orchestrator already has the machinery (AppProfile CRDs, app deployment reconciler, AppProfile update propagation). What's missing is the **store layer** that makes apps discoverable, installable, and upgradeable at runtime without editing YAML files.

### Design — The Gentian App Store

In Android, the Play Store is a registry of APKs. Each APK declares its permissions (camera, storage, contacts), and the OS provisions them on install. In Gentian OS:

| Android | Gentian OS |
|---|---|
| APK with `AndroidManifest.xml` | AppProfile CR (`kernelRequirements`, `valueMapping`, `appSecrets`) |
| Play Store catalogue | `gentian-apps` repo — cluster-scoped AppProfile CRs synced by ArgoCD |
| Install button | Add app to `tenant.spec.apps[]` — orchestrator provisions everything |
| Permissions prompt | `kernelRequirements` — orchestrator provisions OIDC client, database, S3 bucket, etc. |
| Auto-update | AppProfile update reconciler (Inc 14) — bump chart version, all tenants upgrade |
| Uninstall | Remove app from `tenant.spec.apps[]` — orchestrator tears down per `deletionPolicy` |

The App Store has three components:

1. **App Registry** — the `gentian-apps` repo contains AppProfile YAMLs. ArgoCD syncs them to the cluster as cluster-scoped CRs. Adding a new app = committing one YAML file. This is the catalogue.

2. **App Store Controller** (`internal/controller/appstore_controller.go`) — a new reconciler that watches AppProfile CRs and maintains a `AppCatalogue` status resource listing all available apps, their versions, kernel requirements, and compatibility notes. This is what a UI or CLI queries to show "available apps."

3. **App Store API / CLI** (future) — a lightweight REST API or `kubectl` plugin that lists available apps, shows their requirements, and lets admins install/uninstall apps for a tenant. Installs and uninstalls mutate the Tenant CR YAML in `gentian-deployments` and commit+push the change. ArgoCD then reconciles the new state to the cluster — the Git commit is the single source of truth and the full audit trail. Only `gentian-deployments` receives these commits; `gentian-apps` only changes when AppProfiles are added or upgraded (catalogue changes), and `gentian-os` only changes when the orchestrator itself changes. A web UI integrated into the Univention Portal comes later.

**Runtime install flow:**

```
Admin: "Install OpenProject for tenant gtn-demo"
  ↓
kubectl gentian apps install openproject --tenant gtn-demo
  ↓
CLI clones gentian-deployments (or uses a local checkout),
edits tenants/gtn-demo.yaml (appends {profile: openproject} to spec.apps),
commits "feat(gtn-demo): install openproject",
pushes to gentian-deployments main branch
  ↓
ArgoCD detects the change and applies the updated Tenant CR
  ↓
Orchestrator reconciles:
  1. Fetch AppProfile "openproject"
  2. Check kernelRequirements (OIDC, PostgreSQL, S3, SMTP, LDAP, Memcached)
  3. Create OIDC client (Identity reconciler)
  4. Create database (Database reconciler)
  5. Create S3 bucket (Storage reconciler)
  6. Create LDAP bind account (LDAP reconciler)
  7. Create Memcached instance (Cache reconciler)
  8. Create ExternalSecrets (all credentials)
  9. Create ArgoCD Application CR (Pattern A) or Terraform CR (Pattern B)
  10. ArgoCD deploys the Helm chart
  ↓
App ready — SSO works, database provisioned, storage wired
```

**Runtime uninstall flow:**

```
Admin: "Remove OpenProject from tenant gtn-demo"
  ↓
kubectl gentian apps uninstall openproject --tenant gtn-demo
  ↓
CLI clones gentian-deployments (or uses a local checkout),
edits tenants/gtn-demo.yaml (removes {profile: openproject} from spec.apps),
commits "feat(gtn-demo): uninstall openproject",
pushes to gentian-deployments main branch
  ↓
ArgoCD detects the change and applies the updated Tenant CR
  ↓
Orchestrator reconciles:
  1. Delete ArgoCD Application CR → ArgoCD uninstalls Helm release
  2. Garbage-collect IntegrationBindings involving OpenProject
  3. Per deletionPolicy:
     - Delete: drop database, remove S3 bucket, delete OIDC client
     - Retain: revoke credentials, keep data intact
```

### Kernel-level services (not in the App Store)

These services are deployed cluster-wide by Layer 100–150 ApplicationSets. The orchestrator consumes their APIs via Jobs — it does not deploy them per-tenant via AppProfiles. They are **not** installable via the App Store.

| Service | Layer | Rationale |
|---|---|---|
| **Nubus** (Keycloak + UCS LDAP + Portal) | 110 — Identity | Identity provider, single trust anchor for all tenants |
| **Nextcloud** | 130 — Storage | Kernel filesystem service (WebDAV), provisioned via OCS API Jobs |
| **OX App Suite** | 130 — Groupware | Kernel groupware service — tightly coupled with mail kernel extension (SMTP/IMAP), Keycloak, and LDAP. Uses MariaDB, Redis, S3, LDAP bind accounts — all kernel-level shared resources. Deployed once, tenant-scoped via Keycloak realm + LDAP OU + dedicated database |
| **Intercom Service** | 150 — Notifications | Kernel notification gateway |
| **Postfix / Dovecot / Rspamd** | 100e — Mail | Kernel mail extension, shared infrastructure with tenant-scoped config |

**Why OX App Suite is kernel-level:** OX App Suite is the primary mail client, calendar, and contacts interface. It depends on every kernel function (identity, LDAP, MariaDB, Redis, S3, mail). Like Nextcloud, it is deployed once per cluster and serves all tenants through the kernel's isolation mechanisms (Keycloak realms, LDAP OUs, per-tenant databases, per-tenant S3 buckets). It follows the same deployment pattern as all other kernel services — Layer 100 ApplicationSet, Pattern B secret injection, tenant-scoped wiring via API Jobs. Making it tenant-installable would add complexity without a real use case (every openDesk tenant needs groupware).

### App Store increments

#### Inc 19 — App Store controller + catalogue API

**Goal:** Build the App Store controller that maintains a queryable catalogue of available apps, and implement runtime install/uninstall via Git commits to `gentian-deployments`.

**Deliverables:**
- `internal/controller/appstore_controller.go` — watches all AppProfile CRs, maintains `AppCatalogue` status (available apps, versions, requirements summary, installed-by-tenant counts)
- `api/v1alpha1/appcatalogue_types.go` — `AppCatalogue` singleton CR (cluster-scoped) with status listing all available apps
- Validation webhook: when a tenant adds an app to `spec.apps[]`, validate that the referenced AppProfile exists and that the tenant's quota allows another app (`spec.quotas.maxApps`)
- Pre-flight check: before provisioning, verify the cluster has the required kernel services (e.g., reject an app requiring MariaDB if no MariaDB kernel service exists)
- `kubectl gentian apps list` — plugin (or script) that reads the `AppCatalogue` and formats available apps as a table
- `kubectl gentian apps install <app> --tenant <tenant>` — plugin that edits the Tenant CR YAML in a local `gentian-deployments` checkout (or clones it), commits `feat(<tenant>): install <app>`, and pushes; ArgoCD reconciles from there
- `kubectl gentian apps uninstall <app> --tenant <tenant>` — same flow, removes the entry from `spec.apps` and commits `feat(<tenant>): uninstall <app>`
- The plugin requires `GENTIAN_DEPLOYMENTS_REPO` (URL) and `GENTIAN_DEPLOYMENTS_PATH` (local path) env vars, or reads them from a `~/.gentian/config.yaml`; no other repos are touched

**Test:**
- envtest: create 5 AppProfiles → AppCatalogue lists all 5 with correct metadata
- envtest: add app to tenant exceeding `maxApps` → rejected by webhook
- envtest: add app referencing non-existent AppProfile → rejected
- envtest: install + uninstall cycle → full provisioning + cleanup

---

### Migration checklist (applies to every app increment)

Each app increment (20–24) moves an app from `gentian-os/config/samples/` to `gentian-apps/profiles/`. Non-kernel apps must not leave any configuration in `gentian-os` after migration. The increment is complete when:

1. **`gentian-apps/profiles/<app>.yaml`** exists with `extraValues` aligned to `opendesk/helmfile/apps/<app>/values.yaml.gotmpl` defaults
2. **`gentian-os/config/samples/appprofile_<app>.yaml`** is deleted (`git rm`)
3. **`gentian-os/kernel/argocd/projects/gentian.yaml`** sourceRepos includes `gentian-apps` (done once in Inc 20)
4. **`gentian-deployments/dev/app-of-apps.yaml`** Source 3 syncs from `gentian-apps/profiles/` (done once in Inc 20)
5. ArgoCD re-syncs, duplicate warning for the app disappears, AppCatalogue shows the app
6. E2E tests (install → use → uninstall) pass

---

#### Inc 20 — Collabora → Nextcloud Office kernel service

**Goal:** Migrate Collabora from a per-tenant AppProfile to a shared kernel service
("Nextcloud Office"). Collabora is tightly coupled to Nextcloud — it has no OIDC
client, no database, no S3 bucket — and shares a single Nextcloud instance across all
tenants. Deploying a separate Collabora pod per tenant creates an ingress hostname
collision and a global `wopi_url` conflict in Nextcloud.

**Architecture change:**
- Removed `gentian-apps/profiles/collabora.yaml` (AppProfile).
- Added `gentian-os/kernel/services/collabora/` (shared kernel service).
- One Collabora pod in `gentian-dev`, single ingress `office.desk.gentian.org`.
- WOPI URL in Nextcloud management + startup hook set to `http://collabora.gentian-dev.svc.cluster.local:9980`.
- `TenantSpec.Office.Enabled` replaces `spec.apps: [{profile: collabora}]`.
- `ensureOffice` reconciler sets `OfficeReady` condition; no per-tenant provisioning needed.

##### Testing

```bash
# Verify the kernel Collabora pod is Running in the platform namespace
kubectl get pods -n gentian-dev -l app.kubernetes.io/name=collabora-online
# Expected: collabora-<hash> Running

# Verify single shared ingress at office.<domain>
kubectl get ingress -n gentian-dev collabora
# Expected: office.desk.gentian.org → collabora:9980

# Verify WOPI discovery endpoint returns XML
COLLABORA_POD=$(kubectl get pod -n gentian-dev \
  -l app.kubernetes.io/name=collabora-online -o name | head -1)
kubectl exec -n gentian-dev "$COLLABORA_POD" -- \
  curl -sf http://localhost:9980/hosting/discovery | head -5
# Expected: <wopi-discovery> XML

# Verify Nextcloud wopi_url points to kernel service
NC_POD=$(kubectl get pod -n gentian-dev -l app=nextcloud-aio -o name | head -1)
kubectl -n gentian-dev exec "$NC_POD" -- \
  php /var/www/html/occ config:app:get richdocuments wopi_url
# Expected: http://collabora.gentian-dev.svc.cluster.local:9980

# Verify Tenant office condition
kubectl get tenant gtn-demo -o jsonpath='{.status.conditions[?(@.type=="OfficeReady")]}'
# Expected: {"status":"True","reason":"Enabled",...}
```

**Browser test:** Open `https://files.desk.gentian.org`, log in, click **+** → **New document**,
choose `.odt`. Collabora Online editor should load inside Nextcloud.

##### Troubleshooting

- "Failed to load Nextcloud Office" → verify `wopi_url` points to the kernel service:
  `kubectl -n gentian-dev exec <nc-pod> -- php /var/www/html/occ config:app:get richdocuments wopi_url`
- "Unauthorized WOPI host" → `aliasgroups` in `collabora-base-values` ConfigMap must list `https://files.<domain>`.
- Collabora pod not starting → check `collabora-sensitive-values` Secret exists (ESO must have synced
  the admin password from OpenBao path `gentian-os/kernel/apps/collabora`).

---

#### Inc 21 — Element AppProfile (chat / Matrix)

**Goal:** Add Element (Matrix/Synapse) to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, SMTP
- `appSecrets`: `registration_shared_secret`, `intercom_as_token`, `ox_appsuite_as_token`
- `provides`: `chat` (matrix)
- `chart`: `opendesk-element` v6.1.9
- `deploymentMethod`: `crossplane` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/element/values.yaml.gotmpl`

**Actions:**
- Write `profiles/element.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_element.yaml` from `gentian-os` (`git rm`)
- Wire OIDC client, PostgreSQL database, SMTP credentials via `valueMapping`
- Configure Intercom Service app-service bridge (intercom ↔ Synapse)

##### Testing

**Create (Install):**
```bash
kubectl gentian apps install element --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=element -w
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep element
# Expected: element: 1
# Verify: App claim exists and is Ready
kubectl get app element -n tenant-gtn-demo
# Expected: READY=True
```

**Read (Verify — CLI):**
```bash
# 1. Check Synapse federation API
SYNAPSE_POD=$(kubectl get pod -n tenant-gtn-demo \
  -l app.kubernetes.io/component=synapse -o name | head -1)
kubectl exec -n tenant-gtn-demo "$SYNAPSE_POD" -- \
  curl -sf http://localhost:8008/_matrix/client/versions
# Expected: {"versions":["r0.0.1",..."v1.11"],...}

# 2. Check Element web UI is served
kubectl exec -n tenant-gtn-demo "$SYNAPSE_POD" -- \
  curl -sf -o /dev/null -w '%{http_code}' http://localhost:8008/
# Expected: 200 or 302

# 3. Check ingress
kubectl get ingress -n tenant-gtn-demo | grep element
# Expected: ingress to chat.<domain> (e.g. chat.desk.gentian.org)
```

**Read (Verify — Browser):**

1. Open **`https://chat.<domain>`** (e.g. `https://chat.desk.gentian.org`)
2. Click **"Sign in"** → redirects to Keycloak SSO
3. Log in with a valid user (e.g. `mightymouse`)
4. **Expected:** Element Web loads, showing the home screen with room list
5. Click **"+"** → **"New room"** → name it "Test Room" → **Create**
6. Type a message, press Enter
7. **Expected:** Message appears in the room with your display name and timestamp
8. Open a second browser / incognito window, log in as a different user
9. Have the second user join "Test Room" and send a reply
10. **Expected:** Both users see messages in real-time

**Update (Config Change):**
```bash
# Change replica count via Tenant CR config override
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"element","config":{"replicas":2}}]}}'
# Wait for reconciliation
sleep 30
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/component=synapse
# Expected: 2 pods running

# Revert to 1 replica
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"element","config":{"replicas":1}}]}}'
```

**Delete (Uninstall):**
```bash
kubectl gentian apps uninstall element --tenant gtn-demo
# Wait for reconciliation (orphan cleanup deletes the App claim)
sleep 30
# Verify: App claim removed
kubectl get app element -n tenant-gtn-demo
# Expected: Error from server (NotFound)
# Verify: no Element pods
kubectl get pods -n tenant-gtn-demo | grep element
# Expected: no results
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep element
# Expected: element: 0
```

##### Troubleshooting

- "Unable to connect to homeserver" → Synapse not reachable. Check ingress
  and `/.well-known/matrix/client` on the domain.
- SSO redirect fails → OIDC client not created in Keycloak. Check
  `kubectl get secret -n tenant-gtn-demo` for the Element OIDC client secret.
- "Registration is disabled" → expected; users come from OIDC, not self-registration.
- Uninstall: pod persists after removing from `spec.apps` → orchestrator not running the
  latest code with orphan cleanup. Rebuild and redeploy the operator image.

---

#### Inc 21a — Kernel secret seeder (OpenBao write path)

**Why this increment exists.** Incs 3–8 provision *backends* (Keycloak clients,
Postgres roles, MariaDB users, MinIO keys, Redis ACLs, LDAP bind accounts), but
the provisioning Jobs **never persist the generated credentials**. Pattern B
apps read their sensitive values from OpenBao at `gentian-os/tenants/{t}/apps/
{a}/{category}` — so the Terraform plan for every such app fails with `no
secret found at …`. This blocks Element install today and would block every
future app (Jitsi, XWiki, Odoo, Lexoffice …) the same way. Inc 21a closes the
gap once, for every reconciler, via a shared component.

**Principle.** Every reconciler derives the credentials it is about to write
to a backend, persists them in OpenBao **first**, then applies them
idempotently to the backend. After the Job completes, the live backend
password is equal to the OpenBao value by construction. No readback, no
drift, no race.

**Key design choices, aligned with opendesk / `server/`:**

1. **Deterministic derivation** via HKDF-SHA256 (RFC 5869). The operator
   reads the master password once at startup from
   `secret/gentian-os/kernel/internal/master-password` (via the
   `gentian-os-operator` Kubernetes auth role) and keeps it in memory.
   Per-credential values are then derived as:
   `Derive(salt, info) = hex(HKDF-SHA256(master, salt, info))[:40]`
   where `salt = CategoryPath(tenant, app, category)` for tenant-scoped
   secrets (e.g. `gentian-os/tenants/gtn-demo/apps/element/oidc`) and
   `salt = KernelPath(category, name)` for shared kernel services (e.g.
   `gentian-os/kernel/mail/postfix`), so shared services keep a single
   value across tenants while tenant-scoped ones are isolated by
   construction. `info` is the field tag (`password`, `client-secret`,
   `access-key`, …). Re-reconciling a healthy tenant yields the **same**
   value → `kv put` is a no-op → zero churn. When no master is configured
   the seeder transparently falls back to `crypto/rand` so development
   clusters still work.
2. **Write-once semantics** (`cas=0` KV v2 check-and-set) so a later
   `MASTER_PASSWORD` rotation cannot silently overwrite live credentials.
   Explicit rotation is a separate increment.
3. **Canonical paths** identical to what the existing app deployment reconciler
   already reads — no module rewrites needed for the kernel-requirement categories.
4. **One seeder, every kernel reconciler uses it** — DRY. Adding a new app
   (Odoo, etc.) requires zero orchestrator changes: declare
   `kernelRequirements` + `appSecrets` in the AppProfile and everything is
   wired automatically.
5. **Fully automated bootstrap.** `install.sh` / `scripts/seed-openbao.sh`
   write the master password to `secret/gentian-os/kernel/internal/master-password`.
   Nothing in the getting-started flow requires manual OpenBao calls.

**Deliverables:**

- `internal/kernel/secrets/` package:
  - `deriver.go` — `NewDeriver(master)` + `Derive(salt, info, n) string`
    (HKDF-SHA256, deterministic, up to 64-hex output; returns empty string
    when no master is configured so the seeder can fall back to random)
  - `openbao.go` — minimal KV v2 HTTP client: `PutOnce`, `Put`, `Exists`
  - `paths.go` — `CategoryPath(tenant, app, cat)`,
    `InternalPath(tenant, app, name)`, `KernelPath(cat, name)`, and the
    constant `MasterPasswordPath` for the operator bootstrap read
  - `seeder.go` — `Seeder` with one method per category:
    `SeedOIDC`, `SeedDatabase`, `SeedMariaDB`, `SeedS3`, `SeedCache`,
    `SeedSMTP`, `SeedIMAP`, `SeedLDAP`, `SeedAppSecrets`. Each returns the
    derived credential struct so the reconciler can pass it to its Job.
  - Unit tests for determinism and PutOnce idempotence.

- Reconciler rewiring — each existing reconciler now calls the seeder
  *before* creating its Job and passes the derived credential via env var
  from an ephemeral per-Job Secret:
  | Reconciler | Category | Shell-level idempotence |
  |---|---|---|
  | identity | `oidc` | Keycloak API `POST /clients {"secret": …}` + `PUT /clients/{id}/client-secret` |
  | ldap | `ldap` | UDM `create-or-modify` users/ldap-auth with `--set password=…` |
  | database (PG) | `database` | `CREATE ROLE IF NOT EXISTS` + `ALTER ROLE … PASSWORD '…'` |
  | mariadb | `database` (MariaDB flavour) | `CREATE USER IF NOT EXISTS` + `ALTER USER … IDENTIFIED BY '…'` |
  | storage (S3) | `s3` | `mc admin user add` (idempotent wrapper) |
  | cache (Redis) | `cache` | `ACL SETUSER … ON >…` |
  | mail | `smtp`, `imap` | copy kernel `gentian-os/kernel/mail/postfix` relay password into per-app path |
  | apps | `internal/{name}` | purely KV derivation + write (no backend Job) |

- Pattern B app secrets: the app reconciler passes the name→valuePath map to
  the Terraform CR as `app_secrets` (JSON-encoded), which injects each
  AppProfile `appSecrets[].valuePath` as a sensitive Helm value.

- Helm chart — orchestrator `Deployment` learns:
  - `BAO_ADDR` + `BAO_ROLE` env vars (configured via `openbao.address` /
    `openbao.role` in `values.yaml`)
  - Kubernetes auth against `auth/kubernetes/login` using the pod's
    projected ServiceAccount JWT — no static token anywhere
  - Master password is read at startup from
    `secret/gentian-os/kernel/internal/master-password` (populated by
    `scripts/seed-openbao.sh` during the documented bootstrap flow)
  - The `gentian-os-operator` Kubernetes auth role is created by the
    getting-started bootstrap — no new manual steps

**Test:**

- Unit tests: derive determinism, KV client PutOnce idempotence (mock HTTP).
- Envtest: existing reconciler tests adapted to verify that after
  `ensureIdentity`/`ensureDatabase`/etc., the in-memory OpenBao mock has
  the expected keys under the tenant path.
- Cluster smoke test (manual, run once): run the documented bootstrap
  (`seed-openbao.sh`), deploy the operator chart with `openbao.address` +
  `openbao.role` set, force-reconcile `tenant/gtn-demo`, observe Terraform
  plan for element succeed (no `no secret found` error), Synapse pod Ready,
  ingress reachable.

**Scalability.** Every future app (Odoo, Plane, Lexoffice, Metabase …)
declares its infra needs in an AppProfile and the orchestrator does the
right thing automatically. Zero code change per app.

**Status (current iteration):**

- ✅ `internal/kernel/secrets/` package implemented with `Deriver` (HKDF-SHA256,
  `HasMaster()` guard + crypto/rand fallback), `KVClient` (KV v2, `PutOnce`
  cas=0, Kubernetes-auth login against `auth/kubernetes/login`), canonical
  `CategoryPath` / `InternalPath` / `KernelPath` + `MasterPasswordPath`
  constant, and a `Seeder` covering every category listed above. Unit tests
  cover deterministic derivation, cross-tenant diversification, kernel-path
  tenant omission, and `PutOnce` idempotence.
- ✅ `TenantReconciler.Seeder` field (nil-safe) and `cmd/main.go`
  `buildSeeder()` that constructs the KV client from `BAO_ADDR` + `BAO_ROLE`
  (Kubernetes auth only — no static token needed) and reads the master
  password once at startup from `secret/gentian-os/kernel/internal/master-password`.
  When `BAO_ADDR` is unset the seeder is disabled (nil); when the master is
  missing the seeder runs with a crypto/rand fallback (non-deterministic).
- ✅ Identity reconciler — seeds OIDC, passes `OIDC_CLIENT_SECRET` to the
  Keycloak client Job; shell script creates with `"secret"` on POST and
  issues `PUT /clients/{id}` with the derived secret when the client already
  exists (idempotent upsert).
- ✅ Database reconciler — seeds `database`, passes `ROLE_PW`; `CREATE ROLE …
  PASSWORD` on new, `ALTER ROLE … WITH LOGIN PASSWORD` on existing.
- ✅ MariaDB reconciler — seeds `database` (MariaDB flavour) and passes
  `DB_PASS` to the setup Job; existing script already honours the env var.
- ✅ Cache reconciler — seeds `cache` and passes `REDIS_USER_PASSWORD` to
  the `redis-cli` Job; script uses `${REDIS_USER_PASSWORD:-$REDIS_PASSWORD}`
  so the ACL entry gets the derived password when seeded, admin password
  otherwise.
- ✅ LDAP reconciler — seeds `ldap` and passes `BIND_PW` to the UDM Job;
  script wrapped with `if [ -z "${BIND_PW:-}" ]` so the injected password
  wins over the local fallback.
- ✅ Storage reconciler — seeds `s3` with derived access/secret key before
  the bucket Job (bucket Job still runs with admin creds; provisioning a
  matching MinIO user is explicitly out of scope for this increment).
- ✅ Mail reconciler — `seedPerAppMailSecrets` copies the per-tenant SMTP
  credentials into each app's `…/apps/{app}/smtp` path and sets `imap` to
  the platform-kernel dovecot endpoint on the success path of all three
  mail modes.
- ✅ App reconciler — `seedAppSecrets` loop writes each
  `AppProfile.spec.appSecrets[]` entry to `…/internal/{name}` (key `value`)
  before `ensureTerraformCR`/`ensureAppApplication`.
- ✅ Helm chart — `values.yaml` gains `openbao.role` (default
  `gentian-os-operator`); `deployment.yaml` injects `BAO_ADDR` + `BAO_ROLE`
  env vars. No Secret, no static token — Kubernetes auth via the pod's
  projected ServiceAccount JWT.
- ✅ Bootstrap — `scripts/seed-openbao.sh` writes the platform master
  password to `secret/gentian-os/kernel/internal/master-password` (the
  canonical path read by the operator at startup), so a fresh cluster is
  fully bootstrapped by running the documented install flow with no extra
  manual OpenBao calls.
- ⏭️ Cluster smoke test — pending operator image rebuild + redeploy with
  the new chart values.

---

#### Inc 22 — Jitsi AppProfile (video conferencing)

**Goal:** Add Jitsi Meet to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC (JWT/hybrid-matrix-token scheme)
- `appSecrets`: `jwt_app_secret`, `jicofo_auth_password`, `jicofo_component_secret`, `jvb_auth_password`
- `provides`: `videoconference` (webrtc)
- `chart`: `opendesk-jitsi` v3.5.1
- `deploymentMethod`: `crossplane` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/jitsi/values.yaml.gotmpl`

**Actions:**
- Write `profiles/jitsi.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_jitsi.yaml` from `gentian-os` (`git rm`)
- Wire JWT app secret via Keycloak hybrid-matrix-token scheme
- Configure optional Element integration (Jitsi links in chat rooms)

##### Testing

**Create (Install):**
```bash
kubectl gentian apps install jitsi --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=jitsi -w
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep jitsi
# Expected: jitsi: 1
# Verify: App claim exists and is Ready
kubectl get app jitsi -n tenant-gtn-demo
# Expected: READY=True
```

**Read (Verify — CLI):**
```bash
# 1. Check Jitsi web is serving
JITSI_WEB=$(kubectl get pod -n tenant-gtn-demo \
  -l app.kubernetes.io/component=jitsi-web -o name | head -1)
kubectl exec -n tenant-gtn-demo "$JITSI_WEB" -- \
  curl -sf -o /dev/null -w '%{http_code}' http://localhost:80/
# Expected: 200

# 2. Check Orosody (XMPP) is connected
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/component=orosody
# Expected: 1/1 Running

# 3. Check JVB (video bridge) is running
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/component=jvb
# Expected: 1/1 Running

# 4. Check ingress
kubectl get ingress -n tenant-gtn-demo | grep jitsi
# Expected: ingress to meet.<domain> (e.g. meet.desk.gentian.org)
```

**Read (Verify — Browser):**

1. Open **`https://meet.<domain>`** (e.g. `https://meet.desk.gentian.org`)
2. **Expected:** Jitsi Meet landing page loads
3. If SSO is configured: click **"Sign in"** → Keycloak SSO redirect
4. Enter a room name (e.g. "test-room") and click **"Start meeting"** / **"Go"**
5. **Expected:** You enter a video conference room. Browser asks for camera/mic permission.
6. Grant permissions → your video feed appears
7. Open a second browser/tab and join the same room name
8. **Expected:** Both participants see each other's video/audio streams
9. Test screen sharing: click the **share screen** button → select a screen
10. **Expected:** The other participant sees your shared screen

**Update (Config Change):**
```bash
# Change replica count via Tenant CR config override
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"jitsi","config":{"replicas":2}}]}}'
# Wait for reconciliation
sleep 30
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/component=jitsi-web
# Expected: 2 pods running

# Revert to 1 replica
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"jitsi","config":{"replicas":1}}]}}'
```

**Delete (Uninstall):**
```bash
kubectl gentian apps uninstall jitsi --tenant gtn-demo
# Wait for reconciliation (orphan cleanup deletes the App claim)
sleep 30
# Verify: App claim removed
kubectl get app jitsi -n tenant-gtn-demo
# Expected: Error from server (NotFound)
# Verify: no Jitsi pods
kubectl get pods -n tenant-gtn-demo | grep jitsi
# Expected: no results
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep jitsi
# Expected: jitsi: 0
```

##### Troubleshooting

- "Meeting not started" / stuck on loading → JVB pod may not be running or OROP is down.
  Check `kubectl logs -n tenant-gtn-demo <jvb-pod>` for OROP connection errors.
- No audio/video → browser permissions denied, or OROP→JVB UDP port not reachable.
  On single-node setups, usually works with `hostPort`. Check JVB `OROP_UDP_PORT` env var.
- SSO not working → JWT secret mismatch. Check `appSecrets` mapping for `jwt_app_secret`.
- Uninstall: pod persists after removing from `spec.apps` → orchestrator not running the
  latest code with orphan cleanup. Rebuild and redeploy the operator image.

---

#### Inc 23 — OpenProject AppProfile (project management)

**Goal:** Add OpenProject to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, S3, SMTP, LDAP
- `appSecrets`: `admin_password`, `api_admin_password`
- `provides`: `project-management` (http-json)
- `optionalIntegrations`: `file-store` (Nextcloud), `central-navigation` (Portal)
- `chart`: `openproject` v10.1.0
- `deploymentMethod`: `crossplane` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/openproject/values.yaml.gotmpl`

**Actions:**
- Write `profiles/openproject.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_openproject.yaml` from `gentian-os` (`git rm`)
- Wire all 6 kernel requirements via `valueMapping`
- Configure Nextcloud file-store IntegrationBinding (WebDAV read/write)
- Configure Portal central-navigation IntegrationBinding

**Cleanup (carried over from earlier prototyping):**

The kernel-level Keycloak workspace (`kernel/tofu/tenant/keycloak-config/`) historically
contained a hardcoded `module "openproject"` block that read its client secret from
`gentian-os/tenants/gtn-demo/apps/openproject/oidc` — a per-tenant OpenBao path. That
block was removed (commit removing the `data "vault_kv_secret_v2" "openproject"`,
the matching `import {}` and the `module "openproject" {}`); the openproject Keycloak
client is now provisioned per-tenant by the orchestrator's Identity reconciler when
`openproject` appears in a Tenant's `spec.apps`. The shared protocol-mapper SCOPE
(`keycloak_openid_client_scope.openproject` plus its three mappers) stays at kernel
level since it is a realm-wide template, not per-tenant. As part of this increment,
ensure the Identity reconciler creates the `opendesk-openproject` client with the
same redirect URIs, backchannel-logout settings, and default scopes that the removed
module used (see git history for `kernel/services/keycloak-config/`).

##### Testing

**Create (Install):**
```bash
kubectl gentian apps install openproject --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=openproject -w
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep openproject
# Expected: openproject: 1
# Verify: App claim exists and is Ready
kubectl get app openproject -n tenant-gtn-demo
# Expected: READY=True
```

**Read (Verify — CLI):**
```bash
# 1. Check OpenProject API responds
OP_POD=$(kubectl get pod -n tenant-gtn-demo \
  -l app.kubernetes.io/name=openproject -o name | head -1)
kubectl exec -n tenant-gtn-demo "$OP_POD" -- \
  curl -sf http://localhost:8080/api/v3 | python3 -m json.tool | head -10
# Expected: JSON with {"_type":"Root", ...}

# 2. Check worker pod is running (background jobs)
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/component=worker
# Expected: 1/1 Running

# 3. Check ingress
kubectl get ingress -n tenant-gtn-demo | grep openproject
# Expected: ingress to projects.<domain> (e.g. projects.desk.gentian.org)

# 4. Check OIDC provider is configured
kubectl exec -n tenant-gtn-demo "$OP_POD" -- \
  curl -sf http://localhost:8080/api/v3/configuration | python3 -c \
  "import sys,json; c=json.load(sys.stdin); print('SSO:', 'oidc' in str(c).lower())"
# Expected: SSO: True
```

**Read (Verify — Browser):**

1. Open **`https://projects.<domain>`** (e.g. `https://projects.desk.gentian.org`)
2. Click **"Sign in"** → redirects to Keycloak SSO
3. Log in with a valid user (e.g. `mightymouse`)
4. **Expected:** OpenProject dashboard loads, showing the home page
5. Click **"+ Project"** → enter name "E2E Test Project" → **Save**
6. **Expected:** Project is created, you see the project overview page
7. Go to **Work packages** → **+ Create** → type "Test task" → **Save**
8. **Expected:** Work package appears in the list with #ID, status "New"
9. Go to **Files** → upload a small test file (e.g. a .txt or .pdf)
10. **Expected:** File appears in the file list (stored via S3)
11. Check the **Members** tab → **+ Member** → search for an LDAP user
12. **Expected:** LDAP users appear in the autocomplete (proves LDAP integration)

**Update (Config Change):**
```bash
# Change replica count via Tenant CR config override
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"openproject","config":{"replicas":2}}]}}'
# Wait for reconciliation
sleep 30
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=openproject
# Expected: 2 pods running

# Revert to 1 replica
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"openproject","config":{"replicas":1}}]}}'
```

**Delete (Uninstall):**
```bash
kubectl gentian apps uninstall openproject --tenant gtn-demo
# Wait for reconciliation (orphan cleanup deletes the App claim)
sleep 30
# Verify: App claim removed
kubectl get app openproject -n tenant-gtn-demo
# Expected: Error from server (NotFound)
# Verify: no OpenProject pods
kubectl get pods -n tenant-gtn-demo | grep openproject
# Expected: no results
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep openproject
# Expected: openproject: 0
# Verify: database retained (deletionPolicy: Retain), OIDC client revoked
```

##### Troubleshooting

- "502 Bad Gateway" → OpenProject Puma worker still starting. Wait 60s,
  check `kubectl logs -n tenant-gtn-demo <openproject-web-pod>`.
- SSO redirect loop → OIDC client not configured or redirect URI mismatch.
  Check Keycloak admin → Clients → `openproject` client → Valid Redirect URIs.
- "File upload failed" → S3 bucket doesn't exist or credentials wrong.
  Check `kubectl logs <openproject-web-pod>` for S3 errors.
- No LDAP users found → LDAP connection not configured. Check OpenProject
  admin → Authentication → LDAP connections.
- Uninstall: pod persists after removing from `spec.apps` → orchestrator not running the
  latest code with orphan cleanup. Rebuild and redeploy the operator image.

---

#### Inc 24 — XWiki AppProfile (wiki / knowledge management)

**Goal:** Add XWiki to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, SMTP, LDAP
- `provides`: `wiki` (http-json)
- `optionalIntegrations`: `central-navigation` (Portal)
- `chart`: `xwiki` v1.4.4
- `deploymentMethod`: `crossplane` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/xwiki/values.yaml.gotmpl`

**Actions:**
- Write `profiles/xwiki.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_xwiki.yaml` from `gentian-os` (`git rm`)
- Wire OIDC, PostgreSQL, SMTP, LDAP via `valueMapping`
- Handle dot-escaped YAML key paths (`customConfigs.xwiki\.properties.oidc\.secret`)

##### Testing

**Create (Install):**
```bash
kubectl gentian apps install xwiki --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=xwiki -w
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep xwiki
# Expected: xwiki: 1
# Verify: App claim exists and is Ready
kubectl get app xwiki -n tenant-gtn-demo
# Expected: READY=True
```

**Read (Verify — CLI):**
```bash
# 1. Check XWiki REST API
XWIKI_POD=$(kubectl get pod -n tenant-gtn-demo \
  -l app.kubernetes.io/name=xwiki -o name | head -1)
kubectl exec -n tenant-gtn-demo "$XWIKI_POD" -- \
  curl -sf http://localhost:8080/rest -H "Accept: application/json" | head -20
# Expected: JSON with XWiki REST API resources

# 2. Check ingress
kubectl get ingress -n tenant-gtn-demo | grep xwiki
# Expected: ingress to wiki.<domain> or xwiki.<domain>

# 3. Check database connectivity
kubectl exec -n tenant-gtn-demo "$XWIKI_POD" -- \
  curl -sf http://localhost:8080/rest/wikis -H "Accept: application/json" \
  | python3 -c "import sys,json; w=json.load(sys.stdin); print('Wikis:', len(w.get('wikis',{}).get('wikiSummaries',[])))"
# Expected: Wikis: 1 (at least — the main wiki)
```

**Read (Verify — Browser):**

1. Open **`https://wiki.<domain>`** (or the XWiki ingress host from the CLI check above)
2. Click **"Log in"** → redirects to Keycloak SSO
3. Log in with a valid user (e.g. `mightymouse`)
4. **Expected:** XWiki home page loads with the wiki dashboard
5. Click **"Add" → "Page"** (or the **"+"** button in the page tree)
6. Enter title "E2E Test Page", add some content with formatting (bold, headings, a list)
7. Click **"Save & View"**
8. **Expected:** The rendered page appears with your formatting intact
9. Click **"Edit"** again, make a change, save
10. Click **"History"** tab on the page
11. **Expected:** Two revisions are listed with diffs available
12. Log in as a different user → navigate to the same page
13. **Expected:** The page is visible (access control works, LDAP-synced users can read)

**Update (Config Change):**
```bash
# Change replica count via Tenant CR config override
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"xwiki","config":{"replicas":2}}]}}'
# Wait for reconciliation
sleep 30
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=xwiki
# Expected: 2 pods running

# Revert to 1 replica
kubectl patch tenant gtn-demo --type=merge \
  -p '{"spec":{"apps":[{"profile":"xwiki","config":{"replicas":1}}]}}'
```

**Delete (Uninstall):**
```bash
kubectl gentian apps uninstall xwiki --tenant gtn-demo
# Wait for reconciliation (orphan cleanup deletes the App claim)
sleep 30
# Verify: App claim removed
kubectl get app xwiki -n tenant-gtn-demo
# Expected: Error from server (NotFound)
# Verify: no XWiki pods
kubectl get pods -n tenant-gtn-demo | grep xwiki
# Expected: no results
kubectl get appcatalogue default \
  -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep xwiki
# Expected: xwiki: 0
```

##### Troubleshooting

- XWiki stuck on "Loading..." → Java startup can take 2-3 minutes. Check
  `kubectl logs -n tenant-gtn-demo <xwiki-pod>` for "Server started" message.
- SSO redirect fails → OIDC configuration in `xwiki.properties` is wrong.
  Verify `oidc.endpoint.authorization`, `oidc.clientid`, and `oidc.secret` values.
- "Database not available" → PostgreSQL credentials wrong or database not created.
  Check `hibernate.cfg.xml` connection string in the XWiki pod.
- User login works but no permissions → LDAP group sync not configured.
- Uninstall: pod persists after removing from `spec.apps` → orchestrator not running the
  latest code with orphan cleanup. Rebuild and redeploy the operator image.

---

#### Inc 25 — CryptPad AppProfile (collaborative document editing)

**Goal:** Create the `gentian-apps` AppProfile for CryptPad, the end-to-end encrypted real-time collaborative editor.

**Kernel requirements:** Ingress only — CryptPad has no SSO/OIDC, no database, no S3, no LDAP, no SMTP. User accounts are self-contained within the app (encrypted on the client side).

**Actions:**
- Create `gentian-apps/profiles/cryptpad.yaml`:
  - Chart: `cryptpad v0.0.21` from `oci://registry.opencode.de/bmi/opendesk/components/supplier/xwiki/charts-mirror`
  - Image: `registry.opencode.de/bmi/opendesk/components/supplier/xwiki/images-mirror/cryptpad:version-2025.9.0`
  - `spec.ingress.subDomain: "pad"`, `servicePort: 3000`
  - `restrictRegistration: true` — closed registration (no public sign-up)
  - `enableEmbedding: true` — allows embedding in the portal
  - `fullnameOverride: "cryptpad"` for predictable Service name
  - `persistence.enabled: false` (client-side encryption; server files are opaque blobs)
  - Ingress annotation: `nginx.org/websocket-services: "cryptpad"` (required for real-time sync)
  - CSP `frame-ancestors 'self'` annotation for portal embedding
  - `reloader.stakater.com/auto: "true"` in podAnnotations
  - Chart-managed ingress disabled (`ingress.enabled: false` in extraValues)
  - No `valueMapping` entries (no kernel-provisioned secrets needed)
- Delete `gentian-os/config/samples/appprofile_cryptpad.yaml` if present

**Test:** `kubectl apply --dry-run=server -f profiles/cryptpad.yaml` passes. CryptPad pod starts; registration page is inaccessible (`restrictRegistration: true`).

**Troubleshooting:**
- Real-time sync broken → WebSocket not reaching pod. Verify `nginx.org/websocket-services` annotation and that the Ingress controller supports it.
- Embedding fails in portal → Check CSP `frame-ancestors` annotation on the Ingress.
- Pod CrashLoopBackOff → Check `fsGroup: 4001` / `runAsUser: 4001` in `podSecurityContext`.

---

#### Inc 26 — Contract definitions + App Store CI

**Goal:** Define the contract schemas that apps reference and set up CI for the `gentian-apps` repo.

**Actions:**
- Write contract YAML files in `gentian-apps/contracts/`:
  - `file-store.yaml` — WebDAV read/write (provider: Nextcloud kernel service)
  - `filepicker.yaml` — file selection UI (provider: Nextcloud kernel service)
  - `central-navigation.yaml` — Portal link registration (provider: Univention Portal)
  - `project-management.yaml` — task/timeline API (provider: OpenProject)
  - `chat.yaml` — messaging API (provider: Element)
  - `office-editor.yaml` — WOPI document editing (provider: Collabora)
  - `videoconference.yaml` — WebRTC conferencing (provider: Jitsi)
- CI pipeline: `kubeconform` against AppProfile CRD schema for all profiles
- `validate-profiles.sh` — runs `kubectl apply --dry-run=server` against a test cluster
- After this increment, `gentian-os/config/samples/` should contain only non-app CRs (Tenant, IntegrationBinding examples)

**Test:** CI green, all profiles and contracts valid.

---

### App Store summary

| Inc | Title | Key deliverable | Effort |
|---|---|---|---|
| 19 | App Store controller + catalogue API | `AppCatalogue` CR, validation webhook, `kubectl gentian` plugin, runtime install/uninstall | Large |
| 20 | Collabora → Nextcloud Office kernel service | Shared `kernel/services/collabora/`. `Tenant.spec.office.enabled` flag. Removed per-tenant AppProfile. | Small |
| 21 | Element AppProfile | `gentian-apps/profiles/element.yaml`, OIDC+PG+SMTP wiring. Remove from `gentian-os`. | Medium |
| 22 | Jitsi AppProfile | `gentian-apps/profiles/jitsi.yaml`, JWT/OIDC wiring. Remove from `gentian-os`. | Small |
| 23 | OpenProject AppProfile | `gentian-apps/profiles/openproject.yaml`, 6 kernel reqs + Nextcloud integration. Remove from `gentian-os`. | Medium |
| 24 | XWiki AppProfile | `gentian-apps/profiles/xwiki.yaml`, OIDC+PG+SMTP+LDAP wiring. Remove from `gentian-os`. | Medium |
| 25 | CryptPad AppProfile | `gentian-apps/profiles/cryptpad.yaml`, ingress-only (no SSO/DB/LDAP/SMTP). | Small |
| 26 | Contract definitions + CI | `gentian-apps` repo CI, contract schemas, profile validation | Small |

Inc 19 (App Store controller) is the foundation — it must be built first. Incs 20–25 (individual app profiles) can be built in parallel after Inc 19. Inc 26 (contracts + CI) can start anytime after Inc 1 (CRDs exist). Each app profile is an independent PR in the `gentian-apps` repo. After Inc 25, `gentian-os/config/samples/` should contain only `integrationbinding_filepicker.yaml` and `tenant_gtn-demo.yaml` — no AppProfile YAMLs.

---

## Optional Future Features

These features are architecturally sound but not required for an MVP. They can be implemented when there is demand.

| Feature | Description | Depends on | Effort |
|---|---|---|---|
| MCP Discovery Layer | MCP server registry complementing IntegrationBindings (architecture §10.1–10.5) | Inc 11 (IntegrationBinding) | Large |
| Tenant-scoped backup & restore | `BackupTenant` / `RestoreTenant` CRs for per-tenant pgBackRest + Velero + OpenBao snapshots (architecture §9) | Backup strategy design | Large |
| Cross-cluster tenant migration | Move a tenant between clusters (architecture §9.3) | Backup & restore | Large |
| AI-assisted operations | AppProfile generation, health monitoring via LLM (architecture §10.5) | Inc 14 (AppProfiles) + observability | Medium |
| Tenant self-service via bot commits | Tenant admins use CLI/WebUI only; a platform bot validates tenant scope and commits approved changes to `gentian-deployments` in the background (instead of direct human Git edits). This preserves GitOps auditability while improving tenant UX and reducing accidental cross-tenant edits. | Inc 19 (App Store), repo policy checks, service account/bot credentials | Medium |
| PR-based kernel image updates | **Current approach (Inc 21a+):** ArgoCD Image Updater directly patches Application CRs with new digests (fast, no Git commits). **Future enhancement:** Add workflow option to open a pull request in `gentian-deployments` instead of direct patching — preserves full Git audit trail and enables code review gates for production kernel updates. Implementation: Reuse existing Image Updater webhook infrastructure but route to CI job that creates PR via `github.com/actions/github-script` or `gh pr create`. Gating: Branch protection rules enforce approvals before merge. | Architecture §15 (Image Updater) | Small |

