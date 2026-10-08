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
asks the director as you, which checks, commits and records it. The one
command about a person, `tenants activate-admin`, asks the registrar, which
checks the same way and acts at Keycloak. It reaches the director and the
registrar of the current kubectl context, so it needs no configuration.

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

On a single-tenancy cluster (`tenancyMode: single`) there is exactly one tenant
for users, named `user`, which the install creates; `tenants create` with any
other name is refused with a message that names the mode, and `user` itself is
refused only because it exists.

A tenant's administrator — the **tenant admin**, `admin@<tenant domain>`; on a
single-tenancy cluster the **user admin**, `user-admin@<kernel-domain>` — has
**no password**. Hand the
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
kubectl gentian tenants retire demo           # keeps its data
kubectl gentian tenants retire demo --purge   # deletes its data
```

or **Retire** in the console, which asks the same question. Both ask for the
tenant's name typed out (`--yes` skips that in a script).

Retire removes the tenant's directory from git; Argo CD prunes the Tenant and
the operator tears it down under the manifest's `deletionPolicy`, `Retain`
unless edited, so the realm, databases and files stay. Purge first commits
`deletionPolicy: Delete` (and the `gentianos.io/purge-requested` annotation),
waits until the live Tenant carries it, then removes the directory; the tenant
is listed as purging until then. Not `kubectl delete tenant`: git is what the
cluster reconciles towards, so deleting the object just brings it back. The
platform tenant can be neither retired nor purged.

## 5. Tenant App Store

Tenant admins install apps from the **App Store app** or the CLI. On a
cluster with no store the CLI is the only way: the cluster shows no catalogue
of its own.

### The App Store app

A person who may install apps in a tenant has an **App Store** tile whenever
the Cluster claim names a store (`catalogue.storeUrl`) that is not the App
Store app's own address on a cluster, and the cluster's licence report is on
([design/operations.md §6.2](design/operations.md)). The
tile opens the App Store app, which runs on the cluster. It shows the data of
the store outside the cluster — apps, descriptions, reviews, versions, prices
— and installs through the director and the custodian as the person signed
in. The store itself never calls the cluster. See
[design/store-contract.md](design/store-contract.md).

Apps that are installed are administered in the admin console's **Apps** tab:
state, access, integrations, privileges, uninstall and purge.

### CLI

```bash
kubectl gentian apps list --tenant demo               # what the tenant has installed
kubectl gentian apps list --tenant demo --available   # what the tenant's catalogues offer
kubectl gentian apps install xwiki-ce --tenant demo   # the build the catalogue lists, pinned by digest
kubectl gentian apps uninstall xwiki-ce --tenant demo
```

### Catalogues

A catalogue is an https address apps are installed from. The cluster's
administrator adds one for every tenant or for one tenant; a tenant's own
administrator adds one for their tenant where the cluster's administrator
delegated that.

```bash
kubectl gentian catalogues list                        # everything the cluster declares
kubectl gentian catalogues list --tenant demo          # what demo installs from
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue                 # for every tenant
kubectl gentian catalogues add acme https://acme.github.io/acme-catalogue --tenant demo   # for demo only
kubectl gentian catalogues remove acme --tenant demo
kubectl gentian tenants delegate-catalogues demo on    # demo's administrators may add their own
kubectl gentian tenants delegate-catalogues demo off
kubectl gentian catalogues residue                     # what the catalogue left on the cluster
kubectl gentian catalogues residue remove OIDCPackCatalog/xwiki-ce-oidc   # one object, name typed again
```

```
$ kubectl gentian catalogues list
CATALOGUE  FOR           ADDED BY     ADDRESS
gentian    every tenant  the cluster  https://gentian-org.github.io/gentian-apps
acme       tenant demo   the tenant   https://acme.github.io/acme-catalogue
Tenants whose administrators may add catalogues of their own: demo.
```

With `--tenant`, `add` and `remove` are done as the cluster's administrator
when the person is one, and otherwise as the tenant's administrator: that is
refused unless the tenant is delegated, and removes only what the tenant
added. An address must be a public https one; anything else is refused.
[custom-catalogues.md](custom-catalogues.md) says how to build and publish a
catalogue.

Nothing a profile's bundle brought is removed automatically: a piece a newer
build drops stays, and so does a profile after its last uninstall. `residue`
lists what is left, and `residue remove` deletes one object of the list —
never anything a bundle on the cluster still brings.

```
$ kubectl gentian catalogues residue
KIND/NAME                               NAMESPACE            PROFILE     WHY             CREATED     IN EFFECT
ComponentProfile/acme-notes             -                    acme-notes  unused profile  2026-09-30  -
Composition/app-odoo                    -                    -           unowned         2026-07-02  -
ConfigMap/element-ce.portal-bridge-sso  kernel-provisioning  element-ce  dropped         2026-10-07  -
OIDCPackCatalog/gentian-element-oidc    -                    -           unowned         2026-07-02  contested
OIDCPackCatalog/xwiki-ce-oidc           -                    xwiki-ce    orphaned        2026-08-14  YES

  ComponentProfile/acme-notes: unused profile: no tenant has it installed or switched on as an add-on, and no tenant retains data for it; its bundle brings Composition app-acme-notes, which stay
  Composition/app-odoo: not owned by any bundle: it names no profile, and no bundle on this cluster brings it
  ConfigMap/element-ce.portal-bridge-sso: it carries the label gentianos.io/profile-name: element-ce, and the bundle now materialised for element-ce does not bring it
  OIDCPackCatalog/gentian-element-oidc: not owned by any bundle: it names no profile, and no bundle on this cluster brings it; another catalog also holds: element
  OIDCPackCatalog/xwiki-ce-oidc: it carries the label gentianos.io/profile-name: xwiki-ce, and no profile xwiki-ce is on this cluster; the only pack for: xwiki

