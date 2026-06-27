# Disabled ApplicationSets (feat/new-security)

These ApplicationSets are moved out of `kernel/appsets/` so Argo CD does not
sync them while the OpenDesk/Nubus/Intercom stack is replaced by Keycloak +
OpenFGA (see `docs/design/new-security-architecture.md`).

Restore by moving a file back to `kernel/appsets/` when re-enabling a component.
