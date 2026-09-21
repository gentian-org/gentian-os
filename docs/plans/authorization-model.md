# Authorization model

One vocabulary for every plan. The model itself is
[artefacts/model.fga](artefacts/model.fga) — the file `fga model validate`
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
| R2 | **Roles are nouns, assigned only through `group#member`** — one Keycloak group per role, never `[user]` directly. | tuples that name people; a role that can be held without the IdP knowing |
| R3 | **Permissions are `can_<verb>`**, one per verb an enforcement point exposes, computed from roles. **PEPs check permissions, never roles.** | a PEP encoding "admin may…" in code; a new verb without a relation |
| R4 | **One parent relation per type** (`cluster`, `tenant`); derivation is `<relation> from <parent>`, never a copied tuple. | authority granted sideways (principle 5) |
| R5 | **`but not` only for least-privilege invariants** (`can_use: member … but not admin`). | exclusion logic scattered through permissions |
| R6 | **Time is a condition** (`with grant_valid`), never a field a caller compares. | expiry checks that some caller forgets |
| R7 | **Membership is contextual, structure is stored.** A request carries `group:<g>#member@user:<sub>` for the token's `gentian:` groups of that realm; everything else — role-to-group assignments, tenant→cluster, app→tenant, entitlements — is a stored tuple written by the director (AD-12). | a bridge; stale membership |
| R8 | **Every relation ships with three tests**: the grant, the denial for the neighbouring role, the derivation through the parent. | a relation nobody exercised |
| R9 | The **director is the store's only writer**; the store is a projection of git and is rebuilt from it on start. The operator reads. | two writers; a store that cannot be reconstructed |

## 2. Limits R7 imposes, and where they bite

- OpenFGA caps contextual tuples at **100 per request**. A token with more
  than ~100 `gentian:` groups cannot be checked; the edge client's scope
  therefore emits only the groups of the realm it serves, and the shim
  resolves groups by `sub` if a token still exceeds the cap
  (networking.md §6).
- **Reverse queries need a join.** `ListUsers("who can use app A")` answers
  only from stored tuples plus the contextual ones in the request — it
  cannot enumerate people it never stored. An access review is therefore
  OpenFGA (which groups → which role) joined with Keycloak (who is in those
  groups). SOC 2 evidence (roadmap 1.12) is produced by that join, and the
  auditor role has read access to both.
- OpenFGA's own guidance says relying solely on contextual tuples forgoes
  Zanzibar's main benefit (no lookups at check time) and names a hybrid —
  stored memberships for persistent facts, contextual for runtime ones —
  as the usual shape. R7 is a deliberate trade: no sync, no staleness, no
  bridge holding a Keycloak admin credential; the cost is the join above and
  the cap. If `ListUsers` over people ever becomes a product need, the
  change is to store `group#member` tuples fed by Keycloak events (not
  polling) — a change to R7 only, not to the model.

## 3. Object naming and who writes each tuple

| Tuple | Written by | When |
|---|---|---|
| `cluster:<c>#<role>@group:<g>#member` | director | from the Cluster claim's role assignments, on commit |
| `tenant:<t>#cluster@cluster:<c>` | director | tenant deploy |
| `tenant:<t>#<role>@group:gentian:tenant:<t>:<g>#member` | director | tenant deploy (groups are conventional per tenant) |
| `tenant:platform#admin@group:gentian:platform:admin#member` | director | bootstrap — the platform tenant's admins are the platform admins (AD-10) |
| `app:<t>/<p>#tenant@tenant:<t>` | director | app install |
| `app:<t>/<p>#admin@group:gentian:tenant:<t>:app-admins#member` | director | app install |
| `catalogue_entry:<catalogue>/<app>#entitled@tenant:<t>` with `expires_at` | director | signed grant received (ui-restructure §3) |
| `group:<g>#member@user:<sub>` | **nobody** — contextual, per request | — |

## 4. What each enforcement point asks

| PEP | Object | Relations |
|---|---|---|
| Gateway ext-auth shim | `tenant:<t>` for the desktop host; `app:<t>/<p>` for an app host | `can_enter`; `can_use` |
| Director, tenant verbs | `tenant:<t>` | `can_install_app`, `can_set_plan`, `can_set_policy`, `can_grant`, `can_manage_users`, `can_expose`, `can_view` |
| Director, install | `catalogue_entry:<cat>/<app>` with user `tenant:<t>` | `can_install` |
| Director, cluster verbs | `cluster:<c>` | `can_configure`, `can_deploy_tenant`, `can_operate_system`, `can_install_shared`, `can_grant_shared`, `can_approve`, `can_set_admission`, `can_edit_raw`, `can_audit` |
| Desktop tiles (via the director's read API) | `app:<t>/<p>` per installed app | `can_launch` |
| MCP gateway (wave 2) | `agent:<id>` | `can_act` |

## 5. Keeping the plans honest

`make verify-authz-vocabulary` (to add with the model): extract every
`can_[a-z_]+` and every role noun from `docs/plans/*.md` and fail on any
that `model.fga` does not define. The same check runs the other way for
relations the model defines and no document uses.
