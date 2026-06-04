# Getting Started — Gentian OS

This guide covers the prerequisites and steps to bootstrap a Gentian OS kernel
cluster using **`install.sh`** (Phase 1 + Phase 2). After completing this guide
you will have Crossplane, cert-manager, External Secrets Operator, ArgoCD, and
OpenBao running, with all kernel structural resources provisioned by the Cluster XR,
and Nubus deployed via the `provider-helm` Release CR.

---

## Prerequisites

### CLI tools

| Tool         | Minimum version | Install hint |
|--------------|-----------------|--------------|
| `kubectl`    | 1.27+           | <https://kubernetes.io/docs/tasks/tools/> |
| `helm`       | 3.12+           | <https://helm.sh/docs/intro/install/> |
| `jq`         | 1.6+            | `sudo apt install jq` / `brew install jq` |
| `openssl`    | 1.1+            | usually pre-installed |
| `curl`       | any             | usually pre-installed |
| `bao`        | 2.0+            | <https://github.com/openbao/openbao/releases> |
| `crossplane` | 2.0+            | `make install-tools` (in this repo) |
| `kubeconform` | 0.6+           | `make install-tools` (in this repo) |
| `python3`    | 3.9+            | <https://python.org/downloads/> |

### Kubernetes cluster

You need a running, reachable cluster. Both `install.sh` and
`uninstall.sh` verify this at startup via `kubectl cluster-info`.

**Requirements:**
- Kubernetes 1.26+
- Default StorageClass available (tested with `nfs-csi` and `microk8s-hostpath`)
- Ingress controller installed (tested with `ingress-nginx`)
- DNS for `KERNEL_DOMAIN` (kernel UIs) and tenant app zones (`<tenant>.<kernel_domain>` or vanity domains); see [docs/design/multi-tenancy.md](docs/design/multi-tenancy.md) §3

Tested cluster distributions:
- **microk8s** (local dev, with `dns`, `hostpath-storage`, `ingress`, and
  optionally `crossplane` addon)
- Any standard kubeconfig-accessible cluster (k3s, RKE2, EKS, GKE, AKS)

> `install.sh` provisions cert-manager, CloudNativePG, and Stakater Reloader
> automatically. Pass `--no-cluster-infra` (or `INSTALL_CLUSTER_INFRA=0`) to
> skip if your cluster already provides them.

### Environment variables

`install.sh` will **prompt** for any missing value. You can also pre-export
them or store them in the config files below.

**Required:**

| Variable | Description |
|---|---|
| `MASTER_PASSWORD` | Master secret — kernel and app passwords are derived via HKDF-SHA256 |
| `OD_PRIVATE_REGISTRY_USERNAME` | `registry.opencode.de` username |
| `OD_PRIVATE_REGISTRY_PASSWORD` | `registry.opencode.de` token/password |
| `OD_SMTP_RELAY_USERNAME` | SMTP relay username (e.g. Gmail address) |
| `OD_SMTP_RELAY_PASSWORD` | SMTP relay password (e.g. Gmail App Password) |
| `KERNEL_DOMAIN` | Platform-wide DNS suffix (e.g. `platform.example.com`) |
| `EXTERNAL_SMTP_HOST` | Required when `MAIL_SERVICE_MODE=external` (default) |

**Optional:**

| Variable | Default | Description |
|---|---|---|
| `LETSENCRYPT_EMAIL` | `admin@KERNEL_DOMAIN` | Let's Encrypt ACME contact |
| `CF_API_TOKEN` | — | Cloudflare token for DNS-01 wildcard certificates |
| `NETWORK_MODE` | `tunnel` | `tunnel` or `static-ip` |
| `MAIL_SERVICE_MODE` | `external` | `external` or `internal` |

### Config files

Copy the templates before first use:

```bash
cp install.env.template install.env
cp install.secrets.env.template install.secrets.env
```

Both files are loaded automatically by `install.sh` if present. Override paths with:

```bash
./install.sh --config-file /path/to/install.env --secrets-file /path/to/install.secrets.env
```

Or disable file loading entirely:

```bash
./install.sh --no-config-files
```

### Repository variables

`install.sh` prompts for the source repositories the cluster pulls from.
Defaults point at the upstream `gentian-org` org; press `<Enter>` to accept
or override per environment:

| Variable | Default | Used by |
|---|---|---|
| `GENTIAN_APPS_REPO` | `https://github.com/gentian-org/gentian-apps` | ArgoCD `gentian-appprofiles` Application |
| `GENTIAN_APPS_BRANCH` | `main` | same |
| `GENTIAN_DEPLOYMENTS_REPO` | `https://github.com/gentian-org/gentian-deployments` | `kubectl gentian apps install/uninstall` |
| `GENTIAN_DEPLOYMENTS_BRANCH` | `main` | same |
| `GENTIAN_NONINTERACTIVE` | unset | Set to `1` in CI to skip prompts |

