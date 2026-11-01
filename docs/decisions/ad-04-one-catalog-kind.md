---
audience: architect, auditor
type: decision
---

# AD-4 One catalog kind, `ComponentProfile`, with classes

One kind describes system services, apps and agents. A profile lists the
classes it is certified for (`spec.classes`: `service`, `shared-app`, `app`);
the instance carries the one class the responsible administrator chose
(`Component.spec.class`), which must be in that list. `service` is exclusive;
`shared-app` requires `trustTier: platform`, a CRD validation invariant
rather than an admission policy ([app-catalog.md §3](../concepts/app-catalog.md)).
