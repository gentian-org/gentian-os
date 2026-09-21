# Namespace cleanup

Five categories of namespace, each with one authority and one policy set.
Section 2 records the decisions; section 3 places every workload running
today into its target namespace.

Terms: a *profile* is a `ComponentProfile` catalogue entry and an *instance*
is a `Component` deployed from it, as defined in
[component-profile.md](component-profile.md). Today's `AppProfile` is the
profile until that rename lands.

## 1. Categories

| Category | Contains | Installed by | Authority | Compromise guarantee |
| --- | --- | --- | --- | --- |
| `kernel-<function>` | services the OS is made of, including the tier-0 operators | `install.sh`, then Argo CD | break-glass platform admin only | none — this is the trust root |
| `system-<function>` | instances with `tenancy: system`, fulfilling `requires.contracts` of other components | kernel services, from the Cluster claim | platform admin through the director | other system functions; every tenant boundary |
| `shared-<app>` | one instance with `tenancy: shared`, serving several tenants | director, from a Component whose profile certifies `shared` | platform admin through the director | only what the app's own code enforces |
| `tenant-<t>` | the tenant's instances with `tenancy: tenant`, including its desktop BFF | operator, from `Tenant.spec.apps` | tenant admin through the director | every other tenant; the kernel; system services beyond declared contracts |
| `tenant-<t>-dmz` | the tenant's perimeter: publishing proxies that terminate anonymous and protocol traffic | operator, from `expose[]` entries with `surface: perimeter` in the tenant's instances | tenant admin through the director | the tenant's own apps — one least-privilege credential per surface |

A new namespace inside a category needs a different exposure, credential
set, upgrade owner or quota than its neighbour. None of the four → same
namespace.

Every namespace carries `gentianos.io/tier: kernel|system|shared|tenant|tenant-dmz`
and `gentianos.io/function: <function>` (tenant namespaces:
`gentianos.io/tenant: <t>`). Policies select on labels, never on names.

## 2. Decisions

| # | Decision | Rule applied |
| --- | --- | --- |
| D1 | `kernel-authentication` (Keycloak) and `kernel-authorization` (OpenFGA) are separate | exposure differs (public route vs. PEP-only); credential holders differ; security principle 2 |
| D2 | Tier-0 operators are kernel and are named by function, grouped by function not by upstream chart | installed by `install.sh` under break-glass authority; isolation between cluster-admin components guarantees nothing, grouping is for ownership and policy |
| D3 | Crossplane and its providers live in `kernel-provisioning` | the namespace names the function so the software performing it can be replaced |
| D4 | The OpenBao transit seal lives in `kernel-seal`, apart from `kernel-secrets` | the transit's own unseal key is a Secret in the cluster (`openbao-transit-unseal`, written by B-02) beside the vault's storage, so one namespace lets one read-Secrets grant or one namespace-scoped backup unseal the seal and take the vault; separate namespaces put the key and the storage in different RBAC and backup domains. Target for clusters with a KMS: the key leaves the cluster and the transit is unsealed by the KMS |
| D5 | Kernel services use a dedicated CNPG cluster in `kernel-data`; the Bitnami `infra-postgresql` release is retired | kernel identity must not share a data plane with tenants; today that release hosts only kernel databases |
| D6 | The CNPG operator lives in `kernel-data` | the data function's operator |
| D7 | metrics-server stays in `kube-system`, MetalLB in `metallb-system`; both labelled | API-aggregation convention; MetalLB is platform-provided, not installed by gentian-os |
| D8 | System namespaces have no public route; outside access to a system service goes through a tenant's DMZ with that tenant's credential. SMTP/IMAP listeners on `system-mail` are the protocol exception | a system service is reached over declared contracts; "who calls this from outside" is a per-tenant question. Schema invariant: `system` in `tenancy` forbids `expose` (component-profile.md §7) |
| D9 | The stage suffix on infrastructure namespaces is dropped | a cluster has exactly one stage for its lifetime |
| D10 | The portal splits: shell bundle → `shared-shell`; tenant desktop BFF → `tenant-<t>`; platform-admin console → `kernel-control` | holds no state / holds one tenant's credentials / holds the kernel realm |
| D11 | The App Store does not run in the cluster. It is a service operated by Gentian Technologies; the cluster ingests it through the director's API | the catalogue is reference data, not a workload; the cluster holds only what a tenant installs |
| D12 | Tenancy is declared at two levels. The profile's `spec.tenancy` is a list of the modes the entry is certified for; the instance's `spec.tenancy` is the one mode the responsible admin chose and must be a member of that list. `shared` may appear in a profile's list only with `spec.trustTier: platform` and only for a component that is stateless per request or has a natively verifiable tenant model; per-tenant state in one instance is refused. `trustTier` stays in the spec | the certification claim and the deployment decision are different facts made by different people; a component that may run either way must be able to say so. The trust-tier rule is a CRD validation rule, which can read spec fields and not labels or annotations — outside the spec it would silently degrade to an admission policy. The compromise guarantee of a shared instance is only the component's own code |
| D13 | Stateful kernel components — OpenBao, Keycloak, CNPG clusters — are never renamed in place; the taxonomy applies to fresh installs, existing clusters rebuild or keep names and adopt labels | a namespace move is delete-and-recreate; re-initialising OpenBao on a cluster with tenants regenerates every derived credential |
| D14 | System data services are one namespace per engine, named by the function apps declare: `system-postgresql`, `system-mariadb`, `system-cache`, `system-s3` | `kernelRequirements` select per engine; quotas and backup policies differ per engine; an engine may later be backed by a managed service on its own claim; separating stateful services later is a data migration, separating now is a name. These namespaces are the fulfillers a `requires.contracts` entry resolves to; the default per contract is a Cluster-claim setting (component-profile.md §9.1) |
| D15 | vLLM moves into the Cluster composition when it moves to `system-llm`; installer step D-05 is retired | its input is `Cluster.spec.llm.instances`, which only a composition can read |
| D16 | The perimeter namespace is `tenant-<t>-dmz`, built from the tenant's instances' `expose[]` entries with `surface: perimeter` — one publishing proxy per entry, each with its own least-privilege credential and the entry's mandatory `authMode`. Tenant prefix first, qualifier last | the prefix groups a tenant's namespaces for listing, sorting and glob-based tooling, the way `kube-` and `kube-public` do; `dmz` names the function (a mediated perimeter) rather than an exposure property, and avoids colliding with `kube-public`'s meaning of "readable by all". Sourcing the perimeter from `expose[]` rather than a separate list means every perimeter surface carries an `authMode` by construction — the perimeter is where an exposure without declared auth is least acceptable. Budget: namespace names are 63 characters, so a tenant name is at most 52 |

