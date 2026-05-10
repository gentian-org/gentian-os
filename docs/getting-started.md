# Getting Started

This guide describes how to bootstrap the full Gentian OS platform on a fresh
Kubernetes cluster using the Crossplane-based installer, and how to provision
your first tenant.

The bootstrap installs **Crossplane** as the provisioning plane and **ArgoCD**
as the deployment plane, then drives all kernel resource provisioning through a
single `Cluster` XR claim. After completion the cluster is fully self-healing —
drift in any managed resource is detected and corrected by the Crossplane
reconcile loop, while ArgoCD keeps deployed manifests in sync with Git.

---

## Prerequisites

### Required tools (on the operator machine)

| Tool | Notes |
|------|-------|
| `kubectl` | configured and pointing at your target cluster |
| `helm` v3 | installs Crossplane core, ArgoCD, cert-manager, ESO |
| `jq` | JSON parsing in bootstrap scripts |
| `openssl` | HMAC-SHA256 secret derivation from `MASTER_PASSWORD` |
| `curl` | health checks and OpenBao API calls |
| `crossplane` CLI | required for `make test-unit` (render golden tests) |
| `kubeconform` | required for `make test-unit-schema` (XRD schema validation) |

`tofu` (OpenTofu) and `bao` (OpenBao CLI) are installed automatically by
`install.sh`. Pass `SKIP_TOOLS=1` to skip if they are already present.

`crossplane` and `kubeconform` can be installed in one step:

```bash
make install-tools
```

### Required credentials

| Variable | What it is |
|----------|-----------|
| `MASTER_PASSWORD` | Master password used to derive all HKDF-based secrets |
| `OD_PRIVATE_REGISTRY_USERNAME` | `registry.opencode.de` GitLab username |
| `OD_PRIVATE_REGISTRY_PASSWORD` | `registry.opencode.de` Personal Access Token (`read_registry` scope) |
| `OD_SMTP_RELAY_USERNAME` | SMTP relay username (e.g. Gmail address) |
| `OD_SMTP_RELAY_PASSWORD` | SMTP relay password (e.g. Gmail App Password) |
| `KERNEL_DOMAIN` | Cluster-wide platform DNS suffix (for example `platform.example.com`) |

Optional provider-specific input:

| Variable | What it is |
|----------|-----------|
| `CF_API_TOKEN` | Cloudflare API token (Zone:Read + DNS:Edit) used only for optional DNS-01 wildcard issuance |

### Kubernetes requirements

- Kubernetes 1.26+
- Default StorageClass available (tested with `nfs-csi`)
- Ingress controller installed (tested with `ingress-nginx`)
- DNS records for your chosen `KERNEL_DOMAIN` (and optional vanity domains)

> `install.sh` provisions the remaining cluster infrastructure automatically:
> cert-manager, CloudNativePG, and Stakater Reloader. Pass `--no-cluster-infra`
> (or `INSTALL_CLUSTER_INFRA=0`) if your cluster already provides them.

---

## Bootstrap

A single self-contained wrapper script (`install.sh` at the repository root)
runs the full flow; all helper functions live in `scripts/install-lib.sh`.

Gentian OS is fully self-contained — bootstrapping a cluster requires only
the following three repositories (no other Gentian repo is referenced):

- `gentian-os` (this repo) — kernel, Crossplane XRDs/Compositions, and bootstrap
- `gentian-apps` — app catalogue and profiles
- `gentian-deployments` — per-environment overlays

### 1. Clone and enter the repository

```bash
git clone https://github.com/gentian-org/gentian-os.git
cd gentian-os
```

### 2. Export required credentials

Preferred approach (externalized config): use the provided templates.

```bash
cp install.env.template install.env
cp install.secrets.env.template install.secrets.env
chmod 600 install.secrets.env
# edit both files
```

`install.sh` auto-loads these files when present. You can override paths with:

