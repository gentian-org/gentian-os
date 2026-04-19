# Getting Started

This guide describes how to bootstrap the full Gentian platform on a fresh
Kubernetes cluster and provision your first tenant.

---

## Prerequisites

### Required tools (on the operator machine)

| Tool | Notes |
|------|-------|
| `kubectl` | configured and pointing at your target cluster |
| `helm` v3 | |
| `jq` | |
| `openssl` | |
| `curl` | |

`tofu` (OpenTofu) and `bao` (OpenBao CLI) are installed automatically by
`install.sh`. Pass `SKIP_TOOLS=1` to skip if they are already present.

### Required credentials

| Variable | What it is |
|----------|-----------|
| `MASTER_PASSWORD` | Master password used to derive all HMAC-based secrets |
| `OD_PRIVATE_REGISTRY_USERNAME` | `registry.opencode.de` GitLab username |
| `OD_PRIVATE_REGISTRY_PASSWORD` | `registry.opencode.de` Personal Access Token (`read_registry` scope) |
| `OD_SMTP_RELAY_USERNAME` | SMTP relay username (e.g. Gmail address) |
| `OD_SMTP_RELAY_PASSWORD` | SMTP relay password (e.g. Gmail App Password) |

### Kubernetes requirements

- Kubernetes 1.26+
- Default StorageClass available (tested with `nfs-csi`)
- Ingress controller installed (tested with `ingress-nginx`) in the `ingress`
  namespace
- A ClusterIP Service named `ingress-controller` in the `ingress` namespace
  that selects the ingress controller pods (created automatically by
  `install.sh`; required for in-cluster hairpin DNS)
- Wildcard DNS entry pointing to the cluster node IP:
  `*.desk.gentian.org → <node-ip>`

---

## Bootstrap

The bootstrap installs the cluster infrastructure (ArgoCD, OpenBao, ESO, Tofu
Controller) and seeds kernel service credentials. All individual scripts live
in `scripts/`. For a single-command convenience wrapper, use the `install.sh`
from the companion `server/` repository — it calls these same scripts in the
correct order. A self-contained `install.sh` will live in
`gentian-deployments/dev/bootstrap/` once Increment 15 is complete.

### 1. Clone and enter the repository

```bash
git clone https://github.com/gentian-org/gentian-os.git
cd gentian-os
```

### 2. Export required credentials

```bash
export MASTER_PASSWORD="<your-master-password>"
export OD_PRIVATE_REGISTRY_USERNAME="<gitlab-username>"
export OD_PRIVATE_REGISTRY_PASSWORD="<read_registry-token>"
export OD_SMTP_RELAY_USERNAME="<smtp-user@example.com>"
export OD_SMTP_RELAY_PASSWORD="<smtp-app-password>"
```

Any variable not pre-exported will be prompted interactively.

### 3. Run the installer

From the `server/` repository (which wraps the scripts from this repo):

```bash
cd ../server
./install.sh
```

Or run the scripts individually (all paths relative to `gentian-os/`):

```bash
# Steps 1–5: tools, namespaces, ESO, ArgoCD, OCI secrets
SKIP_TOOLS=1  # omit if tofu/bao not installed
bash scripts/install-argocd.sh
bash scripts/create-argocd-oci-secrets.sh "$OD_PRIVATE_REGISTRY_USERNAME" "$OD_PRIVATE_REGISTRY_PASSWORD"

# Steps 6–9: OpenBao bootstrap
kubectl apply -f kernel/bootstrap/openbao-transit-application.yaml
bash scripts/init-openbao-transit.sh
kubectl apply -f kernel/bootstrap/openbao-application.yaml
kubectl apply -f kernel/bootstrap/tofu-controller-application.yaml
kubectl apply -f kernel/bootstrap/globals-application.yaml
# Then initialise primary OpenBao (see server/install.sh step 9 for the full flow)

# Step 10: Configure OpenBao via Tofu
cd kernel/tofu/platform/openbao-init
tofu init -backend=false && tofu apply

# Step 11: Seed kernel secrets
bash scripts/seed-openbao.sh "$MASTER_PASSWORD" "$OD_PRIVATE_REGISTRY_USERNAME" \
  "$OD_PRIVATE_REGISTRY_PASSWORD" "$OD_SMTP_RELAY_USERNAME" "$OD_SMTP_RELAY_PASSWORD"

# Step 12: Apply root ApplicationSet
bash scripts/bootstrap.sh

# Step 13: Install AppCatalogue CRD + kubectl-gentian plugin
kubectl apply -f config/crd/gentianos.io_appcatalogues.yaml
install -m 755 scripts/kubectl-gentian /usr/local/bin/kubectl-gentian
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
| 1 | Installs `tofu` and `bao` CLI tools |
| 2 | Creates namespaces: `openbao`, `external-secrets`, `argocd`, `tofu-system`, `gentian-dev`, `gentian-infra-dev` |
| 2b | Ensures a ClusterIP Service `ingress-controller` exists in the `ingress` namespace (auto-detects the pod selector; needed for hairpin DNS) |
| 3 | Installs External Secrets Operator via Helm (`kernel/eso/values.yaml`) |
| 4 | Installs ArgoCD (NodePort 30443/30880) and applies the `gentian` AppProject |
| 5 | Creates ArgoCD repository secrets for the OCI Helm registries (`scripts/create-argocd-oci-secrets.sh`) |
| 6 | Deploys the `openbao-transit` ArgoCD Application (transit auto-unseal instance using `kernel/openbao/transit-values.yaml`) |
| 7 | Runs `scripts/init-openbao-transit.sh` — initialises transit with Shamir 1-of-1, creates the `openbao-transit-unseal` k8s Secret |
| 8 | Applies the primary `openbao`, `tofu-controller`, and `globals` ArgoCD Applications from `kernel/bootstrap/` |
| 9 | Initialises primary OpenBao with transit seal — auto-unseals immediately; saves recovery key + root token to `${OPENBAO_INIT_FILE}` |
| 10 | Runs `tofu apply` in `kernel/tofu/platform/openbao-init/` to configure KV engine, Kubernetes auth backend, and ESO policy |
| 11 | Runs `scripts/seed-openbao.sh` to write all kernel service secrets into OpenBao |
| 12 | Applies the root ApplicationSet (`kernel/bootstrap/root-applicationset.yaml`) — ArgoCD syncs the full kernel stack |
| 13 | Applies the `AppCatalogue` CRD and installs the `kubectl-gentian` plugin to `/usr/local/bin` |

---

## After bootstrap

### Access ArgoCD

```
URL:      https://<node-ip>:30443
Username: admin
Password: kubectl get secret argocd-initial-admin-secret -n argocd \
              -o jsonpath='{.data.password}' | base64 -d
