# Namespace cleanup

Five categories of namespace, each with one authority and one policy set,
and the target namespace of every workload running today.

Terms: a *profile* is a `ComponentProfile` catalogue entry and an *instance*
is a `Component` deployed from it, as defined in
[component-profile.md](component-profile.md). Today's `AppProfile` is the
profile until that rename lands. The decisions behind the layout are
[architectural-decisions.md](architectural-decisions.md) AD-7 to AD-11.

## 1. Categories

| Category | Contains | Installed by | Authority | Compromise guarantee |
| --- | --- | --- | --- | --- |
| `kernel-<function>` | services the OS is made of, including the tier-0 operators | `install.sh`, then Argo CD | break-glass platform admin only | none — this is the trust root |
| `system-<function>` | instances with `tenancy: system`, fulfilling `requires.contracts` of other components; no public route | kernel services, from the Cluster claim | platform admin through the director | other system functions; every tenant boundary |
| `system-<function>-dmz` | the internet-facing edge of a system service whose protocol needs one: a stateless listener holding one credential to its backend, no data | kernel services, from the Cluster claim | platform admin through the director | the service's backend; every tenant |
| `shared-<app>` | one instance with `tenancy: shared`, serving several tenants; the profile must certify `shared` and carry `trustTier: platform` | director, from a Component whose profile certifies `shared` | platform admin through the director | only what the component's own code enforces |
| `tenant-<t>` | the tenant's instances with `tenancy: tenant`, including its desktop (frontend and BFF) | operator, from `Tenant.spec.apps` | tenant admin through the director | every other tenant; the kernel; system services beyond declared contracts |
| `tenant-<t>-dmz` | the tenant's perimeter: one publishing proxy per `surface: perimeter` entry a perimeter approver has enabled, each with its own least-privilege credential and the entry's mandatory `authMode` | operator, from the tenant's enabled perimeter entries | perimeter approver through the director, within cluster policy — held by the tenant's admins by default (roles §1) | the tenant's own instances |

A new namespace inside a category needs a different exposure, credential
set, upgrade owner or quota than its neighbour. None of the four → same
namespace.

Names are `<tier>-<qualifier>`; for tenants the tenant name is the qualifier
and `-dmz` a suffix, so a tenant's namespaces list and sort together.
Namespace names are 63 characters, so a tenant name is at most 52.

Every namespace carries `gentianos.io/tier: kernel|system|system-dmz|shared|tenant|tenant-dmz`
and `gentianos.io/function: <function>` (tenant namespaces:
`gentianos.io/tenant: <t>`). Policies select on labels, never on names.

The layout applies to fresh installs. Stateful kernel components — OpenBao,
Keycloak, the CNPG clusters — are not renamed in place; an existing cluster
rebuilds from its recovery kit or keeps its names and adopts the labels.

## 2. Inventory: today → target

Every workload found in `scripts/steps/`, `kernel/bootstrap/chart/`,
`kernel/appsets/raw/`, `kernel/services/`, `crossplane/compositions/` and
the catalogue.

### 2.1 Kernel

| Workload | Today | Target | Note |
| --- | --- | --- | --- |
| Argo CD, argocd-image-updater | `argocd`, `argocd-image-updater` | `kernel-gitops` | |
| Crossplane, providers (helm, kubernetes, keycloak, vault), functions | `crossplane-system` | `kernel-provisioning` | named for the function, so the software behind it can change |
| OpenBao | `openbao` | `kernel-secrets` | |
| OpenBao transit seal | `openbao` | `kernel-seal` | its unseal key is an in-cluster Secret (`openbao-transit-unseal`, written by B-02); apart from the vault it sits in a separate RBAC and backup domain. With a KMS the key leaves the cluster |
| External Secrets Operator | `external-secrets` | `kernel-secrets` | |
| Reloader | `stakater-system` | `kernel-secrets` | part of the rotation path |
| Keycloak, `keycloak-idp` config (theme, SMTP ExternalSecret), realm script | `platform-kernel` | `kernel-authentication` | Suze claim `idpNamespace`; apart from OpenFGA because it has a public route and different credential holders |
| Keycloak event listener (SPI provider pushing signed membership events to the director) | — | `kernel-authentication` | new; the director is its only receiver; holds only its signing key |
| OpenFGA | `platform-kernel` | `kernel-authorization` | reachable from enforcement points only |
| gentian-os operator, credential manager, `job-gc` CronJob | `gentian-system` | `kernel-control` | the director joins here |
| Director API endpoint (called by the external App Store) | — | `kernel-control`, route on the kernel gateway, bearer only | the App Store runs outside the cluster, operated by Gentian Technologies |
| `kernel-admin` admin credentials | `platform-kernel` | `kernel-control` | |
| `kernel-admin` `portal-shell` database | `platform-kernel` | `kernel-data` | |
| CNPG `kernel-postgres` for Keycloak, Keycloak extensions, OpenFGA | — (Bitnami `infra-postgresql` in `gentian-infra-<stage>`) | `kernel-data` | kernel identity does not share a data plane with tenants |
| CNPG operator | `cnpg-system` | `kernel-data` | |
| cert-manager, self-signed ClusterIssuers | `cert-manager` | `kernel-edge` | |
| Envoy Gateway, GatewayClass | `envoy-gateway-system` | `kernel-edge` | tenant Gateways stay in `tenant-<t>` |
| external-dns | `external-dns` | `kernel-edge` | |
| cloudflared (tunnel mode), `cf-tunnel` ExternalSecret | operator chart, `gentian-system` | `kernel-edge` | |
| Kyverno, baseline policies | `kyverno` | `kernel-admission` | |
| Platform-admin console | part of `gentian-portal`, `platform-kernel` | `tenant-platform` — see §2.5 | a UI with no authority is not kernel (AD-10); the platform is a tenant whose realm is the kernel realm |
| metrics-server | `kube-system` | stays, labelled | API-aggregation convention |
| MetalLB, Kyverno exception for it | `metallb-system` | stays, labelled | platform-provided, not installed by gentian-os |

