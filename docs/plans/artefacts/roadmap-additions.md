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

## Done in the plans — Reverse authorization queries

This asked for stored `group#member` tuples so "who can use app A" is a
native query. R7 now requires exactly that, and `app#entitled` makes the
answer the app's own group rather than tenant membership. Kept as a record
of why the shape was chosen.

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

## New — Separation of duties inside the director

The director verifies the token, makes the OpenFGA check, writes the tuples
that check reads, signs the commit, holds a Keycloak identity that can manage
users, and drives the credential manager. Decision point, tuple writer and
enforcement point are one process, so a compromised director can grant itself
a relation and then sign a commit Argo applies — cluster-wide execution
through provider-helm. AD-2 makes git the audited source, and AD-12 has the
store rebuilt from git on start, but between restarts the store is
authoritative and nothing compares the two.

- `[ ]` Reconcile store against git **continuously**, not only on start;
  divergence is an alert, not a silent correction.
- `[ ]` Role-assignment tuples derive from git content Argo has applied,
  so authority always trails a reviewable artefact rather than being written
  at request time.
- `[ ]` Consider a second signer for the commits that change authorization
  itself (role assignments, `PlatformSecurityPolicy`), so the director alone
  cannot widen its own powers.

## New — Audit integrity against the holder of break-glass

Break-glass holds kubeconfig and, by the WP-10 decision, the director's
signing key material, so it can act on the cluster and produce commits
attributed to the director. The stated mitigations are custody and audit —
but an audit log in a store the same holder can reach is not evidence about
them.

- `[ ]` Ship the decision log, the API-server audit stream and the issuer
  events to an **append-only** sink the cluster cannot rewrite; off-cluster
  where the deployment allows it.
- `[ ]` Two-person custody of the recovery kit.
- `[ ]` Rotate the director's signing key after any recovery in which the kit
  was opened, and record the rotation as the event that closes the window.

## New — Rollback protection on profile digests

The App Store names the digest the director materialises. A compromised store
can name an older digest of a legitimate, reviewed profile: valid entitlement
signature, valid digest, downgrade to a known-vulnerable version through
entirely correct machinery. Nothing refuses it.

- `[ ]` Refuse a digest older than the one installed unless a human confirms
  a rollback, and record that confirmation.
- `[ ]` Carry a minimum version per catalogue entry, so a withdrawn release
  cannot be reinstalled by naming its digest.

## New — A compensating control for shared instances

AD-4 is honest that a shared instance's guarantee is the app's own code, and
`trustTier: platform` is a review rather than a control. One insecure direct
object reference crosses every bound tenant at once, and none of the
platform's isolation — namespace, NetworkPolicy, per-tenant credentials —
applies inside that process.

- `[ ]` Decide what certification actually requires beyond a tier name:
  a tenant-scoped data path, per-tenant encryption keys the platform holds,
  or per-tenant sub-instances behind one namespace.
- `[ ]` Until then, keep the shared list short and name each entry
  explicitly rather than allowing a tier to imply it.

## New — Multi-factor on the kernel realm

Everything rests on Keycloak and the platform-admin path has no stated second
factor anywhere in the plans. It is the cheapest high-value control on the
list and it is simply absent. Pairs with the step-up item above: step-up
protects individual high-impact verbs, this protects the account itself.

- `[ ]` Require MFA for every account in the kernel realm, hardware-backed
  for `gentian:platform:admin` and `gentian:platform:break-glass`.
- `[ ]` Decide the tenant-realm default and leave it to the tenant's policy.
