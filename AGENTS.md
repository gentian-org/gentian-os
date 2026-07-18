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

## Security & licensing

* **Never commit secrets.** `MASTER_PASSWORD`, SMTP credentials, and all derived kernel/app
  passwords are runtime-injected (OpenBao + External Secrets Operator) — never hardcoded or
  committed, including in `install.secrets.env`-style files.
* **Respect third-party license terms.** `charts/infra/*` and `activepieces/*`-style vendored
  charts carry their own upstream licenses (see each chart's `UPSTREAM.md`) — don't strip
  attribution or relicense vendored code. Check license compatibility before adding new
  dependencies.
