---
audience: architect, auditor
type: decision
---

# AD-2 The director is the only writer to the deployment repository

Every write is authenticated by Keycloak and authorized by OpenFGA; commits
are signed, and Argo CD (`AppProject.spec.sourceIntegrity`) syncs only commits
signed by the director or the break-glass key. The operator writes to the
cluster only and never holds a git credential. Git is thereby the trusted,
audited source of cluster configuration
([control-plane.md §4.1](../concepts/control-plane.md), [security/design.md §3.0](../security/design.md)).
