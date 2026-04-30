# Gentian OS Commands

This document lists key cluster-admin commands for Gentian OS operations.

For tenant-admin app lifecycle commands, see:

- ../../gentian-deployments/README.md

## 1. Install the OS (Cluster Admin)

Run the shared installer from the OS repository:

```bash
bash gentian-os/install.sh
```

This installs kernel services, ArgoCD, OpenBao, the orchestrator, and supporting controllers.

## 2. Verify Core Health

```bash
kubectl get applications -n argocd
kubectl get pods -n gentian-system
kubectl get tenants
```

## 3. Provision a Tenant

Tenants are modeled in the deployments repository as:

- `dev/tenants/definitions/<definition>/...` (what a tenant is)
- `dev/tenants/instances/<instance>/...` (how that definition is instantiated)
- `dev/tenants/kustomization.yaml` (which instances are deployed in dev)

Render and apply the active tenant set for dev:

```bash
kubectl apply -k gentian-deployments/dev/tenants
```

To deploy a tenant instance, add it to `resources:` in
`gentian-deployments/dev/tenants/kustomization.yaml`, then commit/push and
sync/apply:

```yaml
resources:
- instances/gtn-demo
```

Check tenant reconciliation:

```bash
kubectl get tenant gtn-demo -o yaml
kubectl describe tenant gtn-demo
```

## 4. Uninstall a Tenant

Undeploy a tenant instance by removing it from
`gentian-deployments/dev/tenants/kustomization.yaml`, then commit/push and
sync/apply:

```yaml
resources: []
```

Apply the desired state:

```bash
kubectl apply -k gentian-deployments/dev/tenants
```

If you want immediate local convergence before ArgoCD sync, delete the live
Tenant CR after removing the instance from Git:

```bash
kubectl delete tenant gtn-demo --ignore-not-found
```

This undeploys runtime resources but keeps the tenant definition and instance
spec in Git so you can re-deploy later by re-adding the instance entry.

Confirm ArgoCD prunes the Tenant CR:

```bash
kubectl describe application -n argocd gentian-os
kubectl get tenant gtn-demo
```

## 5. List Available App Profiles

```bash
kubectl gentian apps list
```

The `kubectl gentian` commands are the tenant-facing Gentian OS command interface.

Show all available `kubectl gentian` subcommands:

```bash
kubectl gentian --help
```

## 6. Retrieve Admin Credentials

Portal and identity credentials can be read from Kubernetes Secrets.

Cluster admin (Nubus/Portal):

```bash
kubectl get secret nubus-credentials -n gentian-dev -o jsonpath='{.data.admin-password}' | base64 -d && echo
```

Keycloak admin (master realm):

```bash
kubectl get secret nubus-credentials -n gentian-dev -o jsonpath='{.data.keycloak-admin-password}' | base64 -d && echo
```

ArgoCD admin:

```bash
kubectl get secret argocd-initial-admin-secret -n argocd -o jsonpath='{.data.password}' | base64 -d && echo
```

## 7. Key URLs

Given KERNEL_DOMAIN, the main URLs are:

- Portal: https://portal.<KERNEL_DOMAIN>
- Identity admin: https://id.<KERNEL_DOMAIN>

ArgoCD URL depends on service exposure (NodePort/LoadBalancer/Ingress) in your cluster.

## 8. Useful Troubleshooting Commands

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -n 50
kubectl logs -n gentian-system deploy/gentian-os -f
kubectl get integrationbindings -A
kubectl describe application -n argocd gentian-os
```