The chosen values are persisted to `~/.gentian/config` (mode 0600), which the
`kubectl-gentian` plugin sources at runtime.

### Cluster claim

For templated installs, set `KERNEL_DOMAIN` (and optional `LDAP_BASE_DN`)
before `install.sh` runs — it renders `crossplane/claims/dev-cluster.yaml`
from `dev-cluster.yaml.tmpl`. The checked-in `dev-cluster.yaml` example
uses `ldapBaseDn: dc=swp-ldap,dc=internal` for the dev cluster.

---

## What `install.sh` does

| Step | Component | Description |
|------|-----------|-------------|
| 0 | Crossplane | Install Crossplane core (controller + RBAC) via Helm |
| 0b | Crossplane | Apply providers from `crossplane/providers/providers.yaml` (kubernetes, vault, helm, http, keycloak, functions). **Install waits** until helm, kubernetes, vault, and core functions are Healthy |
| 0c | Crossplane | Apply XRD (`XCluster` / `Cluster`) + Composition |
| 1 | Namespaces | Create kernel namespaces |
| 2 | Cluster | Pre-warm cluster (PLEG/CRI race mitigation) |
| 3 | cert-manager | Install cert-manager via Helm |
| 3b | cert-manager | Apply ClusterIssuers (HTTP-01; DNS-01 if Cloudflare token present) |
| 4 | ESO | Install External Secrets Operator |
| 5 | ArgoCD | Install ArgoCD + configure repos + Image Updater |
| 6 | OpenBao | Bootstrap transit-seal instance (ArgoCD app) |
| 7 | OpenBao | Init transit OpenBao (auto-unseal key setup) |
| 8 | OpenBao | Bootstrap remaining kernel ArgoCD apps (openbao, reloader, cnpg, globals) |
| 9 | OpenBao | Init primary OpenBao (KV engine, recovery keys) |
| 10 | OpenBao | Bootstrap Kubernetes auth backend for Crossplane |
| 11 | Crossplane | Create 8 derived-credential K8s Secrets in `crossplane-system` |
| 12 | Cluster XR | Apply Cluster claim → kernel structural resources reconciled by `provider-vault` and `provider-kubernetes`: KV mount + policies + K8s auth backend/roles, KV seed paths (database, cache, storage, identity, mail), ArgoCD AppProject, ESO ClusterSecretStore, cert-manager ClusterIssuer |
| 12b | Secrets | Seed remaining secrets: registry, DNS/Cloudflare, internal |
| 12c _(optional)_ | TLS | Install kernel wildcard Certificate for platform UIs (requires `CF_API_TOKEN`); tenant apps use per-tenant DNS-01 wildcards via the operator |
| **13** | **Crossplane** | **Wait for `provider-helm` Healthy** |
| **14** | **Nubus** | **Create `gentian-dev` / `gentian-infra-dev` namespaces, registry Secrets, non-secret value ConfigMaps, NATS patch ConfigMap, ESO ExternalSecrets (`nubus-credentials`, `nubus-sensitive-values`), provider-helm Release CR** |
| **14b** | **LDAP scope** | **`update.sh --fix-kernel-ldap-scope`** (kernel realm LDAP search base) |
| **15** | **Operator** | **Install gentian-os controller** (CRDs + reconcilers in `gentian-system`) |
| **15b** | **Mail** | **Postfix + Dovecot** when `MAIL_SERVICE_MODE=kernel` |
| **15c** | **App catalogue** | **ArgoCD Application `gentian-appprofiles`** syncs `gentian-apps/profiles/` |

---

## Running the installer

```bash
# Verify your environment first (runs check_prereqs, exits with list of issues)
./install.sh --validate

# Full bootstrap (Phase 1 + Phase 2)
./install.sh
```

The installer is **idempotent**: re-running it after a partial failure will
skip already-completed steps.

### Save OpenBao keys when prompted

During **Step 7** (transit instance init) the script shows the transit Shamir
unseal key. During **Step 9** (primary OpenBao init) it shows the primary
recovery key and root token.

**Save all values to your password manager immediately** — they are displayed
once and written to `${OPENBAO_INIT_FILE}` (default `/tmp/openbao-init.json`)
which is never committed to Git.

The primary OpenBao instance auto-unseals via the transit instance on every
restart — no manual intervention needed after a normal reboot.

---

## After bootstrap

### Inspect Crossplane managed resources

The `Cluster` XR fans out into ~19 managed resources. Inspect them with:

```bash
# All MRs belonging to the Cluster XR composite
kubectl get managed -l crossplane.io/composite=dev-cluster

# Full dependency trace (requires crossplane CLI)
crossplane beta trace cluster dev-cluster -n crossplane-system
```

