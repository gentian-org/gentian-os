# Gentian OS — Crossplane Migration Plan

**Version:** 0.3
**Status:** In progress — P0 ✅  P1 ✅  P2A ✅  P2B ✅  P2C 🔄
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
- [ ] Secret-leak grep returns zero results (to be re-confirmed in CI).

### 5.5 Rollback

```bash
# Re-add infra-workspaces-dev to kernel/services/tofu/manifests/dev/terraform.yaml
# ArgoCD syncs → Tofu Controller re-applies infra-workspaces workspace.
# provider-helm Release MRs can coexist temporarily (same Helm release name,
# Tofu wins on next apply because it owns the Helm state).
# Delete the provider-helm MRs once Tofu is confirmed healthy.
```

---

## 5b. Phase 2C — Migrate Per-Tenant App Provisioning to Crossplane 🔄

**Scope:** Replace the per-tenant Tofu Controller workspaces
(`kernel/tofu/tenant/app-workspace`, `kernel/tofu/tenant/keycloak-config`,
`kernel/tofu/tenant/ox-workspace`) — currently emitted as `Terraform` CRs
by the `AppReconciler` (see
[internal/controller/app_reconciler.go](../internal/controller/app_reconciler.go))
— with native Crossplane resources composed from the `AppProfile` /
`Tenant` spec.

After this phase the Tofu Controller binary itself can be uninstalled
(no remaining workspaces) and `kernel/tofu/` can be deleted from the repo.
This collapses the kernel’s deployment plane to a single technology
(Crossplane + ESO) and is a hard prerequisite for the clean cutover
in Phase 4 / Phase 5.

### 5b.0 Why this is feasible (architectural note)

The `app-workspace` Tofu module today does five things, each of which
has a direct Crossplane equivalent already proven in P2B:

| Tofu thing | Crossplane equivalent (proven in P2B) |
|---|---|
| `helm_release.app` | `helm.crossplane.io/Release` MR with `valuesFrom.secretKeyRef` |
| `templatefile()` rendering of secret values | ESO `ExternalSecret` with a `template` block |
| `kubernetes_*` ad-hoc resources | `kubernetes.crossplane.io/Object` MRs |
| `terraform.tfstate` in MinIO `tofu-state` bucket | Crossplane MR `status` (no separate state store) |
| `dynamic "app_secrets"` map | composition function (`function-go-templating` or `function-kcl`) translating `AppProfile.spec.appSecrets[]` into `valuesFrom` entries |

The `keycloak-config` workspace is a thin wrapper around the
`provider-keycloak` resources already deployed in P1; it migrates to
plain `provider-keycloak` MRs composed from the AppProfile.

The `ox-workspace` workspace is identical to `app-workspace` plus the
OX-specific `templatefile()` step that renders `appsuite.properties`
from OpenBao-derived values — also expressible via ESO `template`.

### 5b.1 Deliverables

- [ ] **`XApp` XRD + Composition** (`crossplane/xrds/app.yaml`,
      `crossplane/compositions/app-default.yaml`) — one XR instance per
      `(tenant, app)` pair. The XR spec mirrors the relevant subset of
      `AppProfile` + `Tenant`: `chart`, `version`, `repository`,
      `extraValues`, `appSecrets[]`, `tenantNamespace`, `domain`.
- [ ] **Composition function** — either pin
      `xpkg.upbound.io/crossplane-contrib/function-go-templating` or
      `function-kcl` (decision in §5b.0). Function reads the XR and emits:
      - 1× `ExternalSecret` (Pattern B `sensitive-values.yaml` template,
        `dataFrom` referencing the `AppProfile.spec.appSecrets[*].valuePath`).
      - 1× `helm.crossplane.io/Release` MR consuming the rendered values.
      - 0..n× `kubernetes.crossplane.io/Object` MRs for any
        `AppProfile.spec.extraResources[]` (Secrets, ConfigMaps,
        NetworkPolicies the chart does not ship).
- [ ] **`XKeycloakAppConfig` XRD + Composition** — emits
      `provider-keycloak` MRs for the per-app realm role / client-scope /
      protocol-mapper objects currently created by the
      `keycloak-config` workspace.
- [ ] **`XOXAppSuite` Composition variant** — reuses the `XApp`
      Composition pipeline plus an extra ESO `ExternalSecret` that
      renders `appsuite.properties` (the only ox-specific templating).
      The dedicated `ox-workspace` Tofu module is retired.
