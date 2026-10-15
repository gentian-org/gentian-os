# seaweedfs chart (vendored)

| Field | Value |
|---|---|
| Purpose | The platform's in-cluster S3 object store: apps' buckets and tenants' backup bundles |
| Upstream | SeaweedFS's own Helm chart, from the SeaweedFS project (github.com/seaweedfs/seaweedfs, `k8s/charts/seaweedfs`) |
| Version | 4.48.0 (app 4.48) |
| Source | `helm pull seaweedfs --repo https://seaweedfs.github.io/seaweedfs/helm --version 4.48.0` (tgz sha256 `fa9beb4b68ee1e56129779643194968210381a33caf47fc177fd17d43efb686e`) |
| Package repo | `charts/infra/packages/` — regenerate with `CHART_PACKAGES_BRANCH=develop scripts/tools/publish-infra-charts.sh` |
| Runtime image | Pinned by digest in `kernel/services/infra-seaweedfs/manifests/templates/configmap.yaml` |
| License | Apache-2.0 (chart and server), Copyright the SeaweedFS authors |

Vendored unchanged: every file below this one is upstream's as pulled. The
platform's values are written in `kernel/services/infra-seaweedfs/manifests/`,
which is the only place they are; the Cluster composition's provider-helm
`Release` reads them from the ConfigMaps that chart renders.

Vendored rather than installed from upstream's repository because the
`Release` installs from the platform's own package index, like every other
engine of the system tier.

Re-vendor:

```bash
helm pull seaweedfs --repo https://seaweedfs.github.io/seaweedfs/helm --version <v> -d /tmp
rm -rf charts/infra/seaweedfs.new && mkdir charts/infra/seaweedfs.new
tar -xzf /tmp/seaweedfs-<v>.tgz -C charts/infra/seaweedfs.new
cp charts/infra/seaweedfs/UPSTREAM.md charts/infra/seaweedfs.new/seaweedfs/
rm -rf charts/infra/seaweedfs && mv charts/infra/seaweedfs.new/seaweedfs charts/infra/seaweedfs
CHART_PACKAGES_BRANCH=develop ./scripts/tools/publish-infra-charts.sh
```
