# Gentian OS Commands

This document lists key cluster-admin commands for Gentian OS operations.

For environment mapping, promotion flows, and GitOps layout, see
[deployment.md](deployment.md).

For tenant-admin app lifecycle commands, see:

- ../../gentian-deployments/README.md

## CLI entry points

The Gentian CLI is a kubectl plugin (`kubectl-gentian`), installed with
`make install-plugin` into `~/.local/bin`, with a shorthand symlink `gtnctl`.
It is a client of the director, like the admin console: sign in once with
`kubectl gentian login` (a code confirmed in the browser), and every command
asks the director as you, which checks, commits and records it. It reaches the
director of the current kubectl context, so it needs no configuration.

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

> **Bringing a tenant on briefly disrupts the shared kernel.** Provisioning adds
> listeners to the kernel Gateway, which reloads Envoy across every host it
> serves; expect transient `404`s on kernel hosts while it happens. Treat
> creating and retiring tenants as maintenance-window operations on a cluster
> with live users.

A tenant is created through the director — the admin console's **Tenants**
tab, or the CLI:

```bash
kubectl gentian tenants create demo --display-name "Demo AG"   # --no-mfa: no second factor for its admin
kubectl gentian tenants list
kubectl get tenant demo -w
```

The director commits `clusters/<cluster>/tenants/demo/` as you; Argo CD syncs
it and the operator provisions the realm, namespaces, database and desktop.

Its administrator, `admin@<tenant domain>`, has **no password**. Hand the
account over with a single-use, expiring activation link — mailed to a recovery
address, or printed once without one:

```bash
kubectl gentian tenants activate-admin demo --recovery-email owner@demo.example
kubectl gentian tenants activate-admin demo                       # prints the link
```

It waits out provisioning, so it can run straight after `create`. Running it
again issues a new link, which is also how a locked-out administrator gets back
in. The recovery address is kept on the account in Keycloak, never in git.

Check reconciliation:

```bash
kubectl get tenant demo -o yaml
kubectl describe tenant demo
```

## 4. Uninstall a Tenant

```bash
kubectl gentian tenants retire demo
```

or **Retire** in the console. The director removes the tenant's directory from
git; Argo CD prunes the Tenant and the operator tears it down. Whether its data
goes with it is the manifest's `deletionPolicy` — `Retain` unless edited — not
the command. Not `kubectl delete tenant`: git is what the cluster reconciles
towards, so deleting the object just brings it back. The platform tenant cannot
be retired.

## 5. Tenant App Store

Tenant admins install apps from the **App Store** (preferred) or the CLI.

### The App Store tile

An administrator's desktop carries an **App Store** tile whenever the Cluster
claim names a store (`catalogue.storeUrl`). It opens the store in a window.
Nothing of the store runs in the cluster: what it shows of this tenant —
what is installed, how it is doing, how much of the plan is used — and what it
does to it — install, remove, purge, add-ons — it asks of the desktop, which
asks the director as the person signed in. Each change is confirmed in a
dialog the desktop draws. See [design/store-contract.md](design/store-contract.md) §7.

### CLI (fallback)

```bash
kubectl gentian apps list --tenant demo
kubectl gentian apps install xwiki-ce --tenant demo
kubectl gentian apps uninstall xwiki-ce --tenant demo
```

Guides:

- [gentian-apps/docs/custom-app-guide.md](../../gentian-apps/docs/custom-app-guide.md) — build new apps
- [gentian-apps/docs/app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) — publish upstream charts

Show all available `kubectl gentian` subcommands:

```bash
kubectl gentian --help
```

## 6. Install and Uninstall Apps

Apps are installed by the director adding them to the tenant's manifest in
git, as the person who asked; the operator reconciles them. The tenant's
administrator usually does this from the App Store; the CLI does the same:

