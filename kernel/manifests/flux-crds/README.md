# Flux source-controller — vendored manifests

This directory contains two vendored files for Flux's source-controller v1.8.3:

- **`source-crds.yaml`** — unmodified upstream CRDs from the
  [source-controller v1.8.3 release](https://github.com/fluxcd/source-controller/releases/download/v1.8.3/source-controller.crds.yaml)
- **`source-controller.yaml`** — static manifest for the controller itself
  (ServiceAccount, ClusterRole, ClusterRoleBindings, Service, Deployment),
  generated from `ghcr.io/fluxcd-community/charts/flux2:2.15.0` with all
  controllers except source-controller disabled and the `flux-check` Job
  (pre-install hook) excluded.

## Why we ship it

Two reasons:

1. The `tofu-controller` chart sets up watches on Flux source kinds
   (`GitRepository`, `OCIRepository`, `Bucket`) regardless of whether we
   run Flux. If those CRDs are absent, the controller's cache sync times
   out during startup and the pod crash-loops.

2. We instantiate `GitRepository` CRs in
   `kernel/services/tofu/manifests/dev/terraform.yaml` (one per
   environment, named `gentian-server`). Every `Terraform` CR in
   `tofu-system` (`infra-workspaces-dev`, `keycloak-config-dev`, every
   per-tenant `tf-*`) references this GitRepository via `sourceRef`.

## Why a static manifest instead of the Helm chart

The `flux2` community chart's pre-install Job (`flux-check`) validates the
Kubernetes version and has failed on k8s 1.31+ with certain chart releases,
requiring careful chart/image version pinning. A static manifest removes
Helm from the critical install path entirely — no pre-install hooks, no
chart compatibility issues, fully reproducible, and no network pull of a
chart tarball at install time.

## Updating

```bash
# 1. Update CRDs
curl -fsSL \
  https://github.com/fluxcd/source-controller/releases/download/vX.Y.Z/source-controller.crds.yaml \
  -o kernel/manifests/flux-crds/source-crds.yaml

# 2. Regenerate the controller manifest
helm template flux2 oci://ghcr.io/fluxcd-community/charts/flux2 \
    --namespace flux-system \
    --version <chart-version> \
    --set installCRDs=false \
    --set sourceController.create=true \
    --set sourceController.tag=vX.Y.Z \
    --set helmController.create=false \
    --set kustomizeController.create=false \
    --set notificationController.create=false \
    --set imageAutomationController.create=false \
    --set imageReflectionController.create=false \
    --set policies.create=false \
    --set rbac.createAggregation=false \
  | python3 -c "
import sys
docs = sys.stdin.read().split('\n---\n')
for doc in docs:
    if doc.strip() and 'kind: Job' not in doc:
        lines = [l for l in doc.splitlines() if not l.startswith('# Source:')]
        print('\n'.join(lines))
        print('---')
" > kernel/manifests/flux-crds/source-controller.yaml
```

Note: upgrading from v1.4.x requires manually deleting the
`ocirepositories.source.toolkit.fluxcd.io` CRD first, because v1.8.x drops
the `v1beta2` storage version that older releases used.
