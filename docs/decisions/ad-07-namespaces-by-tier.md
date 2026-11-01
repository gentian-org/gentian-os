---
audience: architect, auditor
type: decision
---

# AD-7 Namespaces are named by tier

`kernel-<function>`, `system-<function>`, `system-<function>-dmz`,
`shared-<app>`, `tenant-<t>`, `tenant-<t>-dmz`. Tier-0 operators are kernel
and are named by the function they perform (`kernel-provisioning`, not the
product), so the software can change. Policies select on the labels
`gentianos.io/tier` and `gentianos.io/function`, never on names
([kernel.md](../concepts/kernel.md), [kernel/namespaces.yaml](../../kernel/namespaces.yaml)).
