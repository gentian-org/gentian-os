# Gentian OS — cleanup tracker

Audit findings grouped by dimension. Update **Status** as work completes:

| Status | Meaning |
|--------|---------|
| `open` | Actionable; not yet addressed |
| `done` | Fixed or superseded |
| `ignore` | Intentional, accepted debt, or out of scope |

Last reviewed: 2026-07-02

---

## a) Inconsistencies

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| a-1 | done | High | AppGrant documented as “Planned” but implemented | Update `new-security-architecture.md` to **Done**; align Stage 2 exit criteria with `app_grant_reconciler.go` | `docs/design/new-security-architecture.md` L184, L273, L417 vs `internal/controller/app_grant_reconciler.go`, CRDs | Roadmap marks AppGrant Done; security design doc still says Planned |
| a-2 | done | High | Composition names in docs don’t match repo | Replace `app-element` / `app-ox` with `app-default` + `spec.compositionRef` in catalogue repos | `docs/architecture.md` L185, `docs/design/app-catalogue.md` L21, `api/v1alpha1/appprofile_types.go` L119 | Docs reference `app-element` / `app-ox`; only `app-default` exists under `crossplane/compositions/` |
| a-3 | done | High | `kernel/appsets/disabled/` referenced but missing | Remove stale comment or add `kernel/appsets/disabled/` README explaining retired sets | `install.sh` L612, `kernel/appsets/16-keycloak-provider.yaml` L6 | Comment points at a directory that does not exist |
| a-4 | done | Medium | Dual CRD artifact paths | Keep `make manifests` as single write path; add CI check that trees match | `config/crd/` + `charts/gentian-os/crds/` | Synced via `make manifests`; `verify-gen` now diffs chart CRDs too |
| a-5 | done | Medium | Copyright header styles differ | Pick one SPDX header style; apply via linter or one-time pass | e.g. `identity_reconciler.go` vs `implicit_base_apps.go` | Mixed Apache boilerplate vs short one-liner |
| a-6 | done | Medium | Routing ownership split is subtle | Document split in `docs/design/kernel.md`; extract shared `buildAppHTTPRoute` helper used once | `gateway_reconciler.go` L20–23 vs `tenant_edge_manifests.go` L27–29 | Crossplane creates objects; operator owns DNS/cleanup/waits; `appHTTPRoutesForIntents` + intent-based `buildAppHTTPRoute` |
| a-7 | done | Medium | Sidecar OIDC ownership comment contradicts behavior | Fix comment or gate sidecar client Jobs behind same `crossplaneOwnsOIDCClient` flag | `tenant_identity_manifests.go` L277 vs `oidc_pack_jobs.go` L63–78 | Comment says sidecar OIDC is composition-owned; operator may still emit client Jobs |
| a-8 | done | Medium | `export/README.md` stale | Point README at gentian-apps repo; remove nonexistent export path | `export/README.md` L9 | Lists `gentian-apps/` export; directory absent (only `gentian-app-template/`) |
| a-9 | done | Medium | Admin Console P1 “deploy pending” | Mark Done in roadmap or link to gentian-deployments portal Application | `docs/roadmap.md` L80 | Code marked Done in gentian-ui; deployment status unclear in gentian-os |
| a-10 | done | Low | Legacy field still in API | Keep until tile migration complete; remove in next API revision | `appprofile_types.go` L53–57 (`Logo` deprecated) | Documented; resolution in `internal/tiles/resolver.go` |
| a-11 | done | Low | `umc_reconciler_test.go` misnamed | Rename to `portal_redirect_reconciler_test.go` | `internal/controller/umc_reconciler_test.go` | Tests `kernelPortalURL` from `portal_redirect_reconciler.go` |
| a-12 | done | Low | Install step numbering typo | Align banner and error message to same step number | `scripts/install-lib.sh` L2573 vs L2574 | Step 8 banner but error says “Step 7 failed” |

---

