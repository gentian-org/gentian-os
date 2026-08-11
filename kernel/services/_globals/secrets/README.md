# Per-stage global secret manifests

The `gentian-globals-secrets` ApplicationSet points an Application at
`kernel/services/_globals/secrets/<stage>`. Argo CD fails an Application whose
path does not exist:

    kernel/services/_globals/secrets/prod: app path does not exist

so every stage needs a directory here even when it holds nothing — hence the
`.gitkeep` files. Adding a stage means adding `<stage>/.gitkeep`.

This is the one place the per-stage layout survives. The service manifests under
`kernel/services/*/manifests` were converted to Helm charts taking `env` as a
parameter precisely so they would not need this, but that conversion does not
apply here: these directories are intentionally empty, and Argo CD treats a
chart that renders nothing as an error too.
