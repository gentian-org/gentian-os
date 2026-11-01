# keycloakx chart (vendored)

| Field | Value |
|---|---|
| Chart | `keycloakx` 7.3.2 |
| Source | https://codecentric.github.io/helm-charts |
| sha256 | `4ebb630c1f842245f7953be4f80ed080d1979ded25b31d48052f43f012e3c1d7` |
| License | MIT (chart) |

`charts/keycloakx/` is upstream's package, unpacked and unchanged. Helm
loads every chart under `charts/` as a subchart, so `Chart.yaml` lists no
dependency and Argo CD's repo-server renders this chart without fetching
anything.

To move the version: `helm pull keycloakx --repo <source> --version <v>
--untar`, replace the directory, update this table and `keycloakx.image.tag`
in `values.yaml` if the image moves with it (the chart trails Keycloak; see
docs/concepts/iam.md §1.10).
