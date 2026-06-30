# Disabled ApplicationSets (develop)

These ApplicationSets are moved out of `kernel/appsets/` so Argo CD does not
sync them while the OpenDesk/Nubus/Intercom stack is replaced by Keycloak +
OpenFGA (see `docs/design/new-security-architecture.md`).

`10-infra.yaml` is also disabled — redis/minio Helm releases are owned by the
InfraData Crossplane XR (`crossplane/compositions/infra-data.yaml`).

Restore by moving a file back to `kernel/appsets/` when re-enabling a component.
