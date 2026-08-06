# AGENTS.md — Gentian OS

## Project overview

Gentian OS is the kernel of the Gentian ecosystem: a Kubernetes operator, CRDs (`AppProfile`,
`Cluster`, tenant resources), Helm charts, and install/uninstall tooling. See
[README.md](README.md) for scope and [GETTING-STARTED.md](GETTING-STARTED.md) for local/cluster
setup. `docs/` (see [docs/architecture.md](docs/architecture.md) and [docs/design/](docs/design/))
covers architecture in depth.

## Build & deployment — CI/GitOps only

* CI runs in `.github/workflows/ci.yaml` (Go vet/build/test with envtest, plus image
  builds). Cluster configuration is layered and reconciled via ArgoCD, with cluster-specific
  values living in [gentian-deployments](https://github.com/gentian-org/gentian-deployments) (see
  [docs/deployment.md](docs/deployment.md)).
* **Do not build images, run `install.sh`/`uninstall.sh` against a shared cluster, or hand-patch
  live resources.** Changes belong in git (this repo for kernel/chart defaults, `gentian-deployments`
  for cluster-specific overlays) and get applied by CI/ArgoCD reconciliation. Accelerating
  reconciliation — e.g. deleting a stuck resource so the operator/ArgoCD recreates it cleanly —
  is fine; manually recreating it yourself with different config is not.
* `install.sh`/`uninstall.sh` are for local dev clusters and documented bootstrap scenarios
  (see GETTING-STARTED.md), not a substitute for GitOps on shared clusters.

## App customization — the ladder is generic, the apps are not

[docs/app-customization.md](docs/app-customization.md) defines the customization ladder
(L0 Configure → L1 Drop-in → L2 Companion → L3 Extension → L4 Repackage → L5 Patch → L6 Fork)
and the decision procedure agents must follow before customizing an installed app. Two rules
bind work in *this* repo:

* **Nothing app-specific lands here.** `AppProfile.spec.customization` describes an app's ladder
  in app-neutral terms; `internal/customization/` implements rung ordering and policy without
  knowing any profile name. If a customization needs a new operator behaviour, extend the
  generic contract — never add `case "myapp"` to a reconciler.
* **Platform scope (S2) is the narrowest-blast-radius exception, not the default.** A
  customization may enter `gentian-os` only when the mechanism is generic across apps. App-specific
  behaviour belongs in `gentian-apps` at whatever rung fits, regardless of how convenient the
  operator would be.

The `Customization` CRD records deviations at L2+; the operator computes debt signals on its
status (review overdue, upstream stale, version drift, cheaper rung available). Tenant-scoped
L1 drop-ins are reconciled by `internal/controller/dropin_reconciler.go`.

## Security & licensing

* **Never commit secrets.** `MASTER_PASSWORD`, SMTP credentials, and all derived kernel/app
  passwords are runtime-injected (OpenBao + External Secrets Operator) — never hardcoded or
  committed, including in `install.secrets.env`-style files.
* **Respect third-party license terms.** `charts/infra/*` and `activepieces/*`-style vendored
  charts carry their own upstream licenses (see each chart's `UPSTREAM.md`) — don't strip
  attribution or relicense vendored code. Check license compatibility before adding new
  dependencies.
