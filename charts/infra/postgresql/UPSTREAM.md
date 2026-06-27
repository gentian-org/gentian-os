# postgresql chart (vendored)

| Field | Value |
|---|---|
| Source | OpenDesk `opendesk-postgresql` OCI chart |
| Version | 2.1.2 |
| Rescued from | `registry.opencode.de/.../opendesk-postgresql` |
| Licence | Apache-2.0 (chart); runtime image `library/postgres` on Docker Hub |

Vendored for **infra step 1** — removes opencode chart pull. Packaged to
`charts/infra/packages/` for Crossplane `Release` CRs (classic Helm repo on
GitHub raw). Runtime image was already public. Replace with CNPG or vanilla
bootstrap in a later roadmap step.
