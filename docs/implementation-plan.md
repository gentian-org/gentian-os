# Gentian OS — Implementation Plan

**Date:** 2026-04-05
**Scope:** Build Gentian OS to match the architecture document — CRDs, thin orchestrator, kernel services, and deployment pipeline.
**Principle:** Every increment produces a working, testable artifact. The thin orchestrator is the backbone — most other work flows from it.

---

## Strategy

CRDs first, then orchestrator reconcilers one kernel function at a time, then tenant lifecycle, then apps. The `server/` repo is reference material — we cherry-pick proven patterns (HMAC-SHA256 derivation, Pattern A/B, Reloader, ApplicationSet patterns, Tofu modules) and rewrite what doesn't fit.

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
| 9 | App deployment reconciler | ✅ Done | ArgoCD Application CRs per app per tenant. Pattern A + B routing. `valueMapping` rendering. 5 envtest tests. 56 total |
| 10 | Ingress reconciler | ✅ Done | Per-app Ingress CRs + cert-manager wildcard Certificate CR. 5 envtest tests. 61 total |
| 11 | IntegrationBinding reconciler | ✅ Done | Auto-generates bindings when provider + consumer both in tenant app list. 4 envtest tests. 65 total |
| 12 | OpenBao restructuring | ✅ Done | `gentian-os/kernel/` and `gentian-os/tenants/{name}/apps/{app}/` path hierarchy. 65 total |
| 13 | Helm chart + observability | ✅ Done | `charts/gentian-os/` with CRDs, Deployment, RBAC, ServiceMonitor, Grafana dashboard. Prometheus metrics. Printer columns. 65 total |
| 14 | AppProfiles + update reconciler | ✅ Done | 6 AppProfile YAMLs (collabora, element, jitsi, openproject, xwiki, ox-appsuite). All Pattern B (Tofu Controller). 65 total |
| 15 | Deployment repo (gentian-deployments) | ✅ Done | `gentian-deployments/dev/` — bootstrap, app-of-apps, dev-tenant, values, tofu.tfvars. 65 total |
| 16 | Mail kernel extension | ✅ Done | Shared Postfix/Dovecot via kernel ConfigMaps. 4 modes: selfhosted, external, transport-only, disabled. 7 envtest tests. 76 total |
| 17 | Isolation hardening tests | ✅ Done | Cross-tenant NetworkPolicy, ingress/egress rules, ResourceQuota, LimitRange, end-to-end Delete + Retain. 7 envtest tests. 93 total |
| 18 | Single-line domain config | ✅ Done | `variable "domain"` in Tofu. 41 `_base.yaml` → `${domain}` template. 13 HCL → `var.domain`. `file()` → `templatefile()`. 93 total |
| 19 | App Store controller + catalogue API | ✅ Done | `AppCatalogue` singleton CR + `AppStoreReconciler`. `TenantValidator` webhook (maxApps quota + AppProfile existence). `kubectl-gentian` plugin (list/install/uninstall via Git commit to `gentian-deployments`). 6 envtest tests. 99 total |
| 20 | Collabora AppProfile in gentian-apps | ✅ Done | `profiles/collabora.yaml` in `gentian-apps`. Removed from `gentian-os/config/samples/`. ArgoCD Source 3 + AppProject sourceRepos. `extraValues` aligned with opendesk defaults. 99 total |
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

Triggered via annotation: `kubectl annotate tenant gtn-demo gentianos.io/rotate-credentials=all`

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
| §2.1 Kernel Functions — Package manager | AppProfile CRD + orchestrator pipeline | 1, 9, 14 | CRD + reconciler + profiles. App Store: Inc 19 |
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
| §2.4 vCluster isolation | vCluster-per-tenant mode | Optional; namespace mode is default | Optional Future Features |
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

## Optional Future Features

These features are architecturally sound but not required for an MVP. They can be implemented when there is demand.

