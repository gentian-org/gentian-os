# Gentian OS — Crossplane Migration Plan

**Version:** 0.2
**Status:** In progress — P0 ✅  P1 ✅
**Companion to:** [architecture.md](architecture.md), [architecture-crossplane.md](architecture-crossplane.md)

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

## 4. Phase 2 — Migrate Pattern B Charts to `provider-helm`

**Scope:** Replace the Tofu Controller `set_sensitive` releases for
Pattern B charts (Nubus, OX App Suite — see architecture.md §5.3) with
`provider-helm` `Release` MRs that consume secrets via
`valuesFrom: secretKeyRef`.

### 4.1 Deliverables

- [ ] One `Release` MR per Pattern B chart, referencing OpenBao-backed
      Secrets (synced by ESO) for sensitive values.
- [ ] An ESO `ExternalSecret` per chart that materialises a single
      `values.yaml`-shaped Secret for `valuesFrom`.
- [ ] Argo Application updated to track the `Release` MR instead of the
      Tofu workspace.

### 4.2 Unit tests

| Test | Validates |
|---|---|
| `tests/unit/render/release-nubus/` | Generated `Release` MR has correct chart coordinates, `valuesFrom` refs, no plaintext secrets |
| `tests/unit/render/release-ox-appsuite/` | Same for OX |
| `tests/unit/schema/invalid/release-with-plaintext-secret.yaml` | A `Release` MR that puts a secret literal into `set:` (not `valueFrom:`) is rejected by an admission policy (Kyverno or `validatingAdmissionPolicy`) |

The plaintext-rejection policy is part of the deliverable — it
guarantees future Compositions cannot regress the secrets posture.

### 4.3 E2E test (dev cluster)

`make e2e-p2`:

```text
1. Snapshot OpenBao + take a CNPG backup of Nubus's database (safety net).
2. Pause the legacy Tofu Controller GitRepository for Nubus.
3. Apply the Crossplane Release MR for Nubus.
4. kubectl wait --for=condition=Synced release.helm.crossplane.io/nubus
5. Verify pods restart cleanly (Reloader picks up the Secret).
6. Login as a test user via Keycloak (browser check, scriptable with
   chromedp or by curl-ing the OIDC discovery endpoint and decoding a
   token).
7. Repeat for OX App Suite.
8. Grep all rendered Argo manifests and Crossplane MR specs for known
   secret values: must return zero matches.
```

Operator-visible verification: ArgoCD UI now shows a `Release` resource
where it previously showed a Tofu workspace; clicking through reveals
the chart name and `valuesFrom` references but no plaintext.

### 4.4 Acceptance

- Nubus and OX App Suite running healthily on `provider-helm` releases.
- Argo UI shows them as Synced + Healthy.
- Secret-leak grep returns zero results.
- Existing tenant logins continue to work (no realm/user disruption).

### 4.5 Rollback

```bash
kubectl delete release.helm.crossplane.io/nubus
# Resume Tofu Controller GitRepository → Tofu re-deploys the chart
# from the same OpenBao secrets. No data loss because backing DBs/PVs
# are untouched.
```

---

## 5. Phase 3 — `Tenant` XRD Shadow Deployment

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

## 6. Phase 4 — Cutover of a Real Tenant

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

## 7. Phase 5 — Migrate All Tenants, Decommission Legacy Stack

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

## 8. Continuous Verification After Migration

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

## 9. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Composition function rendering performance at high tenant count | Medium | Medium | Phase 5 includes a load test creating 100 tenants in dev; if reconcile latency > 30s/tenant, fall back to KCL function (faster than go-templating for large outputs) |
| `provider-vault` does not support `managementPolicies: [Observe, Create]` semantics on KV-v2 paths | Low | High (could overwrite live secrets) | Validated explicitly in Phase 1 unit + E2E; if missing, wrap KV writes in `provider-kubernetes` `Job` MRs that use `kv_put_once` (same script as today) |
| Adopting existing operator CRs (Phase 4 step 5) recreates them instead | Medium | High (downtime) | Phase 4 dry-run via `crossplane render` first; manual review of `crossplane.io/external-name` annotations; keep backups |
| Team unfamiliarity slows progress | High | Low | Phase 3 unit-test suite doubles as training material; pair-program first 2 AppProfile renders |
| Regression in Pattern B chart secret handling leaks to Argo UI | Low | High (security) | Admission policy from §4.2 unit test rejects plaintext-in-spec; secret-leak grep in every E2E run |

---

## 10. Done Criteria

The migration is **done** when, in production:

1. Every kernel resource is an MR composed from a `Cluster` XR (no
   Tofu state file exists).
2. Every tenant resource is an MR composed from a `Tenant` XR (the Go
   orchestrator is not running).
3. Every Pattern B chart is a `provider-helm` `Release` MR (Tofu
   Controller is not running).
4. The `gentian-os` repo no longer contains a Go module (`go.mod`
   removed) or HCL files (`*.tf` removed).
5. The full unit-test suite is green in CI.
6. The full E2E smoke (`make e2e-p3 && make e2e-p4`) passes on every
   release candidate.
7. A test user can create a tenant from scratch via the standard CR
   path, see all apps reach Healthy in under 10 minutes, log in via
   SSO, and exercise an IntegrationBinding (e.g., open a Nextcloud
   file from OX App Suite) — and then delete the tenant cleanly.

Each criterion is independently verifiable by `kubectl`, `git grep`, or
a browser session — no hidden state, no implicit guarantees.
