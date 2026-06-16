# FAQ

## Is the StorageClass configurable?

Yes. Gentian requires at least one default StorageClass in the cluster, and you
can override the storage class used by kernel workloads via cluster settings in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`
(`STORAGE_CLASS=...`).

Quick checks:

```bash
kubectl get storageclass
kubectl get storageclass -o wide
```

If needed, set a default StorageClass in your cluster (distribution-specific), then
re-run `./install.sh`.

## How should I set up edge routing?

Gentian OS uses **Gateway API + Envoy Gateway** as the only edge stack. Set
`ROUTING_MODE=gateway` in `install.env` and `routingMode: gateway` in
`gentian-deployments/clusters/<cluster>/kernel/values-<stage>.yaml`.

See [design/gateway.md](design/gateway.md) for Gateway API topology (`gentian-envoy` GatewayClass, kernel and tenant Gateways).

Fresh installs run `install.sh` Step 3c, which installs Envoy Gateway into
`envoy-gateway-system` and verifies Gateway API CRDs.

Validate after install:

```bash
kubectl get gatewayclass gentian-envoy
kubectl get gateway -A
kubectl describe gateway kernel-public-gateway -n gentian-dev
```

Acceptance: GatewayClass `Accepted=True`; kernel Gateway listeners reach
`Programmed=True`. On `NETWORK_MODE=tunnel`, the Gateway object may show
`Programmed=False` (`AddressNotAssigned`) while listeners are programmed and
traffic flows via Cloudflare tunnel to the Envoy Service.

Choose exposure by `NETWORK_MODE`:

- `NETWORK_MODE=static-ip`: Envoy Gateway service type `LoadBalancer`
- `NETWORK_MODE=tunnel`: Envoy Gateway stays `ClusterIP`; expose via tunnel/proxy

> **Note:** `ROUTING_MODE=ingress` (ingress-nginx) was removed in Phase E of the
> [gateway migration](gateway-migration.md). Existing clusters must migrate to
> gateway mode before upgrading.