### Access ArgoCD

```
URL:      printed by install.sh at completion
Username: admin
Password: kubectl get secret argocd-initial-admin-secret -n argocd \
              -o jsonpath='{.data.password}' | base64 -d
```

### Monitor sync progress

```bash
kubectl get applicationsets -n argocd
kubectl get applications -n argocd
kubectl get pods -A
```

### Verify the App Store

```bash
# Summary view
kubectl get appcatalogue default

# Full catalogue with per-app details, versions, and tenant install counts
kubectl get appcatalogue default -o yaml

# Via the kubectl plugin
kubectl gentian apps list
```

---

## Provision your first tenant

**Catalogue** (cluster-wide): `gentian-apps/profiles/` → ArgoCD app
**`gentian-appprofiles`** (install step 15c). **Tenant apps** (per org):
edit `spec.apps` in
[`gentian-deployments`](https://github.com/gentian-org/gentian-deployments)
(e.g. `dev/tenants/instances/demo/tenant.yaml`), commit, and apply — the
operator creates `App` claims; Crossplane installs helm Releases. There is
no ArgoCD Application per tenant app.

```bash
# Example: demo tenant with Element + Jitsi
kubectl apply -f "${GENTIAN_DEPLOYMENTS_PATH}/dev/tenants/instances/demo/tenant.yaml"
# Or: kubectl gentian apps install element --tenant demo  (updates Git + applies)
```

Watch provisioning progress:

```bash
kubectl get tenant demo -w
```

The orchestrator provisions these in order:
1. Tenant namespace (`tenant-demo`)
2. Keycloak realm + OIDC clients (via Jobs in `platform-kernel`)
3. LDAP OU + bind accounts (via UDM REST API Jobs)
4. PostgreSQL databases (CloudNativePG `Database` CRs)
5. MariaDB databases (SQL Jobs)
6. MinIO S3 buckets + Nextcloud groups
7. Redis ACL users + Memcached (ArgoCD Application when cache required)
8. App deployment (`App` claims → Crossplane helm Releases)
9. Ingress + TLS certificate
10. IntegrationBinding CRs (auto-wired cross-app contracts)

Provisioning is complete when:

```bash
kubectl get tenant demo -o jsonpath='{.status.phase}'
# → Ready
```

List all tenants:

```bash
kubectl get tenants
```

Decommission a single tenant:

```bash
kubectl gentian tenants delete demo
```

Behavior depends on `spec.deletionPolicy` on the Tenant:
1. `Retain`: keep namespace/data, revoke access and remove orchestration resources.
2. `Delete`: run full cleanup (apps, identity, contracts, namespace resources).

---

## Re-seal / unseal OpenBao

The primary OpenBao (`openbao-0`) auto-unseals via `openbao-transit-0` on
every restart. No manual intervention is needed after a normal node reboot.

If the transit instance itself is sealed (e.g. after losing the
`openbao-transit-unseal` Secret):

```bash
# Unseal the transit instance once:
kubectl exec -n openbao openbao-transit-0 -- bao operator unseal "$TRANSIT_UNSEAL_KEY"
# The primary recovers automatically within seconds.
```

The transit key is stored in your password manager (`gentian/openbao-transit`).

---

## Uninstalling

```bash
# Safe mode — preserves PVC/PV data and OpenBao KV paths.
./uninstall.sh

# Force mode — also deletes data namespaces and bound PVs (dev/test only).
./uninstall.sh -f

# Also remove cert-manager, Reloader, CNPG (only if Gentian-managed).
./uninstall.sh -f --cluster-infra
```

> **Note:** OpenBao KV data is always preserved across uninstalls — `managementPolicies:
> [Observe, Create]` prevents Crossplane from deleting KV paths on XR deletion.
> Re-running `./install.sh` on the same cluster will adopt the existing secrets.

---

## Running the test suite

### Unit tests (no cluster required)

```bash
# Install tooling once
make install-tools

# Run the full unit test suite (render golden tests, XRD schema, function tests)
make test-unit
```

### E2E tests (dev cluster required)

```bash
# Install Crossplane core and verify CRDs
make e2e-p0

# Full Phase 1 kernel provisioning E2E
make e2e-p1

# Tear down
make e2e-p0-clean
```

---

## Troubleshooting

### ArgoCD shows "Unknown" status for bootstrap apps

The bootstrap Applications (OpenBao, globals) may take a few minutes to pull
and deploy. Force a sync:

```bash
kubectl annotate application openbao argocd.argoproj.io/refresh=hard -n argocd
```

### ESO `ClusterSecretStore` not ready

The `openbao` ClusterSecretStore is created by the `globals` Application. If
ESO was not yet ready when it first synced, trigger a re-sync:

```bash
kubectl annotate application globals argocd.argoproj.io/refresh=hard -n argocd
```

### Inspect Crossplane managed resources

```bash
# Overall status of all kernel MRs
kubectl get managed -l crossplane.io/composite=dev-cluster

# Detailed XR status + events
kubectl describe xcluster dev-cluster
```

### Provider not Healthy

```bash
kubectl describe provider.pkg.crossplane.io/provider-vault
kubectl get pods -n crossplane-system
```

### OpenBao auth failure in provider-vault

```bash
# Verify the K8s auth role exists
bao read auth/kubernetes/role/crossplane-provider

# Re-run bootstrap if missing (safe to re-run)
# Set BAO_TOKEN first, then:
./install.sh --validate  # shows current state
```

### ArgoCD credentials

```bash
kubectl get secret argocd-initial-admin-secret -n argocd \
  -o jsonpath='{.data.password}' | base64 -d
```

### Nubus deployment status

```bash
# provider-helm Release status
kubectl get release.helm.crossplane.io/nubus-dev
kubectl describe release.helm.crossplane.io/nubus-dev | tail -20

# ESO secret sync status
kubectl get externalsecret -n gentian-dev

# Nubus pods
kubectl get pods -n gentian-dev -l app.kubernetes.io/part-of=nubus

# Values actually applied (sensitive values are redacted by Helm)
kubectl get release.helm.crossplane.io/nubus-dev \
  -o jsonpath='{.status.atProvider.releaseDescription}'
```

### provider-helm not Healthy

```bash
kubectl describe provider.pkg.crossplane.io/provider-helm
# Check the provider pod logs
kubectl logs -n crossplane-system \
  -l pkg.crossplane.io/revision=provider-helm --tail=50
```

### ESO ExternalSecret stuck

```bash
# Check ESO operator logs
kubectl logs -n external-secrets deploy/external-secrets --tail=50

# Describe the failing ExternalSecret
kubectl describe externalsecret nubus-sensitive-values -n gentian-dev

# Verify the OpenBao path exists
bao kv get gentian-os/kernel/identity/nubus
bao kv get gentian-os/kernel/mail/postfix
```

### Crossplane managed resource stuck

```bash
# Describe the composite to see which MR is blocking
kubectl describe xcluster dev-cluster

# Describe the individual managed resource
kubectl describe <resource-kind> <resource-name>

# Check provider pod logs (e.g. provider-vault)
kubectl logs -n crossplane-system \
  $(kubectl get pods -n crossplane-system \
    -l pkg.crossplane.io/revision=provider-vault -o name | head -1)
```

### `provider-vault` authentication failing

Verify the `openbao-crossplane-token` Secret is valid:

```bash
TOKEN=$(kubectl get secret openbao-crossplane-token -n crossplane-system \
  -o jsonpath='{.data.credentials}' | base64 -d | jq -r '.token')
curl -s -H "X-Vault-Token: ${TOKEN}" \
  http://openbao.openbao.svc.cluster.local:8200/v1/auth/token/lookup-self \
  | jq .data.policies
```

If invalid, re-run `./install.sh` — step 10 mints a fresh token and recreates
the Secret.

### Tenant stuck in provisioning

Check which condition is blocking:

```bash
kubectl describe tenant <name>
```

Look for `False` conditions and check the corresponding Job logs:

```bash
kubectl get jobs -n platform-kernel -l gentianos.io/tenant=<name>
kubectl logs -n platform-kernel job/<name>-keycloak-realm
```

---

## Further reading

- [Architecture](docs/architecture.md) — full system design
- [Design docs](docs/design/) — deep-dives: kernel, multi-tenancy, secrets, mail, operations, agentic AI
- [docs/commands.md](docs/commands.md) — reference for day-2 kubectl commands
- [Roadmap](docs/roadmap.md) — planned features (rotation, SOC 2 hardening)
- [scripts/seed-openbao.sh](scripts/seed-openbao.sh) — bootstrap seed paths (operator uses HKDF-SHA256 at runtime)
- [gentian-ui CI setup](../gentian-ui/docs/ci-setup.md) — portal image build and ArgoCD rollout
- [crossplane/claims/dev-cluster.yaml](crossplane/claims/dev-cluster.yaml) — the Cluster XR claim
- [crossplane/compositions/cluster-default.yaml](crossplane/compositions/cluster-default.yaml) — Composition that provisions all kernel MRs
- [`gentian-apps`](https://github.com/gentian-org/gentian-apps) — catalogue of AppProfile CRs (one per app)
- [`gentian-deployments`](https://github.com/gentian-org/gentian-deployments) — per-environment Tenant and IntegrationBinding CRs