Nothing here is removed automatically. To remove one:
  kubectl gentian catalogues residue remove <kind>/<name>

$ kubectl gentian catalogues residue remove OIDCPackCatalog/xwiki-ce-oidc
Removing the OIDCPackCatalog xwiki-ce-oidc deletes it from the cluster. Nothing puts it back but
installing a build that brings it.
Type xwiki-ce-oidc to confirm: xwiki-ce-oidc
Deleted the OIDCPackCatalog xwiki-ce-oidc.
  the OIDCPackCatalog xwiki-ce-oidc was deleted, asked for by ada@example.com
```

`IN EFFECT` says whether a leftover OIDC pack catalog is still read when a
client is configured. The list is for whoever may audit the cluster; removing
is for whoever may configure it. `--yes` takes the place of typing the name,
and `--namespace` names the namespace of a ConfigMap or a customization
record, which is the catalogue's and no other. An unused profile is removed
from the deployments repository first and deleted from the cluster once Argo
CD has synced that; [custom-catalogues.md](custom-catalogues.md) §6 has the
classes and the rules.

Guides:

- [custom-catalogues.md](custom-catalogues.md) — publish your own apps to a cluster or a tenant
- [gentian-apps/docs/custom-app-guide.md](../../gentian-apps/docs/custom-app-guide.md) — build new apps
- [gentian-apps/docs/app-profile-guide.md](../../gentian-apps/docs/app-profile-guide.md) — publish upstream charts

Show all available `kubectl gentian` subcommands:

```bash
kubectl gentian --help
```

## 6. Install and Uninstall Apps

Apps are installed by the director adding them to the tenant's manifest in
git, as the person who asked; the operator reconciles them. The tenant's
administrator usually does this from the App Store app; the CLI does the same:

```bash
kubectl gentian apps list --tenant demo                              # what the tenant has installed
kubectl gentian apps list --tenant demo --available                  # what the tenant's catalogues offer
kubectl gentian apps install xwiki-ce --tenant demo                  # the build the catalogue lists
kubectl gentian apps install xwiki-ce --tenant demo --from in-house  # when several catalogues serve the name
kubectl gentian apps install xwiki-ce --tenant demo --digest sha256:<64 hex>   # a build you name yourself
kubectl gentian apps install xwiki-ce --tenant demo --for-everyone   # and grants it to every member
kubectl gentian apps uninstall xwiki-ce --tenant demo            # removes the app, keeps its data
kubectl gentian apps uninstall xwiki-ce --tenant demo --purge    # removes the app, then destroys its data
```

**The command line installs a pinned build.** An app is installed from one
of the catalogues the tenant sees — the cluster's and the tenant's own
(`kubectl gentian catalogues list --tenant demo`) — and `install` sends the
director the entry's coordinate and digest. Nothing is on a cluster ahead of
an install: the director fetches that one profile, checks it against the
digest and commits it.

```
$ kubectl gentian apps list --tenant demo --available
SOURCE    APP                VERSION  EDITION  DIGEST               INSTALLED
gentian   nextcloud-base-ce  31.0.4   ce       sha256:2f1c0d9a77b3  yes
gentian   xwiki-ce           16.4.0   ce       sha256:9b41e6c05d12  -
in-house  xwiki-ce           16.5.0   pe       sha256:c07a1be4403f  -

