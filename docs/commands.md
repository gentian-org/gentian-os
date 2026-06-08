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

- `dev/tenants/instances/<tenant>/tenant.yaml` (tenant instance)
- `dev/tenants/instances/<instance>/...` (how that definition is instantiated)
- `dev/tenants/kustomization.yaml` (which instances are deployed in dev)

List available tenant instances and whether they are currently deployed:

```bash
kubectl gentian tenants list
```

Deploy a specific tenant instance:

```bash
kubectl gentian tenants deploy demo
```

Deploy is transactional:

- waits until the Tenant reaches `status.phase=Ready`
- retrieves the initial tenant-admin credentials
- only then prints login credentials
- if provisioning or credential retrieval fails, it rolls back the GitOps deploy and prints `failed to provision tenant, rolling back`
- rollback reverts the GitOps change, triggers ArgoCD prune, deletes the Tenant CR, and waits for operator finalizers (same cleanup path as `tenants undeploy`)

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
kubectl gentian tenants deploy demo --env staging
```

The deploy command updates `resources:` in
`gentian-deployments/<env>/tenants/kustomization.yaml`, commits/pushes, then
applies that Kustomization.

Equivalent Git edit:

```yaml
resources:
- instances/demo
```

Check tenant reconciliation:

```bash
kubectl get tenant demo -o yaml
kubectl describe tenant demo
```

## 4. Uninstall a Tenant

Undeploy a tenant instance:

```bash
kubectl gentian tenants undeploy demo
```

For destructive cleanup that removes all orchestrator-owned artifacts (LDAP
users, databases, mail secrets, UMC releases, and labeled kernel Jobs), use:

```bash
kubectl gentian tenants undeploy demo --purge
# or
kubectl gentian tenants undeploy demo -f
```

The purge flag sets `deletionPolicy=Delete` on the live Tenant CR, waits until
that policy is stable (re-patching if ArgoCD selfHeal reverts it), deletes the
Tenant CR, removes the instance from Git, and immediately syncs the tenants
ArgoCD Application so selfHeal cannot recreate the CR from a stale revision.
It then waits for controller Delete cleanup (LDAP OU delete, databases, etc.).
If the Tenant CR reappears without a `deletionTimestamp`, the plugin re-syncs
ArgoCD and re-issues delete until cleanup completes. After the Tenant CR is
gone it also deletes any remaining kernel artifacts labeled
`gentianos.io/tenant=<name>`.
If a prior undeploy ran Retain cleanup only, purge falls back to an LDAP OU
delete Job when it detects `ldap-lock-<tenant>` without `ldap-ou-delete-<tenant>`.

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
kubectl delete tenant demo --ignore-not-found
```

This undeploys runtime resources but keeps the tenant definition and instance
spec in Git so you can re-deploy later by re-adding the instance entry.

Confirm ArgoCD prunes the Tenant CR:

```bash
kubectl describe application -n argocd gentian-os
kubectl get tenant demo
```

## 5. Tenant App Store

Tenant admins install apps via the **App Store** web UI (preferred) or the CLI.

### Web UI

When `app-store` is installed for a tenant, open:

```text
https://store.<tenant>.<kernel-domain>
```

The UI lists `AppCatalogue` entries, shows kernel requirements, and installs or
uninstalls apps via GitOps commits to `gentian-deployments` (default) or direct
`App` claims when `INSTALL_MODE=k8s` is set on the App Store deployment.

Portal: tenant admins see an **App Store** tile (`allowedGroup: Tenant Admins`).

### CLI (fallback)

```bash
kubectl gentian apps list
kubectl gentian apps install openproject --tenant gtn-demo
kubectl gentian apps uninstall openproject --tenant gtn-demo
```

Guides:

- [gentian-apps/custom-app-guide.md](../../gentian-apps/custom-app-guide.md) — build new apps
- [gentian-apps/app-profile-guide.md](../../gentian-apps/app-profile-guide.md) — publish upstream charts

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

**Two knobs:** `MAIL_SERVICE_MODE` in `install.env` controls whether the
**installer** deploys Postfix/Dovecot into `gentian-dev` and how Postfix
relays (`external` vs `kernel`). **`Tenant.spec.mail.mode`** controls what the
**operator** provisions per organisation. See [design/mail.md](design/mail.md) §0.

On dev, in-cluster SMTP is `postfix-dev.gentian-dev.svc.cluster.local:587`.

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

