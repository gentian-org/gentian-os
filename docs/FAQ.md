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

## How should I set up ingress?

Gentian needs an ingress controller. For Infomaniak with `NETWORK_MODE=static-ip`,
use `ingress-nginx` with a `LoadBalancer` service and point DNS to the external IP.

### 1. Install ingress-nginx

```bash
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update
helm install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace
```

### 2. Get the external IP

```bash
kubectl get svc -n ingress-nginx ingress-nginx-controller -w
```

When `EXTERNAL-IP` is assigned, set that value as `NODE_IP` in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env`
for `NETWORK_MODE=static-ip`.

### 3. Configure DNS

Create/Update DNS records so `*.${KERNEL_DOMAIN}` points to that external IP.
For Cloudflare this is usually an `A` record:

- Type: `A`
- Name: `*` (and optionally `@`)
- Content: `<ingress external IP>`

### 4. Validate ingress path

```bash
kubectl get ingress -A
kubectl get svc -n ingress-nginx ingress-nginx-controller -o wide
```

If you use `NETWORK_MODE=tunnel`, keep ingress internal and terminate exposure through
your tunnel/proxy instead of binding DNS directly to the ingress external IP.
