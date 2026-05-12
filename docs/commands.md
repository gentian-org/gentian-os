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

List available tenant instances and whether they are currently deployed:

```bash
kubectl gentian tenants list
```

Deploy a specific tenant instance:

```bash
kubectl gentian tenants deploy gtn-demo
```

Deploy is transactional:

- waits until the Tenant reaches `status.phase=Ready`
- retrieves the initial tenant-admin credentials
- only then prints login credentials
- if provisioning or credential retrieval fails, it rolls back the GitOps deploy and prints `failed to provision tenant, rolling back`

After successful deploy, the CLI prints tenant-admin login guidance, including:

- readiness check command
- command to read initial credentials from `keycloak-admin-<tenant>` Job logs
- OpenBao fallback command for the tenant-admin password
- realm admin console URL

Render and apply the active tenant set for dev manually (optional):

```bash
kubectl apply -k gentian-deployments/dev/tenants
```

To target another environment, use `--env`:

```bash
kubectl gentian tenants list --env staging
kubectl gentian tenants deploy gtn-demo --env staging
```

The deploy command updates `resources:` in
`gentian-deployments/<env>/tenants/kustomization.yaml`, commits/pushes, then
applies that Kustomization.

Equivalent Git edit:

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

Undeploy a tenant instance:

```bash
kubectl gentian tenants undeploy gtn-demo
```

For destructive cleanup, use:

```bash
kubectl gentian tenants undeploy gtn-demo --purge
```

The undeploy command removes the instance from
`gentian-deployments/<env>/tenants/kustomization.yaml`, commits/pushes, applies
the Kustomization, and deletes the live Tenant CR.

Equivalent Git edit:

```yaml
resources: []
```

Apply the desired state manually (optional):

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

## 9. Kernel Mail Stack (Dovecot + Postfix)

### Enable kernel mail delivery

Kernel mail mode deploys Dovecot alongside Postfix and configures Postfix
to deliver locally via Dovecot LMTP instead of relaying to an external SMTP.

**Step 1** — Update `install.env`:
```ini
MAIL_SERVICE_MODE=kernel
```

**Step 2** — Run `update.sh`. It detects the drift and patches the cluster:
```bash
./update.sh
```

`update.sh` will detect that the deployed Postfix mode (`external`) does not match
the desired mode (`kernel`), patch the `postfix-dev-values` ConfigMap in-cluster
with the correct LMTP transport configuration, re-seed all mail secrets in OpenBao,
and force-refresh the ESO ExternalSecrets. provider-helm reconciles the Release
within a few minutes (or run `argocd app sync gentian-infra-helm-dev` immediately).

### Check mail component health

```bash
# Dovecot
kubectl get release dovecot-dev -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
kubectl logs -n gentian-dev -l app.kubernetes.io/name=dovecot --tail=20

# Postfix
kubectl get release postfix-dev -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
kubectl logs -n gentian-dev -l app.kubernetes.io/name=postfix --tail=20

# ESO secrets synced
kubectl get externalsecret -n gentian-dev dovecot-sensitive-values postfix-sensitive-values
```

### Switch back to external relay mode

```ini
# install.env
MAIL_SERVICE_MODE=external
EXTERNAL_SMTP_HOST=smtp.gmail.com
EXTERNAL_SMTP_PORT=587
OD_SMTP_RELAY_USERNAME=<gmail-address>
OD_SMTP_RELAY_PASSWORD=<app-password>
```

```bash
./update.sh
```

## 10. OX App Suite (per-tenant groupware)

OX App Suite is a per-tenant app deployed via Crossplane App claims. It requires
the kernel mail stack (`MAIL_SERVICE_MODE=kernel`) and MariaDB, OIDC, and LDAP.

### Install OX for a tenant

Add `ox-appsuite` to the tenant's `spec.apps` with `variant: ox` so the
`app-ox` Composition is selected (which handles OX-specific appsuite.properties):

```yaml
# gentian-deployments/dev/tenants/<tenant>.yaml
spec:
  apps:
    - profile: ox-appsuite
      variant: ox
```

Push the change — ArgoCD and the tenant-default Composition will create the
App claim and deploy the chart.

### Check OX deployment status

```bash
# App claim status
kubectl get app ox-appsuite -n tenant-<name> -o wide

# Release status
kubectl get release -n tenant-<name> | grep ox

# OX logs
kubectl logs -n tenant-<name> -l app.kubernetes.io/name=appsuite-public-sector --tail=30
```

### Run OX bootstrap (first-time DB migration)

The bootstrap chart (`opendesk-open-xchange-bootstrap`) creates the initial OX
context and admin user. Deploy it once before the main chart:

```bash
# Apply the bootstrap release manually (or via a separate App claim)
helm upgrade --install ox-bootstrap \
  oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-open-xchange-bootstrap \
  --version 4.0.2 \
  -n tenant-<name> \
  --set global.mysql.host=<mariadb-host> \
  --set global.mysql.database=<db-name> \
  --set global.mysql.auth.user=<db-user> \
  --set global.mysql.auth.password=<db-password>
```

### Deploy OX LDAP connector

The `ox-connector` syncs users and groups from nubus LDAP into OX contexts.
Deploy after the main chart is running:

```bash
helm upgrade --install ox-connector \
  oci://registry.opencode.de/bmi/opendesk/components/supplier/univention/charts-mirror/ox-connector \
  --version 0.34.0 \
  -n tenant-<name> \
  --set openXchange.provisioning.host=http://ums-provisioning-api.gentian-dev.svc.cluster.local \
  --set openXchange.host=http://open-xchange-core-mw-admin.tenant-<name>.svc.cluster.local
```