$ kubectl gentian apps install xwiki-ce --tenant demo --from in-house
Pinned in-house/xwiki-ce  version 16.5.0  sha256:c07a1be4403f
Installing xwiki-ce into demo: committed (4e1f9a2c); the platform installs it.
```

`INSTALLED` says `yes` for the build the tenant has, `other build` when the
tenant has the app at another digest, and `unpinned` when it was installed
with none. The listing is the director's reading of each source's index, the
`ce` and `pe` entries only.

| | |
|---|---|
| One source serves the name | it is used |
| Several do | `--from <source>` says which; without it nothing is installed |
| None does | nothing is installed, and the sources are listed. With both `--from` and `--digest` an entry the listing does not show can still be asked for |
| `--digest` | the build to install instead of the one the listing states. The director fetches the bundle from the source and installs nothing unless it hashes to the digest sent |
| The tenant has no catalogue | nothing can be installed by command; the cluster's administrator adds one (`kubectl gentian catalogues add`) |
| The name is taken by a profile from another catalogue | nothing is installed, and the refusal says who has to rename ([custom-catalogues.md](custom-catalogues.md) §7) |

The director verifies the bundle against the digest before it commits, and
the operator verifies it again at rollout
([design/store-contract.md](design/store-contract.md) §3, §4).

Uninstalling and purging are two different acts, and the CLI says which one it
did.

**Uninstalling** removes the app from the tenant: its workloads and its
sign-in client go, and everything it stored stays — its database, its object
storage, its cache user, its files, its stored credentials, and the access
group with everybody who is in it. The app is then *retained*: uninstalled,
data retained. Installing it again in the same tenant finds all of it.
A backup taken after the uninstall does not hold that data: only one taken
while the app was installed does. The director's answer to an uninstall says
so (`note`), and every later backup names the uninstalled apps it left out
(`status.notIncluded`).

**Purging** destroys what uninstalling kept, and cannot be undone. It is
refused while the app is still the tenant's. `--purge` therefore does both, in
order: it uninstalls the app (or finds it already uninstalled), waits until
the app is gone from the cluster, and then purges it. "Gone" is the platform's
own answer — the tenant no longer names the app, its Component is gone, and
Helm has finished uninstalling its release. The wait is bounded by
`GENTIAN_UNINSTALL_WAIT` seconds (default 900); when it runs out the command
says so, nothing has been purged, and the same command can be run again.
What every act does with every kind of data is one table in
[design/data-lifecycle.md](design/data-lifecycle.md).

A purge is refused, with nothing destroyed, when the app's profile is not on
the cluster — what the app owns cannot be determined without it; installing
the app again from a catalogue source puts the profile back — and when the
vault, the identity provider or the database it will need does not answer.

The purge itself is one request that is answered when it is over. If a step
fails, the command ends with which step failed, what had already been
destroyed and what was not attempted; nothing is rolled back, every step is
safe to repeat, and running the command again continues with what is left.
What each act keeps and destroys, kind by kind, is in
[design/store-contract.md](design/store-contract.md) §8.

`--for-everyone` writes `defaultGrant: true` on the app's entry in the same
commit. Every member of the tenant then has the app by default: the people who
are members when the app's group first exists are added to it once, and
somebody invited later has it pre-selected. It needs the right to grant in the
tenant as well as to install. Without the flag, access is given per person
([design/store-contract.md](design/store-contract.md) §3).

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

No administrator is given a password. The **platform admin**
(`admin@<kernel-domain>`, who signs in at `https://platform.<kernel-domain>/`)
and every **tenant admin** — on a single-tenancy cluster, the one **user
admin**, `user-admin@<kernel-domain>` — are created without
one and handed over through a single-use, expiring link that sets it — and
enrols a second factor unless that was switched off — mailed to a recovery
address or shown once to whoever issued it. There is nothing to retrieve.

