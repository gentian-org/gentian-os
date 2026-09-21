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

## Evaluate — Keycloak Organizations for cross-tenant users

Realm per tenant gives the strongest isolation and forecloses a shared
session across tenants. The widely adopted alternative is one realm with
tenants as Organizations (Keycloak 26+, the same shape as Auth0 and Okta
organizations). Not proposed as a change; recorded so the trade is chosen
rather than inherited.