| Feature | Description | Depends on | Effort |
|---|---|---|---|
| vCluster-per-tenant isolation | Run each tenant in a dedicated vCluster for full API-server-level isolation (`isolation.mode: vcluster`). Namespace mode is the default and sufficient for most deployments. | Inc 2 (namespace reconciler), vCluster operator | Large |
| MCP Discovery Layer | MCP server registry complementing IntegrationBindings (architecture §10.1–10.5) | Inc 11 (IntegrationBinding) | Large |
| Tenant-scoped backup & restore | `BackupTenant` / `RestoreTenant` CRs for per-tenant pgBackRest + Velero + OpenBao snapshots (architecture §9) | Backup strategy design | Large |
| Cross-cluster tenant migration | Move a tenant between clusters (architecture §9.3) | Backup & restore | Large |
| AI-assisted operations | AppProfile generation, health monitoring via LLM (architecture §10.5) | Inc 14 (AppProfiles) + observability | Medium |

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
  9. Create ArgoCD Application CR (or Tofu CR for Pattern B)
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

**Why OX App Suite is kernel-level:** OX App Suite is the primary mail client, calendar, and contacts interface. It depends on every kernel function (identity, LDAP, MariaDB, Redis, S3, mail). Like Nextcloud, it is deployed once per cluster and serves all tenants through the kernel's isolation mechanisms (Keycloak realms, LDAP OUs, per-tenant databases, per-tenant S3 buckets). It follows the same deployment pattern as all other kernel services — Layer 100 ApplicationSet, Tofu Controller for secret injection (Pattern B), tenant-scoped wiring via API Jobs. Making it tenant-installable would add complexity without a real use case (every openDesk tenant needs groupware).

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

#### Inc 20 — Collabora AppProfile (document editing)

**Goal:** First app in the store — Collabora Online (WOPI document editor). Establish the `gentian-apps` repo structure and ArgoCD wiring.

**AppProfile:**
- `kernelRequirements`: none (Collabora authenticates through Nextcloud, not directly)
- `appSecrets`: `admin_password` → `collabora.password`
- `provides`: `office-editor` (wopi)
- `chart`: `collabora-online` v1.1.45
- `deploymentMethod`: `tofu-controller` (Pattern B)
- `extraValues`: `autoscaling.enabled: false`, `replicaCount: 1`, security context, `fullnameOverride: collabora` — aligned with opendesk defaults

**Actions:**
- Write `profiles/collabora.yaml` in `gentian-apps` with `extraValues` matching opendesk `values.yaml.gotmpl`
- Delete `config/samples/appprofile_collabora.yaml` from `gentian-os` (`git rm`)
- Add `https://github.com/gentian-org/gentian-apps` to ArgoCD AppProject `sourceRepos`
- Add Source 3 (`gentian-apps/profiles/`) to `app-of-apps.yaml`
- Configure Nextcloud kernel service to discover and use the Collabora WOPI endpoint (via IntegrationBinding or kernel config)

**E2E test — Install:**
```bash
# Install Collabora for tenant gtn-demo
kubectl gentian apps install collabora --tenant gtn-demo
# Wait for ArgoCD sync + Tofu apply
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=collabora -w
# Verify: pod Running, no HPA (autoscaling disabled), single replica
kubectl get hpa -n tenant-gtn-demo  # should show no collabora HPA
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep collabora
# Expected: collabora: 1
```

**E2E test — Use:**
```bash
# Verify Collabora health endpoint responds
COLLABORA_POD=$(kubectl get pod -n tenant-gtn-demo -l app.kubernetes.io/name=collabora -o name | head -1)
kubectl exec -n tenant-gtn-demo "$COLLABORA_POD" -- curl -sf http://localhost:9980/hosting/discovery | head -5
# Expected: WOPI discovery XML response
# Functional test: open Nextcloud → create/open a .docx or .odt file → Collabora editor loads in browser
```

**E2E test — Uninstall:**
```bash
# Remove Collabora from tenant
kubectl gentian apps uninstall collabora --tenant gtn-demo
# Wait for ArgoCD sync
sleep 30
# Verify: no Collabora pods, catalogue shows 0 installs
kubectl get pods -n tenant-gtn-demo | grep collabora  # Expected: no results
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep collabora
# Expected: collabora: 0
```

---

#### Inc 21 — Element AppProfile (chat / Matrix)

