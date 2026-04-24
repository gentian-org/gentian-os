# Flux source-controller CRDs

`source-crds.yaml` is the unmodified upstream
[`source-controller.crds.yaml`](https://github.com/fluxcd/source-controller/releases)
from Flux source-controller **v1.8.3**.

## Why we ship it

The `tofu-controller` chart sets up watches on Flux source kinds
(`GitRepository`, `OCIRepository`, `Bucket`) regardless of whether we run
Flux. If those CRDs are absent, the controller's cache sync times out
during startup and the pod crash-loops.

We do **not** run Flux's source-controller — we only need the CRDs so the
informer client can register the kinds without erroring out. No instances
of these CRs are ever created.

## Updating

```bash
curl -fsSL \
  https://github.com/fluxcd/source-controller/releases/download/v1.8.3/source-controller.crds.yaml \
  -o kernel/manifests/flux-crds/source-crds.yaml
```

Note: upgrading from v1.4.x requires manually deleting the
`ocirepositories.source.toolkit.fluxcd.io` CRD first, because v1.8.x drops
the `v1beta2` storage version that older releases used.