```bash
./install.sh --config-file /path/to/install.env --secrets-file /path/to/install.secrets.env
```

Or disable file loading explicitly:

```bash
./install.sh --no-config-files
```

Alternative approach (direct environment exports):

```bash
export MASTER_PASSWORD="<your-master-password>"
export OD_PRIVATE_REGISTRY_USERNAME="<gitlab-username>"
export OD_PRIVATE_REGISTRY_PASSWORD="<read_registry-token>"
export OD_SMTP_RELAY_USERNAME="<smtp-user@example.com>"
export OD_SMTP_RELAY_PASSWORD="<smtp-app-password>"
```

Any variable not supplied via config files or environment exports will be prompted interactively.

In addition to credentials, `install.sh` prompts for the source repositories
that the cluster pulls app definitions and per-tenant overlays from. Defaults
point at the upstream `gentian-org` org; press `<Enter>` to accept them or
override per environment:

| Variable | Default | Used by |
| --- | --- | --- |
| `GENTIAN_APPS_REPO` | `https://github.com/gentian-org/gentian-apps` | ArgoCD `gentian-appprofiles` Application |
| `GENTIAN_APPS_BRANCH` | `main` | same |
| `GENTIAN_DEPLOYMENTS_REPO` | `https://github.com/gentian-org/gentian-deployments` | `kubectl gentian apps install/uninstall` |
| `GENTIAN_DEPLOYMENTS_BRANCH` | `main` | same |
| `GENTIAN_NONINTERACTIVE` | unset | set to `1` in CI to skip the prompt |

The chosen values are persisted to `~/.gentian/config` (mode 0600), which the
`kubectl-gentian` plugin sources at runtime.

### 3. Run the installer

From the `gentian-os` repository root:

```bash
./install.sh
```