## b) Unused or legacy code

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| b-1 | done | High | `scratch.patch` — dead artifact | Delete file from repo | `scratch.patch` (~470 lines) | Patch for deleted `app-ox`; Nubus/LDAP init; not referenced |
| b-2 | ignore | High | `install.secrets.env` with real credentials in workspace | Rotate exposed secrets; keep only `install.secrets.env.template` in docs | `install.secrets.env` L11–31 | Gitignored but present locally; rotate if ever committed |
| b-3 | done | Medium | `expected-new.yaml` orphan | Delete file or wire into `Makefile` if it replaces `expected.yaml` | `crossplane/tests/unit/render/tenant-default/expected-new.yaml` | Duplicate of `expected.yaml`; not in `Makefile` render tests |
| b-4 | done | Medium | Legacy install migration paths | Retain until min supported version skips them; document sunset in `install.sh` header | `install.sh` L738–979, `uninstall.sh` L350+ | Removed InfraData + AuthzIdp legacy Release/claim migration |
| b-5 | done | Medium | Legacy Memcached ArgoCD Application cleanup | Keep until no clusters report legacy Application | `cache_reconciler.go` L226–234, L485+ | Removed `deleteLegacyMemcachedApplication` + ArgoCD Application watch |
| b-6 | done | Medium | Legacy ingress/nginx paths | Keep for nginx-mode clusters; document removal when nginx path dropped | `ingress_helpers.go` L61, L139+ | Gateway-only; removed superseded-Ingress auto-cleanup |
| b-7 | done | Medium | `resolveOIDCRedirectURIsLegacy` | Keep for regression tests of old pack format | `oidc_pack_jobs.go` L240 | Removed implicit redirect fallback; packs must declare URIs |
| b-8 | ignore | Low | `.install-state.env` in workspace | No change — local installer cache | Root (gitignored) | Installer cache; intentional local state |
| b-9 | ignore | Low | `install.secrets.env.backup` | No change — user-local backup | Root (gitignored) | Backup of secrets file |

---

## c) Redundant / non-DRY implementations

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| c-1 | done | High | Data-plane provisioning pattern repeated 4× | Introduce `KernelRequirementProvisioner` interface + shared Job wait/condition helpers | `kernel_requirement.go`, reconcilers | `reconcileJobWaitRequirement`, `ensureDeleteJobs`, `newKernelProvisioningJob` |
| c-2 | partial | High | Keycloak logic split across 15+ files | Consolidate shell builders under `keycloak/` package; add sequence diagram to `docs/design/iam.md` | `identity_reconciler.go`, `keycloak_*.go`, `keycloak_shell_helpers.go` | Groups in `internal/keycloak/`; §1.9 diagram; shell helpers still in controller |
| c-3 | done | High | `install-lib.sh` monolith | Split into `scripts/lib/{openbao,argocd,mail,certs,catalogue}.sh`; source from install-lib | `scripts/lib/load.sh` sourced by `install.sh`, `update.sh`, `uninstall.sh` | `install-lib.sh` is thin shim + legacy `main()` |
| c-4 | done | Medium | Gateway route building triplicated | Single `reconcileTenantHTTPRoutes(ctx, tenant, phase)` used by gateway + edge manifests | `gateway_route_helpers.go` | `appHTTPRoutesForIntents` |
| c-5 | done | Medium | Crossplane composition copy for tests | Render test reads `crossplane/compositions/app-default.yaml` directly or symlink | `crossplane/tests/unit/render/app-default/composition.yaml` | Symlink to canonical composition |
| c-6 | done | Medium | Provisioner image constants scattered | Add `internal/kernel/images.go` or Helm values consumed by operator Deployment | Controllers, `kubectl-gentian`, `applifecycle/purge.go` | Helm `provisioners.*` + env vars mirror Memcached pattern |
| c-7 | done | Medium | `collect*Apps` + `collect*AppsForDelete` | Single collector with `mode: provision\|delete` filter | `kernel_requirement.go`, reconcilers | `AppCollectionMode` + `collectKernelApps` |
| c-8 | done | Low | `gentian_groups.go` vs `keycloak_gentian_groups.go` | Rename to consistent `keycloak_*` prefix or merge small helpers | `internal/keycloak/groups.go` | Group naming in keycloak package; controller thin wrappers |

---