## 3. Inventory: today → target

Every workload found in `scripts/steps/`, `kernel/bootstrap/chart/`,
`kernel/appsets/raw/`, `kernel/services/`, `crossplane/compositions/` and
the catalogue.

### 3.1 Kernel

| Workload | Today | Target | Decision |
| --- | --- | --- | --- |
| Argo CD, argocd-image-updater | `argocd`, `argocd-image-updater` | `kernel-gitops` | D2 |
| Crossplane, providers (helm, kubernetes, keycloak, vault), functions | `crossplane-system` | `kernel-provisioning` | D2, D3 |
| OpenBao | `openbao` | `kernel-secrets` | D13 |
| OpenBao transit seal | `openbao` | `kernel-seal` | D4 |
| External Secrets Operator | `external-secrets` | `kernel-secrets` | D2 |
| Reloader | `stakater-system` | `kernel-secrets` | D2 — part of the rotation path |
| Keycloak, `keycloak-idp` config (theme, SMTP ExternalSecret), realm script | `platform-kernel` | `kernel-authentication` | D1 — Suze claim `idpNamespace` |
| OpenFGA | `platform-kernel` | `kernel-authorization` | D1 |
| gentian-os operator, credential manager, `job-gc` CronJob | `gentian-system` | `kernel-control` | D2 — the director joins here |
| Director API endpoint (ingested by the external App Store) | — | `kernel-control`, route on the kernel gateway, bearer only | D11 |
| `kernel-admin` admin credentials | `platform-kernel` | `kernel-control` | D2 |
| `kernel-admin` `portal-shell` database | `platform-kernel` | `kernel-data` | D5 |
| CNPG `kernel-postgres` for Keycloak, Keycloak extensions, OpenFGA, admin console | — (Bitnami `infra-postgresql` in `gentian-infra-<stage>`) | `kernel-data` | D5 |
| CNPG operator | `cnpg-system` | `kernel-data` | D6 |
| cert-manager, self-signed ClusterIssuers | `cert-manager` | `kernel-edge` | D2 |
| Envoy Gateway, GatewayClass | `envoy-gateway-system` | `kernel-edge` | D2 — tenant Gateways stay in `tenant-<t>` |
| external-dns | `external-dns` | `kernel-edge` | D2 |
| cloudflared (tunnel mode), `cf-tunnel` ExternalSecret | operator chart, `gentian-system` | `kernel-edge` | D2 |
| Kyverno, baseline policies | `kyverno` | `kernel-admission` | D2 |
| Platform-admin console | part of `gentian-portal`, `platform-kernel` | `kernel-control` | D10 |
| metrics-server | `kube-system` | stays, labelled | D7 |
| MetalLB, Kyverno exception for it | `metallb-system` | stays, labelled | D7 |

