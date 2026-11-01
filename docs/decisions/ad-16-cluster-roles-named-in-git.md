---
audience: architect, auditor
type: decision
---

# AD-16 Every cluster role but `admin` and `breakGlass` is held by a person named in git

`admin` and `breakGlass` are held by a group of the kernel realm
(`spec.platformRoles`). Every other role is held by name on the Cluster claim
(`spec.platformAccess.roleHolders`), written through the director under
`can_configure`, with a reason and an optional `expiresAt`; the operator
writes the tuple as the account of its realm and removes it with the entry.
Only a token of that realm matches it. The cluster's administrator cannot
take over a holder of the platform tenant: the registrar refuses every write
to such an account. The reverse direction, the cluster's administrators
signing in to a tenant, is the tenant's own switch
(`spec.platformAccess.adminsEnter`) ([iam.md §1.1c](../concepts/iam.md)).