- [ ] **`AppReconciler` rewrite** (Go) —
      [internal/controller/app_reconciler.go](../internal/controller/app_reconciler.go)
      stops emitting `Terraform` CRs and instead emits `XApp` /
      `XKeycloakAppConfig` / `XOXAppSuite` claims. The constants
      `tofuSystemNamespace`, `tofuGitRepositoryName`, `tofuModulePath`,
      `tofuFinalizer`, `tofuStateBucket`, `tofuStateEndpoint` are
      removed. `DeploymentMethod` enum loses the `tofu-controller`
      variant (becomes `crossplane` only).
- [ ] **State migration helper** — `crossplane/e2e/scripts/p2c-adopt-app.sh`
      that, for every existing tenant’s active `Terraform` CR:
      1. Reads the live Helm release name + namespace from the Tofu
         state in MinIO.
      2. Creates the corresponding `XApp` claim with
         `crossplane.io/external-name: <release-name>` so provider-helm
         **adopts** the existing release in place (no pod restart, no
         secret rotation).
      3. Removes the `Terraform` CR (`kubectl delete --wait=false` plus
         finalizer cleanup if the controller is gone).
- [ ] **Tofu Controller uninstall** — once all `Terraform` CRs are gone
      across all envs:
      - `helm uninstall tofu-controller -n tofu-system`
      - Delete `kernel/tofu/` from the repo.
      - Delete `kernel/services/tofu/` and `AppSet 05-tofu.yaml`.
      - Delete `tofu-state` MinIO bucket (after a 30-day retention
        snapshot stored out-of-band).
- [ ] **install.sh / uninstall.sh updates** — drop the Tofu install
      step; add `XApp` claim drain to uninstall (mirrors the P2B
      Pattern B Release drain in Step 1b).
- [ ] **Documentation** —
      [docs/architecture-crossplane.md](architecture-crossplane.md)
      updated to describe `XApp` as the per-app composition root;
      [docs/architecture-legacy.md](architecture-legacy.md) marked as
      historical-only.

### 5b.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/render/xapp-collabora/` | XApp XR + Collabora AppProfile fixture renders the expected Release + ExternalSecret golden YAML |
| `tests/unit/render/xapp-element/` | Same for Element (multiple `appSecrets`: oidc, smtp, db) |
| `tests/unit/render/xapp-extra-resources/` | An AppProfile declaring `extraResources[]` produces matching `provider-kubernetes` Object MRs in dependency order |
| `tests/unit/render/xkeycloakappconfig-default/` | Realm role + client-scope + protocol-mapper MRs match what `keycloak-config` Tofu emits today |
| `tests/unit/render/xoxappsuite/` | OX appsuite.properties ExternalSecret template renders all expected keys; OIDC realm role MR is emitted |
| `tests/unit/functions/render-appsecrets/test_valuepath_to_valuesfrom.py` | `appSecrets[].valuePath` correctly resolves to a `dataFrom.extract` ref + a `valuesFrom` Helm key |
| `tests/unit/orchestrator/app_reconciler_emits_xapp_test.go` | `AppReconciler` emits an `XApp` claim (not a `Terraform` CR) for every AppProfile with `deploymentMethod: crossplane` |
| `tests/unit/orchestrator/app_reconciler_no_tofu_constants.go` | Constants `tofuSystemNamespace` etc. no longer exist in the Go module (compile-time guard) |

### 5b.3 E2E test (dev cluster)

`make e2e-p2c`:

```text
PRE-CHECKS
  1. Snapshot all live Helm release revisions, OpenBao secret versions,
     and tenant OIDC client IDs to a fixture file.
  2. CNPG base backup of every per-tenant database (safety net).

ADOPTION
  3. For each Terraform CR in tofu-system:
     a. Run p2c-adopt-app.sh <terraform-cr> — creates XApp claim with
        crossplane.io/external-name set to existing release name.
     b. kubectl wait --for=condition=Ready xapp/<name>
     c. Verify Helm revision counter unchanged (provider-helm adopted,
        not re-installed).
     d. Delete the Terraform CR.
  4. Repeat for keycloak-config workspaces (XKeycloakAppConfig).
  5. Repeat for ox-workspace (XOXAppSuite).

POST-CHECKS
  6. Diff post-state vs pre-state. Allowed differences:
       - ownerReferences on Helm Secrets / Keycloak CRs (now point at XR).
       - Crossplane controller annotations.
     No Helm revision bump, no pod restart, no OpenBao secret rotation.
  7. Browser smoke: login via Keycloak, open Nextcloud, edit a file in
     Collabora (exercises adopted Release + IntegrationBinding), open
     OX App Suite mailbox.
  8. 30-minute soak: kubectl get xapps,releases.helm.crossplane.io
     shows zero churn (no spec drift, no reconcile thrash).
  9. helm uninstall tofu-controller && verify no orphan finalizers.
 10. Grep repo: `grep -r tofu-system\|tofuSystemNamespace .` returns
     only history/changelog matches.
```

