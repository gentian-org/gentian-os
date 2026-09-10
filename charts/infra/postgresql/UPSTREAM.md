# postgresql chart (vendored)

| Field | Value |
|---|---|
| Purpose | Multi-database PostgreSQL bootstrap for Gentian kernel services |
| Version | 2.1.2 |
| Package repo | `charts/infra/packages/` — regenerate with `scripts/tools/publish-infra-charts.sh` |
| Runtime image | `docker.io/library/postgres:15.13-alpine3.20` |
| Licence | Apache-2.0 (chart); Postgres image licence per Docker Hub |

Deployed via Crossplane `Release` CR; Helm values and ESO secrets are in the
PostgreSQL service tree under `kernel/services/`.