## d) Hardcoded values

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| d-1 | done | High | Postfix sender domain hardcoded | Template `ALLOWED_SENDER_DOMAINS` from `KERNEL_DOMAIN` in install/Helm | `kernel/services/postfix/*` | Placeholder `example.domain`; patched by mail-lib on install/update |
| d-2 | done | Medium | CNPG cluster name fixed | Expose via `charts/gentian-os/values.yaml` `database.clusterName` | `database_reconciler.go`, Helm Deployment | `CNPG_CLUSTER_NAME` env var |
| d-3 | done | Medium | Provisioner images not Helm-configurable | Mirror `MEMCACHED_IMAGE` env vars for postgres/mariadb/redis/alpine init | `internal/kernel/images.go`, Helm `provisioners.*` | Includes Keycloak alpine provisioner |
| d-4 | done | Medium | OpenFGA shell app ID | Document as platform constant in `internal/authz/bridge.go` | `internal/authz/bridge.go` L12: `ShellAppObjectID = "gentian-ui"` | Platform constant; documented |
| d-5 | done | Medium | Default image tags in composition | Ensure cluster ConfigMap always sets `appInit.image` in install | `upsert_gentian_cluster_config` | Set on every install/update crossplane path |
| d-6 | done | Medium | Keycloak user list cap | Paginate with `first`/`max` until empty page | `keycloak_client.go` | Users, group members, and group lookup |
| d-7 | done | Low | gentian-org GitHub URLs | Defaults use `git.example.domain/*` placeholders | `scripts/lib/common.sh`, `values.yaml` | Override via `GENTIAN_*_REPO` env vars |
| d-8 | ignore | Low | `desk.gentian.org` in docs/tests | No change — fixture domain | `crossplane/tests/unit/render/*/xr.yaml`, controller tests | Example domain for fixtures |

---

## e) App-specific values in platform/kernel code

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| e-1 | done | Medium | `od-element` in platform unit tests | Use generic `catalogue-test-app` fixture or pull fixture from gentian-pro testdata | `crossplane/tests/unit/render/tenant-default/xr.yaml`, `oidc_pack_jobs_test.go` | Generic platform fixture; pro profiles stay in gentian-pro |
| e-2 | done | Medium | Element/Jitsi/OpenProject in comments & tests | Move app-specific notes to gentian-pro profile docs; keep generic CSP comments in os | `gateway_reconciler_test.go`, `mac_waiver_test.go`, `cache_test.go` | Generic profile/subdomain names in tests |
| e-3 | done | Medium | Nextcloud in CRD schema text | OpenAPI examples use community Nextcloud (not od-nextcloud) | `appprofile_types.go`, generated CRDs | Examples unchanged where already generic |
| e-4 | done | Medium | OpenProject comment in composition | Generic chart env comment in app-default | `app-default.yaml` | Removed app-specific OpenProject/jitsi examples |
| e-5 | done | Medium | OpenDesk registry comments in secrets template | No OD_PRIVATE_REGISTRY vars in template | `install.secrets.env.template` | Already absent; example domain in OPENPROJECT hint |
| e-6 | done | Medium | `scratch.patch` Nubus/LDAP logic | Delete with b-1; any UDM/LDAP init belongs in gentian-pro composition | `scratch.patch` | Removed in b-1 |
| e-7 | done | Low | `gentian-catalogue-pro` ApplicationSet | Already implemented | `kernel/bootstrap/catalogue-pro-applicationset.yaml.tmpl` | Correct pattern: pro profiles from gentian-pro |
| e-8 | done | Low | OIDC pack catalog | Document CR-driven design | `internal/oidc/cluster_catalog.go` | Packs resolved from OIDCPackCatalog CRs |

---

## f) Inefficient implementations

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| f-1 | done | High | AuthzBridge full-cluster sync every 5 min | Reconcile only changed tenant/realm; use resourceVersion diff or watch Keycloak events | `authz_bridge_reconciler.go` | Event-driven per-realm sync; no idle 5m requeue |
| f-2 | done | High | OpenFGA tuple writes without deletes | Pass delete tuple set to `WriteTuples`; reconcile grant diff on AppGrant update | `authz/bridge.go`, `app_grant_reconciler.go` | Tuple diff + finalizer cleanup on delete |
| f-3 | done | High | Tenant reconcile serializes many subsystems | Split into sub-reconcilers with explicit `ReconcileStage` and short-circuit conditions | `tenant_reconcile_stages.go` | Staged pipeline with short-circuit on requeue |
| f-4 | done | Medium | 2s polling loops on provisioning Jobs | Use watch on Job status + exponential backoff cap | `provisioning_requeue.go`, reconcilers | Job-age exponential backoff (2s–30s) |
| f-5 | ignore | Medium | Keycloak shell wait loop | No change — required for parallel Crossplane Job apply | `keycloak_shell_helpers.go` L46–57 | 90×2s poll inside Job scripts |
| f-6 | done | Medium | Per-app AppProfile GET in loops | List AppProfiles once into map keyed by name | `appprofile_index.go`, collectors | AppProfile index per reconcile path |
| f-7 | done | Medium | Gateway platform reconciler 5 min idle requeue | Requeue only on Gateway/HTTPRoute spec change | `gateway_platform_reconciler.go` | Event-driven; no idle 5m requeue |
| f-8 | done | Low | Authz bridge Keycloak `ListRealmUsers` max=1000 | Paginate user list (same as d-6) | `keycloak_client.go` | Users, group members, groups |