### 5b.4 Acceptance

- [ ] Zero `Terraform` CRs exist in any environment.
- [ ] Tofu Controller chart uninstalled; `kernel/tofu/`,
      `kernel/services/tofu/`, `kernel/appsets/05-tofu.yaml` removed
      from the repo.
- [ ] `internal/controller/app_reconciler.go` contains no `tofu*`
      identifiers.
- [ ] All AppProfiles in `gentian-apps/profiles/` have
      `deploymentMethod: crossplane` (or the field is removed entirely).
- [ ] `gentian-os` Go binary builds with no `terraform` /
      `tf.contrib.fluxcd.io` imports.
- [ ] CI matrix removes the Tofu image and the Tofu Helm chart from
      the dev install workflow.
- [ ] Phase 2C E2E (`make e2e-p2c`) green on dev cluster.

### 5b.5 Rollback

P2C is the most invasive phase before the Tenant XRD work because it
deletes a controller. Rollback paths, in increasing cost:

1. **Per-app rollback (cheap).** While Tofu Controller is still
   installed, recreate the matching `Terraform` CR and delete the
   `XApp` claim with `--cascade=orphan` so the Helm release survives.
   Tofu re-adopts on next sync.
2. **Phase rollback (medium).** Revert the `AppReconciler` change so it
   emits `Terraform` CRs again; redeploy from the previous container
   tag; re-create all `Terraform` CRs from the per-tenant fixtures
   captured in step 1 of the E2E run.
3. **Post-uninstall rollback (expensive).** Reinstall Tofu Controller
   from the chart pinned in `kernel/services/tofu/`; restore the
   `tofu-state` MinIO bucket from the 30-day snapshot; restore the
   `kernel/tofu/` directory from `git revert`. Live Helm releases are
   not touched in any of these steps; only the controller that owns
   their state changes.

---

## 6. Phase 3 — `Tenant` XRD Shadow Deployment

**Scope:** Stand up the `Tenant` XRD + Composition Pipeline, but apply
it only against **shadow tenants** in dev — namespaces named
`tenant-shadow-*` that exist in parallel to the real tenants managed
by the Go orchestrator. Compare resulting MRs to legacy state before
any cutover.

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
1. Apply a shadow Tenant: tenant-shadow-alpha with apps [notes].
2. kubectl wait --for=condition=Ready tenant.gentianos.io/shadow-alpha
3. Diff every composed MR against what the legacy Go orchestrator
   produces for an equivalent input. Differences must be limited to:
     - resource names (shadow- prefix)
     - namespace (tenant-shadow-alpha)
     - labels (gentianos.io/shadow=true)
   Any other diff = FAIL.
4. Verify the shadow Argo Application syncs Healthy.
5. Browser check: the shadow Notes URL responds 200 and SSO works
   against a shadow Keycloak realm.
6. Apply a multi-app shadow Tenant: tenant-shadow-beta with apps
   [nextcloud, ox-appsuite, openproject]. Verify all 3 apps Healthy
   AND the filepicker IntegrationBinding shows status Ready.
7. Delete tenant-shadow-alpha. Verify all composed MRs are GC'd via
   ownerReferences and the namespace is removed.
```

Operator visibility: `kubectl get xtenants` shows shadow tenants
side-by-side with legacy tenants; conditions explain readiness;
`crossplane trace tenant/shadow-alpha` walks the entire MR graph.

### 5.4 Acceptance

- All unit tests green (full catalogue coverage).
- Shadow tenants reach Ready in under 5 minutes (architecture.md §4.2
  baseline).
- Diff against legacy Go orchestrator output is empty modulo the
  documented shadow-prefix differences.
- Manual browser smoke against shadow tenant URLs passes.

### 5.5 Rollback

```bash
kubectl delete tenants -l gentianos.io/shadow=true
kubectl delete -f crossplane/xrds/tenant.yaml
```

The legacy Go orchestrator never stopped running; real tenants are
unaffected throughout this phase.

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
