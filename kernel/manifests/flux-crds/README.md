# Flux source-controller CRDs

`source-crds.yaml` is the unmodified upstream
[`source-controller.crds.yaml`](https://github.com/fluxcd/source-controller/releases)
from Flux source-controller **v1.8.3**.

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

## Companion controller

Because of (2) we **do** run Flux's source-controller. `install.sh`
installs it via the `oci://ghcr.io/fluxcd-community/charts/flux2` chart
in step 9, with every other Flux component disabled. The chart version
and image tag are pinned to v1.8.3 to match these CRDs. Without source-
controller the GitRepository never produces an artifact and every
Terraform CR stays stuck on "Source is not ready, artifact not found" —
Nubus never installs, tenants stall on `IdentityReady` forever.

## Updating

```bash
curl -fsSL \
  https://github.com/fluxcd/source-controller/releases/download/v1.8.3/source-controller.crds.yaml \
  -o kernel/manifests/flux-crds/source-crds.yaml
```

When bumping the CRD version, update `sourceController.tag` in `install.sh`
to match. Note: upgrading from v1.4.x requires manually deleting the
`ocirepositories.source.toolkit.fluxcd.io` CRD first, because v1.8.x drops
the `v1beta2` storage version that older releases used.
