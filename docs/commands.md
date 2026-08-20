# Gentian OS Commands

This document lists key cluster-admin commands for Gentian OS operations.

For environment mapping, promotion flows, and GitOps layout, see
[deployment.md](deployment.md).

For tenant-admin app lifecycle commands, see:

- ../../gentian-deployments/README.md

## CLI entry points

`install.sh` installs the Gentian CLI as a kubectl plugin (`kubectl-gentian` in
`/usr/local/bin`). A shorthand symlink `gtnctl` points at the same binary:

```bash
gtnctl tenants list    # same as kubectl gentian tenants list
```

Use `gtnctl` at the terminal if you prefer a shorter command. All examples below
use the canonical `kubectl gentian` form for consistency in docs and scripts.

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

> **Deploying or undeploying a tenant briefly disrupts the shared kernel.**
> Tenant provisioning is not confined to the tenant's own namespace. It rewrites
> the shared portal's Keycloak BFF client (a `keycloak-portal-bff-<tenant>` Job
> in `platform-kernel`) and adds two listeners to the kernel Gateway, forcing an
> Envoy configuration reload across every host the gateway serves.
>
> Expect transient `404 Not Found` responses from `portal.<kernel-domain>` while
> that happens — Envoy answering before the new configuration is live, not the
> portal crashing (its pods do not restart). Undeploy does the same in reverse.
> Treat `tenants deploy` / `tenants undeploy` as a maintenance-window operation
> on a cluster with live users, and re-check the portal once the tenant reports
> `Ready` before concluding anything is broken.

Each cluster maintains tenant **definitions** under
`clusters/<cluster>/definitions/tenants/<tenant>/`. Fresh installs leave
`clusters/<cluster>/tenants/` empty until a definition is deployed. There's
no `<stage>` segment in either path — a cluster has exactly one stage for
its whole lifetime, so `clusters/<cluster>/...` already scopes everything
under it to that one stage (see
[deployment.md](deployment.md) §1).

| Path | State |
|------|--------|
| `definitions/tenants/<tenant>/` | Defined only (`ACTIVE=no` in list) |
| `tenants/<tenant>/` | Deployed to GitOps (`ACTIVE=yes`) |

List all tenant definitions and deployment status:

```bash
kubectl gentian tenants list
```

`ACTIVE=yes` when the definition is activated under `tenants/` (Argo sync path).
`ACTIVE=no` when defined only under `definitions/`. `LIVE=yes` when the Tenant CR
exists on the cluster.

Deploy a tenant definition:

```bash
kubectl gentian tenants deploy demo
```

If not yet under `tenants/`, deploy copies the definition from `definitions/` first.

Deploy is transactional:

- waits until the Tenant reaches `status.phase=Ready`
- retrieves the initial tenant-admin credentials from OpenBao or the `keycloak-admin-<tenant>` Job
- only then prints login credentials
- if provisioning or credential retrieval fails, it rolls back the GitOps deploy and prints `failed to provision tenant, rolling back`
- rollback reverts the GitOps change, triggers ArgoCD prune, deletes the Tenant CR, and waits for operator finalizers (same cleanup path as `tenants undeploy`)

After successful deploy, the CLI prints tenant-admin login guidance, including:

- readiness check command
- command to read initial credentials from `keycloak-admin-<tenant>` Job logs
- OpenBao fallback command for the tenant-admin password
- realm admin console URL

Render and apply the active tenant set manually (optional):

```bash
kubectl apply -k gentian-deployments/clusters/<cluster>/tenants/demo
```

There's no `--env`/`--stage` flag to target another stage — `GENTIAN_DEPLOYMENTS_CLUSTER_ID`
already selects a cluster that's pinned to exactly one stage. To promote a
tenant to a different stage, promote it to a *different cluster*'s
`definitions/` tree instead (see [deployment.md](deployment.md) §6.3).

The deploy command writes/updates tenant manifests under
`gentian-deployments/clusters/<cluster>/tenants/<tenant>/`, commits/pushes,
and ArgoCD ApplicationSet discovers the directory automatically.

