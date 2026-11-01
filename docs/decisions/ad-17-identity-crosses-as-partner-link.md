---
audience: architect, auditor
type: decision
---

# AD-17 Identity crosses clusters only as a partner link, by standards

A tenant's realm declares partners, bounded by the Cluster claim's
`allowedIssuers`. A person links their two accounts once by signing in at the
partner through their own realm; an app then gets an assertion towards a
partner by standard token exchange at its own realm, authenticated with its
service account token, and the other side turns it into a local account by
the JWT bearer grant. No secret crosses, and only minimal information crosses:
a pairwise subject, and `gentian_roles` for administrators. Nothing in it is
specific to an app ([partners.md](../concepts/partners.md)).