### 3.2 System

| Workload | Today | Target | Decision |
| --- | --- | --- | --- |
| CNPG `postgres` (tenant databases) | `platform-kernel` via `kernel-admin` | `system-postgresql` | D5, D14 |
| `infra-postgresql` (Bitnami) | `gentian-infra-<stage>` | retired | D5 |
| `infra-mariadb` | `gentian-infra-<stage>` | `system-mariadb` | D9, D14 |
| `infra-redis` | `gentian-infra-<stage>` | `system-cache` | D9, D14 |
| `infra-minio` | `gentian-infra-<stage>` | `system-s3` | D9, D14 |
| Postfix | `platform-kernel` | `system-mail` | D8 |
| Dovecot | `platform-kernel` | `system-mail` | D8 |
| LiteLLM proxy, `litellm-db` (CNPG), `redis-llm` | `platform-kernel` | `system-llm` | D8 — public route removed |
| vLLM instances, mock backend | `platform-kernel` via installer step D-05 | `system-llm`, composed by the Cluster claim | D8, D15 |

### 3.3 Shared

| Workload | Today | Target | Decision |
| --- | --- | --- | --- |
| Shell static bundle (`gentian-portal` web) | `platform-kernel` | `shared-shell` | D10 |
| Collabora | sidecar and extra ingress of `nextcloud-base-ce`, per tenant | profile certifies `tenancy: [tenant, shared]` once the WOPI source is verified per tenant; the instance stays `tenant` until a platform admin chooses `shared` → `shared-collabora` | D12 |

### 3.4 Not in the cluster

| Workload | Today | Target | Decision |
| --- | --- | --- | --- |
| App Store (`app-store-me` profile, per tenant) | `tenant-<t>` | external service; the cluster keeps the director's ingestion endpoint (§3.1) and, per tenant, only the installed profiles | D11 |

### 3.5 Tenant

`tenant-<t>` holds, per tenant: the desktop BFF (D10); every app in
`Tenant.spec.apps` as a provider-helm Release; and what the operator
creates around them — Namespace, ResourceQuota, NetworkPolicies, Services,
ConfigMaps, ExternalSecrets, provisioning and export Jobs, the tenant
Gateway and HTTPRoutes.

Catalogue apps today, by family: activepieces, docmost, element (with
Matrix), mathesar, nextcloud (base-ce, base-od, nine addons), odoo (base,
twelve addons), openproject, open-webui, xwiki, litellm-me (API profile),
gentian-subscriptions (API profile). All tenant-scoped.

### 3.6 Tenant DMZ

`tenant-<t>-dmz` holds one publishing proxy per `expose[]` entry with
`surface: perimeter` across the tenant's instances (D16). A `shared`
instance's perimeter entries are published in each granted tenant's DMZ
with that tenant's credential; a `system` instance has none (D8).

The column *authMode* is the field's enum — `oidc | jwt | bearer | basic |
signature | none` — so this table and the schema cannot drift. Entries
present in today's profiles, each to be confirmed against the app before it
is written as an `expose[]` entry:

| Component | Paths | authMode |
| --- | --- | --- |
| nextcloud | `/s/*` share links, `/public.php/*`, `/.well-known/*` | `none` — capability in the URL |
| nextcloud | `/remote.php/dav/*` (WebDAV, CalDAV, CardDAV) | `basic` — app passwords from the broker |
| nextcloud | Collabora WOPI callbacks | `none`, source-restricted to the Collabora instance |
| element | Matrix client API (today `browserProxy` `forward-bearer`) | `bearer` |
| element | `/.well-known/matrix/*` | `none` |
| element | Matrix federation | `signature` |
| openproject | API (today `browserProxy` `forward-bearer`) | `bearer` |
| odoo-website | public website pages | `none` |
| docmost | public sharing | `none` |

Everything not listed is `surface: gateway` and stays on the authenticated
gateway with no bypass.