Equivalent Git change: add/remove the tenant's directory under `tenants/`.

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

For destructive cleanup that removes all orchestrator-owned artifacts (Keycloak
realm, databases, storage, mail secrets, and labeled kernel Jobs), use:

```bash
kubectl gentian tenants undeploy demo --purge
# or
kubectl gentian tenants undeploy demo -f
```

The purge flag sets `deletionPolicy=Delete` on the live Tenant CR, waits until
that policy is stable (re-patching if ArgoCD selfHeal reverts it), deletes the
Tenant CR, removes the instance from Git, and immediately syncs the tenants
ArgoCD Application so selfHeal cannot recreate the CR from a stale revision.
It then waits for controller Delete cleanup (Keycloak realm delete, databases,
storage, mail secrets, and labeled kernel Jobs).
If the Tenant CR reappears without a `deletionTimestamp`, the plugin re-syncs
ArgoCD and re-issues delete until cleanup completes. After the Tenant CR is
gone it also deletes any remaining kernel artifacts labeled
`gentianos.io/tenant=<name>`.
If a prior undeploy ran Retain cleanup only, purge waits for any in-flight
`keycloak-realm-delete-*` Job before removing labeled kernel artifacts.

The undeploy command removes the instance from
`gentian-deployments/clusters/<cluster>/tenants/<tenant>/<env>/`, commits/pushes,
and deletes the live Tenant CR.

Equivalent Git edit:

```yaml
resources: []
```

Apply the desired state manually (optional):

```bash
kubectl apply -k gentian-deployments/clusters/<cluster>/tenants/demo/dev
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
kubectl gentian apps install demo-app --tenant gtn-demo
kubectl gentian apps uninstall demo-app --tenant gtn-demo
```

Guides:

- [gentian-apps/docs/custom-app-guide.md](../../gentian-apps/docs/custom-app-guide.md) — build new apps
- [gentian-apps/docs/app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) — publish upstream charts

Show all available `kubectl gentian` subcommands:

```bash
kubectl gentian --help
```

## 6. Install and Uninstall Apps

Apps are installed by adding an entry to the tenant manifest in
`gentian-deployments` and waiting for Crossplane + the operator to reconcile.

List catalogue profiles (cluster-scoped `AppProfile` CRs):

```bash
kubectl gentian apps list
# shorthand:
gtnctl apps list
```

Install an app on a tenant (commits/pushes GitOps, syncs Argo CD, waits for Ready):

```bash
kubectl gentian apps install demo-app --tenant demo
# shorthand:
gtnctl apps install xwiki --tenant demo
```

Uninstall (removes the app from Git; retains databases and OpenBao secrets by default):

```bash
kubectl gentian apps uninstall demo-app --tenant demo
```

Purge persistent state (Postgres/MariaDB, S3 bucket, Redis keys, OpenBao paths):

```bash
kubectl gentian apps uninstall element --tenant demo --purge
# or
gtnctl apps uninstall xwiki --tenant demo -f
```

Inspect app reconciliation:

```bash
kubectl get app xwiki -n tenant-demo
kubectl get xapp -A | grep xwiki
kubectl get pods -n tenant-demo | grep xwiki
kubectl logs -n tenant-demo -l app.kubernetes.io/instance=xwiki --tail=50
```

A cluster's stage (`dev`, `staging`, `prod`) is fixed at bootstrap via
`GENTIAN_DEPLOYMENTS_STAGE` in `install.env` (see
[deployment.md](deployment.md) §1) — `apps`/`tenants` commands don't take a
`--env`/`--stage` flag; they always target the one cluster selected by
`GENTIAN_DEPLOYMENTS_CLUSTER_ID`.

## 6a. Resource Plans and Usage

A tenant's resource ceiling is chosen from a priced catalogue of `ResourcePlan`
objects, never typed as a quantity — see
[design/resource-plans.md](design/resource-plans.md). The commands below and the
Admin Console's **Resources** tab call the same API, so the rules (the downgrade
guard, the entitlement ceiling, the git write) are enforced once.

