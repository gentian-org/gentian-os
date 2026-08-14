# Repository Folder Structure

What lives where in `gentian-os`, and why each directory exists. For the system
model behind it see [architecture.md](architecture.md); for repo rules see
[../AGENTS.md](../AGENTS.md).

The repo holds **four kinds of artifact**, and almost every directory is one of
them:

| Kind | Directories | Consumed by |
|---|---|---|
| Operator source (Go) | `api/`, `cmd/`, `internal/`, `hack/` | `go build` → operator image |
| Declarative kernel state (YAML) | `kernel/`, `crossplane/`, `charts/` | Argo CD / Crossplane / Helm |
| Bootstrap tooling (Bash/Python) | `install.sh`, `scripts/` | a human running an install |
| Contracts & docs | `authz/`, `config/crd/`, `docs/`, `export/` | tests, envtest, readers |

---

## 1. Operator source

### `api/v1alpha1/`
The **syscall API**. One `*_types.go` per CRD kind — `Tenant`, `AppProfile`,
`AppCatalogue`, `AppPackage`, `AppGrant`, `Customization`, `IntegrationBinding`,
`OIDCPackCatalog`, `PlatformSecurityPolicy` — plus shared types
(`types.go`, `security_types.go`, `tenancy.go`, `catalogue_helpers.go`) and the
controller-gen output `zz_generated.deepcopy.go`.

This package is the only input to `make manifests`; CRD YAML and the chart's
`crds/` are generated from it and must never be hand-edited.

### `cmd/main.go`
The single entrypoint. Wires every reconciler, the optional webhook server, the
app-lifecycle HTTP server, and the OpenBao seeder onto one controller-runtime
manager. Feature switches are environment variables (`ROUTING_MODE`,
`TENANCY_MODE`, `BAO_ADDR`, `GENTIAN_COMMERCE_ENABLED`, …) supplied by the Helm
chart — read this file first to learn what the operator actually runs.

### `internal/`
Operator implementation, split by concern rather than by CRD:

| Package | Responsibility |
|---|---|
| `controller/` | All reconcilers. `tenant_controller.go` is the orchestration loop; the rest are per-concern *reconcilers* (identity, gateway, database, storage, cache, mail, customization, authz bridge, …) that the tenant loop calls in stages. |
| `controller/provisioner/` | Kernel-requirement provisioning helpers shared across reconcilers. |
| `applifecycle/` | The HTTP app-install/uninstall API and its GitOps write-back into `gentian-deployments`. |
| `authz/` | OpenFGA + Keycloak clients, grant/subject mapping, embedded authz model. |
| `catalogue/` | Resolves `AppProfile`s out of `AppCatalogue`/`AppPackage` CRs. |
| `customization/` | Generic customization-ladder ordering and policy (L0–L6). App-neutral by rule — see [app-customization.md](app-customization.md). |
| `kernel/secrets/` | OpenBao KV client, HKDF-SHA256 deterministic derivation, write-once seeding. |
| `kernel/netpolicy/` | NetworkPolicy construction for tenant and kernel namespaces. |
| `kernel/stagingca/`, `kernel/tenantshell/`, `kernel/images.go` | Staging-CA trust bundle, tenant namespace scaffolding, pinned kernel image refs. |
| `keycloak/`, `oidc/` | Keycloak group/shell helpers and OIDC pack resolution from `OIDCPackCatalog`. |
| `provisioning/privilege/` | Privilege-escalation Jobs and their fingerprinting. |
| `security/` | MAC waivers and `PlatformSecurityPolicy` evaluation. |
| `meta/` | Shared label keys, namespace names, Job metadata. Imported almost everywhere. |
| `webhook/` | Validating webhooks (`Tenant` active, `AppProfile` stubbed). |

Tests live beside the code. `internal/controller` runs under **envtest** and is
excluded from `-race` (see `Makefile`/CI).

