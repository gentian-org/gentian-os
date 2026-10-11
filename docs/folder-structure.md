# Repository Folder Structure

What lives where in `gentian-os`, and why each directory exists. For the system
model behind it see [architecture.md](architecture.md); for repo rules see
[../AGENTS.md](../AGENTS.md).

The repo holds **four kinds of artifact**, and almost every directory is one of
them:

| Kind | Directories | Consumed by |
|---|---|---|
| Platform source (Go) | `api/`, `cmd/`, `internal/`, `hack/` | `go build` → one image, run as the operator, the director, the usher, the custodian, the registrar and the bouncer |
| Declarative kernel state (YAML) | `kernel/`, `crossplane/`, `charts/` | Argo CD / Crossplane / Helm |
| Bootstrap tooling (Bash/Python) | `install.sh`, `scripts/` | a human running an install |
| Contracts & docs | `authz/`, `config/crd/`, `docs/` | tests, envtest, readers |

---

## 1. Platform source

### `api/`

A Go module of its own (`api/go.mod`), licensed separately (`api/LICENSE`):
the resource types and the bundle index (`api/bundle`). The root module reaches it through a `replace`,
and `go.work` joins the two for commands run from the root. `./...` does not
cross into it, which is why the Makefile and CI name `./api/...` as well.

### `api/v1alpha1/`
The **syscall API**. One `*_types.go` per kind — `Tenant`, `TenantDomain`,
`ComponentProfile`, `Component`, `AppPackage`, `AppGrant`, `Customization`,
`IntegrationBinding`, `Branding`, `ResourcePlan`, `BackupPolicy`,
`TenantExport`, `TenantExportSchedule`, `TenantRestore`, `MailboxRemoval`,
`CredentialRequirement`, `OIDCPackCatalog`, `PlatformSecurityPolicy` — plus
shared types (`types.go`, `profile_parts.go`, `catalogue_types.go`,
`customization_surface_types.go`, `security_types.go`, `tenancy.go`,
`catalogue_helpers.go`) and the controller-gen output
`zz_generated.deepcopy.go`.

This package is the only input to `make manifests`; CRD YAML and the chart's
`crds/` are generated from it and must never be hand-edited.

### `cmd/`
One binary per process, all built into one image:

| Path | Process |
|---|---|
| `main.go` | The operator. Wires every reconciler, the optional webhook server, the listener the other processes read from, and the OpenBao seeder onto one controller-runtime manager. Settings are environment variables supplied by the Helm chart — read this file first to learn what the operator runs. |
| `director/` | The director: the only writer to the deployments repository. |
| `usher/` | Answers a signed-in person's reads: what is here, what they may open. |
| `custodian/` | Puts a credential a person supplies into the vault. |
| `registrar/` | Manages people and groups at Keycloak. |
| `bouncer/` | The edge's access check, called by the Gateway per request. |
| `director-dev/` | The director on a laptop with stand-ins for its dependencies, for UI development. Not shipped in any image. |

### `internal/`
Implementation, split by concern rather than by kind:

| Package | Responsibility |
|---|---|
| `controller/` | The operator's reconcilers. `tenant_controller.go` and `tenant_reconcile_stages.go` are the tenant loop; `component_reconciler.go` installs each app; the rest are per-concern (identity, gateway and edge, database, storage, cache, mail, customization, backup and restore, authorization projection, tiles, …). |
| `controller/provisioner/` | Service-requirement provisioning helpers shared across reconcilers. |
| `director/` | The director's API (`api/`), token verification (`authn/`), the questions it asks OpenFGA (`authz/`), catalogue fetching and materialising (`catalogue/`), and its git writes (`gitops/`). |
| `usher/`, `custodian/`, `registrar/`, `bouncer/` | The four other services. |
| `applifecycle/` | The operator's listener: the live state and the actions the director and the usher ask it for. |
| `authz/`, `membership/` | OpenFGA and Keycloak clients; keeping OpenFGA's group memberships equal to Keycloak's. |
| `catalogue/`, `profilebundle/`, `bundlestore/` | Resolving a tenant's app to a profile; checking that a profile is the build an install was pinned to; reaching a bundle in object storage. |
| `addresses/`, `hostnames/` | Where a component's entries answer; the address names an app may not take. |
| `tiles/`, `tilecatalogue/` | The tiles a desktop shows, and the format the operator writes them in. |
| `customization/` | Generic customization-ladder ordering and policy (L0–L6). App-neutral by rule — see [app-customization.md](app-customization.md). |
| `backup/` | What a tenant is made of, and capturing it. |
| `branding/`, `locales/` | The cluster's brand as pages read it; the platform's language set. |
| `resourceplan/`, `usage/`, `licencereport/` | Resource plans, usage records, and the licence report. |
| `modelgateway/` | The model gateway as the operator administers it: per-app keys and the tenant's team. |
| `kernel/secrets/` | OpenBao KV client, HKDF-SHA256 derivation, write-once seeding. |
| `kernel/netpolicy/`, `kernel/kernelnet/` | NetworkPolicy construction for tenant and kernel namespaces. |
| `kernel/trustanchor/`, `kernel/tenantshell/`, `kernel/images.go` | Trust-anchor bundle, tenant namespace scaffolding, pinned kernel image refs. |
| `keycloak/`, `oidc/` | Keycloak group and shell helpers; OIDC pack resolution from `OIDCPackCatalog`. |
| `provisioning/privilege/` | Privilege-escalation Jobs and their fingerprinting. |
| `security/` | MAC waivers, privileges and `PlatformSecurityPolicy` evaluation. |
| `layout/`, `meta/` | The namespace layout as Go reads it (the same facts as `kernel/namespaces.yaml`); shared label keys. |
| `schemacheck/` | Comparing the resource definitions a binary was built with against the ones a cluster serves. |
| `handover/`, `tenancy/` | Whether the cluster's human write path has been proven; how many tenants a cluster may carry. |
| `webhook/` | The `Tenant` validating webhook. |