List the catalogue, or what one tenant may pick:

```bash
kubectl gentian resources plans
kubectl gentian resources plans --tenant corp
```

With a tenant, each plan is marked `*` (current) or `x` (not selectable), and a
blocked plan carries the reason — an entitlement it exceeds, or the resource it
does not have room for.

Show a tenant's ceiling and what is committed under it:

```bash
kubectl gentian resources show corp
```

Move a tenant to a plan:

```bash
kubectl gentian resources set corp --plan base-plus-8
```

This commits `clusters/<cluster>/tenants/corp/resource-plan.yaml` and pushes;
ArgoCD applies it on the next sync and the operator reconciles the
`tenant-quota` ResourceQuota. It is refused when the plan is smaller than what
the tenant is using:

```
ERROR: the lifecycle API refused the request (HTTP 409).
  plan small is smaller than what the tenant is using (limits.cpu: using 34, plan allows 32)
```

Kubernetes does not evict pods to fit a shrunken quota — it refuses the *next*
create — so shrinking a tenant too far would otherwise appear to work and fail
hours later at the next restart. `--force` overrides the guard; it is for a
cluster operator who has accepted that cost.

What a window resolves to for invoicing:

```bash
kubectl gentian resources report corp \
  --from 2026-01-01T00:00:00Z --to 2026-02-01T00:00:00Z
```

```
PLAN                  DAYS  FROM                 TO                   SKU
base-plus-8          17.05  2026-01-01T00:00:00  2026-01-18T01:12:00  sku-8
base-plus-16         13.95  2026-01-18T01:12:00  2026-02-01T00:00:00  sku-16
```

Cap what a tenant may choose for itself (absent means uncapped):

```bash
kubectl annotate tenant corp gentianos.io/max-resource-tier=20 --overwrite
```

The plugin reaches the operator's lifecycle API through a port-forward it
establishes and tears down per invocation. Set `GENTIAN_OPERATOR_NAMESPACE` if
the operator does not run in `gentian-system`, or `GENTIAN_LIFECYCLE_URL` to
reach the API directly.

Live consumption (as opposed to what is committed) needs metrics-server, which
is optional:

```bash
bash scripts/steps/A-11-metrics-server.sh   # via the installer driver
helm upgrade gentian-os ... --set usage.metricsServer.enabled=true
```

## 7. Retrieve Admin Credentials

Portal and identity credentials can be read from Kubernetes Secrets or the
`kubectl gentian` plugin.

Gentian portal login (derived from `MASTER_PASSWORD` during install):

```bash
# Printed at end of install.sh (portal-login-bootstrap helpers)
kubectl get secret keycloak-admin -n platform-kernel -o yaml
```

Tenant admin (after `kubectl gentian tenants deploy <tenant>` — or read from Job logs):

```bash
kubectl logs -n platform-kernel job/keycloak-admin-<tenant> --tail=20
```

OpenBao fallback (when seeded):

```bash
bao kv get gentian-os/tenants/<tenant>/identity/admin
```

Keycloak master-realm admin (Suze stack):

```bash
kubectl get secret keycloak-admin -n platform-kernel \
  -o jsonpath='{.data.password}' | base64 -d && echo
```

ArgoCD admin:

```bash
kubectl get secret argocd-initial-admin-secret -n argocd -o jsonpath='{.data.password}' | base64 -d && echo
```

## 8. Key URLs

Given KERNEL_DOMAIN, the main URLs are:

- Portal: https://portal.<KERNEL_DOMAIN>
- Identity admin: https://id.<KERNEL_DOMAIN>

ArgoCD URL depends on service exposure (NodePort/LoadBalancer/Ingress) in your cluster.

