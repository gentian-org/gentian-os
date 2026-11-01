---
audience: architect, auditor
type: decision
---

# AD-8 Kernel trust domains are separate namespaces

`kernel-authentication` (Keycloak, public route) apart from
`kernel-authorization` (OpenFGA, enforcement points only); `kernel-seal` apart
from `kernel-secrets`, because the seal's unseal key is an in-cluster Secret;
kernel identity on its own Postgres in `kernel-data`, not on the tenants' data
plane ([kernel.md](../concepts/kernel.md), [security/design.md §2.8](../security/design.md)).
