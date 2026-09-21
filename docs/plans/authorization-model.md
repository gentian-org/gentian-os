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
| R1a | **Ids contain neither `:` nor `#`** — OpenFGA rejects them. A Keycloak group keeps its name in Keycloak (`gentian:tenant:demo:admins`) and is the object `group:gentian/tenant/demo/admins`: `:` becomes `/`, nothing else changes. Reversible because tenant and profile names are DNS labels. One function does the mapping, in the director; nothing else constructs a group id. | tuples the server refuses — found by running the tests, not by reading |
| R2 | **Roles are nouns, assigned only through `group#member`** — one Keycloak group per role, never `[user]` directly. | tuples that name people; a role that can be held without the IdP knowing |
| R3 | **Permissions are `can_<verb>`**, one per verb an enforcement point exposes, computed from roles. **PEPs check permissions, never roles.** | a PEP encoding "admin may…" in code; a new verb without a relation |
| R4 | **One parent relation per type** (`cluster`, `tenant`); derivation is `<relation> from <parent>`, never a copied tuple. | authority granted sideways (principle 5) |
| R5 | **`but not` only for least-privilege invariants** (`can_use: member … but not admin`). | exclusion logic scattered through permissions |
| R6 | **Time is a condition** (`with grant_valid`), never a field a caller compares. | expiry checks that some caller forgets |
| R7 | **Membership is a stored projection of Keycloak; contextual tuples carry runtime facts only.** `group:<g>#member@user:<sub>` is written by the director from Keycloak's event stream and reconciled toward Keycloak with a read-only client; it is never edited in place. Everything else — role-to-group assignments, tenant→cluster, app→tenant, entitlements — is a stored tuple written by the director from CRs (AD-12). Contextual tuples are for a task's TTL, `acting_for`, device posture. | a polling bridge with an admin credential; a second place membership can be changed; groups in every token |
| R8 | **Every relation ships with three tests**: the grant, the denial for the neighbouring role, the derivation through the parent. | a relation nobody exercised |
| R9 | The **director is the store's only writer**; the store is a projection of git and is rebuilt from it on start. The operator reads. | two writers; a store that cannot be reconstructed |

## 2. What R7 requires, and what it buys

R7 is OpenFGA's own hybrid guidance and the Zanzibar shape: persistent
facts stored, runtime facts contextual. Three invariants keep the
projection honest, and security principle 2 intact:

- **Only Keycloak's events write membership**, through the director. No
  admin API, no console, no hand edit ever creates a `group#member` tuple.
  The feed is Keycloak's event listener SPI pushing signed events; the
  reconcile uses a client with `view-users` only.
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
  | Shim evicts its cached decision (`ReadChanges` poll) | 10 seconds |
  | **Worst case for an authorization change to take effect** | **~16 minutes** |
  | Projection declared stale → alert | no sweep completed in 30 minutes |

  The director records the last accepted event and the last completed sweep;
  those two timestamps are the projection's freshness and are what the alert
  watches. A stale projection does **not** fail checks closed: an identity
  provider hiccup should not become a platform outage, and the sweep will
  correct it. It fails loudly instead.

  Fifteen minutes is tolerable only because it bounds *authorization*
  changes, not lockout. Shutting someone out does not wait for the
  projection: revoking their Keycloak sessions ends the edge session — the
  director records `session:<sid>#revoked` and the shim denies it within one
  poll (networking.md §4) — and
  disabling the user stops new tokens at the issuer. Those are immediate and
  independent of any tuple. The bound covers "this person should no longer
  reach that app", not "this person should be out".

