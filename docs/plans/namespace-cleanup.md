# Namespace cleanup

Five categories of namespace, each with one authority, one policy set and
one answer to "what does the platform still guarantee if this is
compromised". Section 3 is the inventory of everything running today,
placed into the categories, for review. Section 4 lists what the inventory
turned up that is not a namespace question. Section 5 is how to get there
without re-initialising anything that must not be re-initialised.

## 1. The categories

| Category | Contains | Installed by | Authority | Compromise guarantee |
| --- | --- | --- | --- | --- |
| `kernel-<class>` | services the OS is made of, including the tier-0 operators | `install.sh`, then Argo CD | break-glass platform admin only | none — this is the trust root |
| `system-<class>` | services that satisfy app `kernelRequirements` | kernel services, from the Cluster claim | platform admin through the director | other system classes; every tenant boundary |
| `shared-<app>` | one app instance serving several tenants | director, from a profile with `tenancy: shared` | platform admin through the director | only what the app's own code enforces — hence the admission bar in §2.5 |
| `tenant-<t>` | the tenant's apps and its desktop BFF | operator, from `Tenant.spec.apps` | tenant admin through the director | every other tenant; the kernel; the system services beyond declared contracts |
| `tenant-<t>-public` | the tenant's DMZ: publishing proxies for anonymous and protocol traffic | operator, from `publicSurfaces` in the profiles | tenant admin through the director | the tenant's own apps — the proxy holds one least-privilege credential per surface |

The test for a new namespace inside a category: a different exposure, a
different credential set, a different upgrade owner, or a different quota
than its neighbour. None of the four → same namespace.

Policies select on labels, never on names: every namespace carries
`gentianos.io/tier: kernel|system|shared|tenant|tenant-public` and
`gentianos.io/class: <class>` (or `gentianos.io/tenant: <t>`). This is what
makes §5 possible.

## 2. Decisions

### 2.1 Authentication and authorization are separate namespaces

`kernel-authentication` (Keycloak) and `kernel-authorization` (OpenFGA).
Two of the four tests differ: Keycloak has a public route through the
gateway, OpenFGA must be reachable only from the enforcement points;
Keycloak's admin credential and OpenFGA's API token are held by different
callers. Security principle 2 — three answerers, no overlap — is also a
statement about blast radius: a foothold in the public-facing issuer must
not be a foothold in the decision graph. Cost: one NetworkPolicy pair for
the authz bridge, which lives in `kernel-control` anyway.

### 2.2 Tier-0 operators are kernel, and get kernel names

Crossplane, Argo CD, ESO, cert-manager, Kyverno, Envoy Gateway, CNPG,
Reloader are installed by `install.sh` and hold cluster-admin-equivalent
RBAC; by the category definition they are kernel. They move under
`kernel-<class>` grouped by function rather than one namespace per
upstream chart, because isolation *between* cluster-admin components
guarantees nothing — the grouping is for ownership and policy selection.
Two exceptions stay where they are and receive labels only: metrics-server
in `kube-system` (API aggregation convention) and MetalLB in
`metallb-system` (platform-provided, may pre-exist on a self-hosted
cluster; gentian-os only ships the Kyverno exception for it).

### 2.3 The seal is its own namespace

`kernel-seal` holds the OpenBao transit instance. Its whole purpose is to
be a failure and trust domain apart from the OpenBao that holds secrets;
putting both in `kernel-secrets` gives any ServiceAccount that can read
Secrets there both the vault and its unseal key.

### 2.4 Kernel services get their own Postgres

`kernel-data` holds a CNPG Cluster (`kernel-postgres`) serving Keycloak,
Keycloak extensions, OpenFGA and the platform admin console. Today those
databases live on the Bitnami `infra-postgresql` release in
`gentian-infra-<stage>` beside nothing else — tenant databases are on the
CNPG `postgres` cluster — so the move retires a chart and its hand-rolled
`job.databases` bootstrap, and leaves the kernel unable to lose its
identity store to a tenant data-plane incident. The CNPG operator lives in
`kernel-data` too: it is the data class's operator.

### 2.5 Shared apps: none today, and a bar to clear

The category exists; nothing qualifies yet. A profile may declare
`tenancy: shared` only with `trustTier: platform` and one of two shapes:
stateless per request (nothing tenant-owned is stored), or a native tenant
model the platform can verify per tenant (Keycloak realms are the
reference). Per-tenant state inside one instance is refused at admission.

### 2.6 System services have no public route

`system-llm` today carries `llm.<kernel>` publicly. Under the taxonomy a
system namespace is reached from tenant namespaces over declared
contracts, and from outside only through a tenant's DMZ proxy carrying
that tenant's credential. That closes the unauthenticated LLM route and
makes "who is calling LiteLLM from the internet" a per-tenant question
with a per-tenant answer. Mail is the exception by protocol: SMTP
submission and IMAP are TCP listeners on `system-mail`'s own load
balancer, authenticated by the broker's per-user credentials.

### 2.7 The portal splits three ways

