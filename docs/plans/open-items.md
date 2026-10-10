# Open items, M1 to M4

One list, kept current. An item leaves it when the thing works on a cluster,
not when the code exists. Last brought in line with the code on 2026-10-10.

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
| AD-1 | Nine security principles normative; every request passes one named enforcement point | **Holds, with one accepted limit, stated in the decision.** The publishing proxy is a named enforcement point that checks no caller: it filters and limits. On an entry of `authMode: app` it passes the caller's credential to the app, which checks it; the platform does not know that caller, and a removed person's app credential lives until the app revokes it (roadmap 1.34) |
| AD-2 | The director is the only writer of the deployment repository | **Holds.** Commits are signed, Argo CD syncs only commits signed by the director's key or the break-glass key, the operator holds no git credential. For a private repository the installer gives Argo CD the repository's credential directly until the vault's copy takes over (`A-06`, `C-05-repository-handoff`). Argo CD and the director still share one credential that can push |
| AD-3 | The store's data is outside the cluster; its interface is an app on it | **Holds on the cluster's side.** The operator places the App Store app on every tenant but the platform's while the cluster reports its licences and names a store; an install is fetched at its digest, checked, committed, and checked again before rollout. A store address that is the cluster's own App Store host is not taken as a store |
| AD-4 | One catalogue kind, `ComponentProfile` | **Holds.** The design documents name the kind as the code does since 2026-10-09 |
| AD-5 | Privileges are requests with one approval path | **Holds**, except that egress a profile declares reaches the network policy without an approval |
| AD-6 | `authMode` mandatory; a perimeter surface is published per tenant by an approver | **Holds, not yet shown on a cluster.** Publishing works as decided: a proxy in `tenant-<t>-dmz` only for an entry an approver published under `can_expose`, with a review date. The proxy checks no caller, and no entry says it does any more: `authMode: app` passes the caller's credential to the app, which checks it (the accepted limit in AD-1), and `basic`, `signature`, `jwt` and `bearer` are refused on a perimeter entry by the schema. Who approves is as the decision says since it was changed on 2026-10-09: the group `gentian:tenant:<t>:perimeter` is created with the tenant, the cluster's administrator holds `can_expose` in every tenant its cluster operates, and a tenant's admins hold it only where the cluster's administrator switched it on (part 2) |
| AD-7 | Namespaces named by tier | **Holds** |
| AD-8 | Kernel trust domains are separate namespaces | **Holds** |
| AD-9 | System services have no public route | **Holds by default; one claim setting departs from it.** Mail now faces the internet only through a proxy in `system-mail-dmz` that holds nothing; Postfix and Dovecot have no load balancer of their own. The model gateway's console has a claim setting, `llm.console.enabled`, off by default: off, `llm.<kernel domain>` has no route and the edge is not admitted to the gateway, so the cluster is in line with the decision on this point. Switching it on routes the console behind the kernel realm's session and `can_configure`, and is a deliberate departure the cluster's owner takes for that cluster ([llms.md](../design/llms.md)). TURN does not exist |
| AD-10 | The portal splits two ways; the platform is a tenant | **Holds** |
| AD-11 | The target layout applies to fresh installs | **Holds** |
| AD-12 | The authorization store is a projection: the operator writes it, from git and from Keycloak's events; the director asks it | **Holds for who writes, with two things not built, two stated as aims and one credential too many.** The operator receives Keycloak's signed statements and projects memberships, and projects roles, tenants, apps and grants from what git declares; the director asks the store and writes nothing to it. **Not built:** nothing reconciles the stored memberships toward Keycloak, so a statement that never arrives is repaired only by the next one about the same person; and the check at start that the defaults git implies are the ones the store holds does not exist. **An aim since 2026-10-09, not something the platform does:** no code revokes a person's sessions when their groups change, and no realm setting disables offline tokens (an app's client is left the optional scope `offline_access`). **Against the decision:** the operator still holds Keycloak's master administrator credential (defect 15) |
| AD-13 | The edge is the only session authority | **Holds, with the two exceptions the entry names**: the App Store app's sign-in to a store, and the sign-in sidecar. An approved entry that keeps the app's own `Authorization` header is no exception to it: the session is still the edge's and still required |
| AD-14 | Catalogue sources on the Cluster claim; a profile reaches a cluster only at a verified digest | **Holds, with the exception the entry names**: the installer places the Operations Console's profile at install, at the digest its catalogue's index lists or a pin, written as the director writes it. Its default Component is not pinned, so the operator does not compare it at rollout (part 3) |
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
- A sign-out told to the apps inside the cluster, at an address the platform
  builds from the entry's own Service: Nextcloud (`nextcloud-base-ce`) and
  XWiki through their own client, Docmost and OpenProject through the sign-in
  sidecar. What it does not reach is defect 2.
