# Getting Started — Gentian OS

This guide covers the prerequisites and steps to bootstrap a Gentian OS kernel
cluster using **`install.sh`**. After completing this guide
you will have Crossplane, cert-manager, External Secrets Operator, ArgoCD,
OpenBao, and the **Suze** identity stack (Keycloak + OpenFGA) running, with
kernel structural resources provisioned by the Cluster XR.

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
| `gh`         | 2.0+ (optional) | <https://cli.github.com/> — uploads `CI_BOT_PAT` to GitHub Actions during install |
| `bao`        | 2.0+            | <https://github.com/openbao/openbao/releases> |
| `crossplane` | 2.0+            | `make install-tools` (in this repo) |
| `kubeconform` | 0.6+           | `make install-tools` (in this repo) |
| `python3`    | 3.9+            | <https://python.org/downloads/> |

### Kubernetes cluster

You need a running, reachable cluster. Both `install.sh` and
`uninstall.sh` verify this at startup via `kubectl cluster-info`.

**Requirements:**
- Kubernetes 1.26+
- Default StorageClass available (configure per cluster in `gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`)
- Edge routing via Gateway API + Envoy Gateway (`ROUTING_MODE=gateway`; installed by `install.sh` Step 2c — see [docs/design/gateway.md](docs/design/gateway.md) and [docs/faq.md](docs/faq.md))
- DNS for `KERNEL_DOMAIN` (kernel UIs) and tenant app zones (`<tenant>.<kernel_domain>` or vanity domains); see [docs/design/multi-tenancy.md](docs/design/multi-tenancy.md) §3

> `install.sh` provisions cert-manager, CloudNativePG, and Stakater Reloader
> automatically. Pass `--no-cluster-infra` (or `INSTALL_CLUSTER_INFRA=0`) to
> skip if your cluster already provides them.

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
| 13 | Crossplane | Wait for `provider-helm` Healthy |
| 13b | InfraData | Shared PostgreSQL, MariaDB, Redis, MinIO via InfraData XR |
| 13c | Admission | Kyverno MAC admission (Stage 0) |
| 14 | Suze | Gentian IdP: Keycloak + OpenFGA via Suze XR; **verifies** master-realm OIDC discovery in-cluster |
| 15 | Operator | Install gentian-os controller (CRDs + reconcilers in `gentian-system`) |
| 15b | Mail | External SMTP or kernel Postfix/Dovecot when `MAIL_SERVICE_MODE=kernel`; **verifies** Dovecot IMAP/LMTP when kernel |
| 16 | Portal | Gentian portal OIDC login (Stage 1 dogfood) |
| 17 | AppProfiles | ArgoCD ApplicationSet syncs `gentian-apps/profiles/` → AppProfile CRs |
| 17b | App catalogue | `kubectl-gentian` plugin + AppCatalogue CRD |

---

## Configuration

### Config files

Copy the templates before first use:

```bash
cp cluster-settings.env.template \
  ${GENTIAN_DEPLOYMENTS_PATH}/clusters/<cluster>/kernel/cluster-settings.env
cp install.secrets.env.template install.secrets.env
cp install.env.template install.env
```

`install.env`/`install.secrets.env` are loaded automatically by `install.sh`
if present, from this repo. `cluster-settings.env` is different: it's
per-cluster instance data, so it lives and gets committed in
`gentian-deployments`, not here — the template in this repo is just a
starting point to copy from (see "Configure each file, in order" below).

You can (but you should not) override the first two files' paths with:

```bash
./install.sh --config-file /path/to/install.env --secrets-file /path/to/install.secrets.env
```

You can also (but you should not) disable file loading entirely:

```bash
./install.sh --no-config-files
```

### Configure each file, in order

`install.sh` **prompts** for any required value not already set in these
files or pre-exported. Configure them in this order before the first install
run:

**1. `cluster-settings.env`** — `gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`

**Not** `KERNEL_DOMAIN` — that goes in `install.env` (step 3 below; see also
"Cluster claim"). This file **overrides `.install-state.env`** when both are
present.

