# mariadb chart (vendored)

| Field | Value |
|---|---|
| Purpose | Multi-database MariaDB bootstrap for Gentian kernel services |
| Version | 3.0.3 |
| Package repo | `charts/infra/packages/` — regenerate with `scripts/publish-infra-charts.sh` |
| Licence | Apache-2.0 (chart) |

Deployed via Crossplane `Release` CR; Helm values and ESO secrets are in the
MariaDB service tree under `kernel/services/`.
