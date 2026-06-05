# Operations: Backup, DR, Observability, Image Updates

**Companion to:** [architecture.md](../architecture.md)

---

## 1. Backup Strategy

Backup is **per subsystem** — each kernel component uses the
industry-standard tool for its data type, orchestrated centrally.

| Data type | Tool | Scope | Method |
|---|---|---|---|
| PostgreSQL databases | **pgBackRest** or CloudNativePG built-in | Per-database (tenant-scoped restores) | WAL archiving + base backups to S3 |
| MariaDB databases | **Mariabackup** or MariaDB Operator backup CRD | Per-database | Full + incremental to S3 |
| S3 / MinIO buckets | **MinIO replication** or **Restic** | Per-bucket (tenant-scoped) | Cross-site replication or snapshot to external S3 |
| Dovecot mailboxes | **dsync** or **Restic** | Per-domain (`/var/mail/{domain}/`) | Filesystem-level backup or Dovecot-native sync |
| Keycloak realms | **Keycloak realm export** (JSON) | Per-realm (tenant-scoped) | Scheduled export to S3, versioned |
| OpenBao secrets | **OpenBao snapshots** (`bao operator raft snapshot`) | Full vault | Raft snapshots to S3, encrypted |
| Kubernetes resources | **Velero** | Per-namespace (tenant-scoped) | CRD state, ConfigMaps, Secrets (encrypted) |
| LDAP directory | **slapcat** or UDM backup API | Per-OU (tenant-scoped) | LDIF export to S3 |

**Velero** serves as the cross-cutting backup orchestrator for
Kubernetes-native resources (CRDs, ConfigMaps, namespace metadata)
and triggers pre/post-backup hooks coordinating with the
application-specific tools.

## 2. Tenant-Scoped Restore

The per-tenant isolation model (separate databases, buckets, realms,
namespaces) enables **single-tenant restore** without touching others.
Restoring tenant `demo` means:

1. Restore PostgreSQL databases matching `demo_*` from pgBackRest.
2. Restore MinIO buckets matching `demo-*`.
3. Restore Dovecot mailboxes for the tenant mail domain.
4. Re-import the Keycloak realm `demo` from JSON export.
5. Restore namespace `tenant-demo` via Velero.

This sequence will be automated by a `RestoreTenant` CR in a future
release.

## 3. Tenant Migration Between Clusters

Same backup/restore pattern: backup on source, restore on target,
update DNS. The key requirement is that OpenBao secrets are either
migrated or re-provisioned. Re-provisioning is the natural path —
applying the Tenant CR on the target cluster triggers the full
Crossplane Composition, which picks up the existing data from the
restored databases and buckets.

## 4. Disaster Recovery

For full-cluster DR, recovery follows the deployment layers:

1. Bootstrap: install ArgoCD + Crossplane (one-shot script).
2. Apply the `Cluster` XR — Crossplane provisions kernel
   infrastructure from declared state.
3. Restore data: OpenBao snapshots, database backups, S3 replication.
4. Apply Tenant CRs — the operator and Crossplane re-provision tenant
   resources; apps pick up the restored data.

GitOps ensures the desired state of all workloads is recoverable from
Git; only stateful data requires backup restoration. The deterministic
secret-derivation model (see [security.md](security.md)) means kernel
credentials can be regenerated from the master password alone if
OpenBao itself is unrecoverable.

## 5. Observability via the K8s API

Crossplane's MR status model gives uniform observability:

```bash
# Tenant health at a glance
kubectl get tenants
NAME          STATUS         APPS   READY   MAIL         AGE
demo          Ready          2      2/2     selfhosted   30d

# Tenant app installs (Crossplane claims → helm Releases)
kubectl get apps -n tenant-demo
kubectl get releases.helm.crossplane.io -n tenant-demo

# Optional: Crossplane composite for namespace/policy (if XTenant is used)
kubectl get xtenant demo 2>/dev/null || true

# Integration contract health
kubectl get integrationbindings -n tenant-demo

# Kernel / GitOps (not per-tenant app charts)
kubectl get applications -n argocd
```