```bash
kubectl gentian apps list --tenant demo
kubectl gentian apps install xwiki-ce --tenant demo
kubectl gentian apps uninstall xwiki-ce --tenant demo            # keeps its data
kubectl gentian apps uninstall xwiki-ce --tenant demo --purge    # removes it with its data
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
[design/resource-plans.md](design/resource-plans.md). The commands below read
the operator's answers — the same ones the Admin Console's **Resources** screen
shows — so the rules (the downgrade guard, the entitlement ceiling) are enforced
once. Choosing a plan is not a command here: it is a commit the director makes
as the person who asked, from the console or from its API with that person's
token.

List the catalogue, or what one tenant may pick:

```bash
kubectl gentian resources plans --tenant corp
```

With a tenant, each plan is marked `*` (current) or `x` (not selectable), and a
blocked plan carries the reason — an entitlement it exceeds, or the resource it
does not have room for.

Show a tenant's ceiling and what is committed under it:

```bash
kubectl gentian resources show --tenant corp
```

Move a tenant to a plan — through the director, as yourself:

```bash
kubectl gentian resources set --tenant corp --plan nodes-2      # --force: see below
```

A plan change carries who chose it and which decision allowed it. The
director commits `clusters/<cluster>/tenants/corp/resource-plan.yaml` as the
caller; ArgoCD applies it on the next sync, the operator reconciles the
`tenant-quota` ResourceQuota and records the change in the tenant's usage
history. It is refused (409) when the plan is smaller than what the tenant is
using:

```
{"error": "plan small is smaller than what the tenant is using (limits.cpu: using 34, plan allows 32)"}
```

Kubernetes does not evict pods to fit a shrunken quota — it refuses the *next*
create — so shrinking a tenant too far would otherwise appear to work and fail
hours later at the next restart. `"force": true` overrides the guard; it is
accepted only from someone who may configure the cluster, who has accepted
that cost.

What a window resolves to for invoicing:

```bash
kubectl gentian resources report --tenant corp \
  --from 2026-01-01T00:00:00Z --to 2026-02-01T00:00:00Z
```

```
PLAN                  DAYS  FROM                 TO                   SKU
base                 17.05  2026-01-01T00:00:00  2026-01-18T01:12:00  sku-1node
nodes-2              13.95  2026-01-18T01:12:00  2026-02-01T00:00:00  sku-2node
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

## 7. Administrator Accounts

No administrator is given a password. The cluster administrator
(`admin@<kernel-domain>`) and every tenant administrator are created without
one and handed over through a single-use, expiring link that sets it — and
enrols a second factor unless that was switched off — mailed to a recovery
address or shown once to whoever issued it. There is nothing to retrieve.

A new link, for a lost password or a link that expired:

```bash
kubectl gentian tenants activate-admin <tenant> [--recovery-email <address>]
kubectl gentian tenants activate-admin platform      # the cluster administrator
./install.sh --activate-admin                        # break glass: nobody can sign in
```

Keycloak master-realm admin (Suze stack):

```bash
kubectl get secret keycloak-admin -n platform-kernel \
  -o jsonpath='{.data.password}' | base64 -d && echo
```

ArgoCD has no local admin: D-03 sets `admin.enabled: "false"` and deletes
`argocd-initial-admin-secret`. Sign in through Keycloak as a member of
`gentian:platform:admin`. Break glass, when Keycloak itself is down, is
kubectl in the gitops namespace: set a new password, switch the account on,
and reach the server by port-forward (the edge needs Keycloak too):

```bash
kubectl -n kernel-gitops patch secret argocd-secret -p \
  "{\"stringData\":{\"admin.password\":\"$(argocd account bcrypt --password '<new>')\",\"admin.passwordMtime\":\"$(date -u +%FT%TZ)\"}}"
kubectl -n kernel-gitops patch configmap argocd-cm --type merge -p '{"data":{"admin.enabled":"true"}}'
kubectl -n kernel-gitops port-forward svc/argocd-server 8080:443
# afterwards: ./install.sh --force --only D-03 switches it off again
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

**Two knobs:** `spec.mail.serviceMode` on the
**Cluster claim** (`gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml`)
controls whether the kernel deploys Postfix/Dovecot into `platform-kernel` and how
Postfix relays (`external` vs `kernel`). There is no `cluster-settings.env` any more —
that file was replaced by the claim, which the installer reads directly and which
`gentian-cluster-config` republishes to every Composition that needs it.
**`Tenant.spec.mail.mode`** controls what the **operator** provisions per tenant. See
[design/mail.md](design/mail.md) §0.

On a `dev`-stage cluster, in-cluster SMTP is
`postfix-dev.platform-kernel.svc.cluster.local:587`.

**Tunnel clusters:** `MAIL_SERVICE_MODE=system` is rejected when `NETWORK_MODE=tunnel`.
Cloudflare tunnel exposes HTTP/HTTPS only — use `MAIL_SERVICE_MODE=external` with
`EXTERNAL_SMTP_HOST` / `SMTP_RELAY_*` for invitation mail.

### Enable kernel mail delivery

Kernel mail mode deploys Dovecot alongside Postfix and configures Postfix
to deliver locally via Dovecot LMTP instead of relaying to an external SMTP.

**Step 1** — Edit the Cluster claim:

```yaml
spec:
  mail:
    serviceMode: kernel
