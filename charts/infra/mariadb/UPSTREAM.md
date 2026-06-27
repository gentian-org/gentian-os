# mariadb chart (vendored)

| Field | Value |
|---|---|
| Source | OpenDesk `opendesk-mariadb` OCI chart |
| Version | 3.0.3 |
| Rescued from | `registry.opencode.de/.../opendesk-mariadb` |
| Licence | Apache-2.0 (chart) |

Vendored for **infra step 1** — removes opencode chart pull. Packaged to
`charts/infra/packages/` for Crossplane `Release` CRs. Regenerate with
`scripts/publish-infra-charts.sh`. Replace with a public MariaDB chart in a
later roadmap step.