Tenant apps are observed via **`App` claim status** and **helm `Release`
MRs** in `tenant-{name}`. ArgoCD Applications cover kernel services and
catalogue sync (`gentian-appprofiles`), not each tenant app install.

## 6. Metrics

Prometheus metrics exposed by Crossplane and the kernel:

| Metric | Source | Description |
|---|---|---|
| `crossplane_resource_total{kind="XTenant"}` | Crossplane | Total tenants |
| `crossplane_reconcile_duration_seconds` | Crossplane | Reconcile latency per claim kind |
| `crossplane_reconcile_errors_total` | Crossplane | Failed reconciliations |
| `crossplane_resource_ready_status` | Crossplane | Ready conditions per MR |
| `externalsecrets_sync_calls_total` | ESO | OpenBao → K8s Secret sync health |
| `argocd_app_health_status` | ArgoCD | Kernel Application health |
| `gentian_os_credentials_age_seconds` | Custom | Age of oldest credential per tenant |
| `gentian_os_integration_bindings_status` | Custom | Binding health by contract |

## 7. Image Updates via ArgoCD Image Updater

### 7.1 Philosophy

The kernel is a **singular shared service**: one kernel version per
cluster, all tenants on the same version. This is intentional — kernel
upgrades are platform-wide and atomic, unlike app upgrades which can
roll per-tenant.

App upgrades are catalogue-wide: bumping an `AppProfile`'s chart
version propagates to every tenant referencing the profile via
Crossplane helm `Release` reconciliation (and operator-driven
`App` claim updates when `spec.apps` changes).

### 7.2 Per-Environment Update Policies

A per-environment `ImageUpdater` CR watches the registry and updates
the kernel `Application` whenever a new image is published:

| Environment | Policy | Target tag |
|---|---|---|
| **dev** | Aggressive — track latest develop builds | `latest`, `develop`, `newest-build` |
| **staging** | Track release candidates | `semver:v*-rc.*` |
| **prod** | Conservative — only released semver | `semver:v1.x.x` |

```yaml
apiVersion: argocd-image-updater.argoproj.io/v1alpha1
kind: ImageUpdater
metadata:
  name: gentian-os-kernel-prod
  namespace: argocd
spec:
  applicationRefs:
    - namePattern: "prod-kernel-os"
      images:
        - imageName: ghcr.io/gentian-org/gentian-os
          policy: semver:v1.*
          tagsMatchRegex: '^v[0-9]+\.[0-9]+\.[0-9]+$'
          ignoreTagsRegex: '^.*-(rc|alpha|beta)\..*$'
  updateMethod:
    method: argocd          # patch Application params, no Git commit
  webhook:
    enabled: true            # immediate update on registry push
```

The full flow takes 30–60 seconds from image push to running new
pods.

### 7.3 Tenant Impact

Tenants are not parameterised by kernel version — when the kernel
updates, all tenants automatically use the new kernel. This is the
intended behaviour: cluster admins manage kernel versions, tenant
admins do not.

## 8. Safety Guards

- **Stateful Argo Apps** (OpenBao, Crossplane) deploy with `prune: false` and `finalizers: []` —
  prevents Argo from ever deleting them if their manifests are
  temporarily missing from Git. Self-healing remains on for value
  drift.
- **OpenBao paths** managed by Crossplane use
  `managementPolicies: [Observe, Create]` — never overwrite live
  credentials. See [security.md](security.md).
- **Plaintext-secret admission policy** rejects any `Release` MR that
  literally embeds a secret value instead of referencing one.
- **Backup verification** runs daily: pgBackRest verify, MinIO
  replication lag check, OpenBao snapshot integrity check. Failures
  alert on the platform team's PagerDuty.
