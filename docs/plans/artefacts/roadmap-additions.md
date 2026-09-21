# Roadmap additions

Items the architecture cleanup creates or changes, to be merged into
[../../roadmap.md](../../roadmap.md) when the cleanup lands. Each names the
existing item it extends or supersedes.

## Extends 1.9 — Mirror profile bundles, not only charts

Materialise-on-reference (AD-3) fetches a component's profile bundle from
its catalogue source at install time. An air-gapped or mirrored cluster
therefore needs the bundles mirrored alongside the charts and images 1.9
already covers, at the same digests, and the store row's `source` must be
able to point at the mirror. For a private catalogue the mirror also has
to honour the grant-carried fetch token (ui-restructure §3) or be inside
the trust boundary that makes the token unnecessary.

- `[ ]` Extend the mirror target of 1.9 to profile bundles, keyed by
  `profile_digest`.
- `[ ]` Let a `Repository` claim of `role: apps` name a mirror as the
  catalogue source, so the director resolves the digest locally.
- `[ ]` Add the bundle digest to `make lint-image-digests`' scope.

## Supersedes the GPG half of G11 — Argo CD `sourceIntegrity`

Argo CD has deprecated `AppProject.spec.signatureKeys`; the replacement is
`AppProject.spec.sourceIntegrity.git.policies[]` with `gpg.mode:
head|strict` and per-repository key lists (multi-source Applications get
one policy per source). Its limits, verified against the current docs:
git sources only (Helm/OCI sources are not verified — the deployments
repository is git, so this holds); an ApplicationSet whose `project` field
is templated is not verified (ours are literal `project: gentian`); `argocd
app sync --local` is disabled once enforced; GnuPG only, no SSH or sigstore
signatures. `head` verifies the target revision's commit or annotated tag;
`strict` verifies the whole ancestry, which the bootstrap commits made by
humans would fail — so the mode is `head`.

- `[ ]` Write the policy for `gentian-deployments` with the director's key
  and the break-glass key, mode `head`.
- `[ ]` Hold the director's signing key in OpenBao transit and sign through
  it, so the key never sits in the director's memory next to the push
  credential.
- `[ ]` Test: an unsigned commit on `main` does not sync; a commit signed
  with a retired key does not sync.

## Extends 2.12 — Scale-to-zero is a dependency of the per-tenant desktop

AD-10 gives every tenant its own desktop BFF pod and every enabled
perimeter surface its own proxy pod. At hundreds of tenants these are the
largest pod population on the cluster. 2.12 (auto-sleep) stops being an
optimisation and becomes what makes the per-tenant model affordable; the
desktop is the first workload it should cover.

## New — Kubernetes identities for platform roles

The plans give humans no Kubernetes identity, correctly for tenants. For
platform roles two tools need one: Headlamp (2.23) and `kubectl` for
break-glass. The standard answer is the API server's structured
authentication configuration (`AuthenticationConfiguration`, Kubernetes
1.30+): Keycloak's kernel realm as a JWT authenticator, CEL claim mappings
turning `gentian:platform:*` groups into Kubernetes groups, RBAC bindings
per role. Where the control plane is managed and its flags are not ours,
Pinniped (CNCF) provides the same through an impersonating proxy and a
supervisor. Either way tenants get nothing.

- `[ ]` Decide per cluster type: structured auth config or Pinniped.
- `[ ]` Bind `gentian:platform:break-glass` to `cluster-admin` and
  `gentian:platform:auditor` to a read-only ClusterRole; nothing else.
- `[ ]` Retire the pasted ServiceAccount token in 2.23.

## New — Step-up authentication for high-impact permissions

The principles name ABAC "conditioning"; no plan supplies a mechanism.
`can_deploy_tenant`, `can_set_admission`, `can_edit_raw` and
`can_operate_system` should require a fresh authentication (Keycloak
step-up with `acr`, or a re-login within N minutes) checked by the
director from the token's `auth_time`/`acr` claims.

## New — Reverse authorization queries

Membership as contextual tuples (AD-12, R7) means "who can use app A" is a
join of OpenFGA structure with Keycloak groups. If access reviews or a
"members with access" screen become a product need, store `group#member`
tuples fed by Keycloak admin events — a change to R7 only.

## Decided — realm per tenant stays; Organizations only inside a tenant

Keycloak Organizations (26+) are built for the B2B shape — one realm, each
organization a customer with its own IdP federation and email-domain
discovery — and they buy cross-tenant SSO by making the tenant boundary a
membership check inside one directory: one user table, one client list,
one password and MFA policy, one admin scope, one event stream. That is the
shared-app guarantee (AD-4) applied to identity, and the one place the
taxonomy's promise — a tenant compromise stops at the tenant — would
silently weaken. Realm per tenant is kept, for three reasons, in this order:

1. **Sovereignty.** A tenant's realm is *the tenant's* identity: its users,
   credentials, policies, flows and federation are one object that belongs
   to it and to no one else. Under Organizations they are rows in a
   directory the provider owns and every other customer shares. A customer
   who must be able to answer "where is our identity data and who else is
   in that store" gets a clean answer only from a realm.
2. **Portability.** Keycloak exports per realm — users with password hashes
   and MFA credentials, groups, roles, clients, flows, identity providers —
   and nothing that belongs to anyone else. `TenantExport` already captures
   the realm as one artefact (`identity/realm.tar.gz`,
   `tenantexport_controller.go`). Migrating a tenant to another provider is
   therefore an export and an import. Under Organizations there is no
   per-organization export: migration is a filter over the whole customer
   base to extract one customer — reconstructing memberships, picking the
   tenant's clients out of a realm-wide list, resolving users who belong to
   two organizations, and risking another customer's users in the file. A
   multi-customer platform should never have to perform that operation.
3. **Isolation.** A realm is a wall; an organization is a policy.

Not a flag. A single-realm mode would replace "which realm" with "which
organization claim" in the identity reconciler, `app-default`'s OIDC
clients, the OpenBao JWT auth roles (bound to an issuer), Dovecot's
per-realm passdbs, the portal's host→realm login and the gateway's per-zone
session (networking.md L1) — six components with two code paths and a
doubled identity test matrix. It is a second deployment profile, to be
considered only if realm count becomes Keycloak's scaling limit.

Organizations do have a place: **inside a tenant's realm**, for a customer
with subsidiaries or departments. That is a per-tenant feature the tenant
admin turns on and it branches nothing in the platform. Cross-customer
collaboration stays federation or public links (networking.md §5).

- `[ ]` Restore on another cluster *is* the migration: `TenantRestore` must
  accept a bundle from a different kernel domain and rewrite the
  issuer-bound references (app clients, the OpenBao role, the Dovecot
  passdb, the edge client) rather than assume them unchanged. Test: export
  on cluster A, restore on B, first login succeeds with the old password
  and the old second factor.
- `[ ]` Entitlements move with the tenant: a signed grant names
  `(tenant, app)` for one cluster; the migration procedure asks the store
  to re-issue for the new one.
- `[ ]` Offer Organizations within a tenant realm as a tenant-level
  setting, with the tenant admin as organization admin.
- `[ ]` State portability as a product property where the isolation model
  is described (design/multi-tenancy.md): a tenant can leave with its
  users, their passwords and their MFA.