| Variable | Required? | Default | Description |
|---|---|---|---|
| `TENANCY_MODE` | Optional | `multi` | `multi` = per-tenant subdomain + Keycloak realm; `single` = one tenant on `KERNEL_DOMAIN` directly |
| `NETWORK_MODE` | Optional | `tunnel` | `tunnel` = behind a reverse-proxy/Cloudflare Tunnel; `static-ip` = DNS points directly at `NODE_IP` (becomes required) |
| `NODE_IP` | Only if `NETWORK_MODE=static-ip` | auto-detected | Node's public/reachable IP |
| `ROUTING_MODE` | Optional | `gateway` | Envoy Gateway + Gateway API edge routing — must match `routingMode` in `kernel/values.yaml` (auto-scaffolded, see "Cluster claim") |
| `MAIL_SERVICE_MODE` | Optional | `external` | `external` = relay through SMTP (needs `EXTERNAL_SMTP_*` below + secrets in step 2); `kernel` = in-cluster Postfix/Dovecot (needs `NETWORK_MODE=static-ip`) |
| `EXTERNAL_SMTP_HOST` | Only if `MAIL_SERVICE_MODE=external` | — | e.g. `smtp.gmail.com` |
| `EXTERNAL_SMTP_PORT` | Optional | `587` | |
| `EXTERNAL_SMTP_SSL` | Optional | `false` | |
| `EXTERNAL_SMTP_STARTTLS` | Optional | `true` | |
| `SECRET_MODE` | Optional | `derived` | `derived` = HKDF from `MASTER_PASSWORD`; `random` = independently random, stored in OpenBao |
| `MINIO_ENDPOINT` | Optional | per-stage default | Override only if this cluster's infra tier deviates from standard naming |
| `CNPG_HOST` | Optional | per-stage default | Same |
| `STORAGE_CLASS` | Optional | cluster's default StorageClass | Override only if needed |

The template also has an advanced, fully-optional block for LLM/vLLM serving
and tenant namespace resource limits — see the comments in
[`cluster-settings.env.template`](cluster-settings.env.template) directly.

**2. `install.secrets.env`** — this repo, secrets only, never committed

| Variable | Required? | Description |
|---|---|---|
| `MASTER_PASSWORD` | Required | Master secret — kernel and app passwords are derived via HKDF-SHA256 |
| `SMTP_RELAY_USERNAME` | Only if `MAIL_SERVICE_MODE=external` | SMTP relay username (e.g. Gmail address) |
| `SMTP_RELAY_PASSWORD` | Only if `MAIL_SERVICE_MODE=external` | SMTP relay password (e.g. Gmail App Password) |
| `CF_API_TOKEN` | Optional | Cloudflare API token for kernel wildcard DNS-01 |
| `CF_ZONE_NAME` | Optional | Override zone name for CF token verification (compound public suffixes) |
| `CI_BOT_PAT` | Optional | Fine-grained GitHub PAT with **Contents read/write** on `gentian-org/gentian-os` — uploaded to **both** `gentian-os` and `gentian-ui` Actions secrets by `install.sh` (gentian-ui pass-through for `workflow_call` only) |

`MAIL_SERVICE_MODE=kernel` (the default) uses in-cluster Postfix instead — no
relay credentials needed.

**3. `install.env`** — this repo, installer behavior and repo selection

Defaults point at the upstream `gentian-org` org, so pressing `<Enter>` at
the prompt accepts them as-is.

| Variable | Required? | Default | Description |
|---|---|---|---|
| `KERNEL_DOMAIN` | Required (new cluster) | — | Cluster domain; scaffolds the Crossplane Claim and `kernel/values.yaml` on first run — see "Cluster claim" below |
| `GENTIAN_DEPLOYMENTS_CLUSTER` | Required | — | Cluster selector in `gentian-deployments` (`clusters/<cluster>/...`) |
| `GENTIAN_DEPLOYMENTS_STAGE` | Required | — | Stage selector (`dev`, `staging`, `prod`) |
| `GENTIAN_APPS_REPO` | Optional | `https://github.com/gentian-org/gentian-apps` | Source for ArgoCD `gentian-appprofiles` Application |
| `GENTIAN_APPS_BRANCH` | Optional | `main` | same |
| `GENTIAN_DEPLOYMENTS_REPO` | Optional | `https://github.com/gentian-org/gentian-deployments` | GitOps source for tenants and app installs |
| `GENTIAN_DEPLOYMENTS_BRANCH` | Optional | `main` | same |
| `GITHUB_ACTIONS_OS_REPO` | Optional | `gentian-org/gentian-os` | Target repo for ArgoCD pin secrets |
| `GITHUB_ACTIONS_UI_REPO` | Optional | `gentian-org/gentian-ui` | Receives `CI_BOT_PAT` pass-through for `workflow_call` |
| `LETSENCRYPT_EMAIL` | Optional | `admin@KERNEL_DOMAIN` | Let's Encrypt ACME contact |
| `INSTALL_CLUSTER_INFRA` | Optional | `1` | Set `0` only when cert-manager/CNPG/Reloader are already managed |
| `GENTIAN_NONINTERACTIVE` | Optional | unset | Set to `1` in CI to skip prompts |
| `ROUTING_MODE` | Optional | `gateway` | Installer's edge-routing bootstrap flag — must match `ROUTING_MODE` in `cluster-settings.env` (step 1) and `routingMode` in `kernel/values.yaml` |