## 9. Useful Troubleshooting Commands

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -n 50
kubectl logs -n gentian-system deploy/gentian-os -f
kubectl get integrationbindings -A
kubectl describe application -n argocd gentian-os
```

### Gateway API edge routing (`ROUTING_MODE=gateway`)

```bash
# Platform Gateways and routes
kubectl get gatewayclass gentian-envoy
kubectl get gateway -A
kubectl get httproute -A -l app.kubernetes.io/managed-by=gentian-os
kubectl describe gateway kernel-public-gateway -n gentian-dev

# Envoy data plane
kubectl get pods -n envoy-gateway-system
kubectl get svc -n envoy-gateway-system -l gateway.envoyproxy.io/owning-gateway-name=kernel-public-gateway

# Tenant edge status
kubectl get tenant -o custom-columns=NAME:.metadata.name,GATEWAY:.status.conditions[?(@.type==\"GatewayReady\")].status,TUNNEL:.status.conditions[?(@.type==\"TunnelIngressReady\")].status

# Envoy policies attached to routes
kubectl get backendtrafficpolicy -n tenant-demo
kubectl get backendtrafficpolicy -n gentian-dev -l app.kubernetes.io/managed-by=gentian-os
```

On `NETWORK_MODE=tunnel`, `Gateway.status.conditions[Programmed]` may be
`False` (`AddressNotAssigned`) while listener conditions are `Programmed=True`
and traffic reaches Envoy via Cloudflare tunnel. Check `TunnelIngressReady` on
the Tenant and external curl to the public hostname.

### OIDC pack catalogue

Apps with Path B OIDC depend on the cluster-scoped `OIDCPackCatalog` CR shipped
from `gentian-apps` (`profiles/<app>/oidc-catalog.yaml`). Verify packs are synced
before debugging pack Jobs or missing client scopes:

```bash
kubectl get oidcpackcatalog -l gentianos.io/profile-name=demo-app -o yaml
```

List pack keys and confirm a profile's `clientId` is present:

```bash
kubectl get oidcpackcatalog -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.packs}{"\n"}{end}'
```

Standard apps (path A — e.g. Odoo) use `app-default` Client MRs only and do
**not** need a pack entry. See [app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) §8.

## 10. Kernel Mail Stack (Dovecot + Postfix)

**Two knobs:** `MAIL_SERVICE_MODE` in
`gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env` controls whether the
**installer** deploys Postfix/Dovecot into `gentian-dev` and how Postfix
relays (`external` vs `kernel`). **`Tenant.spec.mail.mode`** controls what the
**operator** provisions per organisation. See [design/mail.md](design/mail.md) §0.

On dev, in-cluster SMTP is `postfix-dev.platform-kernel.svc.cluster.local:587`.

**Tunnel clusters:** `MAIL_SERVICE_MODE=kernel` is rejected by `./install.sh --validate` when
`NETWORK_MODE=tunnel`. Cloudflare tunnel exposes HTTP/HTTPS only — use
`MAIL_SERVICE_MODE=external` with `EXTERNAL_SMTP_HOST` / `SMTP_RELAY_*` for invitation mail.

### Enable kernel mail delivery

Kernel mail mode deploys Dovecot alongside Postfix and configures Postfix
to deliver locally via Dovecot LMTP instead of relaying to an external SMTP.

**Step 1** — Update `cluster-settings.env`:

```ini
MAIL_SERVICE_MODE=kernel
```

**Step 2** — Run `./install.sh --update`. It detects the drift and patches the cluster:

```bash
./install.sh --update
```

`install.sh --update` will detect that the deployed Postfix mode (`external`) does not match
the desired mode (`kernel`), patch the `postfix-dev-values` ConfigMap in-cluster
with the correct LMTP transport configuration, re-seed all mail secrets in OpenBao,
and force-refresh the ESO ExternalSecrets. provider-helm reconciles the Release
within a few minutes (or run `argocd app sync gentian-infra-helm-dev` immediately).

### Check mail component health

Install and `install.sh --step D-03-mail` (kernel mode) run automated smoke
checks: Keycloak master-realm OIDC discovery and Dovecot IMAP/LMTP TCP. Re-run
anytime:

```bash
make verify-kernel-services
```

Set `VERIFY_KERNEL_SERVICES=0` to skip during `./install.sh`. Tune timeouts with
`KEYCLOAK_VERIFY_TIMEOUT` / `DOVECOT_VERIFY_TIMEOUT` (seconds, default 300).

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
# gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env
MAIL_SERVICE_MODE=external
EXTERNAL_SMTP_HOST=smtp.gmail.com
EXTERNAL_SMTP_PORT=587
SMTP_RELAY_USERNAME=<gmail-address>
SMTP_RELAY_PASSWORD=<app-password>
```

```bash
./install.sh --update
```


## 11. Tenant Backup (Export)

Tenant admins take backups from the **Admin Console → Backup** tab; the guide
written for them is [tenant-backup-guide.md](tenant-backup-guide.md), and §1 of
it is the key-setup procedure they will forward to you. An export
captures the workspace's databases, buckets, volumes and Keycloak realm into one
encrypted bundle, pausing each app in turn so its data is internally consistent.

### Encryption — choose before you need it

| Choice | Who can decrypt | Use for |
|---|---|---|
| **Platform key** (default) | whoever holds the identity for `backupRecipients`, escrowed with the recovery kit | routine and scheduled backups; the only mode support can help restore |
| **My passphrase** | only the passphrase holder | a bundle the platform must not be able to read |

Both produce ordinary [age](https://age-encryption.org) files. A lost passphrase
means a lost bundle — there is no recovery path, by design.

A cluster must set `backupRecipients` before the default mode works:

```yaml
# gentian-deployments/clusters/<cluster>/kernel/... (gentian-os Helm values)
backupRecipients:
  - age1...          # public key; keep the identity OFF this cluster
```

With none configured, an export fails rather than writing a tenant's data
unencrypted. Generate the pair with `age-keygen` and store the identity with the
recovery kit.

### From the CLI

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExport
metadata:
  name: nightly-2026-08-18
  namespace: tenant-demo
spec: {}                 # every installed app; platform-key encryption
EOF

kubectl get tenantexports -n tenant-demo
kubectl describe tenantexport nightly-2026-08-18 -n tenant-demo
```

Because the default mode needs no input, a `CronJob` that applies a resource
like this is all a scheduled backup requires.

### Watching one

```bash
# Phase, bundle location, and which apps are paused right now
kubectl get tenantexport nightly-2026-08-18 -n tenant-demo \
  -o custom-columns=PHASE:.status.phase,BUNDLE:.status.bundle.prefix,PAUSED:.status.quiesced

# The capture Jobs themselves. Volume archives run in the tenant namespace —
# a PVC is only mountable from its own namespace — everything else in the
# kernel namespace beside the admin secrets.
kubectl get jobs -n platform-kernel -l gentianos.io/tenant-export=nightly-2026-08-18
kubectl get jobs -n tenant-demo -l gentianos.io/tenant-export=nightly-2026-08-18
```

An app listed in `.status.quiesced` is **offline right now**. That is normal
mid-export and worth investigating if it persists: the operator resumes an app
as soon as its capture finishes, and resumes anything it finds paused on every
reconcile, including after a restart.

### Deleting one

Tenant admins delete backups from the Backup tab; this is the same operation:

```bash
kubectl delete tenantexport nightly-2026-08-18 -n tenant-demo
```

A finalizer holds the resource until a cleanup Job has removed the bundle's
objects from the bucket — a deleted backup is gone from storage, not merely
from this list. Deleting a **Running** export is the abort mechanism: paused
apps are resumed and outstanding capture Jobs are stopped first. If the
cleanup Job itself fails, the export is released anyway and the operator log
names the bucket and prefix left behind.

### Reading a bundle

The bundle is a prefix in the tenant's backup bucket. Every artefact is
encrypted; `bundle-info.json` is deliberately not, and names the command that
opens the rest:

```bash
mc cat gentian/demo-gentian-backup/nightly-2026-08-18/bundle-info.json

# Platform key
age -d -i /path/to/identity manifest.json.age > manifest.json
# Your passphrase
age -d manifest.json.age > manifest.json
```

`manifest.json` lists what was captured per app, the chart versions at capture
time, and the pause window each app saw.

## 12. Tenant Restore

A restore **replaces** live data with what a bundle recorded. Anything written
since is gone. It is deliberately awkward to trigger by accident.

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata:
  name: restore-2026-08-18
  namespace: tenant-demo
spec:
  exportRef: nightly-2026-08-18     # or bundle: {bucket, prefix}
  confirmTenant: demo               # must equal the tenant, or it refuses
  decryption:
    identitySecretRef:              # platform-key bundle
      name: backup-identity
    # passphraseSecretRef:          # passphrase bundle
    #   name: my-passphrase
EOF
```

The identity is not on the cluster — that is the point of the escrow — so a
restore is where you prove you still have it:

```bash
kubectl create secret generic backup-identity -n tenant-demo \
  --from-file=identity=/path/to/age-identity.txt
```

Delete that Secret once the restore is done.

### What it does, in order

1. **Preflight** — confirmation matches, no export or restore already running,
   bundle exists and is `Ready`, decryption key present. Nothing is touched
   until all of these pass.
2. **Per app** — pause, load database, load bucket, unpack volumes, run the
   profile's `restore.post` hooks, run `restore.verify`, resume. One app at a
   time.
3. **Tenant-wide, last** — Keycloak realm and the portal shell database. Last
   deliberately: restoring identity earlier would let members sign in to
   half-restored data.

### After a restore, members cannot sign in

Keycloak's export carries no password hashes, so accounts come back without
credentials. `status.passwordResetRequired` says so. Send members through a
reset from **Admin Console → Members**.

## 13. Scheduled Backups

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExportSchedule
metadata:
  name: nightly
  namespace: tenant-demo
spec:
  schedule: "0 3 * * *"    # UTC, always
  keepLast: 7              # older finished exports are deleted; 0 keeps all
EOF

kubectl get tenantexportschedules -n tenant-demo
```

Encryption defaults to the cluster's recipients, which is the only mode that
works unattended — a passphrase has nobody to type it at 03:00.

`status.lastSuccessfulTime` is the field to watch. A schedule that fires nightly
but never succeeds looks healthy by every other measure, and that is precisely
the failure a backup regime cannot afford.

Two behaviours worth knowing:

- A new schedule does **not** fire immediately. Creating a backup the moment
  someone writes YAML would pause a tenant's apps as a side effect.
- A window missed by more than an hour is skipped rather than caught up. Waking
  from a long outage should not take six identical backups, each pausing the
  tenant's apps again.

## 14. Restore Drill

Run this on a scratch tenant before you need it. An untested backup is a
hypothesis.

```bash
# 1. Take a bundle
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantExport
metadata: {name: drill-before, namespace: tenant-demo}
spec: {}
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  tenantexport/drill-before -n tenant-demo --timeout=30m

# 2. Change something you can recognise — a file, a user, a project.

# 3. Put it back
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata: {name: drill-restore, namespace: tenant-demo}
spec:
  exportRef: drill-before
  confirmTenant: demo
  decryption:
    identitySecretRef: {name: backup-identity}
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  tenantrestore/drill-restore -n tenant-demo --timeout=60m

# 4. Check the change is gone, other tenants are untouched, and time it.
kubectl get tenantrestore drill-restore -n tenant-demo \
  -o jsonpath='{.status.startedAt} -> {.status.completedAt}{"\n"}'
```

The measured time is the RTO. Publish it rather than assuming one.

**If a drill wedges:** an app stuck in `.status.quiesced` is offline. The
operator resumes anything it finds paused on the next reconcile, including
after a restart, so deleting the stuck `TenantExport`/`TenantRestore` is the
recovery — the workload's `gentianos.io/pre-export-replicas` annotation records
what it should be scaled back to.
