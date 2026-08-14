# redis chart (vendored)

| Field | Value |
|---|---|
| Purpose | Shared Redis cache for Gentian kernel services |
| Upstream | Bitnami `redis` chart |
| Version | 18.6.1 (app 7.2.3) |
| Source | `oci://registry-1.docker.io/bitnamicharts/redis:18.6.1` |
| Package repo | `charts/infra/packages/` — regenerate with `scripts/tools/publish-infra-charts.sh` |
| Runtime images | Overridden in `kernel/services/redis/values/_base.yaml` (`docker.io/bitnamilegacy/redis`) |
| Licence | Apache-2.0 (chart) |

Deployed by InfraData Crossplane XR (`crossplane/compositions/infra-data.yaml`).
Helm values and ESO secrets are in `kernel/services/infra-redis/`.
Values layering source: `kernel/services/redis/values/` (sync into infra-redis ConfigMaps when changed).

Re-vendor:

```bash
helm pull oci://registry-1.docker.io/bitnamicharts/redis --version 18.6.1 -d /tmp
tar -xzf /tmp/redis-18.6.1.tgz -C /tmp && cp -a /tmp/redis charts/infra/redis
./scripts/tools/publish-infra-charts.sh
```
