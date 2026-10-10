# Authorization model

One vocabulary for every plan. The model itself is
[authz/model/v1/model.fga](../../authz/model/v1/model.fga) — the file `fga model validate`
and `fga model test` run against, and the file every relation named in
[roles-and-authorizations.md](roles-and-authorizations.md),
[operator-split-plan.md](operator-split-plan.md),
[networking.md](networking.md) and [ui-restructure.md](ui-restructure.md)
must appear in. A relation that is not in the file does not exist.

## 1. Rules

These follow OpenFGA's own modelling guidance and the conventions of the
Zanzibar family; each rule names what it prevents.

| # | Rule | Prevents |
|---|---|---|
| R1 | **One type per CRD kind**; object id = the CR's name, namespaced kinds as `<tenant>/<name>` (`app:demo/nextcloud`). | a second naming scheme that has to be mapped |
| R1a | **Ids contain neither `:` nor `#`** — OpenFGA rejects them. A Keycloak group keeps its name in Keycloak (`gentian:tenant:demo:admins`) and is the object `group:gentian/tenant/demo/admins`: `:` becomes `/`, nothing else changes. Reversible because tenant and profile names are DNS labels. One function does the mapping (`internal/director/authz`, which the operator's projection imports); nothing else constructs a group id. | tuples the server refuses — found by running the tests, not by reading |
| R2 | **Roles are nouns, assigned only through `group#member`** — one Keycloak group per role, never `[user]` directly. | tuples that name people; a role that can be held without the IdP knowing |
| R3 | **Permissions are `can_<verb>`**, one per verb an enforcement point exposes, computed from roles. **PEPs check permissions, never roles.** | a PEP encoding "admin may…" in code; a new verb without a relation |
| R4 | **One parent relation per type** (`cluster`, `tenant`); derivation is `<relation> from <parent>`, never a copied tuple. | authority granted sideways (principle 5) |
| R5 | **`but not` only for least-privilege invariants** (`can_use: member … but not admin`). | exclusion logic scattered through permissions |
| R6 | **Time is a condition** on the tuple, never a field a caller compares. The model carries none today: the first is the task TTL that arrives with agents (wave 2). | expiry checks that some caller forgets |
| R7 | **Membership is a stored projection of Keycloak; contextual tuples carry runtime facts only.** `group:<g>#member@user:<sub>` is written by the operator from the signed statements Keycloak's event listener sends it, and reconciled toward Keycloak with a read-only client; it is never edited in place. Everything else — role-to-group assignments, tenant→cluster, app→tenant — is a stored tuple the operator projects from what git declares, once Argo CD has applied it (AD-12). Contextual tuples are for a task's TTL, `acting_for`, device posture. **Changed 2026-10-09**: this rule named the director as the writer; AD-12 names the operator, which is what was built. The reconcile toward Keycloak is not built ([open-items.md](open-items.md)). | a polling bridge with an admin credential; a second place membership can be changed; groups in every token |
| R8 | **Every relation ships with three tests**: the grant, the denial for the neighbouring role, the derivation through the parent. | a relation nobody exercised |
| R9 | **The operator projects; the director asks.** The operator writes the store, from git and from Keycloak's events; the director asks it before every call it serves and writes to it only what one of its own routes is itself responsible for recording, which today is nothing (AD-12). **Changed 2026-10-09** from "the director is the store's only writer; the operator reads". Git holds the **defaults** — the conventional tuples a tenant starts with, which follow from what git declares and can therefore be derived and checked. Everything beyond them is an **action through the director**, recorded when it happened and derivable from nothing else. So the store is not rebuilt on start: the director is to check that the defaults still match and report drift, because rewriting would erase the acts that are not derivable. That check is not built. | two writers; a store whose defaults have silently drifted |

## 2. What R7 requires, and what it buys

R7 is OpenFGA's own hybrid guidance and the Zanzibar shape: persistent
facts stored, runtime facts contextual. Three invariants keep the
projection honest, and security principle 2 intact:

- **Only Keycloak's events write membership**, through the operator
  (**changed 2026-10-09** from "through the director", with AD-12). No
  admin API, no console, no hand edit ever creates a `group#member` tuple.
  The feed is Keycloak's event listener SPI pushing signed events; the
  reconcile is to use a client with `view-users` only, and is not built.
- **Reconciliation corrects toward Keycloak, never away from it.** Drift is
  bounded and reported, not trusted.
- **Contextual tuples never carry a person's memberships.** A request may
  add a task's TTL, an `acting_for`, a device posture — facts that exist
  only for that request.
- **Staleness is bounded, and the bound is published.** The event path is
  sub-second and that is the normal case. What needs a number is the failure
  case, because a lost event is not symmetric: a dropped *addition* fails
  closed and someone complains, a dropped *removal* leaves the tuple in place
  and access quietly persists.

  | | Bound |
  | --- | --- |
  | Event applied, normal path | sub-second |
  | Failed event → targeted re-read of that subject | seconds |
  | Rolling sweep, every realm, cluster complete | **15 minutes** |
  | Bouncer evicts its cached decision (`ReadChanges` poll) | 10 seconds |
  | **Worst case for an authorization change to take effect** | **~16 minutes** |
  | Projection declared stale → alert | no sweep completed in 30 minutes |

  The writer is to record the last accepted event and the last completed
  sweep; those two timestamps are the projection's freshness and are what the
  alert watches. **Not built**: there is no sweep and no targeted re-read, so
  the rows of the table from the second on are the design's, and a statement
  that never arrives is repaired only by the next one about the same person. A stale projection does **not** fail checks closed: an identity
  provider hiccup should not become a platform outage, and the sweep will
  correct it. It fails loudly instead.

  Fifteen minutes is tolerable only because it bounds *authorization*
  changes, not lockout. Shutting someone out does not wait for the
  projection: revoking their Keycloak sessions ends the edge session — the
  design has `session:<sid>#revoked` recorded and the bouncer denying it within
  one poll (networking.md §4); no code writes that tuple today — and
  disabling the user stops new tokens at the issuer. Those are immediate and
  independent of any tuple. The bound covers "this person should no longer
  reach that app", not "this person should be out".

What it buys over a contextual-only design: reverse queries and access
reviews are native (`ListUsers("who can use app A")` is complete, which
roadmap 1.12's SOC 2 evidence needs); checks are indexed lookups with no
per-request tuple cost and no 100-tuple cap; OpenFGA's check cache works;
and no group has to travel in the edge token, so the session cookie stays
small. What it costs: an event path and a reconcile job in the operator,
and a projection that lags Keycloak by the event latency — sub-second on the
normal path, with the sweep as the backstop and ~16 minutes as the published
worst case for an authorization change (§2).

## 3. Object naming and who writes each tuple

**Changed 2026-10-09**: the "Written by" column named the director in every
row but two. AD-12 names the operator as the one that writes the store, and
that is what was built: the structure rows are projected from what git
declares once Argo CD has applied it (`authz_projection_reconciler.go`), the
membership row from Keycloak's signed statements (`membership_listener.go`).
The "When" column still names the act that leads to the tuple; the director
makes the commit, and the tuple follows when the operator has seen the
result. Rows no code writes say so.

| Tuple | Written by | When |
|---|---|---|
| `cluster:<c>#<role>@group:<g>#member` | operator | from the Cluster claim's role assignments, once the claim is applied |
| `tenant:<t>#cluster@cluster:<c>` | operator | tenant deploy |
| `tenant:<t>#<role>@group:gentian/tenant/<t>/<g>#member` | operator | tenant deploy (groups are conventional per tenant) |
| `tenant:<t>#operated_by@cluster:<c>` | operator | tenant deploy, and always for `tenant:platform`. **Consent, not structure**: it is what lets `admin from cluster` reach into the tenant — user management and secret writes included. A tenant that administers itself has it removed (`can_configure` on the cluster, at the tenant's request) and loses nothing else: `cluster` stays, so audit and cluster-scope approval still derive |
| `session:<sid>#revoked@user:<sub>` | nobody yet (not built) | a zone client's back-channel logout arrives; removed when that session's longest token has expired. What lets several bouncer replicas enforce one logout without state of their own |
| *(no bootstrap tuple for the platform tenant)* | — | `tenant#admin` derives `or admin from operated_by`, so a platform administrator is an administrator of `tenant:platform` — and of any tenant that has not withdrawn the consent — through the chain. Writing the platform group into a tenant relation would be the copied tuple R4 forbids, and a tuple somebody has to remember to remove |
| `tenant:<t>#perimeter_approver@group:gentian/tenant/<t>/perimeter#member` | operator | tenant deploy, with the other role tuples. The group exists from the day the tenant does (the tenant's composition creates it, empty). Publishing is its own grant: `can_expose` is `perimeter_approver or admin from operated_by`, so beside the group's members the cluster's administrator approves in every tenant its cluster operates, through the consent tuple and with no tuple of its own — and not in a tenant that withdrew it |
| `tenant:<t>#perimeter_approver@group:gentian/tenant/<t>/admins#member` | operator | **only while the tenant's manifest says `spec.perimeter.adminsApprove: true`**, a switch the cluster's administrator alone sets (director: at tenant creation, and `PUT`/`DELETE /v1/clusters/{c}/tenants/{t}/perimeter-delegation`, `can_configure`). Off by default. The projection writes the tuple when the manifest says so and deletes it when it does not — including one written by hand — so git holds the answer (AD-12); it is not carried in a bundle, and never written for `tenant:platform`. **Changed 2026-10-09** from "written at tenant deploy — the default": that default was never built, and the owner decided the cluster's administrator approves and a tenant's administrators only by this switch (AD-6) |
| `app:<t>/<p>#tenant@tenant:<t>` | operator | app install |
| `app:<t>/<p>#admin@group:gentian/tenant/<t>/app/<p>/admins#member` | nobody yet (not built) | app install, **only when the profile declares a `privilegedRole`** — no internal admin role, no group to create. Per app: one cross-app group made every app administrator an administrator of every other |
| `app:<t>/<p>#entitled@group:…/app/<p>#member`, **or** one per activated addon | operator | app install. The groups already exist (`internal/keycloak/groups.go`) and the rule was the former portal's; the tuple is what lets every enforcement point apply it. Which groups entitle follows that rule exactly: a base with activated addons is entitled by *those* groups, not its own, since a base is entered for the addons inside it. The `gentianDefaultGrant` attribute stays a Keycloak concern — it decides whether the console pre-selects the group when adding a user, not who may enter |
| `contract:<t>/<name>#provider@app:<t>/<p>` and `#consumer@app:<t>/<q>` | nobody yet (not built: `internal/authz/grants.go` builds these tuples and nothing calls it) | on the commit that creates the `AppGrant`. **Changed 2026-10-10**: that commit is the only thing that creates or changes a grant. The operator wrote one for every declared integration and overwrote a narrower one; it now writes none, and what an administrator granted outranks what a profile declares. **Deleting the consumer tuple does not revoke access today**: no enforcement point sits on an app-to-app call, and the credential is already in OpenBao and injected into the consumer's values. Revoking means the director deletes the binding's OpenBao path and re-rolls the consumer. That stays true until workloads carry identity (G8) |
| `shared_instance:<p>#offered_to@tenant:<t>` | nobody yet (not built) | the platform administrator makes a shared instance available to a tenant (`can_grant_shared`). The tenant still sees nothing until its own administrator installs it, and that install is where the director checks `can_bind` for the tenant |
| `group:<g>#member@user:<sub>` | operator, from the signed statements of Keycloak's event listener | on each membership event. The reconcile on an interval and on start, with a read-only client, is not built |

## 4. What each enforcement point asks

| PEP | Object | Relations |
|---|---|---|
| Gateway ext-auth bouncer | `tenant:<t>` for the desktop host; `app:<t>/<p>` for an app host; `cluster:<c>` for a kernel tool host; `session:<sid>` on every miss | `can_enter`; `can_use`; `can_configure`, `can_audit`; `revoked` (deny if true) |
| — | — | *The gateway and the desktop resolve the **same** relation on the **same** object: `can_use`, and `can_launch` which derives from it. One rule, two enforcement points, no second implementation to drift. The tile list is the usher's answer to the same question; a second computation beside it is how the tile list and the route would come to disagree.* |
| *(none yet)* | `contract:<t>/<name>` | `can_consume` — the relation exists so the vocabulary is complete and the director can bound what it binds; there is no east-west PEP to ask it until G8 |
| Custodian | `cluster:<c>` for kernel and system credentials; `tenant:<t>` for a tenant's; `app:<t>/<p>` for a component's own, which follow its tenant's | `can_read_credential` to see that one is required, whether it is set and by whom -- never its value; `can_write_credential` to set it |
| Registrar | `tenant:<t>` for a tenant's people, groups and realm settings; `cluster:<c>` for handing a tenant's administrator account over and for the count of accounts | `can_manage_users`; `can_set_policy` for the realm's password policy (**changed 2026-10-10**: the registrar's action is the one way to set it; the director's commit of the tenant's security policy no longer takes a password block); `can_configure`; `can_audit`. Whatever the store answers, it refuses a change to a group the Cluster claim names for a platform role (`spec.platformRoles`, every field) or to a person in one ([operator-split-plan.md](operator-split-plan.md) §4.4) |
| Director, tenant verbs | `tenant:<t>` | `can_install_app`, `can_set_plan`, `can_set_policy`, `can_grant`, `can_expose`, `can_approve_privilege`, `can_view` |
| Director, install | `tenant:<t>` | `can_install_app`, and nothing else. Nothing is asked about the app or the catalogue it comes from: whether the tenant may have it is decided where its artefacts are pulled, by the credential the tenant holds for their repository (AD-3) |
| Director, catalogues | `cluster:<c>` to add or remove a catalogue for every tenant or for one tenant, and to set whether a tenant's own administrators may add theirs; `tenant:<t>` for a tenant's administrator adding or removing one for their own tenant | `can_configure` on the cluster, `can_audit` to read what it declares. `can_install_app` on the tenant, and not the whole check: the director refuses unless the cluster's administrator delegated it to that tenant, and removes only what the tenant added. `can_view` to read what a tenant installs from. The delegation has no route under a tenant, so no relation on a tenant reaches it |
| Director, cluster verbs | `cluster:<c>` | `can_configure`, `can_deploy_tenant`, `can_operate_system`, `can_install_shared`, `can_grant_shared`, `can_approve`, `can_set_admission`, `can_edit_raw`, `can_audit`. **Changed 2026-10-10**: a pod-security waiver takes effect only where `can_set_admission` put it on the cluster's allowlist and `can_approve` granted it on the install; the allowlist alone no longer waives anything. **Changed 2026-10-10**, second note: a cluster role is asked for by name from a set the platform defines in this repository, and is bound to the component's ServiceAccount only where the set has it, `can_set_admission` permitted it for the profile (`allowedClusterRoles`) and `can_approve` granted it on the install. A profile's own rules are never made into a role. The set is empty |
| Desktop tiles (via the director's read API) | `app:<t>/<p>` per installed app; `tenant:<t>` for admin tiles | `can_launch`; `can_administer` |
| MCP gateway (wave 2) | `agent:<id>` | `can_act` |

## 5. Keeping the plans honest

`make verify-authz-vocabulary` (to add with the model): extract every
`can_[a-z_]+` and every role noun from `docs/plans/*.md` and fail on any
that `model.fga` does not define. The same check runs the other way for
relations the model defines and no document uses.