```

**Step 2** — Commit, let ArgoCD sync the claim, then re-run the driver so the mail
step converges on it:

```bash
./install.sh --only D-04-mail
```

`--force` is not required either direction. `D-04-mail`'s `check()` inspects
state when the desired mode is `kernel` (the Postfix ConfigMap), and always
reports "runs every pass" when it is `external`, since `apply()`'s external
branch is pure verification with no cluster mutation to gate on — cheap enough
to run on every plain install too. There is no separate imperative ConfigMap
patch or manual OpenBao re-seed step to run either way; `apply()` reads the
resolved mode and reconciles Postfix/Dovecot from it.

### Check mail component health

`D-04-mail` runs automated smoke checks on apply: Keycloak master-realm OIDC
discovery and Dovecot IMAP/LMTP TCP. Re-run anytime:

```bash
make verify-kernel-services
```

Set `VERIFY_KERNEL_SERVICES=0` to skip during `./install.sh`. Tune timeouts with
`KEYCLOAK_VERIFY_TIMEOUT` / `DOVECOT_VERIFY_TIMEOUT` (seconds, default 300).

```bash
# Dovecot — deployed only when mail.serviceMode=kernel; absent otherwise
kubectl get release dovecot-dev -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
kubectl logs -n platform-kernel -l app.kubernetes.io/name=dovecot --tail=20

# Postfix — always deployed, in both modes (design/mail.md §8)
kubectl get release postfix-dev -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
kubectl logs -n platform-kernel -l app.kubernetes.io/name=postfix --tail=20

# ESO secrets synced
kubectl get externalsecret -n platform-kernel dovecot-sensitive-values postfix-sensitive-values
```

`dovecot-<stage>` / `postfix-<stage>` above — substitute the cluster's actual stage
(`dev`/`staging`/`prod`) for `-dev`.

### Switch back to external relay mode

```yaml
# gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml
spec:
  mail:
    serviceMode: external
    host: smtp.gmail.com
    port: 587
    ssl: false
    starttls: true
    # username/password go through the credential manager, not the claim —
    # see the "smtp-relay" CredentialRequirement (credentials.yaml)
```

```bash
./install.sh --only D-04-mail --force
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
| **Platform key** (default) | whoever holds the identity for `backupRecipients` — in the recovery kit, and in OpenBao when the cluster escrows it | routine and scheduled backups; the only mode support can help restore |
| **My passphrase** | only the passphrase holder | a bundle the platform must not be able to read |