### 2.2 System

| Workload | Today | Target | Note |
| --- | --- | --- | --- |
| CNPG `postgres` (tenant databases) | `platform-kernel` via `kernel-admin` | `system-postgresql` | |
| `infra-postgresql` (Bitnami) | `gentian-infra-<stage>` | retired | hosts only kernel databases today; they move to `kernel-data` |
| `infra-mariadb` | `gentian-infra-<stage>` | `system-mariadb` | |
| `infra-redis` | `gentian-infra-<stage>` | `system-cache` | |
| `infra-minio` | `gentian-infra-<stage>` | `system-s3` | |
| Dovecot (mailbox store); DKIM signer (milter) holding the per-tenant keys | `platform-kernel` | `system-mail` | `mail.serviceMode: kernel` only; nothing listens publicly |
| Postfix — `:25` inbound, `:587` submission, the relay port apps send to; spam filter; Dovecot proxy on `:993` only while a tenant has IMAP exposure enabled | `platform-kernel`, with the public ports on the store | `system-mail-dmz` | one MTA, no mailboxes, no keys: DKIM is signed by calling the milter in `system-mail`. External IMAP and submission are a perimeter surface of the mail function, default off |
| TURN / SFU for conferencing | — | `system-turn` (tier `system-dmz`) | all edge, no inner part; short-lived HMAC credentials issued to apps over a contract |
| LiteLLM proxy, `litellm-db` (CNPG), `redis-llm` | `platform-kernel` | `system-llm` | public route removed |
| vLLM instances, mock backend | `platform-kernel` via installer step D-05 | `system-llm`, composed by the Cluster claim | D-05 retired; its input is `Cluster.spec.llm.instances` |

One namespace per engine, named by the function a `requires.contracts`
entry declares: quotas, backup policies and future managed-service claims
differ per engine, and separating stateful services later is a data
migration. The stage suffix is dropped: a cluster has one stage.

### 2.3 Shared

None today. The first candidate:

| Workload | Today | Target | Note |
| --- | --- | --- | --- |
| Collabora | sidecar and extra ingress of `nextcloud-base-ce`, per tenant | `shared-collabora` once a platform admin chooses `shared` | the profile certifies `tenancy: [tenant, shared]` once the WOPI source is verified per tenant; the instance stays `tenant` until then |

### 2.4 Not in the cluster

| Workload | Today | Target |
| --- | --- | --- |
| App Store (`app-store-me` profile, per tenant) | `tenant-<t>` | external service; the cluster keeps the director's endpoint (§2.1) and, per tenant, only the installed profiles |

### 2.5 Tenant

`tenant-<t>` holds, per tenant: the desktop (`gentian-portal` web and
api containers, one image); every app in
`Tenant.spec.apps` as a provider-helm Release; and what the operator
creates around them — Namespace, ResourceQuota, NetworkPolicies, Services,
ConfigMaps, ExternalSecrets, provisioning and export Jobs, the tenant
Gateway and HTTPRoutes.

**`tenant-platform` is one of these.** The platform is a tenant whose realm
is the kernel realm (`Tenant/platform`, `isolation.keycloakRealm: kernel`,
AD-10): its desktop is the platform-admin console, its tiles are the admin
apps (console, credential-manager UI, Headlamp when opted in), its members
are the platform admins. It runs under the same quota, policy and authority
model as any tenant and holds nothing the others do not — a compromised
platform desktop yields platform-admin *sessions*, bounded by OpenFGA and
the director, not kernel credentials. That holds only because the desktop
holds no OIDC client secret: the edge is the session authority (AD-13), so
the kernel realm's confidential client lives on the `authenticated` Gateway
in `kernel-edge`, never in a tenant namespace. Two exceptions, both in the operator:
the realm is adopted rather than created, and the tenant cannot be deleted
(a realm-disable Job against the kernel realm would lock every admin out).

Catalogue apps today, by family: activepieces, docmost, element (with
Matrix), mathesar, nextcloud (base-ce, base-od, nine addons), odoo (base,
twelve addons), openproject, open-webui, xwiki, litellm-me (API profile),
gentian-subscriptions (API profile). All tenant-scoped.

### 2.6 Tenant DMZ

`tenant-<t>-dmz` holds one publishing proxy per perimeter surface the
perimeter approver has enabled — a role the tenant's admins hold by
default (roles-and-authorizations.md §1). The profile declares the surface and its
`authMode`; the tenant's enablement, constrained by cluster policy, creates
the proxy; nothing is published by default. Shared and public are
independent: a `shared` instance is published through a tenant's DMZ only
where that tenant enabled it, with that tenant's credential.

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
