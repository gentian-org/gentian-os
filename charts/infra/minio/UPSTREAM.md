# minio chart (vendored)

| Field | Value |
|---|---|
| Purpose | Shared S3-compatible object storage for Gentian kernel services |
| Upstream | Bitnami `minio` chart |
| Version | 16.0.10 (app 2025.4.22) |
| Source | `oci://registry-1.docker.io/bitnamicharts/minio:16.0.10` |
| Package repo | `charts/infra/packages/` — regenerate with `scripts/tools/publish-infra-charts.sh` |
| Runtime images | Overridden in `kernel/services/minio/values/_base.yaml` (`docker.io/bitnamilegacy/*`) |
| Licence | Apache-2.0 (chart) |

Deployed by InfraData Crossplane XR (`crossplane/compositions/infra-data.yaml`).
Helm values and ESO secrets are in `kernel/services/infra-minio/`.
Values layering source: `kernel/services/minio/values/` (sync into infra-minio ConfigMaps when changed).

Re-vendor:

```bash
helm pull oci://registry-1.docker.io/bitnamicharts/minio --version 16.0.10 -d /tmp
tar -xzf /tmp/minio-16.0.10.tgz -C /tmp && cp -a /tmp/minio charts/infra/minio
./scripts/tools/publish-infra-charts.sh
```