What it buys over a contextual-only design: reverse queries and access
reviews are native (`ListUsers("who can use app A")` is complete, which
roadmap 1.12's SOC 2 evidence needs); checks are indexed lookups with no
per-request tuple cost and no 100-tuple cap; OpenFGA's check cache works;
and no group has to travel in the edge token, so the session cookie stays
small. What it costs: an event path and a reconcile job in the director,
and a projection that lags Keycloak by the event latency — sub-second on the
normal path, with the sweep as the backstop and ~16 minutes as the published
worst case for an authorization change (§2).

## 3. Object naming and who writes each tuple

| Tuple | Written by | When |
|---|---|---|
| `cluster:<c>#<role>@group:<g>#member` | director | from the Cluster claim's role assignments, on commit |
| `tenant:<t>#cluster@cluster:<c>` | director | tenant deploy |
| `tenant:<t>#<role>@group:gentian/tenant/<t>/<g>#member` | director | tenant deploy (groups are conventional per tenant) |
| `tenant:<t>#operated_by@cluster:<c>` | director | tenant deploy, and always for `tenant:platform`. **Consent, not structure**: it is what lets `admin from cluster` reach into the tenant — user management and secret writes included. A tenant that administers itself has it removed (`can_configure` on the cluster, at the tenant's request) and loses nothing else: `cluster` stays, so audit and cluster-scope approval still derive |
| `session:<sid>#revoked@user:<sub>` | director | a zone client's back-channel logout arrives; removed when that session's longest token has expired. What lets several shim replicas enforce one logout without state of their own |
| *(no bootstrap tuple for the platform tenant)* | — | `tenant#admin` derives `or admin from operated_by`, so a platform administrator is an administrator of `tenant:platform` — and of any tenant that has not withdrawn the consent — through the chain. Writing the platform group into a tenant relation would be the copied tuple R4 forbids, and a tuple somebody has to remember to remove |
| `tenant:<t>#perimeter_approver@group:gentian/tenant/<t>/admins#member` | director | tenant deploy — the default: publishing is its own grant, but most tenants do not staff the role separately. A tenant that wants the separation removes this tuple and adds its own `:perimeter` group |
| `app:<t>/<p>#tenant@tenant:<t>` | director | app install |
| `app:<t>/<p>#admin@group:gentian/tenant/<t>/app/<p>/admins#member` | director | app install, **only when the profile declares a `privilegedRole`** — no internal admin role, no group to create. Per app: one cross-app group made every app administrator an administrator of every other |
| `app:<t>/<p>#entitled@group:…/app/<p>#member`, **or** one per activated addon | director | app install. The groups already exist (`internal/keycloak/groups.go`) and the rule already runs in the portal; the tuple is what lets a second PEP apply it. Which groups entitle follows the portal exactly: a base with activated addons is entitled by *those* groups, not its own, since a base is entered for the addons inside it. The `gentianDefaultGrant` attribute stays a Keycloak concern — it decides whether the console pre-selects the group when adding a user, not who may enter |
| `contract:<t>/<name>#provider@app:<t>/<p>` and `#consumer@app:<t>/<q>` | director | on the commit that creates the `AppGrant`. **Deleting the consumer tuple does not revoke access today**: no enforcement point sits on an app-to-app call, and the credential is already in OpenBao and injected into the consumer's values. Revoking means the director deletes the binding's OpenBao path and re-rolls the consumer. That stays true until workloads carry identity (G8) |
| `shared_instance:<p>#offered_to@tenant:<t>` | director | the platform administrator makes a shared instance available to a tenant (`can_grant_shared`). The tenant still sees nothing until its own administrator installs it |
| `catalogue_entry:<catalogue>/<app>#entitled@tenant:<t>` with `expires_at` | director | signed grant received (ui-restructure §3) |
| the same tuple, **deleted** | director | signed revocation received — same endpoint, same signature check, committed as a fact; a later commit overrides an earlier `expires_at` (operator-split-plan §3.8) |
| `group:<g>#member@user:<sub>` | director, from Keycloak's event stream; reconciled with a read-only client | on each membership event; reconcile on an interval and on start |

## 4. What each enforcement point asks

| PEP | Object | Relations |
|---|---|---|
| Gateway ext-auth shim | `tenant:<t>` for the desktop host; `app:<t>/<p>` for an app host; `cluster:<c>` for a kernel tool host; `session:<sid>` on every miss | `can_enter`; `can_use`; `can_configure`, `can_audit`; `revoked` (deny if true) |
| — | — | *The gateway and the desktop resolve the **same** relation on the **same** object: `can_use`, and `can_launch` which derives from it. One rule, two enforcement points, no second implementation to drift. The portal stops computing entitlement in Python when the director answers it; keeping both is how the tile list and the route come to disagree.* |
| *(none yet)* | `contract:<t>/<name>` | `can_consume` — the relation exists so the vocabulary is complete and the director can bound what it binds; there is no east-west PEP to ask it until G8 |
| Credential manager | `app:<t>/<p>` for a component's secrets; `cluster:<c>` for kernel and system ones | `can_write_credential`; `can_configure` |
| Director, tenant verbs | `tenant:<t>` | `can_install_app`, `can_set_plan`, `can_set_policy`, `can_grant`, `can_manage_users`, `can_expose`, `can_approve_privilege`, `can_view` |
| Director, install | `catalogue_entry:<cat>/<app>` with user `tenant:<t>` | `can_install` |
| Director, cluster verbs | `cluster:<c>` | `can_configure`, `can_deploy_tenant`, `can_operate_system`, `can_install_shared`, `can_grant_shared`, `can_approve`, `can_set_admission`, `can_edit_raw`, `can_audit` |
| Desktop tiles (via the director's read API) | `app:<t>/<p>` per installed app; `tenant:<t>` for admin tiles | `can_launch`; `can_administer` |
| MCP gateway (wave 2) | `agent:<id>` | `can_act` |

## 5. Keeping the plans honest

`make verify-authz-vocabulary` (to add with the model): extract every
`can_[a-z_]+` and every role noun from `docs/plans/*.md` and fail on any
that `model.fga` does not define. The same check runs the other way for
relations the model defines and no document uses.
