---
audience: architect, auditor
type: concept
---

# Authorization

One vocabulary for the platform's rights: the rules the model follows, the
tuples something writes, and what each enforcement point asks. The model
itself is [authz/model/v1/model.fga](../../authz/model/v1/model.fga), the
file `fga model validate` and `fga model test` run against. A relation that
is not in that file does not exist, and `make verify-authz-vocabulary` holds
this document, the model and the enforcement points to one set of
`can_*` relations.

Rules that apply throughout ([security/principles.md](../security/principles.md)):
identity comes from Keycloak, never from a header; the *may* question is
OpenFGA's, in the vocabulary of the CRD kinds; every write to configuration
passes the director, every write to a secret the custodian; authority is
derived downward, never granted sideways.

## 1. Rules

These follow OpenFGA's own modeling guidance and the conventions of the
Zanzibar family; each rule names what it prevents.

| # | Rule | Prevents |
|---|---|---|
| R1 | **One type per CRD kind**; object id = the CR's name, namespaced kinds as `<tenant>/<name>` (`app:demo/nextcloud`). | a second naming scheme that has to be mapped |
| R1a | **Ids contain neither `:` nor `#`**, which OpenFGA rejects. A position is `position:<tenant>/<name>`, a declared role `role:<tenant>/<profile>/<name>`; tenant, profile, position and role names are DNS labels. One package builds every id (`internal/director/authz`, which the operator's projection and the arbiter import). | tuples the server refuses |
| R2 | **Roles are nouns, held by a person or by a position**, never by a group of the identity provider. On a tenant and on an app a role takes `user:<sub>` or `position:<t>/<name>#holder`; a position is held by `user:<sub>` ([iam.md §1.2](iam.md)). A cluster role is held by name on the Cluster claim (`spec.platformAccess.roleHolders`, AD-16) and takes `user:<realm>/<sub>`, the account qualified by its realm, never a plain `user:<sub>`; so do `perimeter_approver` and `auditor` on a tenant. `break_glass` is held by a person with a reason, until a time. | a role held without git or the store saying so; a holder tuple a token of another realm could match |
| R3 | **Permissions are `can_<verb>`**, one per verb an enforcement point exposes, computed from roles. **PEPs check permissions, never roles.** | a PEP encoding "admin may…" in code; a new verb without a relation |
| R4 | **One parent relation per type** (`cluster`, `tenant`); derivation between scopes is `<relation> from <parent>`, never a copied tuple. | authority granted sideways |
| R4a | **Inside a tenant, what follows is written, not walked.** What a position holds through the positions it includes, a role through the roles it includes, a declared role's use of its app, a base's admin on its add-ons and an add-on role's use of its base are `derived_*` entries the operator writes from what was given (`internal/director/authz/derived.go`), never given by anybody. A check reads them in one step; the given entries stay as they are, so a view tells what was given from what follows. | a check that walks every inclusion, role and add-on of a tenant on every request |
| R5 | **`but not` only for least-privilege invariants.** The model has none today: `admin` on an app includes its other roles, so that somebody can always configure it. | exclusion logic scattered through permissions |
| R7 | **Who holds what is stored; contextual tuples carry runtime facts only.** A person's positions and roles are tuples the operator writes when they are given through the registrar, and takes when they are taken. Everything else -- what a position is, tenant→cluster, app→tenant, the cluster's role holders -- is a stored tuple the operator projects from what git declares once Argo CD has applied it (AD-12). | a copy of rights in the identity provider; a second place a right can be changed; rights in every token |
| R8 | **Every relation ships with three tests** (`authz/model/v1/tests.fga.yaml`): the grant, the denial for the neighboring role, the derivation through the parent. | a relation nobody exercised |
| R9 | **The operator writes; the director asks.** The operator is the store's one writer, from git and from what the registrar asks of it; the director asks the store before every call it serves and writes nothing to it (AD-12). Git holds the defaults: the tuples a tenant starts with follow from what git declares and can be derived and checked. | two writers; a store whose defaults have silently drifted |

## 2. What R7 requires, and what it buys

R7 is OpenFGA's own hybrid guidance and the Zanzibar shape: persistent facts
stored, runtime facts contextual. Three invariants keep the projection
honest:

- **Only the operator writes who holds what.** A position or a role is
  given and taken through the registrar, which asks the operator; no
  console and no hand edit writes a tuple.
- **A change reaches every enforcement point within the bouncer's poll.**
  The bouncer evicts its cached decisions whenever the store's changelog
  moves, which it polls every two seconds, and a cached allow lives five
  minutes at most. Lockout does not wait for the store at all: disabling a
  person or ending their realm session stops new tokens at the issuer, and
  the edge's next refresh fails, so the bound is the access token's
  lifetime, five minutes, independent of any tuple.
- **Only what only shows may be answered from a cache.** The desktop's
  tiles and the access pages ask OpenFGA for its fastest answer
  (`MINIMIZE_LATENCY`), which its check and iterator caches may serve a
  few seconds behind the store; every other check asks
  `HIGHER_CONSISTENCY` and reads the store as it is
  (`internal/openfga`, `WithFastAnswer`).
- **Contextual tuples never carry what a person holds.** A request may add
  facts that exist only for that request; identity and rights are never
  among them.

What it buys over a contextual-only design: reverse queries and access
reviews are native (`ListUsers("who can use app A")` is complete); checks are
indexed lookups with no per-request tuple cost and no 100-tuple cap;
OpenFGA's check cache works; and no right has to travel in the edge token, so
the session cookie stays small. An app that can only read a token is the
exception: its sign-in token carries the person's roles on it, asked of the
store when the token is written ([iam.md §1.8](iam.md)).

## 3. Object naming and who writes each tuple

The operator writes every tuple: the structure rows from what git declares,
once Argo CD has applied it (`internal/controller/authz_projection_reconciler.go`),
the rows of who holds what when the registrar asks
(`internal/applifecycle/http_assignments.go`). The director makes the commit
that leads to a structure tuple, and the tuple follows when the operator has
seen the result.

| Tuple | When |
|---|---|
| `cluster:<c>#<role>@user:<realm>/<sub>` | from the Cluster claim's role holders of scope `cluster` (`admin`, `securityOfficer`, `auditor`, `serviceAdmin`, `sharedAppsAdmin`, `perimeterApprover`, `breakGlass`), declaratively: while the claim names no `admin`, the account the install made holds it; an entry removed from the claim, or past its `expiresAt`, has no tuple, and the operator requeues at the next expiry. `<realm>` is the holder's tenant's realm, the kernel realm for the platform tenant; a subject never contains `/`, so no plain `user:<sub>` matches it |
| `tenant:<t>#perimeter_approver@user:<realm>/<sub>` | from a role holder `perimeterApprover` of scope `tenant:<t>`, an administrator of `t` (the director asks the store whether they administer it when the entry is added; the operator projects no tuple while they are not one), the same way. Publishing and egress beyond the baseline are one grant: `can_expose` is `perimeter_approver or admin from cluster` and `can_approve_privilege` is `can_expose`, so beside the tenant's and the cluster's perimeter approvers (`perimeter_approver from cluster`) the cluster's administrator approves in every tenant, with no tuple of its own, in a tenant that withdrew `operated_by` too, since the perimeter is the cluster's |
| `tenant:<t>#auditor@user:kernel/<sub>` | from a role holder `tenantAuditor`, a person of the platform tenant, of scope `tenant:<t>` the entry names (never the platform tenant), the same way. `can_read_evidence` is `auditor or can_audit from cluster`: the cluster's auditors read every tenant's evidence, a tenant's auditor that tenant's, and nobody of the tenant itself; it grants no entry to the tenant |
| `tenant:<t>#cluster@cluster:<c>` | tenant deploy |
| `position:<t>/<name>#tenant@tenant:<t>`, and each role the position holds: `tenant:<t>#<role>@position:<t>/<name>#holder`, `app:<t>/<p>#<role>@…`, `role:<t>/<p>/<r>#holder@…`; `position:<t>/<a>#included_in@position:<t>/<b>` | tenant deploy, for the four positions every tenant has (User holds `user` on the tenant alone; All apps includes User and holds `user` on every installed app and add-on, re-projected as apps are installed and removed), and for each the tenant declares (`spec.positions`); app install, for the positions the app is given to ([iam.md §1.2](iam.md)) |
| `tenant:<t>#operated_by@cluster:<c>` | tenant deploy, and always for `tenant:platform`. **Consent, not structure**: it is what lets `admin from cluster` reach into the tenant, user management and secret writes included. A tenant that administers itself has it removed (`can_configure` on the cluster, at the tenant's request) and loses nothing else: `cluster` stays, so audit and cluster-scope approval still derive |
| *(no bootstrap tuple for the platform tenant)* | `tenant#admin` derives `or admin from operated_by`, so a platform administrator is an administrator of `tenant:platform`, and of any tenant that has not withdrawn the consent, through the chain. Writing the cluster's administrators into a tenant relation would be the copied tuple R4 forbids, and a tuple somebody has to remember to remove |
| `app:<t>/<p>#tenant@tenant:<t>` | app install |
| `app:<t>/<p>#base@app:<t>/<addon>` and `#addon` the other way; `role:<t>/<p>/<r>#app@app:<t>/<p>`, `#declared`, `#included_in` | app install (`internal/director/authz/apps.go`): an add-on is an app of its own tied to its base, and each role the profile declares is tied to its app |
| `tenant:<t>#derived_<role>`, `app:<t>/<p>#derived_<role>`, `role:<t>/<p>/<r>#derived_holder`, each `@position:<t>/<name>#holder` or `@user:<sub>` | after every projection of a tenant's apps and positions, and after every role given to or taken from a person directly: exactly what follows from what is given (R4a), the stale ones removed |
| `position:<t>/<name>#holder@user:<sub>`; `<object>#<role>@user:<sub>` for a role given directly | when a tenant's administrator gives it, through the registrar; taken the same way, and when the realm says the person was removed. A cluster administrator who enters a tenant, and a person of another tenant who signs in to it, are given theirs when they arrive ([iam.md §1.1c](iam.md)) |

Two relations the model defines that no tuple feeds and no enforcement
point asks: `can_consume` on `contract` (the consent an `AppGrant` records
between two apps of a tenant; nothing sits on an app-to-app call to ask it)
and `can_bind` on `shared_instance`. The `shared_instance` type belongs to
the class `shared-app`, which is a planned structure and not implemented;
the type is in the model so that the vocabulary is complete.

## 4. What each enforcement point asks

The director, the usher, the custodian, the registrar and the bouncer ask for
a verified caller as `user:<sub>` and, when that is refused, once more as
`user:<realm>/<sub>`, with `<realm>` the realm whose keys verified the token
(`CheckAs` in `internal/director/authz`). A role holder's tuple therefore
answers only a token of the realm the claim names for it.

| PEP | Object | Relations |
|---|---|---|
| Bouncer, per host | `tenant:<t>` for the desktop host; `app:<t>/<p>` for an app host; `cluster:<c>` for a kernel tool host | `can_enter`; `can_use`; `can_open_kernel_tools`. The relation per host is the route table the operator writes ([routing.md §4.1](routing.md)) |
| — | — | *The front door and the desktop resolve the **same** relation on the **same** object: `can_use`, and `can_launch`, which derives from it. One rule, two enforcement points, no second implementation to drift. The tile list is the usher's answer to the same question* |
| Bouncer, rights check | `cluster:<c>`; `tenant:<t>` | `can_audit`, `can_configure`; `can_read_evidence`, asked for a person of the platform tenant by a component of that tenant whose profile declares `requires.services.rights.audit`: the evidence service and the console that shows it; `break_glass` of the cluster, by one that declares `requires.services.rights.breakGlass` |
| Usher | `tenant:<t>`; `cluster:<c>`; per tile the object the tile names | `can_enter`, `can_view`, `can_expose` on the tenant; `can_audit`, `can_configure`, `can_approve`, `can_approve_perimeter` on the cluster; `can_launch` on `app:<t>/<p>` for an app tile and `can_administer` on `tenant:<t>` for an administration tile, the relation and object the tile declares (`internal/controller/tile_projection_reconciler.go`) |
| Custodian | `cluster:<c>` for kernel and system credentials; `tenant:<t>` for a tenant's; `app:<t>/<p>` for a component's own, which follow its tenant's | `can_read_credential` to see that one is required, whether it is set and by whom, never its value; `can_write_credential` to set it |
| Registrar | `tenant:<t>` for a tenant's people, what they hold and realm settings; `cluster:<c>` for handing a tenant's administrator account over and for the count of accounts | `can_manage_users`, `can_enter`; `can_configure`, `can_audit`. Whatever the store answers, it refuses a change to a group the Cluster claim names for a platform role, to a person in one, or to a person of the platform tenant the claim names a role holder (`ErrProtected`, [iam.md §1.1c](iam.md)). `can_configure` on the cluster reads the role candidates (`GET /v1/clusters/{c}/role-candidates`) |
| Director, tenant verbs | `tenant:<t>` | `can_install_app`, `can_set_policy` (the realm's password policy among it: the policy is in git, the tenant Composition applies it, and a rebuilt realm comes back with it), `can_grant`, `can_manage_users`, `can_expose`, `can_approve_privilege`, `can_administer`, `can_enter`, `can_view` |
| Director, install | `tenant:<t>` | `can_install_app`, and nothing else. Nothing is asked about the app or the catalog it comes from: whether the tenant may have it is decided where its artifacts are pulled, by the credential the tenant holds for their repository (AD-3) |
| Director, catalogs | `cluster:<c>` to add or remove a catalog for every tenant or for one tenant; `tenant:<t>` for a tenant's administrator adding or removing one for their own tenant | `can_configure` on the cluster, `can_audit` to read what it declares. `can_install_app` on the tenant, and not the whole check: the director refuses unless the cluster's administrator delegated it to that tenant, and removes only what the tenant added. `can_view` to read what a tenant installs from |
| Director, cluster verbs | `cluster:<c>` | `can_configure`, `can_deploy_tenant`, `can_operate_system`, `can_install_shared`, `can_grant_shared`, `can_approve`, `can_approve_perimeter`, `can_set_admission`, `can_edit_raw`, `can_audit`. A pod-security waiver takes effect only where `can_approve` put it on the cluster's allowlist and granted it on the install, both in one approval if need be; the allowlist alone waives nothing. A cluster role is asked for by name from a set the platform defines, and is bound to the component's ServiceAccount only where the set has it, `can_set_admission` permitted it for the profile (`allowedClusterRoles`) and `can_approve` granted it on the install -- `can_set_admission` for a role that writes; a profile's own rules are never made into a role. A profile's own resource types are installed only once `can_set_admission` granted them |
| Arbiter | an app's own store | the app's own relations, under the app's key; the arbiter asks the kernel store nothing ([app-authorization.md](app-authorization.md)) |

## 5. Human roles

One role per layer of the namespace taxonomy ([kernel.md](kernel.md), AD-7),
even where one person holds several: the roles are the units that are
granted, audited and revoked; people are what they are assigned to.

| Layer | Role | Held by | Responsible for | Acts through |
| --- | --- | --- | --- | --- |
| kernel | **Break-glass** | a person the Cluster claim names with a reason, until a time (`roleHolders`); nobody holds it otherwise | recovery when the normal path is down: OpenBao unseal and root, git host administration, direct `kubectl` on `kernel-*`; `can_set_admission`, `can_edit_raw` | kubeconfig and the recovery kit: the one role that bypasses the director |
| kernel | **Platform administrator** | by name: a person of the platform tenant; the account the install made while the claim names none | installing and upgrading the OS (`install.sh`, the Cluster claim, kernel versions); granting every role below; the cluster-level policies that bound them: which `authMode`s and surfaces tenants may enable, resource-plan ceilings | director (`/v1/clusters/{c}/…`), custodian for kernel-scoped credentials |
| kernel | **Security officer** | by name: a person of the platform tenant or an administrator of another tenant (AD-16) | approving what escapes the default posture at **cluster** scope: pod-security waivers and cluster roles, which weaken something protecting the node or reach the Kubernetes API; the exposure policy; catalog entries at `trustTier: platform`; reading the decision and change logs | director (`can_approve`), the audit logs (`can_audit`) |
| kernel | **Auditor** | by name, as the security officer | reading the issuer, decision and change logs across all tenants, and every tenant's audit evidence; nothing else | read-only routes on the director and the usher (`can_audit`); the evidence console (`can_read_evidence`) |
| system | **Service admin** | by name: a person of the platform tenant | running the system engines: `system-postgresql`, `system-mariadb`, `system-cache`, `system-s3`, `system-mail`, `system-llm`, `system-authorization`, `system-turn-dmz` | the kernel tools host (`can_open_kernel_tools`), `can_operate_system` |
| shared | **Shared-apps admin** | by name: a person of the platform tenant | installing shared instances and offering them to tenants (`can_install_shared`, `can_grant_shared`). The class `shared-app` these govern is a planned structure and not implemented; the role and its relations exist so that the vocabulary is complete | — |
| tenant | **Tenant administrator** | the position Tenant administrator | one tenant: installing apps and addons, backup policies and export schedules, integration grants (`AppGrant`), partners, people and what they hold. The position holds no role on any app | director (`/v1/tenants/{t}/…`), the registrar through the administration console, the desktop |
| tenant-dmz | **Perimeter approver** | by name (AD-16): an administrator of the tenant, for that tenant (`tenant:<t>#perimeter_approver`), or a person of the platform tenant, for every tenant (`cluster#perimeter_approver`) | approving **tenant-scope** privilege requests: egress beyond the baseline, which leaves the tenant's own namespace; publishing an entry to the internet, within cluster policy; renewing or withdrawing it at review | director (`/v1/tenants/{t}/exposures/…`, `can_expose`, `can_approve_privilege`) |
| tenant, its apps | **App administrator** | the position App administrator, for every app; or `admin` on one app | administration *inside* the tenant's apps: the role reaches the app in its sign-in token, through its sync Job or through the sign-in sidecar ([iam.md §1.8](iam.md)); no platform rights | the apps |
| tenant | **User** | the position User (the desktop alone), All apps, a position of the tenant's own, or a role on one app | using the apps they hold a role on | the desktop, the apps |
| outside the cluster | **Catalog maintainer** | git host identity | authoring `ComponentProfile`s; a profile's classes (`spec.classes`) and `trustTier` are set in the catalog repository by review | the catalog repository |

Separations that are load-bearing, whatever one person happens to hold:

- **Platform administrator ≠ security officer.** The one who installs is not
  the one who approves exceptions. A waiver, an egress rule or a public
  surface that the same identity requested and approved is a policy hole
  with a signature on it. The cluster's administrator names the security
  officer on the claim with a reason and an optional expiry; the holder keeps
  their own account in their tenant's realm and uses the role signed in to
  their own tenant ([iam.md §1.1c](iam.md)). The registrar refuses every
  write to a platform-tenant holder's account, so the administrator cannot
  take it over through it. The administrator holds `realm-admin` in the
  kernel realm and so could still change the account by hand; the realm's
  admin events are the record of whether that happened, not a refusal.
- **Break-glass is not "platform administrator with more".** It is a
  person named for a time, with a reason: added for an incident, gone at
  the time stated, and the claim's history is the record. The platform
  administrator does not have it by default.
- **Tenant administrator ≠ perimeter approver**, as relations. Publishing to
  the internet is the one tenant decision that changes the blast radius of
  the platform, so it is its own grant and its own audit line, and egress
  beyond the baseline is the same decision in the other direction
  (`can_approve_privilege` is `can_expose`). A tenant's administrator
  approves only once named, and the record shows two decisions, because the
  relation asked is `can_expose`, never `admin` (AD-6).
- **App administrator is not a platform role.** It exists so that a tenant
  can make someone an administrator inside its apps without making them a
  tenant administrator. It confers nothing outside the apps.
- **Users are never administrators by inheritance.** `admin` is an explicit
  assignment; `user` grants nothing of it.
- **Administering a tenant is not using its apps.** The position Tenant
  administrator holds no role on any app, so such an account launches none
  unless it is also given one; the console says so where both are held. One
  layer up, a platform-role account holds nothing in a tenant it has not
  entered. A role held by name adds one cluster relation to a tenant
  administrator's account and nothing else, and only `security_officer`,
  `auditor` or `perimeter_approver`: that tenant's administrators can take
  over any account of its realm, so a role that changes the cluster is
  never held there.
- **A platform administrator enters a tenant only where the tenant said so.**
  The tenant's `operated_by` consent lets the cluster's administrators act on
  the tenant through the director; it does not sign them in to it. A tenant's
  administrators may additionally open their realm to the cluster's
  administrators (`Tenant.spec.platformAccess.adminsEnter`, off by
  default, AD-16), checked at every sign-in and recorded in the tenant
  realm's login events ([iam.md §1.1c](iam.md)).

## 6. Machine identities

Three classes. What class an identity is in follows from one question, does
it write the cluster, and the class fixes everything else: what it may hold,
where it runs, and what confines it.

| Class | Writes the cluster | Holds | Runs in | Confined by |
| --- | --- | --- | --- | --- |
| **Controllers** | yes: that is their job | Kubernetes RBAC, up to cluster-admin-equivalent; OpenBao `kernel/*` | `kernel-*` only | unreachable from any `system-*` or `tenant-*` namespace (the kernel's network rules, on by default) |
| **Enforcement points** | no | the few credentials their one question needs, and never `pods/exec` or `secrets` | `kernel-control`, `kernel-edge`, `system-authorization` | the credential is the only thing they can misuse |
| **Workloads** | never: **zero** Kubernetes RBAC | their own OpenBao prefix; the credentials granted to them as requirements | the tier their `class` puts them in | the namespace: NetworkPolicy from the profile, quota, Kyverno |

**Controllers**: Crossplane and its providers, Argo CD, ESO, cert-manager,
Kyverno, CNPG, Envoy Gateway, and the **operator**. They are the cluster's
hands; there is no least privilege to design here, only containment, which
is why they live in `kernel-*` and nothing outside it can reach them. The
operator is also the one that writes the authorization store (§3) and holds
Keycloak's administrator credential for provisioning realms.

**Enforcement points** answer one question each and hold what that takes.
Every one of them presents its own preshared key to the kernel's OpenFGA
(`kernel/services/openfga`: one key per service, so a key is revoked without
touching the others) and none has a token for the API server mounted:

| Identity | Credentials | Cluster access |
| --- | --- | --- |
| **Director** | the git push key and the signing key, read from the vault; one vault role, `gentian-os-director`, which reads repository credentials (`tenants/+/repositories/+`, `kernel/repositories/+`) and nothing else, to pull OCI catalogs, bound to its ServiceAccount through a token issued for the vault's audience | none: its ServiceAccount is granted nothing and its API-server token is not mounted; a profile reaches the cluster through the commit Argo CD syncs (AD-14). The operator knows it by a token projected for the operator's listener alone |
| **Bouncer** | each realm's `gentian-edge-exchange` client secret (Secret `bouncer-exchange`), to exchange a session's token for one valid at a single app (`exchangeToken`, `internal/bouncer/exchange.go`); it is the one platform component below the edge that holds a client secret | none |
| **Usher** | its OpenFGA key | none |
| **Registrar** | one Keycloak client credential per realm (`view-users`, `query-users`, `query-groups`, `manage-users`; no role over realm settings), mounted as a file per realm; it can name no other realm | none |
| **Custodian** | a write-only vault role bound to its own ServiceAccount (`vault-policy-custodian-write`): it sets a credential's value and returns none on any route | none |
| **Arbiter** | the app OpenFGA instance's preshared key, which no other workload is handed; admits the operator by TokenReview of a token for the audience `gentian-arbiter` | none ([app-authorization.md §2.3](app-authorization.md)) |

**Workloads**: everything else: system engines, tenant apps, the UI
backends, DMZ proxies. None has a ServiceAccount with any RBAC. What each
may read and reach is not decided per identity; it is a function of the
namespace tier and the profile:

| Tier | OpenBao prefix | Reach |
| --- | --- | --- |
| `system-<function>` | `gentian-os/kernel/<function>/*` | ingress from tenant namespaces on the contract port, and from its own `-dmz`; egress unrestricted, except in `system-llm`: the model gateway reaches its database, cache and model servers, the edge, and public addresses on 443 only where the Cluster claim lists a provider; a vLLM instance reaches public addresses on 443 for its weights ([llms.md](llms.md)) |
| `system-<function>-dmz` | one credential: the backend relay or proxy credential | ingress from the internet on the protocol's ports; egress to its backend in `system-<function>` and, for mail, to the internet on `:25` |
| `tenant-<t>` | `gentian-os/tenants/<t>/apps/<app>/*`, per app | `requires.services`, granted integrations and answered egress requests ([edge.md §3](edge.md)), nothing else |
| `tenant-<t>-dmz` | one credential: the surface's app password or scoped token | ingress on the surface's paths; egress to one backend service and port |

The UI backends are ordinary workloads: the desktop is a `class: app`
component holding a granted database and no credential at all; the edge
holds the zone's OIDC client and hands the token on only where an entry says
`forwardToken` ([routing.md §4.1](routing.md)); the platform console is the
same component in `tenant-platform`, the platform tenant whose realm is the
kernel realm (AD-10). Neither has a line in the enforcement-point table
because neither decides anything: they relay to the director and the usher,
and what a desktop shows is what the usher returned for that account's
relations.

**Bootstrap** is the one identity outside the classes: the installer, with
the human's kubeconfig and a bootstrap OpenBao token, until step `E-03`
revokes the token and the handover shows the human write path works.

Three invariants, one per class:

1. No pod in a `system-*` or `tenant-*` namespace has a ServiceAccount bound
   to any Role or ClusterRole.
2. No identity outside `kernel-control` holds a git credential, and no
   enforcement point can `exec`.
3. No controller Service is reachable from outside `kernel-*`.

## 7. Where each decision is enforced

| Question | Answered by | Fed by |
| --- | --- | --- |
| Who is this? | Keycloak: realm `kernel` for platform roles, realm `<t>` for tenant roles | the realm's sign-in |
| May they configure this? | OpenFGA, asked by the **director** | who holds what, given through the registrar; structure (installs, positions, role holders, tenants) projected by the operator from what git declares (AD-12) |
| May they write this secret? | OpenFGA, asked by the **custodian**: `can_write_credential` on `app:<t>/<p>`, on `tenant:<t>`, or on `cluster:<c>` for kernel and system secrets. OpenBao's policy then bounds the *path* the request may touch; it does not make the decision. Deciding in OpenBao policy from a token's claims would put secret-write authority outside `ListUsers` and leave it un-revoked by a tuple delete | the same store |
| May they reach this app? | OpenFGA, asked by the **bouncer**: `can_use`, which is a role on the app, not being a person of the tenant | the token; a decision cached per subject and question, evicted when the store's changelog moves |
| Is this anonymous request valid? | the **publishing proxy** in the DMZ: the entry's `authMode`, and a source restriction where one is declared. It strips every inbound identity header and sets only its own ([routing.md §7](routing.md)) | the credential presented, or none |
| May this pod do this? | the API server (RBAC), NetworkPolicy, Kyverno | the ServiceAccount, the namespace labels |

### 7.1 The model these roles need

The roles above map onto the model as `tenant#admin`, `tenant#user`,
`app#admin`, `app#user` and an app's declared roles, held by a person or
through a position, and `cluster#admin`, `cluster#break_glass`,
`cluster#security_officer`, `cluster#auditor`, `cluster#service_admin`,
`cluster#shared_apps_admin`, `cluster#perimeter_approver`,
`tenant#perimeter_approver` and `tenant#auditor`, held by name as
`user:<realm>/<sub>`; every verb a PEP exposes is a `can_*` relation computed
from them. A platform administrator acts inside a tenant through
`admin from operated_by`, and holds a position there only after entering it.

## 8. Responsibilities by layer, in one line each

- **Kernel**: the platform administrator installs, the security officer
  approves exceptions, the auditor reads, break-glass recovers. Nothing in
  this layer is created by a tenant, and nothing in it decides on a tenant's
  behalf without a tuple that says so.
- **System**: the service admin keeps the engines running and backed up.
  They see every tenant's data engine; they hold no tenant's credentials.
- **Tenant**: the tenant administrator decides what runs and who uses it,
  inside a ceiling the platform set and a namespace the platform enforces.
- **Perimeter**: the perimeter approver decides what the internet may reach,
  one entry at a time, inside what the security officer allowed.
- **App**: the app administrator runs the inside of the tenant's apps.
- **User**: uses what they were given.