A new link, for a lost password or a link that expired:

```bash
kubectl gentian tenants activate-admin <tenant> [--recovery-email <address>]
./install.sh --activate-admin                        # the platform admin
```

The first asks the registrar, which changes nothing about a member of a
group that holds a platform role and answers 403 for the platform admin's
account. That account's link comes from the install host, with the installer's
own credential — which is also the way in when nobody can sign in.

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
    # username/password go through the custodian, not the claim —
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

# The capture Jobs themselves. Each runs where its credential is: a database
# dump beside its database, the realm export in the identity namespace, a
# bucket and the manifest beside the object store, a volume archive in the
# tenant's namespace (design/operations.md §9.4a).
for ns in system-postgresql system-mariadb system-s3 kernel-authentication tenant-demo; do
  kubectl get jobs -n "$ns" -l gentianos.io/tenant-export=nightly-2026-08-18,gentianos.io/tenant=demo
done
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

**Retiring a tenant keeps its bundles; deleting it destroys them, unless it
was told to keep them.** These resources live in the tenant's namespace, so a
teardown removes them. The operator recognises a teardown and does not delete
a bundle because its `TenantExport` went. What happens to the bundles is then
the deletion policy's to say: with `Retain` the backup bucket stays; with
`Delete` the bucket is destroyed with the tenant's other stores, unless the
purge was asked for with `keepBundles` (or the bundles are on external
storage, which the platform never deletes). A bundle you want to outlive its
tenant has to be downloaded, or kept that way
([design/data-lifecycle.md](design/data-lifecycle.md) §2).

Bundles a retired tenant left behind stay until they are removed by hand. On
an erasure request, or simply to reclaim the space, remove them deliberately
once you are sure:

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
time, and the pause window each app saw. It is what a restore goes by. Since
format 2 (`schemaVersion: 2`) each app's `stores` has one entry per artefact
with its `kind`, the `name` of what it was captured from and its `path` in the
bundle, and the app carries its `digest`, `databaseEngine` and `releases`; a
format 1 manifest names apps and kinds only and still restores
([operations.md](design/operations.md) §9.4).

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
2. **Plan** — the operator opens the bundle's manifest with the key and
   decides, against the tenant as it stands, what it puts back. Only apps the
   manifest lists are touched. An app is restored whole or not at all; one
   that is not installed, runs an older build than wrote the data (unless
   `spec.skipVersionCheck`), or has a store the bundle's does not match, is
   named in `status.notRestored` with the reason. With `spec.apps`, an app
   named that cannot be restored refuses the whole restore instead. Still
   nothing has been touched.
