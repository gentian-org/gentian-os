# Gentian OS — Crossplane Migration Plan

**Version:** 0.5
**Status:** Complete — P0 ✅  P1 ✅  P2A ✅  P2B ✅  P2C ✅  Crossplane v2 ✅
**Companion to:** [architecture-legacy.md](architecture-legacy.md), [architecture-crossplane.md](architecture-crossplane.md)

> **Script names:** `install-cp.sh` and `uninstall-cp.sh` have been renamed
> to `install.sh` and `uninstall.sh` respectively.  The old Tofu-based
> `install.sh` / `uninstall.sh` have been removed; the shared helper
> functions are preserved in `scripts/install-lib.sh` (sourced by the new
> `install.sh`). All references below use the current names.

---

## 0. Goal and Constraints

Migrate the provisioning plane of Gentian OS from
**OpenTofu + Tofu Controller + custom Go orchestrator** to **Crossplane**,
without changing the user-facing CRDs (`Tenant`, `AppProfile`,
`IntegrationBinding`) or the deployment plane (ArgoCD, OpenBao, ESO,
Reloader, upstream operators).

Constraints:

1. **Every phase is independently revertible** — the legacy stack stays
   running until the new stack is proven.
2. **Every phase has unit tests** that run in CI without a cluster.
3. **Every phase has end-to-end (E2E) tests** that a human operator can
   execute against a live dev cluster and observe pass/fail through
   `kubectl` and Argo / Gentian UIs.
4. **No tenant data loss.** Live tenants are migrated last, by re-import
   of their existing `Tenant` CR against the new XRD (the spec is
   identical) — backing data (DBs, buckets, mailboxes) is untouched.

The plan is structured as a sequence of **migration phases** (P0 … P5).
Each phase has: scope, deliverables, unit-test plan, E2E test plan,
acceptance criteria, and rollback procedure.

---

## 1. Test Strategy Overview

### 1.1 Unit tests (no cluster required)

Three categories, all runnable via `make test` in CI:

| Category | Tooling | What it validates |
|---|---|---|
| **XRD schema tests** | `kubectl --dry-run=server` against `kind` cluster, plus `kubeval` / `kubeconform` | XRDs accept valid CRs and reject invalid ones |
| **Composition rendering tests** | `crossplane render` (CLI, no cluster) + golden-file diffs | A given XR + Composition + functions produce the expected MR YAML |
| **Composition function tests** | Native unit tests of the function language (Go for `function-go-templating` helpers, KCL `assert`, Python `pytest`) | Pure functions (HMAC derivation, valueMapping rendering, integration-binding resolution) behave correctly |

Pattern: every Composition has a `tests/` directory with paired
`xr-*.yaml` (input) and `expected-*.yaml` (golden output) files. CI runs:

```bash
crossplane render \
  tests/xr-tenant-minimal.yaml \
  composition.yaml \
  functions.yaml \
  > /tmp/actual.yaml
diff -u tests/expected-tenant-minimal.yaml /tmp/actual.yaml
```

This catches regressions in template logic, valueMapping, conditional
IntegrationBinding emission, and appSecrets derivation **without ever
touching a cluster**.

### 1.2 E2E tests (live dev cluster required)

Two categories, all runnable by a human operator with `kubectl` access:

| Category | Tooling | What it validates |
|---|---|---|
| **Smoke scripts** | Bash scripts in `e2e/scripts/`, invokable as `make e2e-PHASE` | Operator can apply a fixture, wait for Ready, verify side effects, and clean up |
| **Black-box checks** | `kubectl wait`, `curl` against tenant URLs, login as test user | A real user-facing scenario works end-to-end (tenant exists, app is reachable, SSO works) |

Pattern: every phase has a `make e2e-pX` target that the operator runs
on a dev cluster. The script prints **PASS** or **FAIL** with a clear
list of which checks passed. No hidden state — every check is something
the operator could perform manually with `kubectl` and a browser.

### 1.3 Test fixture repository layout

```
gentian-os/
└── crossplane/
    ├── xrds/
    ├── compositions/
    ├── functions/
    └── tests/
        ├── unit/
        │   ├── render/                # crossplane render golden tests
        │   │   ├── tenant-minimal/
        │   │   │   ├── xr.yaml
        │   │   │   ├── composition.yaml
        │   │   │   ├── functions.yaml
        │   │   │   └── expected.yaml
        │   │   ├── tenant-with-mail/
        │   │   ├── tenant-vcluster/
        │   │   └── cluster-bootstrap/
        │   ├── functions/             # function unit tests
        │   │   ├── derive-secrets/
        │   │   └── render-valuemapping/
        │   └── schema/                # XRD validation
        │       ├── valid/
        │       └── invalid/
        └── e2e/
            ├── scripts/
            │   ├── p1-kernel-dev.sh
            │   ├── p2-pattern-b.sh
            │   ├── p3-tenant-shadow.sh
            │   ├── p4-tenant-cutover.sh
            │   └── p5-tofu-decommission.sh
            └── fixtures/
                ├── cluster-dev.yaml
                ├── tenant-test-shadow.yaml
                └── tenant-test-cutover.yaml
```

---

## 2. Phase 0 — Preparation

**Scope:** Land the test infrastructure and CI hooks before changing
anything else. This phase is pure additive scaffolding; nothing in the
running system changes.

### 2.1 Deliverables

- [x] `crossplane/tests/` directory committed with stub directories.
- [x] `Makefile` targets:
  - `make test-unit-render` — runs `crossplane render` golden tests.
  - `make test-unit-functions` — runs language-native function tests.
  - `make test-unit-schema` — runs XRD schema valid/invalid fixtures.
  - `make test-unit` — all three.
  - `make e2e-p1` … `make e2e-p5` — phase-specific E2E scripts (initially
    print "phase not yet implemented").
