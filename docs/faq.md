# FAQ

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

Gentian OS uses **Gateway API + Envoy Gateway** as the only edge stack;
`routingMode: gateway` is the only supported value and the default, so there
is nothing to set.

See [design/routing.md](design/routing.md) for the topology: the
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