```

The installer prints these values at the end of Step 12.

### Monitor sync progress

```bash
kubectl get applications -n argocd
kubectl get pods -A
```

### Verify the App Store

Once the `gentian-os` orchestrator is running (step 13 complete), the App
Store catalogue is available:

```bash
# Summary view
kubectl get appcatalogue default

# Full catalogue with per-app details, versions, and tenant install counts
kubectl get appcatalogue default -o yaml

# Via the kubectl plugin
kubectl gentian apps list
```

If `kubectl get appcatalogue` reports `the server doesn't have a resource type
"appcatalogue"`, apply the CRD manually:

```bash
kubectl apply -f config/crd/gentianos.io_appcatalogues.yaml
```

---

## Install the Gentian OS orchestrator

Once the kernel infrastructure is healthy, install the orchestrator Helm chart:

```bash
helm install gentian-os ./charts/gentian-os \
  --namespace gentian-system \
  --create-namespace \
  --set openbao.address=http://openbao.openbao.svc.cluster.local:8200 \
  --set argocd.namespace=argocd
```

Verify the orchestrator is running:

```bash
kubectl get pods -n gentian-system
kubectl get crds | grep gentianos.io
```

---

## Provision your first tenant

Apply a Tenant CR to trigger full tenant provisioning:

```bash
kubectl apply -f config/samples/tenant_gtn-demo.yaml
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
8. App deployment (ArgoCD Application CRs or Tofu Controller `Terraform` CRs)
9. Ingress + TLS certificate
10. IntegrationBinding CRs (auto-wired cross-app contracts)

Provisioning is complete when:

```bash
kubectl get tenant gtn-demo -o jsonpath='{.status.phase}'
# → Ready
```

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

The bootstrap Applications (OpenBao, Tofu Controller, globals) may take a few
minutes to pull and deploy. Force a sync:

```bash
kubectl annotate application openbao argocd.argoproj.io/refresh=hard -n argocd
```

### ESO `ClusterSecretStore` not ready

The `openbao` ClusterSecretStore is created by the `globals` Application. If
ESO was not yet ready when it first synced, trigger a re-sync:

```bash
kubectl annotate application globals argocd.argoproj.io/refresh=hard -n argocd
```

### `tofu apply` fails with "connection refused"

If OpenBao's ClusterIP is unreachable from the operator machine, use a
port-forward:

```bash
kubectl port-forward -n openbao svc/openbao 8200:8200 &
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=$(jq -r .root_token /tmp/openbao-init.json)
cd kernel/tofu/platform/openbao-init
tofu init -backend=false && tofu apply
```

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

- [Architecture](architecture.md) — system design and component relationships
- [Implementation Plan](implementation-plan.md) — increment history and decisions
- [scripts/seed-openbao.sh](../scripts/seed-openbao.sh) — secret derivation details
- [kernel/tofu/platform/openbao-init/](../kernel/tofu/platform/openbao-init/) — OpenBao Tofu workspace
- [config/samples/](../config/samples/) — example Tenant, AppProfile, and IntegrationBinding CRs
