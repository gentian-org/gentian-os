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
| kernel | **Break-glass** | `gentian:platform:break-glass` | recovery when the normal path is down: OpenBao unseal and root, git host administration, direct `kubectl` on `kernel-*`; time-boxed, every action logged out-of-band | kubeconfig and the recovery kit — the only role that bypasses the director |
| kernel | **Platform administrator** | `gentian:platform:operator` | installing and upgrading the OS (`install.sh`, the Cluster claim, kernel versions); granting every role below; the cluster-level policies that bound them — which `authMode`s and surfaces tenants may enable, the `PlatformSecurityPolicy` allowlist, resource-plan ceilings, entitlements | director (`/v1/clusters/{c}/…`), credential manager for kernel-scoped credentials |
| kernel | **Security officer** | `gentian:platform:security` | approving what escapes the default posture: privilege requests (MAC waivers, egress beyond baseline, elevated roles), cluster exposure policy, catalogue entries at `trustTier: platform`; reviewing the decision and change logs | director (approval endpoints), read access to the three audit logs |
| kernel | **Auditor** | `gentian:platform:auditor` | reading the issuer, decision and change logs across all tenants; nothing else | read-only routes on the director; OpenFGA read; git read |
| system | **Service operator** | `gentian:platform:service-operator` | running the system services: capacity, backups and restores, upgrades and engine versions of `system-postgresql`, `system-mariadb`, `system-cache`, `system-s3`, `system-mail`, `system-llm`; the default fulfiller per contract | director (Cluster claim `system` section), credential manager for service admin credentials |
| shared | **Shared-app operator** | `gentian:platform:shared-apps` | installing, upgrading and removing `tenancy: shared` instances; granting and revoking tenants' access to each | director (`/v1/clusters/{c}/shared-apps/…`) |
| tenant | **Tenant administrator** | `gentian:tenant:<t>:admins` | one tenant: installing apps within entitlements, addons, resource plan within the ceiling, backup policies and export schedules, integration grants (`AppGrant`), users and groups in the tenant realm | director (`/v1/tenants/{t}/…`), tenant desktop console |
| tenant-dmz | **Perimeter approver** | `gentian:tenant:<t>:perimeter` | enabling and disabling a public surface for the tenant, within cluster policy; the credentials the DMZ proxies hold | director (`/v1/tenants/{t}/exposure/…`) |
| tenant, one app | **App administrator** | `gentian:tenant:<t>:app-admins` | administration *inside* one installed app — the app's own admin role, reconciled from `privilegedRole`; no platform rights | the app |
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
- **Tenant administrator ≠ perimeter approver.** Publishing to the internet
  is the one tenant decision that changes the blast radius of the platform,
  so it is its own grant and its own audit line. Small tenants will give
  both to the same person; the record still shows two decisions.
- **App administrator is not a platform role.** It exists so that a tenant
  can make someone an Odoo or Nextcloud admin without making them a tenant
  administrator. It confers nothing outside the app.
- **Members are never administrators by group inheritance.** Today's model
  reads `admin: [user] or member`; the target model has `admin` as an
  explicit assignment only.

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
| **Director** | the git push key | read-only, plus create/update on `appprofiles` — the single write, for materialise-on-reference |
| **Credential manager** | none of its own — it exchanges the caller's token | read on `CredentialRequirement`, write on the handover record |
| **Authz bridge** | Keycloak admin, to sync groups into OpenFGA | read on `Tenant`, `AppGrant` |
| **Gateway ext-auth shim** | none — verifies the caller's token, asks OpenFGA | none |

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
`tenancy: tenant` component holding its realm's OIDC client secret and a
granted database; the platform-admin console BFF is the same shape in the
kernel realm. Neither has a line in the enforcement-point table because
neither decides anything — they relay to the director.

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
| May they configure this? | OpenFGA, asked by the **director** | the token's groups as contextual tuples; structure (installs, grants, entitlements) written by controllers |
| May they write this secret? | OpenBao's role bound claims, asked by the **credential manager** | the token's groups |
| May they reach this app? | OpenFGA, asked by the **gateway ext-auth shim** | the token; a session decision cached per user and route |
| May this agent do this on their behalf? | OpenFGA, asked by the **MCP gateway** | `acting_for` and the task's TTL |
| May this pod do this? | the API server (RBAC), NetworkPolicy, Kyverno | the ServiceAccount, the namespace labels |

### 3.1 The authorization model these roles need

Relations on the CRD kinds, in the `type per kind` discipline of principle
4. Membership arrives per request as contextual tuples; only the assignment
of a role to a group, and structure, is stored.

```
type cluster
  relations
    define break_glass:       [group#member]
    define operator:          [group#member]
    define security_officer:  [group#member]
    define auditor:           [group#member]
    define service_operator:  [group#member]
    define shared_app_operator: [group#member]
    define can_configure:     operator
    define can_approve:       security_officer
    define can_audit:         auditor or security_officer
    define can_operate_system: service_operator
    define can_operate_shared: shared_app_operator

type tenant
  relations
    define cluster:            [cluster]
    define admin:              [group#member]
    define perimeter_approver: [group#member]
    define member:             [group#member]
    define can_install_app:    admin or operator from cluster
    define can_set_plan:       admin or operator from cluster
    define can_grant:          admin
    define can_expose:         perimeter_approver
    define can_launch:         member or admin

type app
  relations
    define tenant:   [tenant]
    define admin:    [group#member]
    define can_use:  member from tenant
    define can_administer: admin
```

Every relation ships with a case in `authz/model/*/tests.fga.yaml`
covering the grant, the denial for the neighbouring role, and the
derivation through `cluster`.

## 4. Responsibilities by layer, in one line each

- **Kernel** — the platform administrator installs, the security officer
  approves exceptions, the auditor reads, break-glass recovers. Nothing in
  this layer is created by a tenant, and nothing in it decides on a
  tenant's behalf without a tuple that says so.
- **System** — the service operator keeps the fulfillers running and backed
  up. They see every tenant's data engine; they hold no tenant's
  credentials.
- **Shared** — the shared-app operator runs the instance and decides which
  tenants may use it; the instance's own code is the only isolation between
  those tenants, which is why the profile needed `trustTier: platform`.
- **Tenant** — the tenant administrator decides what runs and who uses it,
  inside a ceiling the platform set and a namespace the platform enforces.
- **Perimeter** — the perimeter approver decides what the internet may
  reach, one surface at a time, inside what the security officer allowed.
- **App** — the app administrator runs the inside of one app.
- **Member** — uses what they were given.
- **Agent** — does what its human could, for as long as its task lasts.