Tests live beside the code. `internal/controller` runs under **envtest** and is
excluded from `-race` (see `Makefile`/CI).

### `hack/`
Licence-header templates used by `controller-gen` and `golangci-lint`
(goheader).

---

## 2. Declarative kernel state

### `kernel/` — everything Argo CD applies that is *not* an XR

| Path | Purpose |
|---|---|
| `namespaces.yaml` | The cluster's namespace layout: the one list the installer creates from, the operator selects on and the lints check against. |
| `platforms.yaml` | Where a cluster runs and who hosts its DNS: one table, passed as a values file to the charts that need to know. |
| `bootstrap/chart/` | Helm chart of the Argo CD bootstrap objects — the `gentian` AppProject, the kernel Applications (Reloader, CNPG, Kyverno, Headlamp, OpenBao and its seal, the kernel Postgres, external-dns), the `gentian-os` Application and the root of the ApplicationSets — rendered by the install steps with cluster, stage and ref values. |
| `appsets/` | A Helm chart whose only job is to pass `raw/*.yaml` through while substituting stage, git ref, repositories and namespaces. `raw/NN-*.yaml` are the child ApplicationSets: data plane, identity, mail, model gateway, claims, catalogue, tenants, the Keycloak provider. |
| `argocd/` | Argo CD's own install values and repository credentials. |
| `services/<name>/` | One chart per kernel or system service (Keycloak, OpenFGA, the data stores, Postfix, Dovecot, the mail edge, the model gateway, …). |
| `data/` | The two CloudNativePG clusters: `kernel-postgres` and `tenant-postgres`. |
| `values/` | Helm values for charts installed outside the service pattern (`cnpg.yaml`, `reloader.yaml`, `external-dns.yaml`) plus `env/*.yaml` baselines. |
| `openbao/`, `eso/` | Values for the components that must exist before any secret can flow. |
| `manifests/` | cert-manager issuers and the kernel wildcard certificate, the GatewayClass, EnvoyProxy and Gateways, and the Job-GC CronJob. |
| `security/kyverno/policies/` | Admission policies. |
| `security/network-policies/` | The NetworkPolicies of the kernel namespaces (generated). |
| `credentials/` | The `CredentialRequirement` catalogue, generated from `credentials.yaml` at the root. |
| `extensions/keycloak-event-listener/` | The Keycloak extension that sends signed membership events to the operator. |

### `crossplane/` — the provisioning plane
| Path | Purpose |
|---|---|
| `xrds/` | Composite definitions: `Cluster`, `Tenant`, `App`, `Repository`, `Suze`. These generate the `apps.gentianos.io` / `xtenants.gentianos.io` CRDs on-cluster — which is exactly why the chart must *not* ship them. |
| `compositions/` | The pipelines: `cluster-default`, `tenant-default`, `app-default`, `repository-default`, `suze`. A catalogue entry may bring a Composition of its own in its bundle and name it in `spec.package.composition`. |
| `providers/` | `Provider` and `Function` packages, their `ProviderConfig`s, the RBAC they need, and `activation.yaml` — the provider resource types a cluster creates. |
| `tests/unit/render/` | Golden-file tests: each case is `xr.yaml` + a `composition.yaml` **copy** of the deployed Composition + `functions.yaml` + `expected.yaml`, run by `make test-unit-render`. A copy, not a symlink, so `check-render-fixtures.sh` has something to compare — a stale copy keeps a golden test green against a Composition nobody runs. |
| `tests/unit/schema/` | `valid/` fixtures that must pass and `invalid/` fixtures that must be rejected by `crossplane beta validate` against `xrds/`. |
| `tests/e2e/scripts/` | The kernel-service smoke check (`make verify-kernel-services`). |
| `functions/` | Reserved for in-repo composition functions; empty today (only pipeline functions from upstream packages are used). |

