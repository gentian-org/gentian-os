# openfga chart (vendored)

| Field | Value |
|---|---|
| Chart | `openfga` 0.3.10, repackaged as `0.3.10-gentian.1` |
| Source | https://openfga.github.io/helm-charts |
| Upstream sha256 | `27f22d368309d8b184301b090509d130a615e4dea8fedcee7a72044391a83d1d` (openfga-0.3.10.tgz) |
| License | Apache-2.0 (chart) |

`charts/openfga/` is upstream's package, unpacked, minus its bundled Bitnami
`postgresql` and `mysql` subcharts and their two `dependencies` entries (and
the `Chart.lock` that listed them). The `common` subchart stays: the
templates use it. Nothing under `templates/` or in `values.yaml` is changed.

Why removed rather than switched off: Helm honors a nested chart's
`condition` only when the parent chart declares its dependencies, and a
parent that declares them makes Argo CD's repo-server run
`helm dependency build`, which reaches out to the chart repositories. With
no declaration, Helm loads every archive in `charts/` as a subchart and
renders offline -- but then also renders the two databases this cluster
never runs (kernel-postgres serves OpenFGA). Taking them out leaves one
path, the offline one.

To move the version: `helm pull openfga --repo <source> --version <v>
--untar`, delete `charts/mysql`, `charts/postgresql`, `Chart.lock` and the
two `dependencies` entries, set `version: <v>-gentian.1`, replace the
directory and update this table.
