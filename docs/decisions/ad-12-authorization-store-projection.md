---
audience: architect, auditor
type: decision
---

# AD-12 The authorization store has one writer; rights are held there and nowhere else

Who holds which position and role is stored in OpenFGA and in no other
place: the identity provider holds accounts and no right, and no right
travels in a token for a platform decision. The operator is the store's one
writer. Structure (positions, cluster role holders, tenants, installs,
grants) it projects from what git declares, once Argo CD has applied it;
who holds a position or a role it writes when a tenant's administrator gives
or takes it through the registrar. The operator writes; the director asks.
Git holds the defaults, and everything beyond them is an action through the
director or the registrar
([authorization.md](../concepts/authorization.md)).