### `hack/`
Licence-header templates used by `controller-gen` and `golangci-lint`
(goheader).

---

## 2. Declarative kernel state

### `kernel/` — everything Argo CD applies that is *not* an XR
The kernel's static/GitOps side, ordered by bootstrap stage:

| Path | Purpose |
|---|---|
| `bootstrap/*.yaml.tmpl` | Argo CD `Application`/`ApplicationSet` templates the installer renders with `KERNEL_DOMAIN`, branch, stage. `root-applicationset.yaml.tmpl` is the app-of-apps root. |
| `appsets/` | A Helm chart whose only job is to pass `raw/*.yaml` through verbatim while substituting stage, git ref, and domain. `raw/NN-*.yaml` are the numbered child ApplicationSets (external-secrets, crossplane platform, admission, infra-data, infra-helm, suze, openbao-config, keycloak-provider). |
| `argocd/` | Argo CD `AppProject` and private-registry repo credentials applied directly by `scripts/lib/argocd.sh`. |
| `services/<name>/manifests/` | Per-kernel-service Helm chart (ConfigMap of non-secret values + `ExternalSecret` + provider-helm `Release`). Takes `env` as a parameter, which is why there is no per-stage directory tree. |
| `services/_globals/secrets/<stage>/` | Intentionally empty per-stage dirs — Argo CD rejects an Application whose path is missing. See the README there. |
| `values/` | Standalone Helm values for charts installed outside the service pattern (`cnpg.yaml`, `reloader.yaml`) plus `env/*.yaml` baselines. |
| `openbao/`, `eso/` | Values for the two components that must exist before any secret can flow. |
| `manifests/` | Plain YAML with no chart around it: cert-manager issuers and the kernel wildcard cert, the GatewayClass and EnvoyProxy, and the Job-GC CronJob. |
| `security/kyverno/policies/` | Admission policies (`gentian-baseline`, app-label enforcement). |

### `crossplane/` — the provisioning plane
| Path | Purpose |
|---|---|
| `xrds/` | Composite definitions: `Cluster`, `Tenant`, `App`, `InfraData`, `Suze`. These generate the `apps.gentianos.io` / `xtenants.gentianos.io` CRDs on-cluster — which is exactly why the chart must *not* ship them. |
| `compositions/` | The pipelines: `cluster-default`, `tenant-default`, `app-default`, `infra-data`, `suze`. Catalogue repos may ship their own compositions and point at them via `AppProfile.spec.compositionRef`. |
| `providers/` | `Provider` packages, their `ProviderConfig`s, and the RBAC they need. |
| `tests/unit/render/` | Golden-file tests: each case is `xr.yaml` + a `composition.yaml` **symlink** into `compositions/` + `functions.yaml` + `expected.yaml`, run by `make test-unit-render`. |
| `tests/unit/schema/` | `valid/` fixtures that must pass and `invalid/` fixtures that must be rejected by `crossplane beta validate` against `xrds/`. |
| `tests/e2e/scripts/` | Staged live-cluster scripts P0–P4 plus the kernel-service smoke check, exposed as `make e2e-p*`. |
| `functions/` | Reserved for in-repo composition functions; empty today (only pipeline functions from upstream packages are used). |

### `charts/`
| Path | Purpose |
|---|---|
| `gentian-os/` | The operator chart: Deployment, RBAC, webhook + cert, ServiceMonitor/dashboard, the kernel-services ConfigMap, and the `PlatformSecurityPolicy` default. |
| `gentian-os/crds/` | Generated CRDs the chart owns. Crossplane XRD-generated kinds are deliberately excluded — see the README there. |
| `infra/<name>/` | Vendored upstream data-store charts (`postgresql`, `mariadb`, `redis`, `minio`), each with an `UPSTREAM.md` recording provenance and local deltas. |
| `infra/packages/` | A classic Helm repo (`index.yaml` + `.tgz`) served straight from raw.githubusercontent, regenerated by `scripts/tools/publish-infra-charts.sh`. provider-helm `Release`s pull from here. |