3. **Per app** — pause, load the database (on PostgreSQL also every database
   the app's role owned), make the bucket with its user and policy and load
   it, unpack volumes, run the profile's `restore.post` hooks, run
   `restore.verify`, resume. One app at a time.
4. **Tenant-wide, last** — Keycloak realm and the portal shell database. Last
   deliberately: restoring identity earlier would let members sign in to
   half-restored data.

```bash
kubectl get tenantrestore restore-2026-08-18 -n tenant-demo -o jsonpath='{.status}' | jq \
  '{phase, complete, notRestored, nameDerivation, bundleSchemaVersion, notes}'
```

`phase: Ready` with `complete: false` is a restore that left something out;
`notRestored` says what and why. `notes` is what no restore brings back
([data-lifecycle.md](design/data-lifecycle.md) §5).

### After a restore, members cannot sign in

Keycloak's export carries no password hashes, so accounts come back without
credentials. `status.passwordResetRequired` says so. Send members through a
reset from **Admin Console → Members**.

### After a restore, credentials somebody entered are missing

A bundle holds no stored credential. The platform's own were made for the
tenant when it was provisioned and are unchanged; anything a person typed in —
a repository's password, an SMTP relay's, an API key — is not in the bundle and
has to be entered again. Data an app sealed with a secret the platform
generated for it reads only on the cluster the bundle was taken on, or one
built from its recovery kit.

## 12a. Tenant Import

Create plus Restore, from a bundle, on this cluster or another one
(sovereignty-concept.md §4.3). The bundle is the `.gentian` file **Download**
produced, or a bucket prefix the cluster can reach.

```bash
kubectl gentian tenants import acme-nightly.gentian --identity-file acme-key.txt
kubectl gentian tenants import acme-nightly.gentian --passphrase          # prompts
kubectl gentian tenants import acme-nightly.gentian --identity-file k.txt --name acme2
kubectl gentian tenants import --bucket acme-gentian-backup --prefix nightly-20261001 --identity-file k.txt
```

What happens: the file is uploaded to the cluster's own storage; the director
opens the manifest with the key and refuses a name the cluster already has.
It then makes sure of the definition of every app the bundle's tenant lists:
on the cluster already at the build the bundle records, or fetched at that
build from one of the cluster's catalogues and committed. If one cannot be
had, the import is refused with `422`, names each, and has changed nothing.
It commits the tenant from the manifest's settings (deletionPolicy Retain,
whatever the bundle said), waits until the operator reports the tenant Ready
with its apps up, and starts a `TenantRestore`. The command follows the
import and prints each phase: `declared`, `provisioning`, `restoring`,
`ready`.

The new tenant's realm, database prefix and bucket prefix follow from its own
name, whatever the bundle states. With `--name acme2` beside `acme`, nothing
of `acme` is touched. A tenant whose realm or prefixes are already another
tenant's is refused with `409`, on import and on create.

The import is recorded in git beside the tenant (`import.json`) until it has
finished, so it survives a restart of the director. The record holds no key.
If the director restarts before the restore has started, the status says
`awaiting-key`: run the same command again, with the same bundle, name and
key, and the import goes on. If the restore was already running, nothing is
needed.

Directly against the director:

```
POST /v1/clusters/{c}/bundles                 body: the .gentian bytes -> {bundle}
POST /v1/clusters/{c}/bundles/inspect         {bundle, decryption} -> manifest
POST /v1/clusters/{c}/tenants/import          {bundle, decryption, name?} -> 202 status
GET  /v1/clusters/{c}/tenants/{t}/import      the status
```

`decryption` is `{"passphrase": "..."}` or `{"identity": "AGE-SECRET-KEY-1..."}`.
The key is written into a Secret the restore owns and goes with it. Members
come back without credentials (no bundle carries them); the status says
`passwordResetRequired`, and carries the restore's `complete`, `notRestored`
and `notes`.

The uploaded file is kept in the cluster's `gentian-imports` bucket until a
restore of it has run to its end, restored or failed, and is then removed. A
restore refused before it changed anything leaves it, so the import can be
asked for again without uploading twice; an upload that is never restored
stays until it is removed by hand (`mc rm --recursive --force
gentian/gentian-imports/<prefix>/`). App grants are not in a bundle: they are
declared in git, and a tenant imported into another cluster has them to set
again.

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
through the custodian. That is deliberate: the gap shows when the
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