---

## g) Ugly implementations (refactor candidates)

| Item | Status | Sev | Finding | Suggested Solution | Location | Notes |
|------|--------|-----|---------|-------------------|----------|-------|
| g-1 | open | High | `TenantReconciler.Reconcile` god function | Extract `reconcileIdentity`, `reconcileDataPlane`, `reconcileApps` methods | `tenant_controller.go` L333–575 (~240 lines) | Mixes validation, XR patching, ensure* calls, conditions, metrics |
| g-2 | open | High | `identity_reconciler.go` shell-script generation | Migrate provisioning to Keycloak Admin REST (`keycloak_client.go`) incrementally | L489+, 842 lines total | Large embedded curl/jq scripts; REST path exists in `keycloak_client.go` |
| g-3 | open | High | `app-default.yaml` composition | Split into nested templates: `fetch`, `init`, `release`, `policy` | 1,236 lines | Single template: fetch, init Jobs, Helm, netpolicy, secrets |
| g-4 | open | Medium | `gateway_policy.go` | Split into `gateway_intent.go`, `gateway_stale.go`, `reference_grant.go` | 394 lines | Mixes intent collection, BTP parsing, stale deletion, ReferenceGrants |
| g-5 | open | Medium | `install.sh` + legacy retirement | Move migrations to `scripts/migrations/` invoked once per version | 1,305 lines with inline migration | Half greenfield install, half upgrade archaeologist |
| g-6 | open | Medium | Error handling in browser-security / portal redirect | Return `RequeueAfter` on non-blocking errors with metric | `tenant_controller.go` L509–515 | Logged as non-blocking without guaranteed requeue |
| g-7 | open | Low | Import ordering in some files | Run `goimports` / golangci-lint import grouping | e.g. `database_reconciler.go` L22–23 | stdlib/third-party mix |

---

## Test coverage gaps

| Item | Status | Component | Test file? | Gap | Suggested Solution |
|------|--------|-----------|------------|-----|-------------------|
| t-1 | open | `TenantReconciler` (integration) | `tenant_controller_test.go` | Partial envtest | Extend envtest cases for app install + data-plane Job paths |
| t-2 | open | `AppGrantReconciler` | None | OpenFGA tuple sync untested | Add envtest with fake OpenFGA or mock client; assert write + delete tuples |
| t-3 | open | `PlatformSecurityPolicyReconciler` | None | MAC waiver ConfigMap sync untested | Golden test for ConfigMap patch from AppProfile annotations |
| t-4 | open | `AppPrivilegeReconciler` | None | Privilege fingerprinting untested at reconciler level | Table-driven tests for fingerprint + Keycloak role mapping |
| t-5 | open | `MacWaiverReconciler` (tenant ensure) | None | Logic in `internal/security/mac_waiver_test.go` only | Wire reconciler envtest calling shared waiver helpers |
| t-6 | open | `AuthzBridgeReconciler` | None | Full-cluster sync untested | Mock Keycloak + OpenFGA; assert incremental sync on tenant change |
| t-7 | open | `PortalRedirectReconciler` | `portal_redirect_reconciler_test.go` | Minimal | Add cases for kernel vs tenant portal URLs |
| t-8 | ignore | Crossplane `app-default` render | `crossplane/tests/unit/render/app-default/` | Exists | No change |
| t-9 | done | Crossplane `tenant-default` | Uses `od-element` fixture | Pro-specific fixture | Generic `catalogue-test-app` fixture (see e-1) |
| t-10 | ignore | `app-element` / `app-ox` compositions | No goldens in gentian-os | Custom compositions live in catalogue repos | Test in gentian-pro repo instead |
| t-11 | open | `internal/applifecycle` | `gitops_test.go` only | Service/reconcile paths thin | Add tests for install/uninstall gitops commit shape |
| t-12 | open | `internal/webhook` | `tenant_validator_test.go` | Tenant only; no AppProfile webhook | Implement AppProfile webhook or remove doc claim (see dvc-5) |

