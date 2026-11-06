---
audience: customer, operator
type: overview
---

# FAQ

## What does it run on?

A Kubernetes cluster, version 1.33 or newer: your own servers or any
provider. Traffic reaches it through a Cloudflare Tunnel, the default, or at a
fixed IP address of its own, which its own mail server needs. The services it
builds on are other projects' software under their own licenses (Keycloak,
OpenFGA, OpenBao, PostgreSQL, SeaweedFS, Envoy Gateway, Argo CD, Crossplane),
each pinned to the release. [GETTING-STARTED.md](../../GETTING-STARTED.md)
lists what a cluster needs before the installer runs;
[versions.md](../reference/versions.md) lists the pins.

## Where do the apps come from?

A catalog: an https address serving an index and one bundle per app.
gentian-apps is the open-source one. Anyone may publish another, and a
cluster or a tenant adds it by its address; nothing copies a catalog into a
cluster ahead of an install. [custom-catalogs.md](../guides/custom-catalogs.md)
has the format and a worked example.

## How do I contribute?

Open a pull request on GitHub and sign off each commit with `git commit -s`,
which states the Developer Certificate of Origin. There is no contributor
agreement and no copyright to assign: a contribution is licensed under the
license of the files it touches, and you keep the copyright in what you
write. [CONTRIBUTING.md](../../CONTRIBUTING.md) has the process,
[LICENSING.md](../../LICENSING.md) the licenses.

## What state is it in?

Early, and in the open. Releases are tagged in gentian-os and described under
[releases](../releases/v0.4.md); the issues say what the code still lacks.
Read both before you depend on it.

## Is the StorageClass configurable?

Yes. Gentian requires at least one default StorageClass in the cluster, and you
can name another for kernel workloads on the Cluster claim,
`clusters/<cluster>/kernel/claims/cluster.yaml` in the deployments repository
(`spec.storageClass`; empty means the cluster's default). The installer asks
for it on the first run.

Quick checks:

```bash
kubectl get storageclass
kubectl get storageclass -o wide
```

If needed, set a default StorageClass in your cluster (distribution-specific), then
re-run `./install.sh`.

## How should I set up edge routing?

Gentian OS uses **Gateway API + Envoy Gateway** as the only edge stack, so
there is nothing to set.

See [routing.md](../concepts/routing.md) for the topology: the
`gentian-envoy` GatewayClass and the kernel's two Gateways, `authenticated`
and `perimeter`, in `kernel-edge`.

Installer step `A-05-envoy-gateway` installs Envoy Gateway into `kernel-edge`
and the Gateway API CRDs.

Validate after install:

```bash
kubectl get gatewayclass gentian-envoy
kubectl get gateway -n kernel-edge
```

Acceptance: GatewayClass `Accepted=True`; the Gateways' listeners reach
`Programmed=True`. With `networkMode: tunnel`, a Gateway may show
`Programmed=False` (`AddressNotAssigned`) while its listeners are programmed
and traffic flows through the tunnel to the Envoy Service.

How traffic reaches the cluster is `spec.networkMode` on the Cluster claim:

- `static-ip`: the Envoy Service is of type `LoadBalancer`
- `tunnel`: the Envoy Service stays `ClusterIP`, reached through a tunnel or
  reverse proxy. A cluster behind a tunnel cannot run its own mail stack.
