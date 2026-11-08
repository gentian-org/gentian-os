---
audience: architect, auditor
type: decision
---

# AD-14 Without a store, apps are installed by command only

`kubectl gentian apps install` asks the director on the same route and under
the same question as every other install (AD-3). The cluster renders no
catalog of its own in any interface; a catalog is what the director fetches
from: an https address serving an index and one profile bundle per file, or
an OCI artifact read with the credential of a repository its owner declared,
never a directory of the deployments repository. Addresses stay public. One
bundle, one digest. A profile reaches a cluster in this way and in no other;
the director answers an index's `ce`, `me` and `ie` entries so a digest can
be looked up without a store. A catalog exists for the whole cluster, for one
tenant by the cluster's administrator, or for one tenant by its own
administrator where that was delegated. One exception: the installer's
default profiles, placed before there is a director to ask and held to the
same digest rule ([store-contract.md §9](../concepts/store-contract.md),
[custom-catalogs.md §4.4](../guides/custom-catalogs.md),
[reference/install.md](../reference/install.md), "The default profile").
