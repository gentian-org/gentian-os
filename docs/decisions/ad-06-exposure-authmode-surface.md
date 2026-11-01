---
audience: architect, auditor
type: decision
---

# AD-6 Exposure carries a mandatory `authMode` and a `surface`

`authMode` has no default; `surface` is `gateway` or `perimeter`. A perimeter
surface is a capability the profile declares; a proxy exists in
`tenant-<t>-dmz` only for an entry a perimeter approver published, under
`can_expose`, never `admin`, so publishing is always its own audit line. The
same relation answers egress beyond the baseline. Who holds it is named in
git on the Cluster claim and projected by the operator
([security/design.md §2.14](../security/design.md), [iam.md §1.1c](../concepts/iam.md)). What the
platform does for a published entry is one of two things: `none` forwards the
declared paths and passes no credential; `app` passes the caller's
`Authorization` header to the app, at a lower rate. An approval is of a kind,
and the operator acts on it only while the entry still declares that kind.
`basic`, `signature`, `jwt` and `bearer` are refused on a perimeter entry,
because nothing could enforce them ([routing.md §7](../concepts/routing.md)).
