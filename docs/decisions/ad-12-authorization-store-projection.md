---
audience: architect, auditor
type: decision
---

# AD-12 The authorization store is a projection; Keycloak's events are the only source of membership

Group membership is stored in OpenFGA as `group#member` tuples written by the
operator from the signed statements Keycloak's event listener sends it, never
edited in place; nothing re-reads them, and a membership the store lacks
returns as each person's groups next change. No group travels in a token for
a platform decision. Structure (role-to-group assignments, tenants, installs,
grants) is projected by the operator from what git declares, once Argo CD has
applied it. The operator projects; the director asks. Git holds the defaults,
and everything beyond them is an action through the director
([authorization.md](../concepts/authorization.md)).
