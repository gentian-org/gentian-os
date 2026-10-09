# Open items, M1 to M4

One list, kept current. An item leaves it when the thing works on a cluster,
not when the code exists. Last brought in line with the code on 2026-10-09.

M1 is *the platform administrator signs in and the installer leaves a cluster a
tenant can be provisioned on*; it was reached on 2026-10-03. M2 is *the first
functional tenant*, M3 *the first user invited by a tenant administrator*, M4
*the first app a user can actually work in*. The milestones and their steps
live in [implementation-plan.md](implementation-plan.md); this file is only
what is still open and why.

Four parts: the architectural decisions against the code, what is built and
has never run on a cluster, the defects that are known and open, and the
decisions that wait for the owner.

## 1. The architectural decisions, against the code

Checked against the implementation, not against the plans. "Holds" means the
code does what the decision says; it does not mean a cluster has shown it.

| AD | | State |
| --- | --- | --- |
| AD-1 | Nine security principles normative; every request passes one named enforcement point | **Holds, with one gap.** The publishing proxy is a named enforcement point that checks no caller: it filters and limits, and verifies nothing for any `authMode` (see AD-6) |
| AD-2 | The director is the only writer of the deployment repository | **Holds.** Commits are signed, Argo CD syncs only commits signed by the director's key or the break-glass key, the operator holds no git credential. For a private repository the installer gives Argo CD the repository's credential directly until the vault's copy takes over (`A-06`, `C-05-repository-handoff`). Argo CD and the director still share one credential that can push |
| AD-3 | The store's data is outside the cluster; its interface is an app on it | **Holds on the cluster's side.** The operator places the App Store app on every tenant but the platform's while the cluster reports its licences and names a store; an install is fetched at its digest, checked, committed, and checked again before rollout. A store address that is the cluster's own App Store host is not taken as a store |
| AD-4 | One catalogue kind, `ComponentProfile` | **Holds.** [app-customization.md](../app-customization.md) still says `AppProfile` in many places |
| AD-5 | Privileges are requests with one approval path | **Holds**, except that egress a profile declares reaches the network policy without an approval |
| AD-6 | `authMode` mandatory; a perimeter surface is published per tenant by an approver | **Deviation.** Publishing works as decided: a proxy in `tenant-<t>-dmz` only for an entry an approver published under `can_expose`, with a review date. Two things do not: the proxy checks no caller, so `basic`, `signature` and `jwt` on a perimeter entry promise nothing today; and `can_expose` is held by the members of the group `gentian:tenant:<t>:perimeter` alone, a group nothing creates, so the tenant's admins do not hold it by default as the decision says |
| AD-7 | Namespaces named by tier | **Holds** |
| AD-8 | Kernel trust domains are separate namespaces | **Holds** |
| AD-9 | System services have no public route | **Deviation, one of two closed.** Mail now faces the internet only through a proxy in `system-mail-dmz` that holds nothing; Postfix and Dovecot have no load balancer of their own. Still open: the model gateway's console is routed at `llm.<kernel domain>`, behind the kernel realm's session and `can_configure`, whenever the cluster runs the model gateway, and no setting takes the route away. TURN does not exist |
| AD-10 | The portal splits two ways; the platform is a tenant | **Holds** |
| AD-11 | The target layout applies to fresh installs | **Holds** |
| AD-12 | The authorization store is a projection: the operator writes it, from git and from Keycloak's events; the director asks it | **Holds for who writes, with four things not built and one credential too many.** The operator receives Keycloak's signed statements and projects memberships, and projects roles, tenants, apps and grants from what git declares; the director asks the store and writes nothing to it. **Not built:** no code revokes a person's sessions when their groups change; no realm setting disables offline tokens (an app's client is left the optional scope `offline_access`); nothing reconciles the stored memberships toward Keycloak, so a statement that never arrives is repaired only by the next one about the same person; and the check at start that the defaults git implies are the ones the store holds does not exist. **Against the decision:** the operator still holds Keycloak's master administrator credential (defect 15) |
| AD-13 | The edge is the only session authority | **Holds, with the two exceptions the entry names**: the App Store app's sign-in to a store, and the sign-in sidecar |
| AD-14 | Catalogue sources on the Cluster claim; a profile reaches a cluster only at a verified digest | **Deviation, not decided.** Sources on the claim and per tenant, delegation, one bundle under one digest, and the console's catalogue view hidden: as decided. But the installer fetches the Operations Console's profile by address at install and commits it with no digest (see part 4) |
| AD-15 | Multi-language is a core requirement | **Partly.** Desktop, console and sign-in pages are translated. A component's `description` and a store listing's text are single strings; the desktop has no check for a missing translation in CI |

## 2. Built, and never run on a cluster