---

## 3. Bootstrap tooling

`install.sh` is the operator-facing entrypoint — a driver over `scripts/steps/`, run
forward to install or converge and backward (`--uninstall`) to tear down. It is
**dev-cluster / documented-bootstrap only**; shared clusters change via GitOps.


`scripts/` is grouped by **who runs it**, which is the only distinction that
predicts where a file belongs:

| | Contents | Run by |
|---|---|---|
| `steps/` | One file per install step, `check`/`apply`/`destroy` | The driver |
| `lib/` | Everything sourced. `load.sh` is the single entrypoint | Sourced, never executed |
| `bootstrap/` | One-shot helpers a step shells out to during an install | Steps |
| `gen/` | Code generators | `make gen-all` |
| `lint/` | Repository checks | `make lint-shell` and CI |
| `tools/` | Maintainer utilities on no install path | A human, occasionally |

Two files stay at the top because their path is part of their interface:
`kubectl-gentian`, which kubectl discovers by name on `PATH`, and
`check-credentials.sh`, which operators run directly.

The rule that keeps this from decaying: **anything sourced lives in `lib/`.**
`lib-runtime.sh`, `mail-lib.sh`, `llm-lib.sh`, `verify-kernel-services.sh` and
`portal-login-bootstrap.sh` used to sit one level up while being sourced by the
same `load.sh`, which meant "is it a library?" could not be answered by looking
at the directory.

Configuration surfaces at the repo root:

| File | Scope |
|---|---|
| `install.env.template` | Non-secret installer inputs. Cluster behaviour has moved out of here into `gentian-deployments`. |
| `install.secrets.env.template` | `MASTER_PASSWORD`, registry, SMTP, Cloudflare, Git tokens. Real copies are gitignored and never committed. |
| `cluster-settings.env.template` | The only template whose destination is `gentian-deployments/clusters/<cluster>/kernel/` — network mode, storage class, mail mode. |

---

## 4. Contracts, generated schemas, docs

| Path | Purpose |
|---|---|
| `authz/model/v0/` | The OpenFGA authorization model (`model.fga`, `model.json`) and its test suite. Embedded into the operator by `internal/authz/model_embed.go` — the version directory is the migration unit. |
| `config/crd/` | Two different things: controller-gen output for `gentianos.io_*`, **and** hand-maintained fixtures that only exist so envtest can start — third-party CRDs (Argo CD, cert-manager, Gateway API, CNPG, provider-helm) and stubs for the Crossplane-owned `apps`/`xtenants` kinds. |
| `config/rbac/` | Gitignored controller-gen intermediate; the committed artifact is the chart's `clusterrole.yaml`. |
| `docs/` | `architecture.md` and its `design/` deep-dives; `deployment.md`, `commands.md`, `app-customization.md`, `faq.md`, `roadmap.md`; `research/` for exploratory notes. |
| `export/` | Tarballs and git bundles used to seed sibling repos when an agent lacks push access. Not part of the build. |

Root docs: `README.md` (scope and what this repo is *not*), `AGENTS.md` (rules
for coding agents), `GETTING-STARTED.md` (full bootstrap walkthrough).

---

## 5. Where a change belongs

| Change | Goes in |
|---|---|
| New CRD field | `api/v1alpha1/`, then `make gen-all` |
| New reconciler behaviour | `internal/controller/` (generic — never `case "myapp"`) |
| New kernel service | `kernel/services/<name>/manifests/` + an element in the matching `kernel/appsets/raw/NN-*.yaml` |
| New tenant/app provisioning graph | `crossplane/compositions/` + a golden test under `crossplane/tests/unit/render/` |
| App-specific anything | **`gentian-apps`**, not here |
| Per-cluster values, tenant instances | **`gentian-deployments`**, not here |
