---
audience: architect, auditor
type: decision
---

# AD-11 The layout applies to fresh installs

Stateful kernel components (OpenBao, Keycloak, CNPG clusters) are never
renamed in place; an existing cluster rebuilds from its recovery kit or keeps
its names and adopts the labels ([kernel.md](../concepts/kernel.md),
[recovery.md](../guides/recovery.md)).