Both produce ordinary [age](https://age-encryption.org) files. A lost passphrase
means a lost bundle — there is no recovery path, by design.

A cluster needs a key pair before the default mode works, and
`./install.sh --export-recovery-kit` makes one on a cluster that has none: the
public half goes to OpenBao at `gentian-os/kernel/backup/recipients`, the
private half into the kit and, unless `spec.backup.escrowIdentity` is `false`,
to `gentian-os/kernel/backup/identity` as well. It never regenerates — a second
pair would orphan every bundle written to the first.

Pin the public half in git so the operator prefers a value the cluster cannot
rewrite:

```yaml
# gentian-deployments/clusters/<cluster>/kernel/values.yaml
backupRecipients:
  - age1...          # public key; safe to commit
```

With no recipient anywhere, an export fails rather than writing a tenant's data
unencrypted.

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

**Tearing a tenant down does not delete its bundles.** These resources live in
the tenant's namespace, so retiring a tenant with `deletionPolicy: Delete` removes them — and if
that removed the bundles too, "purge the tenant, then restore it" would destroy
the only thing that could restore it. The operator recognises a teardown and
keeps the objects, logging the bucket and prefix for each.

The consequence is real and points the other way: bundles outlive their tenant
and nothing removes them afterwards. On an erasure request, or simply to
reclaim the space, remove them deliberately once you are sure:

```bash
# What a deleted tenant left behind
mc ls gentian/demo-gentian-backup/

# Removing it is not reversible and no backup remains afterwards
mc rm --recursive --force gentian/demo-gentian-backup/
```

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

#### When the bundle is on external storage

`mc` above is aliased to the platform's own MinIO. A tenant whose policy names
an external endpoint writes there instead, with a credential the operator keeps
in the kernel namespace rather than the tenant's — so reading such a bundle
means aliasing that endpoint first:

```bash
ns=platform-kernel                       # not the tenant namespace
sec=backup-destination-<tenant>          # from status.bundle.credentialSecret
ak=$(kubectl get secret "$sec" -n "$ns" -o jsonpath='{.data.accessKey}'  | base64 -d)
sk=$(kubectl get secret "$sec" -n "$ns" -o jsonpath='{.data.secretKey}' | base64 -d)

mc alias set ext https://sos-ch-dk-2.exo.io "$ak" "$sk" --api S3v4
mc ls  ext/<bucket>/<prefix>/
mc cat ext/<bucket>/<prefix>/bundle-info.json
```

`kubectl get tenantexport <name> -n tenant-<t> -o jsonpath='{.status.bundle}'`
names the endpoint, bucket, prefix and credential for any given export, so
nothing here has to be guessed.

The identity to decrypt with comes from the recovery kit, from the printed QR,
or — on a cluster that escrows it — straight out of OpenBao:

```bash
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > id.txt
mc cat ext/<bucket>/<prefix>/manifest.json.age | age -d -i id.txt
```

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

Where the identity comes from depends on `spec.backup.escrowIdentity`. With it
`true` (the default) OpenBao holds a copy at `gentian-os/kernel/backup/identity`,
readable by `cluster-admin` and denied to External Secrets — so a cluster
administrator can restore without the kit. With it `false` the kit is the only
copy, and a restore is where you prove you still have it:

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

## 13. Backup Policy

Where bundles go, how often, and how long they are kept. One cluster default,
overridden per tenant.

```bash
kubectl apply -f - <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: BackupPolicy
metadata:
  name: default          # the cluster policy is a singleton by this name
spec:
  scope: cluster
  destination:
    endpoint: https://sos-ch-gva-2.exo.io
    bucket: gentian-bundles
    region: ch-gva-2
  schedule: "0 3 * * *"  # UTC; omit for no scheduled backups
  retention:
    keepLast: 7
    keepWeekly: 4
    keepMonthly: 12
EOF

kubectl get backuppolicies
```

**Naming an endpoint declares a credential rather than taking one.** The
operator creates a `CredentialRequirement` — `backup-destination` for the
cluster, `backup-destination-<tenant>` for a tenant — and the policy reports
`Accepted=False` with `CredentialUnsatisfied` until the keys are supplied
through the credential manager. That is deliberate: the gap shows when the
destination is set, not at 03:00 when the first export fails.

```bash
kubectl get backuppolicy default \
  -o jsonpath='{.status.credentialRequirement} satisfied={.status.credentialSatisfied}{"\n"}'
```

**The name is not free.** Every reader fetches a policy by name: the cluster's
as `default`, a tenant's as the tenant's own name. Admission enforces both, so a
misnamed policy is rejected at `kubectl apply` rather than accepted and then
read by nothing.

A tenant states its own with `scope: tenant` and a `tenant` field. Name it from
one variable so the two cannot drift:

```bash
TENANT=demo
kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: BackupPolicy
metadata:
  name: ${TENANT}          # must equal spec.tenant
spec:
  scope: tenant
  tenant: ${TENANT}
  destination:
    endpoint: https://sos-ch-gva-2.exo.io
    bucket: ${TENANT}-bundles
    region: ch-gva-2
  schedule: "0 3 * * *"
EOF
```

Every field is optional and an unset one inherits. `suspendSchedule: true` means
*none*, as distinct from *not stated* — without it a tenant could not opt out of
a cluster-wide schedule. Set `allowTenantOverride: false` on the cluster policy
to refuse tenant policies outright; they are then rejected rather than ignored.

Retention tiers are a union: a bundle any rule keeps survives. `keepLast: 7`
alone reaches back seven nights; adding `keepMonthly: 12` reaches back a year
for the cost of twelve more bundles. With nothing set, nothing is deleted.

**A bundle records where it was written.** Changing a destination does not move
what already exists, so restore and cleanup read each bundle's own endpoint
rather than the policy's current answer — last month's bundles stay reachable
after a tenant moves its storage.

### Object Lock

Full bundles suit WORM storage because no object is ever rewritten; each export
occupies a fresh prefix. Set a default retention on the bucket and every
uploaded object inherits it:

```bash
mc retention set --default COMPLIANCE 30d gentian/gentian-bundles
```

Retention deletes will then fail for locked objects until they expire, which is
the lock doing its job. Size the policy's tiers against the lock period rather
than against the bucket.

## 14. Scheduled Backups

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

## 15. Restore Drill

Run this on a scratch tenant before you need it. An untested backup is a
hypothesis. For the procedures themselves — rebuilding a cluster, restoring a
tenant with the cluster key, or with a tenant's own — see
[recovery-playbook.md](recovery-playbook.md), or run `scripts/recovery.sh`,
which performs them.

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

### The other recovery: the kit

A tenant bundle restores a tenant into a working cluster. It cannot rebuild the
cluster — the derived credentials every kernel service authenticates with come
from the master password and the derivation salt, which live in OpenBao, and a
disaster that loses OpenBao's storage loses them. That is what the recovery kit
carries, and it is worth proving it round-trips before trusting it:

```bash
./install.sh --export-recovery-kit          # writes an encrypted kit, mode 0600
make verify-recovery-kit                    # export, load back, compare byte-exact
```

`verify-recovery-kit` needs no cluster. It exports a kit from known values and
reads it back in a clean shell, asserting the salt and every credential return
unaltered through newlines, quotes and shell metacharacters, and that a
tampered key name, a file that is not a kit, and a missing identity are each
refused rather than partially applied. A salt that comes back wrong by one byte
reproduces a whole cluster of wrong passwords, and the symptom is an admin
login that fails for no visible reason.

Storing the kit in the cluster it protects defeats it. It belongs wherever the
organisation keeps break-glass material, beside the backup identity.

**If a drill wedges:** an app stuck in `.status.quiesced` is offline. The
operator resumes anything it finds paused on the next reconcile, including
after a restart, so deleting the stuck `TenantExport`/`TenantRestore` is the
recovery — the workload's `gentianos.io/pre-export-replicas` annotation records
what it should be scaled back to.

Both of those depend on the operator still reconciling, so establish that
first. Every step above is a reconcile away from working, and a controller that
has stopped will not take the delete either: a `TenantExport` carries a
finalizer, and deleting it hangs rather than resuming anything.

```bash
# The export controller's own log. Silence since the last "paused app for
# capture", while other controllers keep logging, is a stopped worker.
kubectl -n gentian-system logs deploy/gentian-os | grep tenantexport | tail

# What a stopped worker usually is: a cache that cannot sync, because the
# ServiceAccount may not list the type something read through it.
kubectl -n gentian-system logs deploy/gentian-os | grep -i forbidden
```

A restart clears the wedge and the resume follows from it — the operator
resumes anything it finds paused. It does not clear the cause: if the log names
a Forbidden, the missing rule is a marker and a `make gen-all` away, and the
export will wedge again on the next attempt without it.

```bash
kubectl -n gentian-system rollout restart deploy/gentian-os
```
