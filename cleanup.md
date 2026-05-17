# Cleanup & Known Issues

Issues found during a full cross-repo review (gentian-os, gentian-apps,
gentian-deployments) on 2026-05-17.  Severity: 🔴 Critical · 🟠 High · 🟡 Medium · 🔵 Low

---

## 🔴 Critical

### 1. Cluster claim hardcodes `desk.gentian.org` — `KERNEL_DOMAIN` from `install.env` is ignored
**File:** `crossplane/claims/dev-cluster.yaml`  
`install.sh` applies `dev-cluster.yaml` verbatim. The `kernelDomain` and
`letsencryptEmail` fields are hardcoded to `desk.gentian.org`, so setting a
different `KERNEL_DOMAIN` in `install.env` has no effect on the cluster.  
**Fix:** Template the claim; `install.sh` runs `envsubst` before applying.
`make e2e-p1` continues to use the static `.yaml` with the example domain.
**Status:** ✅ Fixed — `crossplane/claims/dev-cluster.yaml.tmpl` introduced;
`install.sh` now uses `envsubst`.

---

### 2. `minio/mc:latest` provisioner image — unpinned
**File:** `internal/controller/storage_reconciler.go:38`  
Every S3 bucket provisioning Job pulls `minio/mc:latest` at runtime. A
breaking upstream release silently breaks all tenant storage provisioning.  
**Fix:** Pin to `minio/mc:RELEASE.2025-04-03T17-07-56Z`.  
**Status:** ✅ Fixed.

---

### 3. `gtn-demo-2` excluded from `dev/tenants/kustomization.yaml`
**File:** `gentian-deployments/dev/tenants/kustomization.yaml`  
`gtn-demo-2` has a complete Kustomize definition but is not listed in the
top-level kustomization, so ArgoCD never syncs it.  
**Fix:** Add `- instances/gtn-demo-2` to the resources list.  
**Status:** ✅ Fixed.

---

### 4. ~~`gtn-demo-2` domain collision with `gtn-demo`~~ — *Not a real issue*
When `spec.domain` is unset, `EffectiveDomain()` returns
`<name>.<kernelDomain>`, so `gtn-demo-2` resolves to
`gtn-demo-2.<kernelDomain>` — distinct from `gtn-demo`'s explicit vanity
domain `desk.gentian.org`. No collision.

---

### 5. `jitsi.yaml` uses `subdomain` (lowercase) — silently ignored by API
**File:** `gentian-apps/profiles/jitsi.yaml:104`  
The CRD field is `json:"subDomain,omitempty"` (camelCase). The profile uses
`subdomain` (lowercase), which Kubernetes ignores on apply. The ingress
subdomain falls back to the operator default instead of the intended `meet`.  
**Fix:** Change `subdomain` → `subDomain`.  
**Status:** ✅ Fixed.

---

## 🟠 High

### 6. `infraNamespace`/`servicesNamespace` hardcoded in Go binary
**File:** `internal/controller/tenant_controller.go:62,66`  
`infraNamespace = "gentian-infra-dev"` and `servicesNamespace = "gentian-dev"`
are compiled constants. The operator deployed to staging/prod always reads
registry credentials and writes NetworkPolicy rules for the dev namespaces.
A `TODO` comment on line 61 acknowledges this.  
**Fix:** Read from env vars `INFRA_NAMESPACE` / `SERVICES_NAMESPACE` with
current values as defaults; inject via Helm values.
**Status:** ✅ Fixed — `infraNamespace`/`servicesNamespace` are now `var` driven
by `os.Getenv`; Helm `deployment.yaml` and `values.yaml` updated.

### 7. `ox-appsuite.yaml` LDAP host hardcoded to dev cluster
**File:** `gentian-apps/profiles/ox-appsuite.yaml:133`  
`nubus-dev-ldap-server.gentian-dev.svc.cluster.local`, the base DN
`dc=swp-ldap,dc=internal`, and the bind DN are hardcoded dev values.
OX App Suite cannot be deployed to staging/prod without editing this
cluster-scoped CR. A `TODO` comment in the file acknowledges this.  
**Fix:** Source LDAP connection details from a cluster-scoped ConfigMap or
from the Cluster XR status; remove hardcoded values from the AppProfile.
**Status:** ✅ Fixed — `install.sh` upserts `gentian-cluster-config` ConfigMap
in `crossplane-system`; `app-ox.yaml` fetches it via `function-extra-resources`
and substitutes `${LDAP_HOST}`, `${LDAP_BASE_DN}`, `${LDAP_BIND_DN}` in
`extraValues.raw`; `ox-appsuite.yaml` now uses placeholders.

### 8. Cluster claim `kernelDomain` disconnected from Tenant `domain` source
Both `dev-cluster.yaml` (Cluster claim) and `gtn-demo/patch.yaml` (Tenant)
independently specify the platform domain. There is no single source of truth.  
**Fix:** Tenants without an explicit `spec.domain` already fall back to
`<name>.<kernelDomain>` via `EffectiveDomain()`; document this as the
canonical pattern and discourage overriding `domain` in Tenant CRs unless
a custom vanity domain is truly needed.
**Status:** ✅ Fixed — `docs/architecture.md` §6 updated with canonical pattern.