Most of what was built since M1 is in this part. Each item has tests; none has
been seen working on a cluster. Until it has, it is not done.

### Signing in and the front door

- The front door in its present order: the Gateway signs a person in first and
  asks the bouncer second, on Envoy Gateway 1.9.2 and Keycloak 26.8.0.
- The session's cookies and tokens stopping at the edge.
- The sign-in sidecar, for an app that supports neither OIDC nor SAML, and the
  App Admin role it reads from the realm's signed answer. Three apps declare
  it.
- A mailbox opened with a sign-in token, for an app that declared it.
- The bouncer's rights check for a component that holds a key for it.

### Addresses and publishing

- The two tenancy modes, and the user tenant of a single-tenancy cluster
  created by the installer after the handover (`E-04`).
- Address names an app may not take, refused by the operator and the director.
- A tenant bound to a domain of its own with the command line.
- What a tenant's apps ask to publish as a read; approval, review and
  withdrawal on the command line and in the administration console.
- A website on the main address of a single-tenancy cluster, with the
  approver's acknowledgement.
- The publishing proxy's limits and filters, and the limit on sign-in posts at
  the Gateway.

### Network

- The kernel namespaces' rules for incoming connections. They are off by
  default (`KERNEL_NETWORK_POLICIES`) for exactly this reason: turn them on
  after a successful install with `./install.sh --only A-01,B-01`.
- The rules on the shared stores, the kernel's PostgreSQL, the mail servers and
  the model gateway's namespace.
- A contract between two apps carrying traffic once it is granted.

### Mail

- The mail proxy in `system-mail-dmz` and the client's address passed on to the
  servers. Its test runs the rendered proxy and servers in local containers.
- Every mail object written into the namespace of the server that reads it,
  and a tenant's mail held as not ready (`PostfixMapMissing`) until Postfix's
  map names its domain.

### Catalogue and apps

- Catalogues added per cluster and per tenant, read when needed; nothing copied
  into a cluster ahead of an install.
- A profile bundle with its companions under one digest, and the operator's
  check of all of it before rollout.
- What a newer build of an app left behind, listed and removed.
- The App Store app placed on tenants where the cluster offers a store.
- The model gateway as a requirement an app declares, with its image named by
  tag and digest.

### Data

- Backup bundles of schema version 3: the data of uninstalled apps that was
  kept, the platform desktop's database, mailboxes, and the rights that follow
  from nothing else.
- A restore into a new tenant (`TenantRestore.spec.intoNewTenant`), and an
  import that gives the tenant its own names.
- A tenant's deletion removing its mailboxes, and failing loudly.

### Installer and director

- Argo CD given a private deployment repository's credential before the claim
  that would supply it.
- `--dry-run` and `--validate` changing nothing; the break-glass key found by
  its recorded id; the OpenBao command line fetched and checked.
- The default catalogue chosen by the ref the platform is installed from.
- The director's clean stop and its write retry bounded by time.

Carried over from before, and still not shown:

- Whether Argo CD accepts the director's signatures on a cluster after a
  rebuild from the recovery kit.
- The Keycloak event listener recording administrative events with the request
  id, so that the registrar's record and Keycloak's join. It projects group
  membership and drops the rest.
- Opening an app without a second sign-in (M4.6), which changed since it last
  worked.

## 3. Known defects, open

Each is true of the code today.