The shell's static bundle → `shared-shell` (public code, no state). The
tenant desktop BFF → `tenant-<t>`, one per tenant, with only that
tenant's realm client and `{t}_shell` database credential. The
platform-admin console → `kernel-control`, in the kernel realm, a separate
deployment. Today all three are one pod in `platform-kernel` holding one
ServiceAccount for every tenant.

## 3. Inventory: today → target

Every workload found in `scripts/steps/`, `kernel/bootstrap/chart/`,
`kernel/appsets/raw/`, `kernel/services/`, `crossplane/compositions/` and
the catalogue. "Moves" means the workload's namespace changes; "stays"
means labels only.

### 3.1 Kernel

| Workload | Today | Target | Notes |
| --- | --- | --- | --- |
| Argo CD, argocd-image-updater | `argocd`, `argocd-image-updater` | `kernel-gitops` | 77 installer references to `-n argocd`; see §5 |
| Crossplane, providers (helm, kubernetes, keycloak, vault), functions | `crossplane-system` | `kernel-crossplane` | provider RBAC scoping (roadmap 1.16) is the real work here |
| OpenBao | `openbao` | `kernel-secrets` | stateful; **never renamed in place** (§5) |
| OpenBao transit seal | `openbao` | `kernel-seal` | §2.3 |
| External Secrets Operator | `external-secrets` | `kernel-secrets` | |
| Reloader | `stakater-system` | `kernel-secrets` | part of the rotation path (security.md §9) |
| Keycloak, `keycloak-idp` config (theme, SMTP ExternalSecret), realm script | `platform-kernel` | `kernel-authentication` | composed by the Suze claim (`idpNamespace`) |
| OpenFGA | `platform-kernel` | `kernel-authorization` | composed by the Suze claim |
| gentian-os operator, credential manager, `job-gc` CronJob | `gentian-system` | `kernel-control` | director joins here (operator-split-plan.md) |
| `kernel-admin`: admin credentials, `portal-shell-database`, CNPG `postgres` | `platform-kernel` | split: admin credentials → `kernel-control`; `portal-shell` DB → `kernel-data`; CNPG `postgres` cluster → `system-data` | the one chart that mixes kernel and system today |
| CNPG operator | `cnpg-system` | `kernel-data` | |
| **new:** CNPG `kernel-postgres` for Keycloak, OpenFGA, admin console | — | `kernel-data` | §2.4 |
| cert-manager, self-signed ClusterIssuers | `cert-manager` | `kernel-edge` | |
| Envoy Gateway, GatewayClass | `envoy-gateway-system` | `kernel-edge` | tenant Gateways stay in `tenant-<t>` |
| external-dns | `external-dns` | `kernel-edge` | |
| cloudflared (tunnel mode) | operator chart, `gentian-system` | `kernel-edge` | ExternalSecret `cf-tunnel` moves with it |
| Kyverno + baseline policies | `kyverno` | `kernel-admission` | |
| **new:** platform-admin console | part of `gentian-portal` in `platform-kernel` | `kernel-control` | §2.7 |
| metrics-server | `kube-system` | stays, labelled | |
| MetalLB (+ Kyverno exception) | `metallb-system` | stays, labelled | not installed by gentian-os |

### 3.2 System

| Workload | Today | Target | Notes |
| --- | --- | --- | --- |
| CNPG `postgres` (tenant databases) | `platform-kernel` via `kernel-admin` | `system-data` | `database_reconciler` `CNPG_CLUSTER_NAME` gains a namespace |
| `infra-postgresql` (Bitnami) | `gentian-infra-<stage>` | **retired** after §2.4 | hosts only Keycloak/OpenFGA databases today |
| `infra-mariadb`, `infra-redis`, `infra-minio` | `gentian-infra-<stage>` | `system-data` | drop the stage suffix: a cluster has exactly one stage |
| Postfix | `platform-kernel` | `system-mail` | `mail.serviceMode: kernel` only |
| Dovecot | `platform-kernel` | `system-mail` | conditional ApplicationSet |
| LiteLLM proxy, `litellm-db` (CNPG), `redis-llm` | `platform-kernel` | `system-llm` | public route removed (§2.6) |
| vLLM instances, mock backend | `platform-kernel` via D-05 | `system-llm` | still applied by the installer; belongs in the Cluster composition |

### 3.3 Shared

| Workload | Today | Target | Notes |
| --- | --- | --- | --- |
| Shell static bundle (`gentian-portal` web) | `platform-kernel` | `shared-shell` | §2.7 |
| *candidate:* Collabora | sidecar / extra ingress of `nextcloud-base-ce`, per tenant | `shared-collabora` | stateless per request; only if the WOPI source is verified per tenant |
| *candidate:* App Store (`app-store-me`) | per tenant, `platform` app | `shared-app-store` | relay-only in the target design; per tenant until then |

### 3.4 Tenant

`tenant-<t>` holds, per tenant: the desktop BFF (§2.7); every app in
`Tenant.spec.apps` as a provider-helm Release; and what the operator
creates around them — Namespace, ResourceQuota, NetworkPolicies, Services,
ConfigMaps, ExternalSecrets, provisioning and export Jobs, the tenant
Gateway and HTTPRoutes.

