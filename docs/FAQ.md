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

Gentian OS supports two edge stacks, selected by `ROUTING_MODE` / Helm `routingMode`:

| Mode | Stack | Install |
|---|---|---|
| `gateway` (default) | Gateway API + Envoy Gateway | `install.sh` Step 3c when `ROUTING_MODE=gateway` |
| `ingress` (legacy) | ingress-nginx `Ingress` | Manual or pre-existing (see below) |

See [design/gateway.md](design/gateway.md) for Gateway API topology (`gentian-envoy` GatewayClass, kernel and tenant Gateways).

### Gateway API path (`ROUTING_MODE=gateway`)

Set in `gentian-deployments/clusters/<cluster>/kernel/values-<stage>.yaml`:

```yaml
routingMode: gateway
```

Fresh installs with `ROUTING_MODE=gateway` in `install.env` (or the same value from deployments values loaded at install time) install Envoy Gateway into `envoy-gateway-system` and verify Gateway API CRDs.

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

### Ingress-nginx path (`ROUTING_MODE=ingress`, legacy)

Gentian always needs an edge controller when using ingress mode.

Choose one preparation path:

- `NETWORK_MODE=static-ip` (CSP / public IP): ingress-nginx should be exposed as a `LoadBalancer` service.
- `NETWORK_MODE=tunnel` (self-hosted behind tunnel/proxy): ingress-nginx should stay internal (no `LoadBalancer` required).

### 1. Install ingress-nginx (ingress mode)

```bash
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update
```

### 2A. CSP path (`NETWORK_MODE=static-ip`): use LoadBalancer

Install (or upgrade) ingress-nginx with service type `LoadBalancer`:

```bash
helm install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace \
  --set controller.service.type=LoadBalancer
```

If already installed:

```bash
helm upgrade ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --set controller.service.type=LoadBalancer
```

Get the external IP:

```bash
kubectl get svc -n ingress-nginx ingress-nginx-controller -w
```

When `EXTERNAL-IP` is assigned, set that value as `NODE_IP` in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`
for `NETWORK_MODE=static-ip`.

Configure DNS so `*.${KERNEL_DOMAIN}` points to the ingress external IP.
For Cloudflare this is usually an `A` record (`*` and optionally `@`).

### 2B. Self-hosted path (`NETWORK_MODE=tunnel`): keep ingress internal

Do not require a `LoadBalancer` service for ingress-nginx. Use internal ingress and
expose traffic through your tunnel/proxy.

Optional explicit setting:

```bash
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace \
  --set controller.service.type=ClusterIP
```

In this mode, DNS points to your tunnel/proxy endpoint, not to an ingress external IP.

### 3. Validate ingress service type and ingress resources

```bash
kubectl get svc -n ingress-nginx ingress-nginx-controller -o wide
kubectl get ingress -A
```

Expected:

- CSP/static-ip path: `TYPE=LoadBalancer` and an `EXTERNAL-IP` is assigned.
- Self-hosted/tunnel path: `TYPE=ClusterIP` (or internal-only), and exposure is handled by the tunnel/proxy.
