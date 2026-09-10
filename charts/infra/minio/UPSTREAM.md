# minio chart (vendored)

| Field | Value |
|---|---|
| Purpose | Shared S3-compatible object storage for Gentian kernel services |
| Upstream | Bitnami `minio` chart |
| Version | 16.0.10 (app 2025.4.22) |
| Source | `oci://registry-1.docker.io/bitnamicharts/minio:16.0.10` |
| Package repo | `charts/infra/packages/` — regenerate with `scripts/tools/publish-infra-charts.sh` |
| Runtime images | Overridden in `kernel/services/infra-minio/manifests/templates/configmap.yaml` (`docker.io/bitnamilegacy/*`) |
| Licence | Apache-2.0 (chart) |

Deployed by InfraData Crossplane XR (`crossplane/compositions/infra-data.yaml`).
Helm values and ESO secrets are in `kernel/services/infra-minio/`.
Values reach the `Release` through `kernel/services/infra-minio/manifests/`, which
is the only place they are written.

Re-vendor:

```bash
helm pull oci://registry-1.docker.io/bitnamicharts/minio --version 16.0.10 -d /tmp
tar -xzf /tmp/minio-16.0.10.tgz -C /tmp && cp -a /tmp/minio charts/infra/minio
./scripts/tools/publish-infra-charts.sh
```