---

## Docs vs code — roadmap mismatches

| Item | Status | Doc claim | Code reality | Suggested Solution |
|------|--------|-----------|--------------|-------------------|
| dvc-1 | open | AppGrant “Planned” (`new-security-architecture.md`) | Implemented — CRD + `app_grant_reconciler.go` | Update doc to **Done** (same as a-1) |
| dvc-2 | open | Generic sidecars in `app-default` “not implemented” (`app-catalogue-security.md` L353) | `AppProfile.spec.Sidecars` handled in `oidc_pack_jobs.go` | Mark sidecar OIDC as implemented; document operator vs composition ownership |
| dvc-3 | open | `app-element` / `app-ox` in architecture | Not in gentian-os compositions; in catalogue repos | Doc pass (same as a-2) |
| dvc-4 | ignore | Secret rotation via annotations (`roadmap.md` L148) | Not implemented | Keep on roadmap until implemented |
| dvc-5 | open | AppProfile validating webhook (`app-catalogue-security.md` L347) | Not implemented (Tenant webhook only) | Implement webhook or mark as deferred in security doc |
| dvc-6 | ignore | Stage 0 MAC “complete for dev” (`roadmap.md` L12) | Substantial implementation in netpolicy + Kyverno + AppGrant | Clarify “dev-complete” vs production hardening in roadmap |

---

## Prioritized top 10 (action list)

| Item | Status | Action | Suggested Solution |
|------|--------|--------|-------------------|
| p-1 | done | Delete `scratch.patch` and `expected-new.yaml` (see b-1, b-3, e-6) | Removed in b-1/b-3 |
| p-2 | open | Reconcile security docs with code (AppGrant, composition names) (see a-1, a-2, dvc-1, dvc-3) | One docs PR updating architecture + security + catalogue guides |
| p-3 | done | Parameterize Postfix `ALLOWED_SENDER_DOMAINS` from `KERNEL_DOMAIN` (see d-1) | mail-lib patch + example.domain placeholder in manifests |
| p-4 | done | Fix OpenFGA sync semantics (deletes, pagination, event-driven) (see f-1, f-2, f-8) | Tuple diff in bridge; paginate Keycloak users |
| p-5 | open | Extract shared kernel-requirement provisioner (DB/MariaDB/storage/cache) (see c-1) | New `internal/controller/provisioner/` package |
| p-6 | done | Refactor `tenant_controller.Reconcile` into staged pipeline (see f-3, g-1) | Phased reconcile with typed stages |
| p-7 | open | Update architecture/catalogue docs (`app-default` + `compositionRef`) (see a-2, dvc-3) | Overlap with p-2 |
| p-8 | open | Add reconciler tests (AppGrant, AuthzBridge, PlatformSecurityPolicy) (see t-2, t-3, t-6) | envtest + mocks per reconciler |
| p-9 | done | Split `install-lib.sh` into focused modules (see c-3) | `scripts/lib/load.sh` + domain modules; thin `install-lib.sh` shim |
| p-10 | open | Deduplicate `app-default` composition for unit render tests (see c-5) | Symlink or read parent composition in render harness |

---

## Completed since audit (2026-07-02)

| Item | Status | Description | Suggested Solution |
|------|--------|-------------|-------------------|
| x-1 | done | `install_catalogue_pro_sync` missing closing brace (shellcheck SC1072) | N/A — fixed in `152ab1b` |
| x-2 | done | `gentian-catalogue-pro` ApplicationSet + Argo CD project allowlist (see e-7) | N/A — deployed; document repo PAT setup in install guide |
| x-3 | done | Cleanup batch a-1–a-3, a-5, a-7–a-12 (docs, headers, API, tests, install) | See git diff on `develop`; a-4/a-6 answered in chat, not implemented |
| x-4 | done | Legacy artifact cleanup b-1/b-3/b-6/b-7 (nginx paths, OIDC fallback, dead files) | See git diff on `develop` |
| x-5 | done | Remove legacy infra/Memcached/Ingress migration paths (b-4, b-5, ingress cleanup) | Assumes clean clusters; InfraData XR only |