### `charts/`
| Path | Purpose |
|---|---|
| `gentian-os/` | The platform's chart: the operator, director, usher, custodian, registrar and bouncer with their RBAC and network policies, the webhook and its certificate, the `ComponentProfile`s of the desktop, the administration console, the App Store app and the sign-in page, the default `ResourcePlan`s and `PlatformSecurityPolicy`. |
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
| `tests/` | Checks with a fixture rather than a repository to scan — a stubbed CLI, asserted return codes, no cluster | `make lint-shell` and CI |
| `tools/` | Maintainer utilities on no install path | A human, occasionally |
| `dev/` | Running the director on a laptop (`director-dev.sh`) | A developer |

`lint/` and `tests/` differ by what they are pointed at, not by how they run:
a lint scans the repository and reports on what it finds there, a test builds
the situation it wants and asserts an answer. Both are shell, both run under
`make lint-shell`, and a check that needs a fixture belongs in the second.

Three files stay at the top because their path is part of their interface:
`kubectl-gentian`, which kubectl discovers by name on `PATH`,
`check-credentials.sh`, which operators run directly, and `recovery.sh`, which
is run from a rescue shell against a cluster that may have nothing else left.

The rule that keeps this from decaying: **anything sourced lives in `lib/`.**
`lib-runtime.sh`, `mail-lib.sh`, `llm-lib.sh`, `verify-kernel-services.sh` and
`portal-login-bootstrap.sh` used to sit one level up while being sourced by the
same `load.sh`, which meant "is it a library?" could not be answered by looking
at the directory.

Configuration surfaces at the repo root:

| File | Scope |
|---|---|
| `install.env.template` | Non-secret installer inputs: how to run the install. What the cluster is lives on the Cluster claim in the deployments repository. |
| `versions.yaml` | The version of every external component the installer pulls, pinned once per release. |
| `credentials.yaml` | The catalogue of credentials that must be supplied from outside the cluster. |

---

## 4. Contracts, generated schemas, docs

| Path | Purpose |
|---|---|
| `authz/model/v0/`, `v1/` | The OpenFGA authorization model (`model.fga`, `model.json`) and its tests, one directory per version. `v1` is what the director asks against (`make gen-authz-model` copies it to `internal/director/authz/model.json`); `v0` is still embedded by `internal/authz/model_embed.go`. |
| `config/crd/` | Two different things: controller-gen output for `gentianos.io_*`, **and** hand-maintained fixtures that only exist so envtest can start — third-party CRDs (Argo CD, cert-manager, Gateway API, CNPG, provider-helm, provider-keycloak) and stubs for the Crossplane-owned `apps`/`xtenants` kinds. |
| `config/rbac/` | Gitignored controller-gen intermediate; the committed artifact is the chart's `clusterrole.yaml`. |
| `docs/` | `architecture.md` and its `design/` deep-dives; `security-principles.md`; `deployment.md`, `install-reference.md`, `commands.md`, `app-customization.md`, `custom-catalogues.md`, `faq.md`, `roadmap.md`; the operator runbooks `recovery-playbook.md`, `tenant-backup-guide.md` and `node-pool-migration.md`; `releases/` for one file per release; `plans/` for working plans; `research/` for exploratory notes. |

Root docs: `README.md` (scope and what this repo is *not*), `AGENTS.md` (rules
for coding agents), `LICENSING.md` and `CONTRIBUTING.md`, `GETTING-STARTED.md` (the steps to a running cluster; flags,
troubleshooting and non-default installs are in `docs/install-reference.md`).

---

## 5. Where a change belongs

| Change | Goes in |
|---|---|
| New CRD field | `api/v1alpha1/`, then `make gen-all` |
| New reconciler behaviour | `internal/controller/` (generic — never `case "myapp"`) |
| New kernel service | `kernel/services/<name>/` + an element in the matching `kernel/appsets/raw/NN-*.yaml` |
| New tenant/app provisioning graph | `crossplane/compositions/` + a golden test under `crossplane/tests/unit/render/` |
| App-specific anything | **`gentian-apps`**, not here |
| Per-cluster values | the **deployments repository**, not here |
| A tenant, an app install | through the director, which commits it to the deployments repository |
| A user interface (desktop, consoles) | **`gentian-ui`**, not here |