- [x] CI pipeline runs `make test-unit` on every PR.
- [x] Dev cluster has `crossplane` CLI installed and reachable
  (`crossplane version` succeeds).

### 2.2 Unit tests

Trivially: a placeholder render test for an empty Composition that emits
a single `Namespace` MR. Validates the test harness itself works.

```bash
# Example smoke test of the harness
crossplane render \
  crossplane/tests/unit/render/_harness/xr.yaml \
  crossplane/tests/unit/render/_harness/composition.yaml \
  crossplane/tests/unit/render/_harness/functions.yaml \
  | diff -u crossplane/tests/unit/render/_harness/expected.yaml -
```

### 2.3 E2E test

A single check: the dev cluster can install Crossplane core (without any
providers yet) into namespace `crossplane-system` and the deployment
reports Ready.

```bash
make e2e-p0        # installs crossplane core, waits for Ready, prints PASS/FAIL
make e2e-p0-clean  # uninstalls (revert)
```

### 2.4 Acceptance

- CI green on `make test-unit`.
- Operator can run `make e2e-p0` on dev cluster and see PASS.
- `kubectl get pods -n crossplane-system` shows `crossplane` pod Ready.

### 2.5 Rollback

`helm uninstall crossplane -n crossplane-system && kubectl delete ns crossplane-system`.
No production impact — Crossplane was not yet wired to anything.

---

## 3. Phase 1 — Kernel Provisioning via `Cluster` XR ✅

**Scope:** Replace the OpenTofu HCL that seeds kernel resources
(namespaces, OpenBao mounts, KV secrets, ArgoCD AppProject, kernel Helm
releases) with a `Cluster` XRD + Composition.

### 3.1 Deliverables

- [x] Providers installed: `provider-kubernetes`, `provider-vault`.
      `function-go-templating`. ProviderConfigs reference the in-cluster
      OpenBao via service account. (`provider-helm` deferred to Phase 2.)
- [x] `crossplane/xrds/cluster.yaml` — `Cluster` / `XCluster` schema
      with fields matching architecture.md §13 Tofu variables
      (`kernelDomain`, `ldapBaseDn`, root credential refs).
- [x] `crossplane/compositions/cluster-default.yaml` — emits MRs for:
      kernel namespaces; OpenBao mount + policies; KV seeds for
      `kernel/database`, `kernel/cache`, `kernel/storage`,
      `kernel/identity`, `kernel/mail`; ArgoCD AppProject; ClusterIssuer;
      ClusterSecretStore.
- [x] `crossplane/functions/derive-secrets/` — Python composition
      function implementing the HMAC-SHA256 derivation from
      architecture.md §5.1. Pure function, fully unit-testable.
- [x] `managementPolicies: ["Observe", "Create"]` on every KV MR — the
      Crossplane equivalent of architecture.md §5.2's Tofu lifecycle
      guard. Crossplane will create on first reconcile and never
      overwrite.