This runs all bootstrap steps end-to-end (see [What the installer does](#what-the-installer-does) below).
The script is idempotent — re-running it on a partially-bootstrapped cluster
picks up where it left off. After completion:

- Crossplane core, providers, XRD, and Composition are installed and ready.
- The `Cluster` XR claim has reconciled all 19+ kernel managed resources
  (OpenBao KV mounts, policies, K8s auth backend, ESO ClusterSecretStore,
  ArgoCD AppProject, cert-manager ClusterIssuer).
- All kernel secrets are seeded in OpenBao.
- The root ArgoCD ApplicationSet is bootstrapped — it drives minio, redis,
  IAM, and infrastructure Helm releases via ArgoCD Applications.
- Nubus is deployed via the `provider-helm` Release CR.

To validate the current cluster state without making changes:

```bash
./install.sh --validate
```

### 4. Save OpenBao keys when prompted

During **Step 7** (Transit instance init) the script shows the transit Shamir
unseal key. During **Step 9** (Primary OpenBao init) it shows the primary
recovery key and root token.

**Save all values to your password manager immediately** — they are displayed
once and written to `${OPENBAO_INIT_FILE}` (default `/tmp/openbao-init.json`)
which is never committed to Git.

The primary OpenBao instance auto-unseals via the transit instance on every
restart — no manual intervention needed after a normal reboot.

---

## What the installer does

| Step | What happens |
|------|-------------|
| 0 | Installs `tofu` and `bao` CLI tools |
| 0 | Installs Crossplane core via Helm (`crossplane-stable/crossplane` v1.18.0) into `crossplane-system` |
| 0b/0c | Installs Crossplane providers (`provider-helm`, `provider-kubernetes`, `provider-vault`, `function-go-templating`, `function-auto-ready`) and applies `ProviderConfig`s, XRD (`XCluster`/`Cluster`), and Composition (`cluster-default`) |
| 1 | Creates namespaces: `openbao`, `external-secrets`, `argocd`, `tofu-system`, `gentian-dev`, `gentian-infra-dev` |
| 2 | Pre-warms the cluster (OCI image pull-through cache, node labels) |
| 3 | Installs cert-manager via Helm |
| 3b | Creates kernel cert-manager resources: HTTP-01 ClusterIssuer and optional DNS-01 Cloudflare ClusterIssuer. If `CF_API_TOKEN` is provided, also applies a wildcard Certificate for `*.${KERNEL_DOMAIN}`. |
| 4 | Installs External Secrets Operator via Helm (`kernel/eso/values.yaml`) |
| 5 | Installs ArgoCD and applies the `gentian` AppProject |
| 5b | Configures ArgoCD repository credentials for the OCI Helm registries |
| 5c | Installs ArgoCD Image Updater |
| 6 | Deploys the `openbao-transit` ArgoCD Application (transit auto-unseal instance) |
| 7 | Initialises transit OpenBao with Shamir 1-of-1; creates the `openbao-transit-unseal` k8s Secret |
| 8 | Applies the primary `openbao`, `reloader`, `cnpg`, and `globals` ArgoCD Applications from `kernel/bootstrap/` |
| 9 | Initialises primary OpenBao with transit seal — auto-unseals immediately; saves recovery key + root token to `${OPENBAO_INIT_FILE}` |
| 10 | Bootstraps OpenBao for Crossplane: enables KV v2 mount at `secret/`, enables the Kubernetes auth backend, writes `crossplane-write` / `eso-read` / `tofu-write` policies, creates Kubernetes auth roles, mints and stores the `openbao-crossplane-token` Secret used by `provider-vault` |
| 11 | Creates HMAC-derived input Secrets in `crossplane-system` (one per OpenBao KV path: postgresql, mariadb, redis, minio, nubus, keycloak-bootstrap, postfix, master-password) |
| 12 | Applies the `Cluster` XR claim (`crossplane/claims/dev-cluster.yaml`) and waits for the `XCluster` composite to become Ready — provisions all 19+ kernel managed resources via `provider-vault` and `provider-kubernetes` |
| 12b | Seeds remaining OpenBao KV paths not managed by the Cluster XR (registry credentials, DNS/Cloudflare, app-level paths) |
| 12c | (Optional) Applies a wildcard TLS Certificate for `*.${KERNEL_DOMAIN}` — requires `CF_API_TOKEN` |
| 12d | Applies the root ArgoCD ApplicationSet (`kernel/bootstrap/root-applicationset.yaml`) — ArgoCD syncs minio, redis, MariaDB, IAM, infra Helm releases |
| 13 | Waits for `provider-helm` Healthy (already included in providers from step 0b) |
| 14 | Deploys Nubus: creates namespaces, registry credentials, plain-values ConfigMaps, ESO ExternalSecrets, and the `provider-helm` Release CR (`nubus-dev`) |
---

## After bootstrap

### Inspect Crossplane managed resources

The `Cluster` XR fans out into ~19 managed resources. Inspect them with:

```bash
# All MRs belonging to the Cluster XR composite
kubectl get managed -l crossplane.io/composite=<xr-name>

# Or get the composite name from the Claim
XR=$(kubectl get cluster dev-cluster -n crossplane-system \
  -o jsonpath='{.spec.resourceRef.name}')
kubectl get managed -l crossplane.io/composite="${XR}"

# Full dependency trace (requires crossplane CLI)
crossplane beta trace cluster dev-cluster -n crossplane-system
```

### Monitor ArgoCD ApplicationSets

The root ApplicationSet (`gentian-appsets`) drives all kernel workloads:

```bash
kubectl get applicationsets -n argocd
kubectl get applications -n argocd
```

### Access ArgoCD

```
URL:      printed by install.sh at completion
Username: admin
Password: kubectl get secret argocd-initial-admin-secret -n argocd \
              -o jsonpath='{.data.password}' | base64 -d
```

### Inspect the Nubus Release

```bash
# Crossplane Release CR status
kubectl get release.helm.crossplane.io/nubus-dev

# Nubus pods
kubectl get pods -n gentian-dev -l app.kubernetes.io/part-of=nubus
```

### Monitor sync progress

```bash
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

Apply a Tenant CR to trigger full tenant provisioning. Example Tenant CRs
live in the [`gentian-deployments`](https://github.com/gentian-org/gentian-deployments)
repo (per-environment) and example AppProfile CRs in
[`gentian-apps`](https://github.com/gentian-org/gentian-apps) under `profiles/`.

```bash
kubectl apply -f path/to/your/tenant.yaml
```

Watch provisioning progress:

```bash
kubectl get tenant gtn-demo -w
```

The orchestrator provisions these in order:
1. Tenant namespace (`tenant-gtn-demo`)
2. Keycloak realm + OIDC clients (via Jobs in `platform-kernel`)
3. LDAP OU + bind accounts (via UDM REST API Jobs)
4. PostgreSQL databases (CloudNativePG `Database` CRs)
5. MariaDB databases (SQL Jobs)
6. MinIO S3 buckets + Nextcloud groups
7. Redis ACL users + Memcached ArgoCD Applications
8. App deployment (ArgoCD Application CRs)
9. Ingress + TLS certificate
10. IntegrationBinding CRs (auto-wired cross-app contracts)

Provisioning is complete when:

```bash
kubectl get tenant gtn-demo -o jsonpath='{.status.phase}'
# → Ready
```

List all tenants:

```bash
kubectl get tenants
```

Decommission a single tenant:

```bash
kubectl gentian tenants delete gtn-demo
```

For full destructive cleanup in dev/test:

```bash
kubectl gentian tenants delete gtn-demo --purge
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

### Crossplane managed resource stuck in `NotFound` or not reconciling

```bash
# Describe the composite to see which MR is blocking
kubectl describe xcluster <xr-name>

# Describe the individual managed resource
kubectl describe <resource-kind> <resource-name>

# Check provider pod logs (e.g. provider-vault)
kubectl logs -n crossplane-system \
  $(kubectl get pods -n crossplane-system -l pkg.crossplane.io/revision=provider-vault -o name | head -1)
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

## Uninstall

```bash
# Safe mode — preserves PVC/PV data
./uninstall.sh

# Force mode — deletes all namespaces and bound PVs (dev/test only)
./uninstall.sh -f

# Also remove cert-manager/reloader/CNPG (only if Gentian-managed)
./uninstall.sh -f --cluster-infra
```

OpenBao KV data is always preserved across uninstalls — `managementPolicies:
[Observe, Create]` prevents Crossplane from deleting KV paths on XR deletion.
Re-running `./install.sh` on the same cluster will adopt the existing secrets.

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

## Further reading

- [Architecture — Crossplane](architecture-crossplane.md) — full system design (current)
- [Architecture — Legacy](architecture-legacy.md) — previous Go orchestrator + OpenTofu design
- [Crossplane Migration Plan](crossplane-migration-plan.md) — P0–P5 migration phases and status
- [Design docs](design/) — deep-dives: kernel, multi-tenancy, secrets, mail, operations, agentic AI
- [commands.md](commands.md) — reference for day-2 kubectl commands
- [Implementation Plan](implementation-plan.md) — increment history and decisions
- [scripts/seed-openbao.sh](../scripts/seed-openbao.sh) — secret derivation details (HMAC-SHA256)
- [crossplane/claims/dev-cluster.yaml](../crossplane/claims/dev-cluster.yaml) — the Cluster XR claim
- [crossplane/compositions/cluster-default.yaml](../crossplane/compositions/cluster-default.yaml) — Composition that provisions all kernel MRs
- [`gentian-apps`](https://github.com/gentian-org/gentian-apps) — catalogue of AppProfile CRs (one per app)
- [`gentian-deployments`](https://github.com/gentian-org/gentian-deployments) — per-environment Tenant and IntegrationBinding CRs
