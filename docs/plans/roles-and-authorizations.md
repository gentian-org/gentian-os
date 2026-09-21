# Roles and authorizations

Who may do what, at which layer, through which enforcement point. One role
per layer of the namespace taxonomy ([namespace-cleanup.md](namespace-cleanup.md),
AD-7), even where one person holds several — the roles are the units that
are granted, audited and revoked; people are what they are assigned to.
Machine identities are listed the same way, by the layer they run in and
the least authority that layer needs.

Rules that apply throughout ([security-principles.md](../security-principles.md)):
identity comes from Keycloak, never from a header (1); the *may* question
is OpenFGA's, in the vocabulary of the CRD kinds (2, 4); every write to
configuration passes the director, every write to a secret the credential
manager (3, 9); authority is derived downward, never granted sideways (5).

## 1. Human roles

| Layer | Role | Keycloak group | Responsible for | Acts through |
| --- | --- | --- | --- | --- |
| kernel | **Break-glass** | `gentian:platform:break-glass` | recovery when the normal path is down (every action reaching the API server is recorded by API-server audit logging, WP-10 — a kubeconfig session raises no Keycloak event, so nothing else would see it): OpenBao unseal and root, git host administration, direct `kubectl` on `kernel-*`; time-boxed, every action logged out-of-band | kubeconfig and the recovery kit — the only role that bypasses the director |
| kernel | **Platform administrator** | `gentian:platform:admin` | installing and upgrading the OS (`install.sh`, the Cluster claim, kernel versions); granting every role below; the cluster-level policies that bound them — which `authMode`s and surfaces tenants may enable, the `PlatformSecurityPolicy` allowlist, resource-plan ceilings, entitlements | director (`/v1/clusters/{c}/…`), credential manager for kernel-scoped credentials |
| kernel | **Security officer** | `gentian:platform:security` | approving what escapes the default posture at **cluster** scope: pod-security waivers and cluster roles, which weaken something protecting the node or reach the Kubernetes API (component-profile.md §3.1; egress is the tenant's to approve), the cluster exposure ceiling — modes the cluster refuses outright, whether a `none` surface must provide the exposure-policy contract, default and maximum lifetime, review interval (component-profile.md §5.1) — and catalogue entries at `trustTier: platform`; reviewing the decision and change logs and the cluster-wide exposure view | director (approval endpoints, `/v1/clusters/{c}/exposure`), read access to the three audit logs |
| kernel | **Auditor** | `gentian:platform:auditor` | reading the issuer, decision and change logs across all tenants, and the cluster-wide exposure view — every public endpoint, its owner, expiry, and the condensed proxy log; nothing else | read-only routes on the director; OpenFGA read; git read |
| system | **Service admin** | `gentian:platform:service-admin` | running the system services: capacity, backups and restores, upgrades and engine versions of `system-postgresql`, `system-mariadb`, `system-cache`, `system-s3`, `system-mail`, `system-llm`; the default fulfiller per contract | director (Cluster claim `system` section), credential manager for service admin credentials |
| shared | **Shared-apps admin** | `gentian:platform:shared-apps-admin` | installing, upgrading and removing `tenancy: shared` instances; granting and revoking tenants' access to each | director (`/v1/clusters/{c}/shared-apps/…`) |
| tenant | **Tenant administrator** | `gentian:tenant:<t>:admins` | one tenant: installing apps within entitlements, addons, resource plan within the ceiling, backup policies and export schedules, integration grants (`AppGrant`), approving **tenant-scope** privilege requests — egress beyond the baseline, which leaves the tenant's own namespace (component-profile.md §3.1) — users and groups in the tenant realm. A dedicated account: holds no `members` or `app:*` group, launches no app | director (`/v1/tenants/{t}/…`), the tenant desktop showing admin tiles only |
| tenant-dmz | **Perimeter approver** | `gentian:tenant:<t>:perimeter` | enabling and disabling a public surface for the tenant, within cluster policy; setting its host, owner and expiry; renewing or revoking at review; the credentials the DMZ proxies hold; reading the tenant's exposure view — surfaces, condensed proxy log, public objects — and revoking an object through the `exposure-policy` contract | director (`/v1/tenants/{t}/exposure/…`) |
| tenant, one app | **App administrator** | `gentian:tenant:<t>:app:<p>:admins` — per app, and only for a profile that declares a `privilegedRole` | administration *inside* one installed app — the app's own admin role, reconciled from `privilegedRole`; no platform rights | the app |
| tenant | **Member** | `gentian:tenant:<t>:members`, `gentian:tenant:<t>:app:<profile>` | using the apps they are entitled to | tenant desktop, the apps |
| any | **Agent** | a Keycloak client per agent, token exchanged with `act` | acting for one human within that human's rights and one task's TTL | MCP gateway, apps |
| outside the cluster | **Catalogue maintainer** | git host and App Store identity | authoring `ComponentProfile`s; certifying `tenancy` modes and `trustTier` through reviewed pull requests | catalogue repository; the store |

Separations that are load-bearing, whatever one person happens to hold:

- **Platform administrator ≠ security officer.** The one who installs is not
  the one who approves exceptions. A waiver, an egress rule or a public
  surface that the same identity requested and approved is a policy hole
  with a signature on it.
- **Break-glass is not "platform administrator with more".** It is a
  separate group with no standing membership: added for an incident,
  removed after, each use visible in the issuer log because the group
  itself is the trigger. The platform administrator does not have it by
  default.
- **Tenant administrator ≠ perimeter approver**, as relations. Publishing to
  the internet is the one tenant decision that changes the blast radius of
  the platform, so it is its own grant and its own audit line. Most tenants
  will not staff it separately, so the **default is that they are the same
  people**: at tenant deploy the director writes the admins group into
  `tenant:<t>#perimeter_approver` (authorization-model.md §3). A tenant that
  wants the separation removes that one tuple and populates its own
  `:perimeter` group. Either way the record shows two decisions, because the
  relation asked is `can_expose`, never `admin`.
- **App administrator is not a platform role.** It exists so that a tenant
  can make someone an Odoo or Nextcloud admin without making them a tenant
  administrator. It confers nothing outside the app.
- **Members are never administrators by group inheritance.** Today's model
  reads `admin: [user] or member`; the target model has `admin` as an
  explicit assignment only.
- **Administrators are never members — least privilege per account, not
  per person.** An account that installs apps and sets privileges does not
  also write e-mail. A tenant administrator's account holds the `admins`
  group and nothing else: no `members`, no `app:*`, no app tiles, no OIDC
  scope on any app client; a person who needs both has two accounts, and
  the desktop shows each account only what its relations grant
  ([iam.md §1.3](../design/iam.md) already states this; today's model
  contradicts it with `can_launch: … or admin from parent`). Enforced in
  three places, none of them the UI: the target model derives `can_launch`
  from `member` alone (§3.1); the director refuses a membership event that
  would put an account in both `gentian:tenant:<t>:admins` and `:members`,
  the reconcile flags any such account, and both are recorded in the
  decision log; and app OIDC clients are granted to member groups
  only, so an admin token is not accepted by any app even if presented.
  The same holds one layer up: a platform-role account (`gentian:platform:*`)
  is a member of no tenant, `tenant-platform` included.

## 2. Machine identities

Three classes. What class an identity is in follows from one question —
does it write the cluster? — and the class fixes everything else: what it
may hold, where it runs, and what confines it.

| Class | Writes the cluster | Holds | Runs in | Confined by |
| --- | --- | --- | --- | --- |
| **Controllers** | yes — that is their job | Kubernetes RBAC, up to cluster-admin-equivalent; OpenBao `kernel/*` | `kernel-*` only | unreachable from any `system-*`, `shared-*` or `tenant-*` namespace |
| **Enforcement points** | no (one exception, below) | exactly **one** credential each, and never `pods/exec` or `secrets` | `kernel-control`, `kernel-edge` | the credential is the only thing they can misuse |
| **Workloads** | never — **zero** Kubernetes RBAC | their own OpenBao prefix; the credentials granted to them as requirements | the tier their `tenancy` puts them in | the namespace: NetworkPolicy from the profile, quota, Kyverno |

**Controllers** — Crossplane and its providers, Argo CD, ESO, cert-manager,
Kyverno, CNPG, Envoy Gateway, and the **operator**. They are the cluster's
hands; there is no least privilege to design here, only containment, which
is why they live in `kernel-*` and nothing outside it can reach them. The
one open item is scoping Crossplane's providers per role (roadmap 1.16).

**Enforcement points** — each holds one credential and answers one question:

| Identity | The one credential | Cluster access |
| --- | --- | --- |
| **Director** | the git push key (signing through OpenBao transit, so the key never leaves the vault); to OpenFGA it authenticates with its projected ServiceAccount token (`authn.method: oidc`), not a stored secret | read-only, plus create/update on `appprofiles` — the single write, for materialise-on-reference. Writes OpenFGA: the store's only writer |
| **Credential manager** | none of its own — it exchanges the caller's token | read on `CredentialRequirement`, write on the handover record |
| **Gateway ext-auth shim** | none — verifies the caller's token, asks OpenFGA | none |

The polling bridge is gone (AD-12); what replaces it is an event path.
Keycloak's event listener pushes membership changes to the director, which
writes them as `group#member` tuples; a reconcile with a **read-only**
Keycloak client corrects the projection toward Keycloak — never the other
way. Everything else in the store is structure, and **only the director
writes any of it** — installs, grants, entitlements and the role-to-group
assignments from the Cluster claim, each tuple written in the same
operation as the commit it reflects. The store is a projection of Keycloak
and git: the director creates it and the model on first start and rebuilds
the tuples from both, so nothing is lost if it is dropped. The operator
reads. The vocabulary is
[authorization-model.md](authorization-model.md).

**Workloads** — everything else: system services, shared instances, tenant
apps, both UI backends, DMZ proxies, agents. None has a ServiceAccount with
any RBAC. What each may read and reach is not decided per identity; it is a
function of the namespace tier and the profile:

| Tier | OpenBao prefix | Reach |
| --- | --- | --- |
| `system-<function>` | `gentian-os/kernel/<function>/*` | ingress from tenant and shared namespaces on the contract port, and from its own `-dmz`; egress only to declared upstreams (LLM providers) |
| `system-<function>-dmz` | one credential: the backend relay or proxy credential | ingress from the internet on the protocol's ports; egress to its backend in `system-<function>` and, for mail, to the internet on `:25` |
| `shared-<app>` | `gentian-os/shared/<app>/*`; per-tenant credentials issued by the kernel, never a shared secret | system services over granted contracts; granted tenants' gateways |
| `tenant-<t>` | `gentian-os/tenants/<t>/apps/<app>/*` — per app; today per tenant | `requires.contracts` and granted integrations, nothing else |
| `tenant-<t>-dmz` | one credential: the surface's app password or scoped token | ingress on the surface's paths; egress to one backend service and port |

The UI backends are ordinary workloads: the tenant desktop BFF is a
`tenancy: tenant` component holding a granted database and no credential at
all — the edge holds the zone's OIDC client and forwards the token to the
desktop route only (networking.md §4, ui-restructure.md §1); the platform-admin console is the same component in
`tenant-platform`, the platform tenant whose realm is the kernel realm
(AD-10). Neither has a line in the enforcement-point table because neither
decides anything — they relay to the director, and what a desktop shows is
what the director returned for that account's relations: admin tiles for an
admin account, app tiles for a member account, never both.

**Bootstrap** is the one identity outside the classes: the installer, with
the human's kubeconfig and a bootstrap OpenBao token, until `E-04` revokes
the token and the handover record shows the human write path works.

Three invariants, one per class, each a scripted test:

1. No pod in a `system-*`, `shared-*` or `tenant-*` namespace has a
   ServiceAccount bound to any Role or ClusterRole.
2. No identity outside `kernel-control` holds a git credential, and no
   enforcement point can `exec`.
3. No controller Service is reachable from outside `kernel-*`.

## 3. Where each decision is enforced

| Question | Answered by | Fed by |
| --- | --- | --- |
| Who is this? | Keycloak — realm `kernel` for platform roles, realm `<t>` for tenant roles | groups in the token |
| May they configure this? | OpenFGA, asked by the **director** | the membership projection fed by Keycloak's events; structure (installs, grants, entitlements, role assignments) written by the director |
| May they write this secret? | OpenFGA, asked by the **credential manager** — `can_write_credential` on `app:<t>/<p>`, `can_configure` on `cluster:<c>` for kernel and system secrets. OpenBao's policy then bounds the *path* the request may touch; it does not make the decision (principle 2). Deciding in OpenBao policy from token groups put secret-write authority outside `ListUsers` and left it un-revoked by a tuple delete | the membership projection |
| May they reach this app? | OpenFGA, asked by the **gateway ext-auth shim** — `can_use`, which is the app's own entitlement group, not tenant membership | the token; a decision cached per session and route |
| Is this anonymous request valid? | the **publishing proxy** in the DMZ — the entry's `authMode`, and a source restriction where one is declared. It strips every inbound identity header and sets only its own | the credential presented, or none |
| May this agent do this on their behalf? | OpenFGA, asked by the **MCP gateway** | `acting_for` and the task's TTL |
| May this pod do this? | the API server (RBAC), NetworkPolicy, Kyverno | the ServiceAccount, the namespace labels |

### 3.1 The authorization model these roles need

The model is [artefacts/model.fga](artefacts/model.fga), with the rules
behind it in [authorization-model.md](authorization-model.md). It is not
restated here: the roles above map onto it as `cluster#admin`,
`cluster#security_officer`, `cluster#auditor`, `cluster#service_admin`,
`cluster#shared_apps_admin`, `cluster#break_glass`, `tenant#admin`,
`tenant#perimeter_approver`, `tenant#member` and `app#admin`, one Keycloak
group each; every verb a PEP exposes is a `can_*` relation computed from
them. Two invariants the model carries for this document: an admin account
reaches the desktop (`tenant#can_enter`) but launches no app
(`app#can_use: member … but not admin`), and a platform administrator acts
inside a tenant only through `admin from cluster` — never by holding a
tenant group.

Every relation ships with a case in `authz/model/*/tests.fga.yaml`
covering the grant, the denial for the neighbouring role, and the
derivation through `cluster`.

## 4. Responsibilities by layer, in one line each

- **Kernel** — the platform administrator installs, the security officer
  approves exceptions, the auditor reads, break-glass recovers. Nothing in
  this layer is created by a tenant, and nothing in it decides on a
  tenant's behalf without a tuple that says so.
- **System** — the service admin keeps the fulfillers running and backed
  up. They see every tenant's data engine; they hold no tenant's
  credentials.
- **Shared** — the shared-apps admin runs the instance and decides which
  tenants may use it; the instance's own code is the only isolation between
  those tenants, which is why the profile needed `trustTier: platform`.
- **Tenant** — the tenant administrator decides what runs and who uses it,
  inside a ceiling the platform set and a namespace the platform enforces.
- **Perimeter** — the perimeter approver decides what the internet may
  reach, one surface at a time, inside what the security officer allowed.
- **App** — the app administrator runs the inside of one app.
- **Member** — uses what they were given.
- **Agent** — does what its human could, for as long as its task lasts.