Catalogue apps today, by family: activepieces, docmost, element (with
Matrix), mathesar, nextcloud (base-ce, base-od, nine addons), odoo (base,
twelve addons), openproject, open-webui, xwiki, litellm-me (API profile),
gentian-subscriptions (API profile), app-store. All are tenant-scoped; the
Keycloak realm is tenant-scoped but is not a namespace.

### 3.5 Tenant DMZ

`tenant-<t>-public` holds one publishing proxy per declared public surface.
From the profiles, the surfaces that exist today and would move behind it
(each to be verified against the app before the field is written):

| App | Surface | Auth at the proxy |
| --- | --- | --- |
| nextcloud | `/s/*` share links, `/public.php/*`, `/.well-known/*` | none — capability in the URL |
| nextcloud | `/remote.php/dav/*` (WebDAV, CalDAV, CardDAV) | basic — app passwords from the broker |
| nextcloud | Collabora WOPI callbacks | none, source-restricted |
| element | Matrix client API (`browserProxy` `forward-bearer`), `/.well-known/matrix/*`, federation | bearer / none / federation signature |
| openproject | API (`browserProxy` `forward-bearer`) | bearer |
| app-store | `api`, `oauth` proxies | bearer / none |
| odoo-website | public website pages | none |
| docmost | public sharing | none |
| any app | LiteLLM access from outside the cluster | bearer, tenant key → `system-llm` (§2.6) |

Everything not listed stays on the authenticated gateway with no bypass.

## 4. What the inventory found beyond namespaces

- **`platform-kernel` is five things.** Identity, authorization, control,
  mail, LLM and the tenants' Postgres share one namespace and therefore one
  same-namespace network and secret scope. A LiteLLM pod behind a public
  route is a neighbour of Keycloak.
- **Two Postgres stacks.** Bitnami `infra-postgresql` for the kernel's
  databases, CNPG `postgres` for tenants, each with its own bootstrap
  mechanism. §2.4 leaves one operator and two clusters.
- **`kernel-admin` mixes tiers.** It composes the tenants' shared CNPG
  cluster, the portal's database and the admin credentials — one chart,
  three categories.
- **The stage suffix on `gentian-infra-<stage>`** contradicts the
  deployments repository's own rule that a cluster has one stage for its
  lifetime.
- **Operator-side namespace names are strings.** `meta.KernelNamespace`,
  `netpolicy.Config{InfraNamespace, ServicesNamespace, OpenbaoNamespace}`,
  `CNPG_CLUSTER_NAME` without a namespace. They become label selectors in
  §5 step 1, or the rename is a five-file code change every time.
- **OpenBao Kubernetes-auth roles bind ServiceAccount namespaces.** A
  renamed namespace is a new principal to OpenBao; the roles in
  `cluster-default.yaml` (`AuthBackendRole` ×5) and `openbao-config` are
  re-issued as part of any move.

## 5. Getting there

The taxonomy is a target for **fresh installs**. Stateful kernel
components — OpenBao, Keycloak, the CNPG clusters — are not renamed in
place: a namespace move is a delete-and-recreate, and re-initialising
OpenBao on a cluster with tenants regenerates every derived credential.
An existing cluster either rebuilds from its recovery kit into the new
layout or keeps its names and adopts the labels.

1. **Labels first.** Add `gentianos.io/tier` and `gentianos.io/class` to
   every namespace the installer or a composition creates. Change
   NetworkPolicy generation, Kyverno policy scoping and the operator's
   namespace constants to select on them. Nothing moves; every policy
   stops caring what a namespace is called.
2. **Stateless moves.** Operator, director, credential manager, Argo CD,
   Crossplane, ESO, Reloader, cert-manager, Envoy Gateway, external-dns,
   Kyverno: rename in `install.sh`, the bootstrap chart and the
   ApplicationSets. Re-issue OpenBao roles for the operator's new
   ServiceAccount namespace. Fresh installs only; one release.
3. **The kernel Postgres.** Create `kernel-data` with `kernel-postgres`;
   point Keycloak, OpenFGA and the admin console at it; retire
   `infra-postgresql`. On an existing cluster this is a database migration
   with downtime for login, done under the recovery kit.
4. **Split `platform-kernel`.** Suze claim `idpNamespace` →
   `kernel-authentication` / `kernel-authorization`; mail and LLM
   ApplicationSets → `system-*`; `kernel-admin` split three ways. Fresh
   installs only.
5. **DMZ and portal split** follow their own plans; they create namespaces,
   they do not rename any.

## 6. For review

- Is `kernel-seal` worth a namespace, or is the transit instance's
  separation adequately expressed by its own ServiceAccount and Secret?
- `kernel-gitops` vs keeping `argocd`: the rename touches 77 installer
  lines and every runbook. The labels in step 1 make the rename cosmetic;
  it could be deferred indefinitely without weakening anything.
- Should `system-data` be one namespace or one per engine
  (`system-postgres`, `system-mariadb`, …)? One owner and one quota today
  argues for one; a managed-database future argues for per engine.
- vLLM is still installer-applied (D-05). Moving it to `system-llm` is a
  chance to move it into the Cluster composition, which its own
  ApplicationSet comment already asks for.