| | Defect | Where it is described |
| --- | --- | --- |
| 1 | **An app's own client loses its token at the front door.** On a route with a session the Gateway removes the `Authorization` header a client sent; a gateway entry with another `authMode` is not routed at all. Sync clients, mobile apps and scripts that bring a token of their own do not reach their app | [routing.md §4.1](../design/routing.md) |
| 2 | **Signing out does not reach most apps.** The session ends at the Gateway when its access token runs out. The realm tells only an app whose own OIDC client declares a back-channel logout address; any other session an app keeps lasts until it ends by itself, a sidecar's at most an hour | AD-13, [security.md §2.12](../design/security.md) |
| 3 | **One key opens the rights store.** Six programs present the same OpenFGA key, and it can write | [operator-split-plan.md §6](operator-split-plan.md) |
| 4 | **Nobody may publish by default.** `can_expose` needs the group `gentian:tenant:<t>:perimeter`, which no install and no tenant creation makes; a tenant's admin has to create it and join it before anything can be approved | part 1, AD-6 |
| 5 | **The publishing proxy checks no caller**, whatever the entry's `authMode` | [security.md §2.14](../design/security.md) |
| 6 | **Identity headers are not signed.** An app and a sign-in sidecar believe them; network rules are what keeps another pod from sending its own | [operator-split-plan.md §6](operator-split-plan.md) |
| 7 | **No kernel namespace restricts outgoing connections** | [security.md §2.13](../design/security.md) |
| 8 | **An import carries the source's approvals.** The new tenant's manifest is written from the bundle, so the privileges granted and the entries published in the exported tenant arrive approved, and nobody on the importing cluster approved them | [data-lifecycle.md](../design/data-lifecycle.md) |
| 9 | **A deleted tenant leaves entries behind**: its rights and memberships in the rights store, which a later tenant of the same name would inherit, and its keys in the shared cache | [data-lifecycle.md](../design/data-lifecycle.md) |
| 10 | **A removed person's mailbox stays** until the tenant is deleted | [mail.md](../design/mail.md) |
| 11 | **The Operations Console's profile arrives without a digest** | part 4 |
| 12 | **The model gateway's console cannot be switched off** separately from the model gateway | part 1, AD-9 |
| 13 | **Crossplane's providers install every resource type they ship** — about 400 — and the compositions use about 27. Nothing narrows what is installed | `crossplane/providers/` |
| 14 | **Two images float.** `vllm/vllm-openai` falls back to `latest` when the claim names no tag, and the model gateway's cache runs `redis:alpine`. `lint-image-pins` lists the first as known and does not look at the second | `kernel/services/llm/` |
| 15 | **The operator holds Keycloak's master administrator credential.** It reads the `keycloak-admin` Secret and hands it to the Jobs that configure realms, clients and groups. The process that writes the rights store can therefore change any identity as well | part 1, AD-12; `identity_reconciler.go` |
| 16 | **Argo CD's repository credential can push** | [operator-split-plan.md §6](operator-split-plan.md) |
| 17 | **A change of a person's groups does not end their sessions**, and offline tokens are not disabled. AD-12 states both; no code does either. An app that read groups from its own token keeps them until that token or the app's session ends | part 1, AD-12 |

## 4. Decisions waiting for the owner

1. **The Operations Console's profile at install** (AD-14). The installer
   fetches it by address and commits it with no digest; since the catalogue
   address answers, it really arrives that way, and the cluster takes it for a
   profile the platform placed. Either the installer pins it to the digest the
   catalogue's index lists and refuses other bytes, or it places nothing and
   the profile is installed through the director like any other.
2. **Who may publish by default** (AD-6). The decision gives the right to the
   tenant's admins unless a tenant separates the role; the code gives it to a
   group that does not exist. Either the tenant's admins group is written as
   perimeter approver when a tenant is made, or the decision is changed to say
   that the role is always staffed separately, and the group is created empty.
3. **Sessions and offline tokens when a person's groups change** (AD-12).
   Who writes the rights store is decided: the operator, and AD-12 says so.
   Two sentences of the decision have no code: a membership change revokes
   the person's sessions, and offline tokens are disabled. Either they are
   built, or the decision is changed to say what bounds a stale group
   instead (the access token's lifetime and `sessionMaxAge`). The operator's
   hold on Keycloak's master administrator credential is defect 15.
4. **What `basic`, `signature` and `jwt` mean on a perimeter entry** (AD-6,
   AD-1). Either the proxy verifies them, or the schema refuses them there
   until it does, or they stay as a statement about the app that the platform
   does not check.
5. **A route for a client that brings its own token** (defect 1). Either a
   gateway entry may ask for a route where the bouncer verifies the client's
   bearer token, or such paths are published through the publishing proxy and
   checked by the app.
6. **The model gateway's console** (AD-9). Either it gets a switch, off by
   default, or AD-9 names it with the kernel's own tools as a console behind
   the kernel session.
7. **External IMAP and submission.** The plans had ports 587 and 993 closed
   until a tenant's approver opened them; they are open whenever the cluster
   runs its own mail. Either that is accepted, or the mail proxy gets a
   setting per port.
8. **When the kernel's network rules become the default.** The intent is:
   once a fresh install has passed with them on.
9. **Should the package union admit a `composition` beside a `chart`?** It
   does today, because some entries are both. The sign-in sidecar removed one
   reason for an app to bring its own Composition; if the others go the same
   way the rule can return to exactly one.

## 5. Known and deliberately not now

- **Moving a tenant to another cluster goes through export and import**, which
  re-mint what the platform issues. The alternative, in which the recovery kit
  is the cluster's identity and a new server that imports it becomes the old
  cluster, is not taken: it would bind a tenant's portability to its
  provider's kit.
- **The director reads the cluster's catalogue sources from the claim in
  git.** An edit made outside the director reaches it with the next commit it
  reads.
- **The check at start of the rights store's defaults** (AD-12): wanted, and
  it reports, it does not repair. A store that has diverged is a question,
  because rewriting it would erase the grants and revocations that are
  nobody's default.