**Goal:** Add Element (Matrix/Synapse) to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, SMTP
- `appSecrets`: `registration_shared_secret`, `intercom_as_token`, `ox_appsuite_as_token`
- `provides`: `chat` (matrix)
- `chart`: `opendesk-element` v6.1.9
- `deploymentMethod`: `tofu-controller` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/element/values.yaml.gotmpl`

**Actions:**
- Write `profiles/element.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_element.yaml` from `gentian-os` (`git rm`)
- Wire OIDC client, PostgreSQL database, SMTP credentials via `valueMapping`
- Configure Intercom Service app-service bridge (intercom ↔ Synapse)

**E2E test — Install:**
```bash
kubectl gentian apps install element --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=element -w
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep element
# Expected: element: 1
```

**E2E test — Use:**
```bash
# Verify Synapse health
SYNAPSE_POD=$(kubectl get pod -n tenant-gtn-demo -l app.kubernetes.io/component=synapse -o name | head -1)
kubectl exec -n tenant-gtn-demo "$SYNAPSE_POD" -- curl -sf http://localhost:8008/_matrix/client/versions
# Expected: JSON with supported Matrix versions
# Functional test: open Element web UI → SSO login via Keycloak → send a message → message appears
```

**E2E test — Uninstall:**
```bash
kubectl gentian apps uninstall element --tenant gtn-demo
sleep 30
kubectl get pods -n tenant-gtn-demo | grep element  # Expected: no results
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep element
# Expected: element: 0
```

---

#### Inc 22 — Jitsi AppProfile (video conferencing)

**Goal:** Add Jitsi Meet to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC (JWT/hybrid-matrix-token scheme)
- `appSecrets`: `jwt_app_secret`, `jicofo_auth_password`, `jicofo_component_secret`, `jvb_auth_password`
- `provides`: `videoconference` (webrtc)
- `chart`: `opendesk-jitsi` v3.5.1
- `deploymentMethod`: `tofu-controller` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/jitsi/values.yaml.gotmpl`

**Actions:**
- Write `profiles/jitsi.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_jitsi.yaml` from `gentian-os` (`git rm`)
- Wire JWT app secret via Keycloak hybrid-matrix-token scheme
- Configure optional Element integration (Jitsi links in chat rooms)

**E2E test — Install:**
```bash
kubectl gentian apps install jitsi --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=jitsi -w
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep jitsi
# Expected: jitsi: 1
```

**E2E test — Use:**
```bash
# Verify Jitsi web health
JITSI_POD=$(kubectl get pod -n tenant-gtn-demo -l app.kubernetes.io/component=jitsi-web -o name | head -1)
kubectl exec -n tenant-gtn-demo "$JITSI_POD" -- curl -sf http://localhost:80/ | head -5
# Expected: HTML landing page
# Functional test: open Jitsi URL → SSO login → create video conference room → audio/video works
```

**E2E test — Uninstall:**
```bash
kubectl gentian apps uninstall jitsi --tenant gtn-demo
sleep 30
kubectl get pods -n tenant-gtn-demo | grep jitsi  # Expected: no results
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep jitsi
# Expected: jitsi: 0
```

---

#### Inc 23 — OpenProject AppProfile (project management)

