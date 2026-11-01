# redis chart

| Field | Value |
|---|---|
| Purpose | Valkey for the shared kernel cache |
| Upstream | None: written here, around the official image |
| Image | `docker.io/valkey/valkey`, pinned by digest in `values.yaml` |
| Package repo | `charts/infra/packages/`, regenerate with `scripts/tools/publish-infra-charts.sh` |
| License | Valkey is BSD-3-Clause |

The chart keeps the name `redis`, the release and the object names of the
Bitnami chart it replaced, so a live install upgrades in place. Apps speak the
Redis protocol to it; `redis` is also the name of the cache engine in a profile.

Helm values and ESO secrets are in `kernel/services/infra-redis/`.
