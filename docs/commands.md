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

Apply a tenant manifest from deployments repository:

```bash
kubectl apply -f gentian-deployments/dev/tenants/dev-tenant.yaml
```

Check tenant reconciliation:

```bash
kubectl get tenant gtn-demo -o yaml
kubectl describe tenant gtn-demo
```

## 4. Uninstall a Tenant

Use the GitOps-aware tenant delete command:

```bash
kubectl gentian tenants delete gtn-demo
```

This removes the tenant manifest from `gentian-deployments`, commits and pushes
the change, then deletes the live Tenant CR so ArgoCD does not recreate it.

Confirm ArgoCD prunes the Tenant CR:

```bash
kubectl describe application -n argocd gentian-os
kubectl get tenant gtn-demo
```

For full deprovisioning in dev/test, use:

```bash
kubectl gentian tenants delete gtn-demo --purge
```

`--purge` first commits `deletionPolicy: Delete`, applies it, then removes the
tenant from GitOps so the operator performs destructive cleanup.

## 5. List Available App Profiles

```bash
kubectl gentian apps list
```

The `kubectl gentian` commands are the tenant-facing Gentian OS command interface.

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
