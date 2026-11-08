---
audience: architect, auditor
type: decision
---

# AD-3 The store's data lives outside the cluster; its interface is an app on it

The store, a service run by a vendor, holds and serves what is known about
apps and what a tenant acquired; it never calls the cluster and decides
nothing for it. The App Store app, installed per tenant, renders the store's
data as data and installs through the director and the custodian under the
signed-in person's own token. There are no entitlements and no license
gating: supply is controlled at the source, by a credential for the
repository that holds a paid app's chart; the digest pins the profile bundle,
and payment never passes through the cluster. Browsing needs no sign-in; the
store depends on usage statistics ([store-contract.md](../concepts/store-contract.md)).
