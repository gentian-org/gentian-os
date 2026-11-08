---
audience: architect, auditor
type: decision
---

# AD-9 System services have no public route

HTTP access from outside goes through a tenant's DMZ with that tenant's
credential. A system service whose protocol needs an internet listener (mail,
TURN) puts that listener in its own DMZ namespace, `system-<function>-dmz`,
as a stateless edge holding one credential to the backend. Data services are
one namespace per engine, named by the function a requirement declares
([networking.md](../concepts/networking.md), [mail.md](../concepts/mail.md)).
