# Operator-owned CRDs

Helm installs everything in this directory automatically and claims server-side
apply ownership of it. Only CRDs whose schema this chart is the source of truth
for belong here.

## Do not add Crossplane XRD-generated CRDs

Crossplane creates a CRD for every XRD — the composite (`x<plural>.gentianos.io`)
and, when `claimNames` is set, the claim (`<plural>.gentianos.io`) — and stamps
each with an ownerReference to its CompositeResourceDefinition plus a `crossplane`
field manager on `.spec.versions`.

Shipping the same CRD here gives it two owners. `kubectl apply` tolerates that
(client-side apply, with a "missing last-applied-configuration" warning), so it
looks fine right up until `helm install`, which uses server-side apply and fails:

    Error: failed to install CRD crds/gentianos.io_apps.yaml: conflict occurred
    while applying object /apps.gentianos.io ...: conflict with "crossplane"
    using apiextensions.k8s.io/v1: .spec.versions

`gentianos.io_apps.yaml` (claim of `xapps.gentianos.io`) and
`gentianos.io_xtenants.yaml` (composite of `xtenants.gentianos.io`) were removed
for exactly this reason. Both still exist on-cluster — install.sh applies the
XRDs in Step 0b, long before this chart is installed in Step 13 — so nothing is
lost by not duplicating them.

Note `tenants.gentianos.io` DOES belong here: the `xtenants` XRD sets no
`claimNames`, so Crossplane never generates it and the operator owns it outright.