### 9. `VAULT_TOKEN` exported to process environment in `install.sh:260`
`export VAULT_TOKEN="${BAO_TOKEN}"` makes the root token visible via
`/proc/<pid>/environ` and `ps aux e`.  
**Fix:** Pass the token per-command via `env VAULT_TOKEN=... bao ...` or
write to a restricted temp file, unexport after bootstrap completes.
**Status:** ✅ Fixed — `unset VAULT_TOKEN` added at the end of
`bootstrap_openbao_for_crossplane()` so the root token is scrubbed from
the process environment for the remainder of the install run.

### 10. `crossplane/functions/derive-secrets/` is dead code
The Python secret derivation function is not referenced by any Composition,
XRD, or Function manifest. Derivation happens inside `install.sh` via
`_derive`. The directory misleads readers into thinking Crossplane derives
secrets independently.  
**Fix:** Remove or archive `crossplane/functions/derive-secrets/`.
**Status:** ✅ Fixed — directory deleted.

---

## 🟡 Medium

### 11. Staging/prod `image-updater.yaml` target non-existent Applications
`staging/kernel/image-updater.yaml` targets `staging-kernel-os`;
`prod/kernel/image-updater.yaml` targets `prod-kernel-os`. Neither
Application CR exists in the repo. The updater CRDs are inert.
**Status:** ✅ Fixed — `namePattern` corrected to `gentian-os` in both files.

### 12. KV mount hardcoded in policy rules despite being a spec field
`cluster-default.yaml` composition emits policy rules with literal
`path "secret/data/gentian-os/*"` even though `spec.openbao.kvMount` is
configurable. Using a non-default mount name silently produces a wrong policy.
**Status:** ✅ Fixed — `install.sh` bootstrap_openbao_for_crossplane() now uses
`_kv_mount=${KV_MOUNT:-secret}` throughout. `dev-cluster.yaml.tmpl` uses
`${KV_MOUNT}`. `install.env.template` documents the variable.

### 13. No enforcement of `gentianos.io/profile-name` label on AppProfiles
`function-extra-resources` fetches AppProfiles by this label. All current
profiles have it manually, but there is no webhook or controller to enforce
or auto-apply it. A new profile without the label causes a silent composition
failure (`minMatch: 1` not satisfied).
**Status:** ✅ Fixed — AppStoreReconciler now auto-sets the `gentianos.io/profile-name`
label on any AppProfile where it is missing or incorrect.

### 14. Memcached chart version/repo hardcoded in Go binary
`cache_reconciler.go:40-41` — upgrading requires an operator redeploy, not
a config change.
**Status:** ✅ Fixed — `memcachedChartRepo/Name/Version` are now `var` driven by
`MEMCACHED_CHART_REPO/NAME/VERSION` env vars. Helm `values.yaml` exposes
`memcached.chartRepo/Name/Version`; `deployment.yaml` injects them.

### 15. No credential rotation mechanism
All credentials use `PutOnce` semantics. There is no `kubectl gentian rotate`
command or any CronJob to refresh OIDC secrets, database passwords, or S3
keys.
**Status:** 🟡 Open — out of scope for this review; tracked separately.

### 16. `uninstall.sh` strips Tenant finalizers without waiting for operator
The force-strip of finalizers may orphan resources if the operator is
mid-reconcile.
**Status:** ✅ Fixed — uninstall.sh now checks whether the operator pod is Running
before force-stripping; if running, extends the timeout by 30 s to allow
in-flight reconciliation to complete before forceful removal.

### 17. Integration binding reconciler is a stub
`ensureIntegrationBindings` is called from the main reconcile loop but
contains no auto-wiring logic. The `optionalIntegrations` fields in element,
jitsi, and ox-appsuite never produce bindings.
**Status:** ❌ Not a real issue — `ensureIntegrationBindings` is fully implemented:
it fetches AppProfiles, resolves `optionalIntegrations`, finds matching
providers in the tenant, and creates/GCs `IntegrationBinding` CRs. The
bindings depend on AppProfiles declaring `optionalIntegrations` and `provides`
(see issue 19).

---

## 🔵 Low

### 18. `dev/kernel/tofu.tfvars` is a leftover from the old OpenTofu setup
**File:** `gentian-deployments/dev/kernel/tofu.tfvars`  
OpenTofu was replaced by Crossplane. This file has no current consumer.  
**Fix:** Remove.

### 19. `ox-appsuite` AppProfile missing `provides` field
Other profiles declare what contracts they expose. OX App Suite does not,
making it invisible to IntegrationBinding auto-wiring.

### 20. `cpu: '32'` uses single-quoted string quota
**File:** `gentian-deployments/dev/tenants/definitions/gtn-demo-base/tenant.yaml`  
Inconsistent with `memory: 32Gi` and `storage: 100Gi`. Use unquoted or
double-quoted.

### 21. `argocd.project: "gentian"` in `values-dev.yaml` vs AppProject `gentianos-tenants`
**File:** `gentian-deployments/dev/kernel/values-dev.yaml`  
These need to be aligned or ArgoCD project restriction will be mis-set.

### 22. `gtn-demo-2` patched via two mechanisms in `kustomization.yaml`
Name is patched both via `patch.yaml` and an inline strategic-merge patch.
Only one is needed.

### 23. `prod.yaml` TODO storageClass still open
**File:** `gentian-os/kernel/values/env/prod.yaml:23`  
`storageClass: longhorn  # TODO: adjust to actual prod storage class`
Will be applied verbatim to prod.

### 24. Unused `KVClient.List()` in `internal/kernel/secrets/openbao.go`
Implemented but never called outside tests.