- The bouncer's rights check for a component that holds a key for it.

### Addresses and publishing

- The two tenancy modes, and the user tenant of a single-tenancy cluster
  created by the installer after the handover (`E-04`).
- Address names an app may not take, refused by the operator and the director.
- A tenant bound to a domain of its own with the command line, and the
  director refusing that for any tenant of a single-tenancy cluster and for
  the platform tenant on every cluster.
- What a tenant's apps ask to publish as a read; approval, review and
  withdrawal on the command line and in the administration console.
- A website on the main address of a single-tenancy cluster, with the
  approver's acknowledgement.
- Who approves a public address: the perimeter group created with a tenant,
  the cluster's administrator approving in a tenant its cluster operates, the
  switch by which a tenant's administrators approve
  (`spec.perimeter.adminsApprove`) reaching the rights store and leaving it
  again, and the registrar refusing a change of who approves to somebody who
  may not approve.
- The user tenant of a single-tenancy cluster written by the install with
  that switch on, so that the user admin approves there.
- The publishing proxy's limits and filters, and the limit on sign-in posts at
  the Gateway.
- A public entry that passes its callers' credential to the app (`authMode:
  app`), with its lower limit. Tested against the proxy's own image; no app
  in the catalogue declares one yet.
- An entry behind sign-in that keeps the app's own `Authorization` header
  (`clientAuthorization: app`) once approved. The policy is validated against
  the pinned Envoy Gateway definitions and the bouncer's decisions are
  tested; no browser has been through it on a cluster.

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
- Mail to a removed person's address refused at `RCPT` while the record of
  the removal stands and nobody holds the address. Shown against the rendered
  Postfix in local containers.

### Catalogue and apps

- Catalogues added per cluster and per tenant, read when needed; nothing copied
  into a cluster ahead of an install.
- A catalogue kept in a directory of the cluster's own deployments
  repository, declared by the cluster's administrator and read from the
  director's checkout. Held by tests of the director against a local
  repository.
- A profile bundle with its companions under one digest, and the operator's
  check of all of it before rollout.
- The same check for a Component the operator places by default, against the
  bundle recorded with its profile.
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
- A removed person's mailbox archived or deleted, as whoever removes the
  person chooses at that moment; archived mailboxes in backups, listed, and
  deletable. Proven in local containers, not on a cluster.

### Installer and director

- Argo CD given a private deployment repository's credential before the claim
  that would supply it.
- `--dry-run` and `--validate` changing nothing; the break-glass key found by
  its recorded id; the OpenBao command line fetched and checked.
- The default catalogue chosen by the ref the platform is installed from.
- Crossplane installed without its activate-everything policy, and only the
  25 provider resource types in use activated (`A-04`, `B-05`): the four
  providers then bring 42 CRDs in place of the 391 they ship (the 25, the ten
  configuration kinds of the vault and Keycloak providers, and all seven of
  provider-kubernetes and provider-helm, which cannot be narrowed). Not shown
  on a cluster: that the two providers come up healthy with only those types, that
  `B-05`'s wait for them ends, how a composed resource of a type that is not
  activated shows on its composite, and `CROSSPLANE_ACTIVATE_ALL=true` as the
  way back on a cluster that was installed without it.
- The default profile placed only at the digest its catalogue's index lists or
  a pin, written as the director writes it.
- The installer refusing when the break-glass key `keys.env` records is not on
  the machine, and `--rotate-break-glass-key` -- in particular that Argo CD
  accepts the repository again once `B-01` and `B-10` have run with the new
  id.
- The director's clean stop and its write retry bounded by time.

- The Models tab of the administration console and `kubectl gentian models`:
  the director's `GET` and `PUT /v1/clusters/{c}/models`, the commit of the
  claim, the gateway restarting with the new list after the sync, and the
  flags on models that cannot answer.
- A provider's credential declared from the claim: the Cluster composition
  composing `llm-provider-<name>` and its probe, taking over the one the
  installer applied for a provider on a cluster installed before, and removing
  them with the provider. The gateway leaving out a provider that names
  another's token property.
- The gateway's console switched on and off from the Models tab.

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
| 1 | **An app's own client and its token: closed in two parts, open in a third.** A browser page that sends the app's own token keeps it on an entry that declares `clientAuthorization: app`, once the tenant's perimeter approver approved it; without the declaration or the approval the Gateway still removes the header. A client with no browser session (sync client, mobile app, script, webhook) reaches the app through a public entry of `authMode: app` with a credential the app issued. **Still open**: a client that needs the app's cookies on a public address does not work, which is what Synapse's sign-in needs, so Element is not served yet; no bearer token is verified at the edge; and the catalogue's apps have to declare the entries before any of it helps them | [routing.md §4.1, §7](../design/routing.md) |
| 2 | **Signing out does not reach every app.** The session ends at the Gateway when its access token runs out, and the realm now tells an app inside the cluster where the app can be told (built 2026-10-09): Nextcloud (`nextcloud-base-ce`) and XWiki through their own client, Docmost and OpenProject through the sign-in sidecar. Still open: Activepieces 0.28.0 has nothing to end a session with (an hour at most); Open WebUI needs a switch and Redis (on the roadmap, item 2.27); Element needs Synapse's switch and its own Composition to register an address; `nextcloud-base-od` declares it and has not been run; Mathesar and Odoo cannot be told at all. The realm tells once and not when a session only runs out, so an app's own session lifetime remains the bound | AD-13, [iam.md §1.12](../design/iam.md), [security.md §2.12, §2.15](../design/security.md) |
| 3 | **One key opens the rights store.** Six programs present the same OpenFGA key, and it can write | [operator-split-plan.md §6](operator-split-plan.md) |
| 4 | **Closed in the code on 2026-10-09, not yet shown on a cluster: nobody could publish by default.** The group `gentian:tenant:<t>:perimeter` is now created with every tenant, and the cluster's administrator approves in every tenant its cluster operates. A cluster installed before that gets both when the operator and the tenant's composition are updated; until then approving still needs a hand-made group | part 1 and 2, AD-6 |
| 5 | **The publishing proxy checks no caller.** No entry claims otherwise any more: the modes that named a check are refused by the schema, and `authMode: app` hands the credential to the app. What stays open is the consequence the owner accepted for now: the platform does not know the caller on such a path, and a removed person's app password or token works until the app revokes it | [security.md §2.14](../design/security.md), AD-1, [roadmap.md](../roadmap.md) 1.34 |
| 6 | **Identity headers are not signed.** An app and a sign-in sidecar believe them; network rules are what keeps another pod from sending its own | [operator-split-plan.md §6](operator-split-plan.md) |
| 7 | **No kernel namespace restricts outgoing connections** | [security.md §2.13](../design/security.md) |
| 8 | **An import carries the source's approvals.** The new tenant's manifest is written from the bundle, so the privileges granted and the entries approved in the exported tenant — public addresses, with their kind, and kept `Authorization` headers — arrive approved, and nobody on the importing cluster approved them | [data-lifecycle.md](../design/data-lifecycle.md) |
| 9 | **A deleted tenant leaves entries behind**: its rights and memberships in the rights store, which a later tenant of the same name would inherit, and its keys in the shared cache. Not now: on the roadmap, item 1.38 | [data-lifecycle.md](../design/data-lifecycle.md), [roadmap.md](../roadmap.md) |
| 10 | **Mail to a removed person's address is accepted again once the record of the removal is gone.** Closed in the code on 2026-10-09 for as long as the record stands, and shown against Postfix in local containers, not on a cluster: the address is refused at `RCPT`, archived or deleted. The record of a deleted mailbox is removed after 30 days, and under the recipient policy `catchall` (the default) the address is accepted from then like any other nobody owns. A person removed before this was built is not known. An IMAP session open at the removal is not ended | [mail.md §5c](../design/mail.md) |
| 11 | **A default Component's digest is stated only on its profile.** Closed in part on 2026-10-09: the operator now compares the profile, and what its bundle brings, with the bundle recorded with it before it rolls out a Component it placed by default, and holds the Component when they differ. Open: no second place states that digest, as the tenant's manifest does for an install, so a profile replaced together with its bundle passes; on the roadmap, item 1.39. The digest the installer uses comes from the catalogue's own unsigned index unless the person installing pins one | AD-14, [security.md §2.11](../design/security.md), [roadmap.md](../roadmap.md) |
| 12 | **The model gateway's console switch has not run on a cluster.** The console is off unless the claim says `llm.console.enabled: true`, so the defect as it stood -- no way to serve models without the console -- is closed in the code. Not yet seen: that an upgraded cluster loses the route and the edge's rule at the gateway, and that the console works behind the edge when switched on (its own sign-in and its `Authorization` header are held by tests of the rendered policy only) | part 1, AD-9; [llms.md](../design/llms.md) |
| 13 | **A cluster installed before 2026-10-09 keeps every Crossplane resource type** — the 391 CRDs of the four providers, and provider-http's until `B-05` removes that provider. A fresh install creates 42: the 25 types the platform uses and the 17 that cannot be left out (`crossplane/providers/activation.yaml`, held by `make lint-provider-activation`); Crossplane never deactivates a type, so nothing narrows an existing cluster. The narrowed list has not run on a cluster yet (part 2) | [install-reference.md §4](../install-reference.md) |
| 14 | **Two images float.** `vllm/vllm-openai` falls back to `latest` when the claim names no tag, and the model gateway's cache runs `redis:alpine`. `lint-image-pins` lists the first as known and does not look at the second | `kernel/services/llm/` |
| 15 | **The operator holds Keycloak's master administrator credential.** It reads the `keycloak-admin` Secret and hands it to the Jobs that configure realms, clients and groups. The process that writes the rights store can therefore change any identity as well | part 1, AD-12; `identity_reconciler.go` |
| 16 | **Argo CD's repository credential can push** | [operator-split-plan.md §6](operator-split-plan.md) |
| 17 | **A change of a person's groups does not end their sessions**, and offline tokens are not disabled. AD-12 states both as an aim since 2026-10-09; no code does either. An app that read groups from its own token keeps them until that token or the app's session ends | part 1, AD-12 |
| 18 | **The development catalogue is not republished until the released branch can be built.** The catalogue repository's publishing job builds the released and the development catalogue together, and fails while the released branch lacks the catalogue build script. A profile changed on the development branch therefore does not reach `…/gentian-apps/develop`, and a cluster installed from a branch keeps reading the last catalogue that was published | [custom-catalogues.md §3](../custom-catalogues.md) |
| 19 | **The XWiki profile on the development catalogue cannot be installed.** It names a chart (`xwiki-ce` in the upstream chart repository, which serves `xwiki`) and an image that do not exist. Sign-out reaching XWiki (defect 2, part 2) is therefore held by tests of the profile only | the catalogue repository, `profiles/xwiki/xwiki-ce` |
| 20 | **`backchannelLogoutUrl` remains in profiles outside this repository.** The schema refuses the field (`OIDCClientSpec`, `api/v1alpha1/profile_parts.go`): a bundle that still carries it is not admitted, and the app is not installed or updated from it, until the profile declares `backchannelLogout` (exposure and path) instead. The catalogue's released branch and some extension profiles still carry the old field | [app-customization.md](../app-customization.md) §2, [iam.md §1.12](../design/iam.md) |
| 21 | **Removing a tenant's file reports "unchanged" and leaves the file when the tenant has no `kustomization.yaml`.** `writeTenantFileLocked` (`internal/director/gitops/backup.go`) decides that there is nothing to remove from the read error of the kustomization, not from that of the file. Unbinding a tenant's domain (`tenants domain <t> --remove`) in such a tenant directory answers `unchanged` and `domain.yaml` stays | `internal/director/gitops/backup.go` |
| 22 | **The smoke check of a cluster's own mail cannot pass.** `make verify-kernel-services` (and `e2e-p5-keycloak-dovecot`) run `crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh`, which calls `verify_keycloak_installation`; that function was deleted from `scripts/lib/verify-kernel-services.sh`, so the script always counts one error. The Dovecot check it also calls now looks in `system-mail`, and has not been run | `scripts/lib/verify-kernel-services.sh` |
| 23 | **A model the cluster serves itself is offered and not served.** Closed in the code on 2026-10-09 for what the claim declares, not yet shown on a cluster: the gateway's model list is its configuration file, written by its chart from `spec.llm.providers` and `spec.llm.instances` of the claim the LLM ApplicationSet hands it. What stays open: nothing starts the vLLM instance behind an entry of `instances` (`kernel/services/llm/chart` is delivered by no Application, has no NetworkPolicy, lacks the pod settings the admission baseline requires and defaults its image to `latest`), so such a model is listed and a call to it fails. The administration console's Models tab and `kubectl gentian models list` show every such model as not served, from that fact and not from the cluster. For a provider's model the tab reads the custodian's list of credentials and flags a missing token; nothing on the cluster does, nothing probes the provider, and a wrong token still shows as a failing call. A provider's credential requirement is declared from the claim by the Cluster composition | [llms.md §5, §6](../design/llms.md), [roadmap.md §2.28](../roadmap.md) |
| 24 | **Closed in the code on 2026-10-10, not yet shown on a cluster: a granted cluster role created nothing.** A profile asks for a cluster role by name; the operator binds it where the platform's set has it, the allowlist permits it for the profile and it was granted on the install. The set is empty and the operator has no permission on RBAC objects, so the path that binds has run in tests only. The first role brings those permissions with it. [roles-and-authorizations.md](roles-and-authorizations.md) §2 still states that no workload's ServiceAccount is bound to any role, which the first role of the set ends | [security.md §3.4](../design/security.md) |
| 25 | **The password policy is not in git, and a second change of it is probably undone.** Since 2026-10-10 the registrar's action is the one way to set it and the director commits no password block. A realm that is rebuilt comes back without the policy. The tenant Composition declares the realm without `passwordPolicy` unless the manifest carries `spec.security.password`, and leaves the provider to fill undeclared fields from what it observes: the first policy the registrar sets is then copied into the realm resource and held, and a later change is set back to it. That is what the same resource did to the realm's mail server until every field of it was declared (the note at `smtpServer` in `tenant-default.yaml`); for the password policy it has not been run on a cluster. A manifest that still carries `spec.security.password` has the Composition write that over what the registrar set; the director drops the block from `security-policy.yaml` the next time it writes that file | [admin-console.md §4.5](../design/admin-console.md) |
| 26 | **Closed in the code on 2026-10-10, not yet shown on a cluster: the operator granted integrations by itself, and a waiver needed no grant.** The operator writes no `AppGrant` and cannot (its role has neither create nor patch); a binding reports what was granted. A pod-security waiver needs the allowlist and the grant on the install. Grants the operator wrote before stay in the cluster | [security.md §3.4](../design/security.md) |

## 4. Decisions waiting for the owner

1. **Whether a default Component is pinned** (AD-14). **Decided
   2026-10-09** and built: the operator compares a Component it places by
   default with the digest of the bundle recorded with its profile, at
   rollout, as it does for an install. What is left is defect 11.
   **Decided 2026-10-09**: stating that digest in a second place, on the
   Cluster claim or the Component, is not built now; on the roadmap, item
   1.39.
2. **Who may publish by default** (AD-6): decided on 2026-10-09 and built.
   The perimeter group is created with the tenant, the cluster's
   administrator approves, and a tenant's admins approve only where the
   switch is on. **Decided 2026-10-09**, the one choice that was left: the
   install writes the user tenant of a single-tenancy cluster with the switch
   on, so the user admin approves there, and the director's create route
   does the same for that tenant when the request does not state the switch.
   Every other tenant starts with it off, and a user tenant that exists is
   not rewritten.
3. **Sessions and offline tokens when a person's groups change** (AD-12).
   **Decided 2026-10-09**: AD-12 states both as an aim, not as something the
   platform does. Neither is built (defect 17). The operator's hold on
   Keycloak's master administrator credential is defect 15.
4. **What `basic`, `signature` and `jwt` mean on a perimeter entry** (AD-6,
   AD-1). **Decided 2026-10-09**: the schema refuses them there, with
   `bearer`, until they can be enforced; what can be delivered now is
   `authMode: app`, where the app checks.
5. **A route for a client that brings its own token** (defect 1). **Decided
   2026-10-09**: both, each as an entry type the tenant's perimeter approver
   approves — behind sign-in the page's own header is kept
   (`clientAuthorization: app`), and a client without a session goes through
   the publishing proxy and is checked by the app (`authMode: app`). The
   app's cookies on a public address are left for later, with Synapse.
6. **The model gateway's console** (AD-9). **Decided 2026-10-09**: it gets a
   switch, off by default, and stays a console for platform administrators
   where a cluster switches it on. Built as `llm.console.enabled` on the
   Cluster claim; AD-9 is unchanged. What is left is defect 12.
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
10. **Mail to a removed person's address** (defect 10). **Decided
    2026-10-09** and built: it is refused, whether the mailbox was archived
    or deleted. **Decided 2026-10-09**: a deleted mailbox's address is
    refused for the 30 days its record is kept, and no longer. The list is
    built by listing a domain's people at each sync; a lookup per address is
    on the roadmap, item 2.29.
11. **Whether a perimeter approver who is not a tenant administrator is let
    into the admin console.** **Decided 2026-10-09: no.** The console stays
    the tenant administrators'.
12. **Open WebUI ending a session at a sign-out.** **Decided 2026-10-09**:
    not built now; on the roadmap, item 2.27.
13. **Three credentials that stay derived under `secretMode: random`.** The
    mode now reaches what the operator makes for an app, and the installer
    keeps what it made. Three are still computed from the master password in
    both modes ([security.md §6.3](../design/security.md)). **Decided
    2026-10-10** for an app's own secrets (`spec.appSecrets`): a bundle
    carries the secrets that cannot be made again where it is restored, only
    in a part that is encrypted to the backup key, and the app's secrets then
    follow the mode. Not built. A bundle is encrypted in every mode
    (`internal/backup/encrypt.go`), so the condition can be met; what is
    still to be decided before it is built:
    - *A restore into another tenant.* An import makes the new tenant's apps
      first, each with secrets of its own, so the bundle's secrets would
      replace stored ones. A right granted in one tenant is not carried into
      another by a file (`TenantRestore.spec.intoNewTenant`); whether a
      secret is, and to which paths, is open.
    - *A restore replaces a stored value.* The same holds for an app purged
      and installed again in the same tenant: the first value stored at a
      path would no longer be the path's value (security.md §7).
    - *A bundle says nothing of who wrote it.* Anyone who knows the
      cluster's public key can encrypt to it. Today a made-up bundle
      replaces data; with secrets it would also set values in OpenBao.
    - *A bundle encrypted to a requester's own key or passphrase.* It is not
      encrypted to the backup key, so it would carry no secret, and under
      `random` would not bring an app's encrypted data back readable.
    - *Who can open a bundle.* With `spec.backup.escrowIdentity` (the
      default) the backup key is in OpenBao, where the cluster
      administrators' policy reads it and is refused a tenant's paths; the
      bundles would give that policy the tenant's app secrets.
    - *Which secrets.* A profile names its secrets and does not say which of
      them stored data depends on; all of an app's would travel, or a
      profile gets a field for it.
    The key Keycloak's event listener signs with, and the kernel realm's own
    mail login on a cluster that runs its own mail server, stay derived:
    each needs a place in OpenBao that nobody has chosen, and neither is
    part of a tenant's bundle.
14. **Whether a change of `secretMode` on an installed cluster is refused.**
    Today it converts nothing and existing credentials stay, with one
    exception: back from `random` to `derived`, the installer writes derived
    keys over `gentian-os/kernel/llm`. Refusing needs a record of the mode
    the cluster was installed with, which nothing keeps.
15. **How the master password's presence is known without ESO reading it.**
    `eso-read` cannot deny `gentian-os/kernel/internal/master-password` (and
    with it the salt) the way it denies the backup key, while the probe
    `credreq-master-password` reads that path: denied, the probe fails, and
    the custodian and `make check-credentials` report the master password as
    missing. It needs a requirement that has no probe, and something else the
    installer's catalogue step, the custodian and the check take as the
    answer.
16. **Whether the platform snapshots OpenBao.** Under `secretMode: random`
    OpenBao holds the only copy of what was drawn at random, no bundle and no
    recovery kit carries it, and nothing takes a snapshot. **Decided
    2026-10-10**: it will, and `random` is to be put through recovery drills
    with it; on the roadmap, item 2.30.
17. **What a cluster configuration without a mode means.** **Decided
    2026-10-10**, as built: a `gentian-cluster-config` that is not there yet,
    or that was written before it carried `secretMode`, reads as `derived`,
    which is what every cluster did before the operator read the mode
    (`ClusterSecretMode` in `internal/controller/cluster_config.go`).
18. **What a cluster administrator may point a model provider at.**
    **Decided 2026-10-10**, and built: a public `https` address only, by the
    rules for a catalogue's address, and a provider reads its own token
    (`<name>_api_key`) and no other's. The director refuses anything else and
    the gateway's chart offers no model of a provider that names another's
    property ([llms.md §5](../design/llms.md)).
19. **How a provider that is new to the platform gets its credential.**
    **Decided 2026-10-10**, and built: the Cluster composition declares the
    requirement `llm-provider-<name>` from the claim, with a fixed path and a
    field computed from the name. `credentials.yaml` lists no provider.
20. **Where a model's real health comes from.** **Decided 2026-10-10**: the
    console keeps flagging from the declared fact; a real health status is on
    the roadmap, item 2.31.
21. **Whether the Models tab also carries the gateway's console switch and
    GPU time slicing.** **Decided 2026-10-10**: where reasonable. The console
    switch is on the tab and in the director's route, with a warning. GPU
    time slicing is not: `gpuTimeSliceReplicas` is read by nothing, so a
    switch for it would change nothing (roadmap, item 2.28).
22. **What becomes of the grants the operator wrote, and whether an install
    grants anything.** **Decided 2026-10-10**: the grants an earlier operator
    wrote are left as they are, and nothing is done about them. An install
    grants nothing: an integration waits for the tenant's administrator. That
    may be adjusted later.
23. **How a cluster role is granted.** **Decided 2026-10-10** and built: the
    platform defines a fixed, named set in this repository, a profile asks by
    name, and the operator binds on the set, the allowlist and the grant
    (defect 24). The set is empty. Open with the first role: which roles are
    in it. Open now: a profile names the ServiceAccount the role is bound to,
    because the operator knows no other way to learn which account a chart
    runs its pods under; it is held to the component's own namespace and may
    not be `default`.
24. **Whether the Tenant keeps `spec.security.password`.** Defect 25: the
    field and the Composition's use of it are what is left of the commit
    path, and removing them changes the Tenant's schema.
25. **Where a backup key is made.** The administration console no longer
    offers to make one. The Operations Console shows the same choice and its
    backend serves neither the route that would make a key nor the one that
    reads or keeps the workspace's key, and no platform service makes one. A
    person makes a key with `age-keygen` and pastes the public half.
    **Decided 2026-10-10**: left as it is; nothing further is built.
26. **What a purge does with the kernel's volumes.** **Decided 2026-10-10**:
    the behaviour as built stands, and nothing is to change. A purge does
    not drain the claims of any kernel namespace before the reverse pass:
    the vault and the kernel's database stay up through it, and their claims
    go with their namespaces. The volumes are deleted in the pass after it
    (`purge_delete_volumes`, `scripts/lib/teardown.sh`), which takes the
    volumes of the kernel and system namespaces by name and every tenant's
    by prefix.

## 5. Known and deliberately not now

- **Moving a tenant to another cluster goes through export and import**, which
  re-mint what the platform issues. The alternative, in which the recovery kit
  is the cluster's identity and a new server that imports it becomes the old
  cluster, is not taken: it would bind a tenant's portability to its
  provider's kit.
- **The director reads the cluster's catalogue sources from the claim in
  git.** An edit made outside the director reaches it with the next commit it
  reads.
- **A private catalogue for a tenant's own administrator.** A catalogue in
  the deployments repository is the cluster administrator's to declare, also
  for one tenant; a tenant's administrator adds public addresses only, and
  no credential is sent to a catalogue.
- **A Cluster-claim setting for the mail proxy's load balancer** -- the PROXY
  header from a load balancer that is itself a proxy, the addresses it may
  come from, and the pinned mail address. They are values of the mail
  proxy's chart only. On the roadmap, item 2.25
  ([roadmap.md](../roadmap.md)).
- **Removing a deleted tenant's rights, memberships and shared-cache keys at
  deletion** (defect 9). On the roadmap, item 1.38.
- **The check at start of the rights store's defaults** (AD-12): wanted, and
  it reports, it does not repair. A store that has diverged is a question,
  because rewriting it would erase the grants and revocations that are
  nobody's default.

## 6. Carried over from the work packages

The work packages were a list of what the architecture cleanup had to build,
and the list is gone. This is what was still open on it and is recorded
nowhere else in this file or on the [roadmap](../roadmap.md). Each line was
checked against the code on 2026-10-10 for whether the thing exists, not for
whether it works. Where a plan specifies the item, the plan is named.

### The director and its callers

- **Tokens bound to their holder for callers that are not a browser** (DPoP,
  RFC 9449): the command line, an install asked for by a store, an agent.
  Nothing implements it.
- **One account that is both an administrator and a member** is not refused
  anywhere ([roles-and-authorizations.md](roles-and-authorizations.md) §1).
- **The operator still carries the install paths the director replaced**:
  `cmd/main.go` links `internal/applifecycle`. Removing them was the last
  step of the split ([operator-split-plan.md](operator-split-plan.md)).
- **A read for every write** on the director's API has not been checked
  route by route.

### Rights

- **The comparison of the rights store with Keycloak** — at start, after a
  failed event, and as a pass over every realm within fifteen minutes — and
  the alert when none has completed
  ([authorization-model.md](authorization-model.md) §2). The membership
  listener applies events; nothing re-reads. A lost *removal* leaves access
  in place until the person's next event.

### Network and publishing

- **Outgoing connections declared on the claims**
  (`Cluster.spec.network.egressAllow`, `Tenant.spec.network.egressAllow`
  and `denyKernel`, a tenant's list bounded by the cluster's): no such
  fields exist.
- **A filter for request content on a public entry that checks no caller**
  (a web application firewall on `authMode: none`): not built
  ([networking.md](networking.md)).
- **The publishing contract an app implements** (`exposure-policy`, and the
  cluster switch that would require it before an entry of `authMode: none`
  is enabled): not built ([target-component-structure.md](target-component-structure.md) §5.2).
- **A job that compares listeners, DMZ routes, DNS records and certificates
  with the approved entries**, and **the view of a public entry's traffic**
  (summary, condensed log, complete log): not built
  ([networking.md](networking.md) §8).
- **Proxy access logs with tokens hashed, in a log store with a retention
  the security officer sets**: waits on the log store
  ([security-gap-closing.md](security-gap-closing.md) G10).

### Catalogue

- **Admission rules for who may create which class of component in which
  tier of namespace**: not checked.
- **`credential.appliesWhen` on a `Repository` claim**, from which the
  credential requirements and the installer's own test of whether one
  applies would both be generated: no such field.
- **`AppPackage` is still a kind of the cluster**
  (`api/v1alpha1/apppackage_types.go`); the plan retired it with the
  catalogue that lived in the cluster.

### Installer and cluster

- **API-server audit logging.** The installer ships no audit policy. Until
  it does, what somebody does with a kubeconfig — break-glass above all —
  is recorded nowhere: it raises no Keycloak event and passes no service of
  the platform.
- **Kubernetes identities for the platform's roles**, with break-glass bound
  to `cluster-admin` by group, and **a second sign-in step for the acts
  that matter most**
  ([artefacts/roadmap-additions.md](artefacts/roadmap-additions.md)).
- **Tests that try to break each control** after a fresh install, as
  scripts.
- **A place in the deployments repository for the cluster's security
  policy and its exceptions** (`clusters/<c>/kernel/security/`), synced by
  an Application of its own.
- **The threat model run again against the code.**

### Export, import and purge

Specified by [sovereignty-concept.md](sovereignty-concept.md).

- **Offboarding as one act**: export to the tenant's key, hand over, purge,
  with the signed record of the deletion (roadmap item 5.5).
- **Canonical forms** of an app's data in a bundle (`spec.backup.canonical`
  on a profile): no such field.
- **`TenantExportSchedule` and `BackupPolicy` are still kinds of this
  repository** (`api/v1alpha1`); the concept has scheduling outside it, and
  [scheduled-backups-out.md](scheduled-backups-out.md) is the plan for the
  move.

### Not this repository's

The desktop and the administration console, and a store outside the
cluster, had work packages of their own. What was open there — the second
sign-in on the Cluster tile, the Identity tile behind
two hostnames, a store's confirmation of an install, the license shown per
entry — belongs with those programs and is not carried here.