**Goal:** Add OpenProject to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, S3, SMTP, LDAP
- `appSecrets`: `admin_password`, `api_admin_password`
- `provides`: `project-management` (http-json)
- `optionalIntegrations`: `file-store` (Nextcloud), `central-navigation` (Portal)
- `chart`: `openproject` v10.1.0
- `deploymentMethod`: `tofu-controller` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/openproject/values.yaml.gotmpl`

**Actions:**
- Write `profiles/openproject.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_openproject.yaml` from `gentian-os` (`git rm`)
- Wire all 6 kernel requirements via `valueMapping`
- Configure Nextcloud file-store IntegrationBinding (WebDAV read/write)
- Configure Portal central-navigation IntegrationBinding

**E2E test — Install:**
```bash
kubectl gentian apps install openproject --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=openproject -w
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep openproject
# Expected: openproject: 1
```

**E2E test — Use:**
```bash
# Verify OpenProject health
OP_POD=$(kubectl get pod -n tenant-gtn-demo -l app.kubernetes.io/name=openproject -o name | head -1)
kubectl exec -n tenant-gtn-demo "$OP_POD" -- curl -sf http://localhost:8080/api/v3
# Expected: JSON API root response
# Functional test: open OpenProject URL → SSO login via Keycloak → LDAP users visible →
#   create project → attach file (stored via S3) → project appears in Portal navigation
```

**E2E test — Uninstall:**
```bash
kubectl gentian apps uninstall openproject --tenant gtn-demo
sleep 30
kubectl get pods -n tenant-gtn-demo | grep openproject  # Expected: no results
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep openproject
# Expected: openproject: 0
# Verify: database retained (deletionPolicy: Retain), OIDC client revoked
```

---

#### Inc 24 — XWiki AppProfile (wiki / knowledge management)

**Goal:** Add XWiki to the app store. Remove from `gentian-os`.

**AppProfile:**
- `kernelRequirements`: OIDC, PostgreSQL, SMTP, LDAP
- `provides`: `wiki` (http-json)
- `optionalIntegrations`: `central-navigation` (Portal)
- `chart`: `xwiki` v1.4.4
- `deploymentMethod`: `tofu-controller` (Pattern B)
- `extraValues`: aligned with `opendesk/helmfile/apps/xwiki/values.yaml.gotmpl`

**Actions:**
- Write `profiles/xwiki.yaml` in `gentian-apps` with `extraValues` from opendesk
- Delete `config/samples/appprofile_xwiki.yaml` from `gentian-os` (`git rm`)
- Wire OIDC, PostgreSQL, SMTP, LDAP via `valueMapping`
- Handle dot-escaped YAML key paths (`customConfigs.xwiki\.properties.oidc\.secret`)

**E2E test — Install:**
```bash
kubectl gentian apps install xwiki --tenant gtn-demo
kubectl get pods -n tenant-gtn-demo -l app.kubernetes.io/name=xwiki -w
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep xwiki
# Expected: xwiki: 1
```

**E2E test — Use:**
```bash
# Verify XWiki health
XWIKI_POD=$(kubectl get pod -n tenant-gtn-demo -l app.kubernetes.io/name=xwiki -o name | head -1)
kubectl exec -n tenant-gtn-demo "$XWIKI_POD" -- curl -sf http://localhost:8080/rest
# Expected: XWiki REST API response
# Functional test: open XWiki URL → SSO login via Keycloak → LDAP users visible →
#   create wiki page → edit and save → page renders correctly
```

**E2E test — Uninstall:**
```bash
kubectl gentian apps uninstall xwiki --tenant gtn-demo
sleep 30
kubectl get pods -n tenant-gtn-demo | grep xwiki  # Expected: no results
kubectl get appcatalogue default -o jsonpath='{range .status.apps[*]}{.name}: {.installedCount}{"\n"}{end}' | grep xwiki
# Expected: xwiki: 0
```

---

#### Inc 25 — Contract definitions + App Store CI

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
| 20 | Collabora AppProfile | `gentian-apps/profiles/collabora.yaml`, ArgoCD Source 3, AppProject fix. Remove from `gentian-os`. | Small |
| 21 | Element AppProfile | `gentian-apps/profiles/element.yaml`, OIDC+PG+SMTP wiring. Remove from `gentian-os`. | Medium |
| 22 | Jitsi AppProfile | `gentian-apps/profiles/jitsi.yaml`, JWT/OIDC wiring. Remove from `gentian-os`. | Small |
| 23 | OpenProject AppProfile | `gentian-apps/profiles/openproject.yaml`, 6 kernel reqs + Nextcloud integration. Remove from `gentian-os`. | Medium |
| 24 | XWiki AppProfile | `gentian-apps/profiles/xwiki.yaml`, OIDC+PG+SMTP+LDAP wiring. Remove from `gentian-os`. | Medium |
| 25 | Contract definitions + CI | `gentian-apps` repo CI, contract schemas, profile validation | Small |

Inc 19 (App Store controller) is the foundation — it must be built first. Incs 20–24 (individual app profiles) can be built in parallel after Inc 19. Inc 25 (contracts + CI) can start anytime after Inc 1 (CRDs exist). Each app profile is an independent PR in the `gentian-apps` repo. After Inc 24, `gentian-os/config/samples/` should contain only `integrationbinding_filepicker.yaml` and `tenant_gtn-demo.yaml` — no AppProfile YAMLs.
