---
audience: architect, auditor
type: decision
---

# AD-10 The desktop is the desktop, for everyone; the consoles are apps

The desktop runs per tenant in `tenant-<t>` with that tenant's credentials
only, and the platform is a tenant with a desktop like every other. What
differs between a platform administrator, a tenant administrator and a member
is the tiles their relations grant, never the image; administrator and member
are disjoint roles. The desktop is a relay for tiles and carries no
administrative code path. The administration console and the App Store app
are components installed per tenant from their own profiles. Nothing with
authority leaves `kernel-control`; nothing without authority stays in it. No
console holds a client secret: the edge is the only session authority (AD-13)
([admin-console.md](../concepts/admin-console.md), [iam.md §1.3](../concepts/iam.md)).