The chosen values are persisted to `~/.gentian/config` (mode 0600), which the
`kubectl-gentian` plugin sources at runtime.

### Cluster claim

On first run, `install.sh` scaffolds
`gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml`,
`infra-data.yaml`, `suze.yaml`, and `kernel/values.yaml` (operator Helm
values: `kernelDomain`, `stage`, `appLifecycle.*`, etc. — see the generated
file's comments) from `KERNEL_DOMAIN`, `GENTIAN_DEPLOYMENTS_STAGE`, and
`GENTIAN_DEPLOYMENTS_REPO` in `install.env`, then commits them — that Claim,
not anything in `gentian-os`, is the single source of truth for the
cluster's domain from then on. Existing files are never overwritten, so it's
safe to re-run. See [docs/deployment.md](docs/deployment.md) §1/§3.

---

## Running the installer

```bash
# Verify your environment first (runs check_prereqs, exits with list of issues)
./install.sh --validate

# Full bootstrap
./install.sh
```

The installer is **idempotent**: re-running it after a partial failure will
skip already-completed steps.

### Save OpenBao keys when prompted

During **Step 5b** (transit instance init) the script shows the transit Shamir
unseal key. During **Step 7** (primary OpenBao init) it shows the primary
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

Portal image rollouts (`gentian-ui` → GHCR → Argo CD Image Updater) happen
automatically after bootstrap; no action needed. See
[docs/deployment.md](docs/deployment.md) for how that pipeline works.

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

Install completes with **no tenants** deployed. Tenant definitions (such as
`demo`) live under
[`gentian-deployments`](https://github.com/gentian-org/gentian-deployments)
at `clusters/<cluster>/definitions/<tenant>/`.

### Define a tenant

`demo` ships as a ready-to-deploy example (used throughout the rest of this
section). To provision a real tenant, copy it as a starting point rather
than writing `tenant.yaml` from scratch:

```bash
cp -r ${GENTIAN_DEPLOYMENTS_PATH}/clusters/<cluster>/definitions/demo \
      ${GENTIAN_DEPLOYMENTS_PATH}/clusters/<cluster>/definitions/<tenant>
```

Edit `definitions/<tenant>/tenant.yaml`:

| Field | Change to |
|---|---|
| `metadata.name` | `<tenant>` |
| `spec.displayName` | Human-readable name |
| `spec.adminEmail` | Tenant admin's address |
| `spec.isolation.databasePrefix` | Unique per tenant, e.g. `<tenant>_` |
| `spec.isolation.keycloakRealm` | Unique per tenant, e.g. `<tenant>` |
| `spec.isolation.s3Prefix` | Unique per tenant, e.g. `<tenant>-` |
| `spec.apps` | App Store profiles to install by default — `app-store` gives the tenant admin self-service install (see "Install an app" below); drop it to start with no apps |

Commit and push `definitions/<tenant>/tenant.yaml`. This only defines the
tenant — it isn't live until you deploy it, next.

### Deploy a tenant

```bash
# Deploy the demo tenant definition (Element; Jitsi is an Element sidecar)
kubectl gentian tenants deploy demo

# List definitions and whether each is deployed/live
kubectl gentian tenants list
```

Watch provisioning progress:

```bash
kubectl get tenant demo -w
```

The orchestrator provisions these in order:

1. Tenant namespace (`tenant-demo`)
2. Keycloak realm, Gentian groups, tenant admin, and broker IdP (Jobs in `platform-kernel`)
3. PostgreSQL databases (CloudNativePG `Database` CRs)
4. MariaDB databases (SQL Jobs)
5. MinIO S3 buckets
6. Redis ACL users + Memcached Deployment (when cache required)
7. App deployment (`App` claims → Crossplane helm Releases)
8. Ingress + TLS certificate
9. IntegrationBinding CRs (auto-wired cross-app contracts)

Provisioning is complete when:

```bash
kubectl get tenant demo -o jsonpath='{.status.phase}'
# → Ready
```

### Install an app

**Catalogue** (cluster-wide): `gentian-apps/profiles/` → ArgoCD app
**`gentian-appprofiles`** (install step 17). **Tenant apps** (per org):
add profiles to `spec.apps` in `gentian-deployments` (GitOps). The operator
creates `App` claims after Argo CD syncs the tenant manifest; Crossplane
installs helm Releases.

Install and uninstall always go through **GitOps** — edit
`gentian-deployments`, commit, push, Argo CD sync, wait. From your
workstation (requires a `GENTIAN_DEPLOYMENTS_PATH` checkout):

```bash
kubectl gentian apps install element --tenant demo
```

There's also a web App Store UI once a tenant is provisioned — see
[docs/commands.md](docs/commands.md#5-tenant-app-store) for that and other
install/uninstall options.

### Decommission a tenant

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

By default, `./uninstall.sh` **undeploys all tenants first** (GitOps manifests
removed from `gentian-deployments` and live `Tenant` CRs deleted from the
cluster) so ArgoCD does not recreate them on the next install.

```bash
# Safe mode — undeploy tenants, preserve PVC/PV data and OpenBao KV paths.
./uninstall.sh

# Force mode — tenant undeploy uses --purge, deletes data namespaces and bound PVs,
# and removes Envoy Gateway, Kyverno, and orphaned gentianos.io CRDs/RBAC/catalogue.
./uninstall.sh -f

# Keep tenant workloads and Git manifests; only tear down Gentian OS kernel/infra.
./uninstall.sh --keep-tenants

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

# Full kernel provisioning E2E
make e2e-p1

# After Stage 1 install: Keycloak OIDC + Dovecot TCP smoke (kernel mail only)
make e2e-p5-keycloak-dovecot
# or: make verify-kernel-services

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

### Suze / Keycloak status

```bash
# Suze composite and Helm releases
kubectl get xsuze,suze -n crossplane-system
kubectl get release.helm.crossplane.io -l crossplane.io/composite=dev-suze

# Keycloak and OpenFGA pods (platform-kernel)
kubectl get pods -n platform-kernel -l app.kubernetes.io/part-of=suze

# OpenFGA runtime secret
kubectl get secret openfga-runtime -n platform-kernel
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
kubectl describe externalsecret -n platform-kernel

# Verify the OpenBao path exists (example)
bao kv get gentian-os/kernel/identity/keycloak
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

- [Deployment environments](docs/deployment.md) — dev / staging / prod mapping, promotion flows, GitOps layout
- [Architecture](docs/architecture.md) — full system design
- [FAQ](docs/faq.md) — quick operational answers (edge routing, storage class)
- [Design docs](docs/design/) — deep-dives: kernel, multi-tenancy, secrets, mail, operations, agentic AI
- [docs/commands.md](docs/commands.md) — reference for day-2 kubectl commands
- [Roadmap](docs/roadmap.md) — planned features (rotation, SOC 2 hardening)
- [scripts/seed-openbao.sh](scripts/seed-openbao.sh) — bootstrap seed paths (operator uses HKDF-SHA256 at runtime)
- [gentian-ui CI setup](../gentian-ui/docs/ci-setup.md) — portal image build and ArgoCD rollout
- [crossplane/xrds/cluster.yaml](crossplane/xrds/cluster.yaml) — Cluster XR schema (only `kernelDomain` required — see `gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml` for the actual per-cluster Claim)
- [crossplane/compositions/cluster-default.yaml](crossplane/compositions/cluster-default.yaml) — Composition that provisions all kernel MRs
- [`gentian-apps`](https://github.com/gentian-org/gentian-apps) — AppProfile catalogue (`profiles/`) and first-party app source (`apps/`). See [custom-app-guide.md](https://github.com/gentian-org/gentian-apps/blob/main/custom-app-guide.md) to build apps.
- [`gentian-deployments`](https://github.com/gentian-org/gentian-deployments) — per-environment Tenant and IntegrationBinding CRs