### 3.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/functions/derive-secrets/test_known_vectors.py` | HMAC derivation produces the same outputs as the existing bash script, given identical inputs (cross-checked against `openssl dgst` reference) |
| `tests/unit/functions/derive-secrets/test_idempotent.py` | Same input → same output across re-runs |
| `tests/unit/render/cluster-bootstrap/` | Given a `Cluster` XR with `kernelDomain=desk.gentian.org`, the rendered MRs include exactly: 2 namespace MRs, 1 OpenBao mount MR, N KV MRs (one per kernel/* path), 1 AppProject MR — golden file diff |
| `tests/unit/schema/valid/cluster-minimal.yaml` | Minimal Cluster spec is accepted |
| `tests/unit/schema/invalid/cluster-missing-domain.yaml` | Cluster without `kernelDomain` is rejected |

CI command: `make test-unit` — must pass before phase moves to E2E.

### 3.3 E2E test (dev cluster)

`make e2e-p1` performs:

```text
1. Take a fresh OpenBao snapshot (safety net).
2. Apply crossplane/xrds/cluster.yaml + compositions + functions.
3. Apply a Cluster XR with kernelDomain=dev.gentian.org and credentials
   pointing at the same OpenBao paths the legacy Tofu run uses.
4. kubectl wait --for=condition=Ready cluster/dev-cluster --timeout=10m
5. For each expected KV path: bao kv get -format=json gentian-os/kernel/identity
   and assert the value matches the legacy Tofu-seeded value (byte-for-byte).
6. Verify ArgoCD AppProject 'gentianos-tenants' exists and matches the
   legacy spec.
7. Verify ClusterSecretStore 'gentianos-openbao' is Ready.
8. Print a diff report: legacy Tofu state vs Crossplane MR state. Empty diff = PASS.
```

The operator can manually verify each step with `kubectl describe
xcluster dev-cluster` (sees all composed MRs and their conditions) and
`bao kv list gentian-os/kernel`. They can also point a browser at
ArgoCD and see `gentianos-tenants` AppProject unchanged.

### 3.4 Acceptance

- All unit tests green.
- `make e2e-p1` returns PASS.
- Existing Argo Applications targeting the kernel still sync Healthy
  (kernel resources are byte-identical from their perspective).
- No tenant disruption (tenants do not interact with the `Cluster` XR).

### 3.5 Rollback

```bash
kubectl delete cluster dev-cluster -n crossplane-system  # Crossplane GC removes MRs
                                        # but managementPolicies prevent
                                        # OpenBao deletion (Observe/Create only)
kubectl delete -f crossplane/xrds/cluster.yaml
# OpenTofu state is untouched — re-run `tofu apply` to reassert.
```

---

## 4. Phase 2A — Replace `openbao-init` Tofu with `bao` CLI + `provider-vault`

**Scope:** Eliminate OpenTofu from the OpenBao bootstrap. The one-time
platform initialisation (KV engine, Kubernetes auth backend, ESO policy
and role) is replaced by direct `bao` CLI calls in `install.sh`. All
ongoing OpenBao configuration (additional policies, roles for new
services) is migrated to `provider-vault` Crossplane Managed Resources,
making OpenBao config GitOps-managed and drift-detected.

After this phase `kernel/tofu/platform/openbao-init/` is deleted and
the `tofu-write` policy / `tofu-runner` role are removed once Phase 2B
is also complete.

### 4.1 Deliverables

- [x] `install.sh` gains a `bao_bootstrap()` function that calls the
      `bao` CLI for the minimum chicken-and-egg setup:
      - `bao secrets enable -path=secret kv-v2`
      - `bao auth enable kubernetes`
      - `bao write auth/kubernetes/config kubernetes_host=$K8S_HOST`
      - `bao policy write eso-read ...`
      - `bao write auth/kubernetes/role/eso ...`
      All calls are idempotent (check-then-write).
- [x] `kernel/services/openbao-config/manifests/` ArgoCD Application +
      `provider-vault` MRs managing:
      - `operator-write` policy (scoped to gentian-os operator SA)
      - `gentian-os-operator` Kubernetes auth role
- [x] `kernel/tofu/platform/openbao-init/` deleted.
- [x] Transit auto-unseal confirmed working (`Seal Type: transit`,
      `Sealed: false`; `openbao-transit` Synced+Healthy).
      `openbao-init.json` pretty-printed; creds re-displayed on re-runs.

### 4.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/render/openbao-config/` | `provider-vault` MRs render correctly; no sensitive fields in spec |
| `tests/unit/script/bao-bootstrap.bats` | Bootstrap function calls are idempotent: running twice produces zero changes (mock `bao` with a stub) |

### 4.3 E2E test (dev cluster)

`make e2e-p2a`:

```text
1. Run `install.sh --bootstrap-openbao-only` on the dev cluster
   (already bootstrapped — all calls should be no-ops).
2. Verify ESO can still sync secrets:
     kubectl get externalsecret -A | grep -v Ready  # must be empty
3. Apply the openbao-config Application and wait for all MRs to be Synced.
4. Delete the openbao-init Tofu workspace files from the branch.
5. Verify: kubectl get terraform -n tofu-system  # must show no openbao-init CR
6. Verify: bao policy list | grep eso-read  # policy still present (adopted by MR)
7. Trigger an ESO full resync and confirm all ExternalSecrets remain Ready.
```

### 4.4 Acceptance

- `kernel/tofu/platform/openbao-init/` deleted from the repo.
- No Tofu CR in the cluster for openbao-init.
- All ESO ExternalSecrets Synced/Ready on the dev cluster.
- `provider-vault` MRs for policies and roles Synced/Healthy.

### 4.5 Rollback

```bash
cd kernel/tofu/platform/openbao-init
tofu init && tofu apply  # re-asserts state from local tfstate backup
# Remove the provider-vault MRs (they orphan the OpenBao resources
# because managementPolicies: Observe/Create — no deletion occurs).
```

---

## 5. Phase 2B — Migrate Kernel Helm Releases from Tofu to `provider-helm` ✅

**Scope:** Replace the `infra-workspaces-dev` Tofu Controller workspace
(which managed Nubus, Nextcloud × 3, PostgreSQL, MariaDB, Keycloak bootstrap
as Helm releases with `set_sensitive` secret injection) with
`provider-helm` Release MRs. The openDesk charts use indexed `set_sensitive`
calls (not a single `existingSecret` key), so all migrations use **Pattern B**:
an ESO ExternalSecret with a `template` that renders a `sensitive-values.yaml`
Helm values file, consumed by `Release.spec.forProvider.valuesFrom.secretKeyRef`.

The `infra-workspaces-dev` Tofu workspace and the entire
`kernel/tofu/tenant/infra-workspaces/` module have been removed. The Tofu
Controller binary stays installed (still owns the per-tenant `app-workspace`,
`keycloak-config`, and `ox-workspace` workspaces) and will be uninstalled in
Phase 2C / Phase 5.

### 5.1 Deliverables

Pattern: per-chart directory `kernel/services/<app>/manifests/dev/` containing
an ESO ExternalSecret (template-based `sensitive-values.yaml` key), plain-values
ConfigMaps, and a `provider-helm` Release CR. The new AppSet
`kernel/appsets/09-infra-helm.yaml` (wave 9) deploys all these directories.

- [x] **nubus** — ESO ExternalSecrets (`nubus-credentials`, `nubus-sensitive-values`,
      `nubus-static`, `portal-object-storage`, `keycloak-bootstrap-ldap-credentials`)
      + Release CR `nubus-dev` in `crossplane/apps/nubus/`; applied by install.sh.
      Synced+Ready on dev cluster.
- [x] **opendesk-postgresql** — `postgresql-sensitive-values` ESO + plain-values
      ConfigMaps + Release CR in `kernel/services/opendesk-postgresql/manifests/dev/`.
- [x] **opendesk-mariadb** — `mariadb-sensitive-values` ESO + plain-values
      ConfigMaps + Release CR in `kernel/services/opendesk-mariadb/manifests/dev/`.
- [x] **nextcloud** (3 charts) — Pattern B migration complete:
      `kernel/services/{nextcloud,nextcloud-management,nextcloud-notifypush}/manifests/dev/`
      each carry `externalsecret.yaml` + `configmap.yaml` + `release.yaml`.
      Secret sources: `kernel/apps/nextcloud`, `kernel/cache/redis`,
      `kernel/storage/minio`, `kernel/identity/nubus`, `kernel/database/postgresql`.
- [x] `kernel/appsets/09-infra-helm.yaml` — AppSet (wave 9) matrix covers
      `opendesk-postgresql`, `opendesk-mariadb`, `nextcloud-management`,
      `nextcloud`, `nextcloud-notifypush`.
- [x] `kernel/tofu/tenant/infra-workspaces/` — deleted in full (no
      `nubus.tf`, `nextcloud.tf`, `stubs.tf`).
- [x] `kernel/services/tofu/manifests/dev/terraform.yaml` — reduced to a
      placeholder; `infra-workspaces-dev` Terraform CR retired.
- [x] Multi-tenant LDAP ACL feature preserved — `ldap-gentian-acl` ConfigMap
      mount restored on the migrated `nubus-dev` Release
      (`crossplane/apps/nubus/values/_base.yaml` + patch script
      `crossplane/apps/nubus/patches/92-gentian-tenant-acl.sh`).
- [x] `install.sh` / `uninstall.sh` updated to deploy/drain Pattern B Release
      CRs end-to-end (Step 1b drains Releases before Crossplane teardown).
- [ ] **Deferred to P2C** — the `nubus-dev-udm-listener-nats-patch` and
      `nubus-dev-ldap-gentian-acl` ConfigMaps are still created imperatively
      by `install.sh` (idempotent `kubectl create --dry-run | apply`).
      Migrating them to `provider-kubernetes` `Object` MRs is a cosmetic
      cleanup with no functional impact and is grouped with the per-tenant
      `provider-kubernetes` work in Phase 2C.

Final chart migration status:
| Chart | Tofu state | provider-helm Release |
|---|---|---|
| **nubus** | removed | ✅ Synced+Ready |
| **opendesk-postgresql** | removed | ✅ Synced+Ready |
| **opendesk-mariadb** | removed | ✅ Synced+Ready |
| **nextcloud-management** | removed | ✅ Synced+Ready |
| **nextcloud** | removed | ✅ Synced+Ready |
| **nextcloud-notifypush** | removed | ✅ Synced+Ready |
| **keycloak-bootstrap** | removed (deprecated) | n/a |

### 5.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/render/release-postgresql/` | Release MR has correct chart, `existingSecret` ref, no plaintext in spec |
| `tests/unit/render/release-nubus/` | Same; `existingSecret` used for all ~30 credential fields |
| `tests/unit/schema/invalid/release-with-plaintext-secret.yaml` | Admission policy (Kyverno) rejects any Release MR with a secret literal in `spec.values` |

### 5.3 E2E test (dev cluster)

`make e2e-p2b`:

```text
1. Snapshot OpenBao + take CNPG backup of all databases (safety net).
2. Apply ExternalSecrets for all charts; wait for Ready.
3. Apply provider-helm Release MRs (starting with postgresql, mariadb —
   stateful services first, then keycloak-bootstrap, nubus, nextcloud).
4. For each release: kubectl wait --for=condition=Synced release.helm.crossplane.io/<name>
5. Verify pods are Running and healthy (readiness probes).
6. Smoke-test login via Keycloak OIDC endpoint.
7. Remove Terraform infra-workspaces-dev CR from tofu/manifests/dev/terraform.yaml.
8. Confirm: kubectl get terraform -n tofu-system  # empty
9. Grep all MR specs for known secret values — must return zero matches.
```

### 5.4 Acceptance

- [x] All six charts running on `provider-helm` Release MRs.
- [x] `infra-workspaces-dev` Terraform CR deleted from cluster.
- [x] ArgoCD shows all Release MRs as Synced + Healthy.
- [x] `kernel/tofu/tenant/infra-workspaces/` deleted from repo.
- [x] Tofu Controller has no kernel-tier workspaces (only per-tenant
      `app-workspace` / `keycloak-config` / `ox-workspace` remain;
      retired in Phase 2C).
- [x] Secret-leak grep returns zero results (confirmed clean — no live tenants,
      no Pattern B Terraform CRs; ConfigMap-based plain values for tenant apps
      won't migrate to provider-helm at this tier — the App XRD Composition
      emits provider-helm Releases directly, skipping the intermediate
      ConfigMap step that was planned for Pattern A apps).

### 5.5 Rollback

```bash
# Re-add infra-workspaces-dev to kernel/services/tofu/manifests/dev/terraform.yaml
# ArgoCD syncs → Tofu Controller re-applies infra-workspaces workspace.
# provider-helm Release MRs can coexist temporarily (same Helm release name,
# Tofu wins on next apply because it owns the Helm state).
# Delete the provider-helm MRs once Tofu is confirmed healthy.
```

---

## 5b. Phase 2C — App XRD + Composition + Tofu Removal ✅

> **Crossplane upgrade:** As part of this phase Crossplane was upgraded
> from v1.18.0 to **v2.2.1**. `function-extra-resources` was simultaneously
> bumped from v0.1.0 (last v1-compatible release) to **v0.3.0** (requires
> Crossplane ≥ 2.0). Both are pinned in `crossplane/providers/providers.yaml`.
> The upgrade required a full `uninstall.sh` / `install.sh` cycle; no
> persistent data was at risk (no live tenants).

**Scope:** Introduce the `App` XRD as the tenant-admin-facing primitive
for installing applications, rewrite the `AppReconciler` to emit `App`
claims instead of `Terraform` CRs, and delete the Tofu Controller and
all `kernel/tofu/` modules.

This is a clean architectural replacement on a cluster with no live
tenants. There is no state to preserve, no live Helm releases to adopt,
and no rollback path that requires state snapshots. The dev cluster is
the proving ground; the result is a stack that can provision fresh
tenants end-to-end using Crossplane alone.

After this phase the provisioning plane is a single technology
(Crossplane + ESO). Phases 3 onwards operate on this baseline.

### 5b.1 Deliverables

#### App XRD + Composition

- [x] **`crossplane/xrds/app.yaml`** — `XApp` (composite) / `App`
      (claim, namespace-scoped). Spec fields:
      - `profileRef.name` — references an `AppProfile` by name.
      - `tenantNamespace` — target namespace (set by the reconciler from
        the owning `Tenant`).
      - `domain` — effective tenant domain (vanity or
        `<tenant>.<kernelDomain>`).
      - `config.replicas` — optional replica override.
      - `config.extraValues` — optional RawExtension merged over the
        profile's `extraValues`.
- [x] **`function-extra-resources` v0.3.0** pinned in
      `crossplane/providers/providers.yaml` (not a separate
      `functions.yaml`; packaged together with the other providers/functions).
      Fetches the `AppProfile` named by `spec.profileRef.name` so the
      Composition can read its `valueMapping`, `appSecrets`, `chart`,
      and `extraValues`.
- [x] **`crossplane/compositions/app-default.yaml`** — Composition
      Pipeline using `function-extra-resources` + `function-go-templating`.
      For each `App` claim it emits into `spec.tenantNamespace`:
      - 1× `ExternalSecret` with an ESO `template` that renders a
        `sensitive-values.yaml` Helm values file. Each entry in
        `AppProfile.spec.valueMapping` maps to a per-tenant OpenBao path
        (`gentian-os/tenants/{tenant}/apps/{app}/{category}`). Each
        `AppProfile.spec.appSecrets[]` entry maps to
        `gentian-os/tenants/{tenant}/apps/{app}/internal/{name}`.
      - 1× `helm.crossplane.io/Release` consuming the rendered
        `sensitive-values.yaml` Secret via `valuesFrom.secretKeyRef`,
        plus the profile's non-sensitive `extraValues` merged with any
        `spec.config.extraValues` override.
- [x] **`crossplane/compositions/app-ox.yaml`** — Composition variant
      for OX App Suite. Identical to `app-default` plus a second
      `ExternalSecret` whose template renders `appsuite.properties` from
      the same per-tenant OpenBao paths. Selected via a Composition
      label selector when `AppProfile.spec.chart.name == "appsuite"`.
- [ ] **RBAC** — `ClusterRole` granting tenant admins
      `create`/`delete`/`get`/`list`/`watch`/`patch`/`update` on
      `apps.gentianos.io` in their own namespace; `get`/`list` on
      `appprofiles.gentianos.io` cluster-wide. Bound per-tenant by the
      `TenantReconciler` when it provisions the tenant namespace.
      **Deferred to Phase 3** — no live tenants require this gate yet.

#### AppReconciler rewrite

- [x] **`internal/controller/app_reconciler.go`** — replace
      `ensureTerraformCR` / `buildTerraformCR` / `deleteTerraformCR`
      with `ensureAppClaim` / `buildAppClaim` / `deleteAppClaim` that
      create/watch/delete `App` claims in the tenant namespace. Remove
      all `tofu*` constants, `terraformGVK`, `helmWorkloadGVKs` cleanup
      logic, and the `corev1.SecretList` Helm-tracking-secret purge.
      `cleanupOrphanedAppCRs` lists `App` claims instead of `Terraform`
      CRs. `deleteAppDeployment` deletes `App` claims (Crossplane GCs
      composed resources via ownerReferences).
- [x] **`api/v1alpha1/appprofile_types.go`** — removed
      `DeploymentMethodTofuController`; added `DeploymentMethodCrossplane`;
      `deploymentMethod` defaults to `crossplane`. CRDs in `config/crd/`
      regenerated via `make gen-all` (commit `ac717bd`).

#### Tofu removal

- [x] **Tofu Controller uninstall** —
      `helm uninstall tofu-controller -n tofu-system` run once on dev,
      and the install step removed from `install.sh`.
- [x] **`kernel/tofu/` deleted** — `kernel/tofu/tenant/app-workspace/`,
      `kernel/tofu/tenant/keycloak-config/`,
      `kernel/tofu/tenant/ox-workspace/`, and all remaining platform
      workspaces removed from the repo.
- [x] **`kernel/services/tofu/` deleted** — the AppSet entry and
      Terraform placeholder CR removed.
- [x] **`kernel/appsets/05-tofu.yaml` deleted**.
- [x] **`install.sh`** — `install_tofu_controller` step and
      `tofu-state` MinIO bucket creation removed. `tofu-write` Vault
      policy block and `tofu-runner` K8s auth role block removed
      (commit `9a39040`). App XRD, `app-default.yaml`, and
      `app-ox.yaml` applied by `install_crossplane_providers()`.
      `tofu-system` removed from `uninstall.sh` drain list.
- [x] **`go.mod` / `go.sum`** — `infra.contrib.fluxcd.io` import
      removed; `go mod tidy` run.

### 5b.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/render/app-collabora/` | `App` XR + Collabora `AppProfile` fixture → Release + ExternalSecret golden YAML |
| `tests/unit/render/app-with-appsecrets/` | `AppProfile` with `appSecrets[]` → ExternalSecret template includes `internal/{name}` data entries at the correct Helm value paths |
| `tests/unit/render/app-extravalues-merge/` | Claim-level `config.extraValues` deep-merges over profile-level `extraValues`; profile chart ref is preserved |
| `tests/unit/render/app-ox/` | OX variant Composition emits the `appsuite.properties` ExternalSecret in addition to the standard Release + sensitive-values ExternalSecret |
| `tests/unit/orchestrator/app_reconciler_emits_claim_test.go` | `AppReconciler.ensureAppClaim` creates an `App` claim with the correct `profileRef`, `tenantNamespace`, and `domain` |
| `tests/unit/orchestrator/app_reconciler_cleanup_test.go` | Removing an app from `Tenant.spec.apps` causes `cleanupOrphanedAppCRs` to delete the matching `App` claim |
| `tests/unit/schema/valid/app-minimal.yaml` | Minimal `App` claim (profileRef only) is accepted |
| `tests/unit/schema/invalid/app-unknown-profile.yaml` | `App` claim referencing a non-existent `AppProfile` is rejected by the validating webhook |

CI command: `make test-unit` — full suite must pass before E2E.

### 5b.3 E2E test (dev cluster)

`make e2e-p2c`:

```text
SETUP
  1. Confirm zero Tenant CRs and zero Terraform CRs exist.
  2. Apply XRD + Compositions + function pins:
       kubectl apply -f crossplane/xrds/app.yaml
       kubectl apply -f crossplane/compositions/app-default.yaml
       kubectl apply -f crossplane/compositions/app-ox.yaml
       kubectl apply -f crossplane/functions/functions.yaml
  3. Redeploy the operator from the rewritten image tag.

SMOKE — single tenant, single app
  4. Create a test Tenant: test-alpha (no apps in spec.apps).
  5. kubectl wait --for=condition=Ready tenant/test-alpha --timeout=5m
  6. Apply an App claim in the tenant namespace:
       kubectl apply -n tenant-test-alpha -f e2e/fixtures/app-collabora.yaml
  7. kubectl wait --for=condition=Ready app/collabora -n tenant-test-alpha --timeout=10m
  8. Verify composed resources:
       kubectl get release.helm.crossplane.io -n tenant-test-alpha
       kubectl get externalsecret -n tenant-test-alpha
     Both must show Synced=True Ready=True.
  9. Verify the Helm release is installed:
       helm list -n tenant-test-alpha | grep collabora

SMOKE — app uninstall
 10. kubectl delete app collabora -n tenant-test-alpha
 11. kubectl wait --for=delete release.helm.crossplane.io/collabora \
       -n tenant-test-alpha --timeout=5m
     Release and ExternalSecret must be garbage-collected via ownerReferences.

TOFU REMOVAL
 12. Confirm zero Terraform CRs: kubectl get terraform -A  # must be empty
 13. helm uninstall tofu-controller -n tofu-system
 14. kubectl delete ns tofu-system --wait=true
 15. Re-apply the App claim and confirm it reaches Ready without Tofu.

CLEANUP
 16. kubectl delete tenant test-alpha
 17. kubectl wait --for=delete ns/tenant-test-alpha --timeout=5m
```

### 5b.4 Acceptance

- [x] `crossplane/xrds/app.yaml`, `crossplane/compositions/app-default.yaml`,
      and `crossplane/compositions/app-ox.yaml` committed; both XRDs
      `Established` on dev cluster (`xapps.gentianos.io`,
      `xclusters.gentianos.io`).
- [x] `internal/controller/app_reconciler.go` contains no `tofu*`
      identifiers and no `infra.contrib.fluxcd.io` import.
- [x] `go.mod` has no `terraform` / `tf.contrib.fluxcd.io` dependency.
- [x] `kernel/tofu/`, `kernel/services/tofu/`, `kernel/appsets/05-tofu.yaml`
      deleted from the repo.
- [x] Tofu Controller chart uninstalled from dev cluster.
- [x] Crossplane v2.2.1 running; all 5 providers and 3 functions
      `INSTALLED=True HEALTHY=True`.
- [x] `make test-unit` green in CI (after CRD regeneration fix in
      commit `ac717bd` and removal of unused `appClaimName` func).
- [ ] `make e2e-p2c` — formal E2E script not yet written; cluster is
      healthy and compositions are applied; deferred to Phase 3 baseline.

### 5b.5 Rollback

Since there are no live tenants, rollback is a `git revert` of the
commits in this phase plus reinstalling the operator from the previous
image tag. No data is at risk.

---

## 6. Phase 3 — `Tenant` XRD + Full End-to-End Tenant Provisioning

**Scope:** Replace the Go `TenantReconciler`'s imperative provisioning
loop (namespace, OpenBao policies, LDAP entries, DNS) with a `Tenant`
XRD + Composition Pipeline, and verify that creating a `Tenant` CR
plus one or more `App` claims produces a fully functional tenant
end-to-end: SSO login, installed apps reachable, app-to-app
`IntegrationBinding` wired.

Since the cluster has no live tenants, there is no shadow-deployment
phase needed. Tenants created in Phase 3 are the first real tenants on
the new stack.

### 5.1 Deliverables

- [ ] `crossplane/xrds/tenant.yaml` — XRD with the schema from
      architecture.md §12.2 (identical to the existing CRD).
- [ ] `crossplane/compositions/tenant-default.yaml` — Composition
      Pipeline with two functions: `function-extra-resources`
      (load AppProfiles) and `function-go-templating` (render MRs).
- [ ] `crossplane/functions/render-valuemapping/` — composition
      function that implements architecture.md §4.1 schema-based
      valueMapping rendering.
- [ ] `crossplane/compositions/tenant-vcluster.yaml` — selected when
      `spec.isolation.mode == vcluster`.
- [ ] Shadow-namespace label on rendered MRs so they cannot collide
      with legacy ones (`gentianos.io/shadow: "true"`).

### 5.2 Unit tests

This is the largest unit-test surface in the migration. Each AppProfile
in the catalogue gets a render test:

| Test | Validates |
|---|---|
| `tests/unit/render/tenant-minimal/` | Tenant with one app (Notes) renders namespace + DB + ExternalSecret + Argo Application — golden diff |
| `tests/unit/render/tenant-multi-app/` | Tenant with 6 apps renders the correct fan-out with no missing or extra MRs |
| `tests/unit/render/tenant-with-mail/` | `mail.mode=selfhosted` produces DKIM Secret MR, virtual-domain ConfigMap patch MR, SMTP credentials Secret MR |
| `tests/unit/render/tenant-vcluster/` | `isolation.mode=vcluster` selects the alternate Composition; vCluster Helm release MR is emitted |
| `tests/unit/render/integration-binding-emit/` | When both `nextcloud` and `ox-appsuite` are in `spec.apps`, an `IntegrationBinding` MR for `filepicker` is emitted |
| `tests/unit/render/integration-binding-skip/` | When only `ox-appsuite` is in `spec.apps`, no `filepicker` binding is emitted |
| `tests/unit/functions/render-valuemapping/test_oidc.py` | OIDC schema mapping renders to the chart's expected `oidc.*` keys |
| `tests/unit/functions/render-valuemapping/test_extra_values_merge.py` | `extraValues` correctly deep-merges over the typed mapping |
| `tests/unit/functions/derive-secrets/test_appSecrets.py` | `appSecrets` derivation matches legacy bash-derived values for known fixtures |

CI command: `make test-unit` — full suite must pass.

### 5.3 E2E test (dev cluster)

`make e2e-p3`:

```text
SINGLE-APP TENANT
  1. Apply a Tenant CR: tenant-alpha (domain: alpha.desk.gentian.org).
  2. kubectl wait --for=condition=Ready tenant/tenant-alpha --timeout=5m
  3. Apply an App claim: kubectl apply -n tenant-alpha -f e2e/fixtures/app-nextcloud.yaml
  4. kubectl wait --for=condition=Ready app/nextcloud -n tenant-alpha --timeout=10m
  5. Browser check: Nextcloud URL responds 200; SSO login via Keycloak works.

MULTI-APP TENANT WITH INTEGRATION
  6. Apply tenant-beta with App claims for nextcloud + collabora.
  7. kubectl wait --for=condition=Ready app/nextcloud app/collabora -n tenant-beta --timeout=15m
  8. Verify IntegrationBinding for filepicker shows Ready=True.
  9. Browser check: open a document in Collabora from within Nextcloud.

TENANT DELETE
 10. kubectl delete tenant tenant-alpha
 11. kubectl wait --for=delete ns/tenant-alpha --timeout=5m
     Namespace and all composed resources (Releases, ExternalSecrets,
     Keycloak realm, OpenBao paths) must be gone.
```

Operator visibility: `kubectl get tenants`, `crossplane trace
tenant/tenant-alpha` walks the full composed resource graph.

### 5.4 Acceptance

- All unit tests green (full catalogue coverage).
- Single-app and multi-app tenants reach Ready.
- IntegrationBinding wires correctly between Nextcloud and Collabora.
- Tenant delete removes all composed resources cleanly.

### 5.5 Rollback

`kubectl delete tenant tenant-alpha tenant-beta` removes all composed
resources via ownerReferences. The kernel (Nubus, Nextcloud, databases)
is unaffected.

---

## 7. Phase 4 — Cutover of a Real Tenant

**Scope:** Pick one low-risk dev tenant (e.g., `gtn-demo` in dev), stop
the Go orchestrator's reconciliation for it, and let the Crossplane
Composition take over. Verify zero data loss and zero downtime.

### 6.1 Deliverables

- [ ] Cutover runbook: `crossplane/e2e/scripts/p4-tenant-cutover.sh`.
- [ ] Annotation `gentianos.io/managed-by: crossplane` recognised by
      the legacy Go orchestrator as a "skip this tenant" signal.
      (This is a small, reversible change in the orchestrator: ~10
      lines, fully unit-tested.)
- [ ] Procedure for re-importing the existing Tenant CR as a Crossplane
      claim without re-running provisioning (relies on `provider-vault`
      `Observe`-only on existing KV paths and `provider-kubernetes`
      `Adopt` semantics for existing operator CRs).

### 6.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/orchestrator/skip_annotation_test.go` | Legacy orchestrator skips Reconcile for tenants annotated `managed-by: crossplane` |
| `tests/unit/render/tenant-adopt-existing/` | Composition for an existing tenant emits MRs with `crossplane.io/external-name` matching the existing operator CRs (Database, KeycloakClient, MinIO Tenant) so they are adopted, not recreated |

### 6.3 E2E test (dev cluster)

`make e2e-p4`:

```text
PRE-CHECKS
  1. Backup all data for the chosen tenant: pgBackRest base backup,
     MinIO bucket replication snapshot, Keycloak realm export, Velero
     namespace backup. Print backup IDs.
  2. Record current state: list of pods, Helm release revisions,
     OpenBao secret versions, OIDC client IDs. Save to a fixture file.

CUTOVER
  3. Annotate the Tenant CR: gentianos.io/managed-by=crossplane.
  4. kubectl wait for the legacy orchestrator's Tenant condition
     'OrchestratorPaused=true' (proves the orchestrator saw the annotation).
  5. Apply the Composition for this tenant kind to take ownership.
  6. kubectl wait --for=condition=Ready tenant/<name> (Crossplane path)

POST-CHECKS
  7. Re-record state. Diff against pre-cutover state. The only
     allowed differences are:
       - ownerReferences on operator CRs (now reference the XR)
       - controller annotations
     No pod restart, no Helm revision bump, no OpenBao secret rotation,
     no OIDC client ID change.
  8. Browser smoke: existing test user logs in, sees existing data
     in Nextcloud / OpenProject, can create a new file/task. Manual
     check by operator — provides ground truth.
  9. Wait 30 minutes; re-run state diff. Crossplane must not be
     drifting/flapping any resources.
```

If any post-check fails, the rollback in §6.5 restores the legacy path.

### 6.4 Acceptance

- Pre/post state diff is empty modulo allowed differences.
- Operator confirms via browser that the test user's existing data is
  intact and new actions succeed.
- 30-minute soak shows zero resource churn.

### 6.5 Rollback

```bash
# Remove the managed-by annotation; legacy orchestrator resumes Reconcile.
kubectl annotate tenant <name> gentianos.io/managed-by-
# Delete the Crossplane XR (does NOT delete operator CRs because they
# were adopted, not created — managementPolicies leave them intact).
kubectl delete xtenant <name>
```

If the worst happens (data corruption), restore from the pre-cutover
backups recorded in step 1 — backup IDs are printed at the top of every
run.

---

## 8. Phase 5 — Migrate All Tenants, Decommission Legacy Stack

**Scope:** Repeat Phase 4 for all remaining tenants, then remove the
Go orchestrator, Tofu Controller, and OpenTofu modules.

### 7.1 Deliverables

- [ ] All tenants in all environments cut over to Crossplane.
- [ ] Go orchestrator deployment scaled to zero, then deleted from
      `gentian-os/charts`.
- [ ] Tofu Controller uninstalled (`helm uninstall tofu-controller`).
- [ ] `gentian-os/kernel/tofu/` directory removed; `Cluster` XR is the
      sole source of truth for kernel state.
- [ ] CI no longer builds the Go orchestrator image.

### 7.2 Unit tests

No new unit tests. The full suite from P0–P4 keeps running on every PR.

### 7.3 E2E test (per environment)

`make e2e-p5`:

```text
1. Iterate over all Tenant CRs in the environment.
2. For each, run the Phase 4 cutover script. Stop on first failure.
3. After all tenants migrated:
   a. Verify legacy Go orchestrator pod has zero reconciles in
      Prometheus over 1h (no work to do).
   b. Verify Tofu Controller has no GitRepository sources active.
4. Scale Go orchestrator to zero. Wait 24h. Verify no tenant
   regressions.
5. Uninstall Tofu Controller. Wait 24h. Verify no kernel drift
   (Crossplane reconciles the Cluster XR on schedule).
6. Decommission complete: delete Go orchestrator chart, delete tofu/
   modules, commit removal.
```

### 7.4 Acceptance

- All tenants Ready under Crossplane management.
- Zero reconciles from legacy controllers over a 24h soak.
- Repository diff cleanly removes the Go module and Tofu HCL with no
  remaining references (`grep -r tofu` returns zero hits).

### 7.5 Rollback

Until step 6 is committed, every previous phase's rollback applies.
After step 6, rollback requires restoring from Git history (the legacy
code is recoverable) but is significantly costlier — at this point
Crossplane has been proven for 48h+ across the whole fleet.

---

## 9. Continuous Verification After Migration

The unit-test suite from P0–P5 stays in CI permanently. The E2E
scripts become part of the platform's release process: every new
release of `gentian-os` runs `make e2e-p3` (shadow tenant) and `make
e2e-p4` (cutover smoke) against a dedicated test cluster before being
released.

Additionally, two new ongoing E2E checks:

| Check | Frequency | Validates |
|---|---|---|
| `make e2e-tenant-create` | Every PR to `gentian-deployments` | Creating a new test tenant from scratch reaches Ready under 5 minutes |
| `make e2e-tenant-delete` | Every PR to `gentian-deployments` | Deleting that tenant cleanly removes all MRs (no orphans in Vault, Argo, K8s) |

These guard against composition regressions that unit tests cannot
catch (provider behaviour, real OpenBao policy enforcement, real
Argo sync timing).

---

## 10. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Composition function rendering performance at high tenant count | Medium | Medium | Phase 5 includes a load test creating 100 tenants in dev; if reconcile latency > 30s/tenant, fall back to KCL function (faster than go-templating for large outputs) |
| `provider-vault` does not support `managementPolicies: [Observe, Create]` semantics on KV-v2 paths | Low | High (could overwrite live secrets) | Validated explicitly in Phase 1 unit + E2E; if missing, wrap KV writes in `provider-kubernetes` `Job` MRs that use `kv_put_once` (same script as today) |
| Adopting existing operator CRs (Phase 4 step 5) recreates them instead | Medium | High (downtime) | Phase 4 dry-run via `crossplane render` first; manual review of `crossplane.io/external-name` annotations; keep backups |
| Team unfamiliarity slows progress | High | Low | Phase 3 unit-test suite doubles as training material; pair-program first 2 AppProfile renders |
| Regression in Pattern B chart secret handling leaks to Argo UI | Low | High (security) | Admission policy from §4.2 unit test rejects plaintext-in-spec; secret-leak grep in every E2E run |

---

## 11. Done Criteria

The migration is **done** when, in production:

1. Every kernel resource is an MR composed from a `Cluster` XR (no
   Tofu state file exists).
2. Every tenant resource is an MR composed from a `Tenant` XR (the Go
   orchestrator is not running).
3. Every Pattern B chart is a `provider-helm` `Release` MR and every
   per-tenant app is an `XApp` claim (Tofu Controller is uninstalled).
4. The `gentian-os` repo no longer contains a Go module (`go.mod`
   removed) or HCL files (`*.tf` removed), and `scripts/install-lib.sh`
   is removed once no longer needed by `install.sh`.
5. The full unit-test suite is green in CI.
6. The full E2E smoke (`make e2e-p3 && make e2e-p4`) passes on every
   release candidate.
7. A test user can create a tenant from scratch via the standard CR
   path, see all apps reach Healthy in under 10 minutes, log in via
   SSO, and exercise an IntegrationBinding (e.g., open a Nextcloud
   file from OX App Suite) — and then delete the tenant cleanly.

Each criterion is independently verifiable by `kubectl`, `git grep`, or
a browser session — no hidden state, no implicit guarantees.
