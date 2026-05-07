# Getting Started — Gentian OS (Crossplane-based install)

This guide covers the prerequisites and steps to bootstrap a Gentian OS kernel
cluster using **`install-cp.sh`** (Phase 1). After completing this guide you
will have Crossplane, cert-manager, External Secrets Operator, ArgoCD, and
OpenBao running, with all kernel structural resources provisioned by the
Cluster XR.

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
| `python3`    | 3.9+            | <https://python.org/downloads/> |

### Kubernetes cluster

You need a running, reachable cluster. Both `install-cp.sh` and
`uninstall-cp.sh` verify this at startup via `kubectl cluster-info`.

Tested cluster distributions:
- **microk8s** (local dev, with `dns`, `hostpath-storage`, `ingress`, and
  optionally `crossplane` addon)
- Any standard kubeconfig-accessible cluster (k3s, RKE2, EKS, GKE, AKS)

### Environment variables

`install-cp.sh` will **prompt** for any missing value. You can also pre-export
them or store them in the config files below.

**Required:**

| Variable | Description |
|---|---|
| `MASTER_PASSWORD` | HMAC master secret — all app passwords are derived from this |
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

Both files are loaded automatically by `install-cp.sh` if present.

### Cluster claim

Edit `crossplane/claims/dev-cluster.yaml` to set your `kernelDomain`,
`ldapBaseDn`, OpenBao address, and other cluster-level parameters before
running the installer.

---

## What `install-cp.sh` does

| Step | Component | Description |
|------|-----------|-------------|
| 0 | Crossplane | Install Crossplane core (controller + RBAC) via Helm |
| 0b | Crossplane | Install providers: `provider-kubernetes`, `provider-vault`, `function-go-templating` |
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
| 10 | OpenBao | Bootstrap Kubernetes auth backend for Crossplane *(replaces `tofu apply openbao-init`)* |
| 11 | Crossplane | Create 8 derived-credential K8s Secrets in `crossplane-system` |
| 12 | Cluster XR | Apply Cluster claim → kernel structural resources reconciled by provider-vault and provider-kubernetes: KV mount + policies + K8s auth backend/roles, KV seed paths (database, cache, storage, identity, mail), ArgoCD AppProject, ESO ClusterSecretStore, cert-manager ClusterIssuer |
| 12b | Secrets | Seed remaining secrets: registry, DNS/Cloudflare, internal |
| 13 | TLS | Install kernel wildcard Certificate (requires `CF_API_TOKEN`) |

**Not done in Phase 1** (planned for later phases):

| Phase | Component | Description |
|-------|-----------|-------------|
| P2 | Apps | Pattern B app charts (Nubus, OX App Suite, …) via `provider-helm` |
| P3 | Tenants | Tenant XRD + provisioning via Cluster XR |

---

## Running the installer

```bash
# Verify your environment first (runs check_prereqs, exits with list of issues)
./install-cp.sh --validate

# Full bootstrap (Phase 1)
./install-cp.sh
```

The installer is **idempotent**: re-running it after a partial failure will
skip already-completed steps.

---

## Uninstalling

```bash
# Safe teardown: removes Crossplane, ArgoCD, ESO, cert-manager.
# Preserves PVC/PV data and OpenBao KV paths.
./uninstall-cp.sh

# Full teardown: also deletes data namespaces and bound PVs.
./uninstall-cp.sh -f
```

> **Note:** OpenBao KV data is always preserved because the Cluster XR uses
> `managementPolicies: [Observe, Create]` on KV-seed managed resources.
> Crossplane never deletes KV paths when the XR is deleted.

---

## Troubleshooting

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
./install-cp.sh --validate  # shows current state
```

### ArgoCD credentials

```bash
kubectl get secret argocd-initial-admin-secret -n argocd \
  -o jsonpath='{.data.password}' | base64 -d
```
