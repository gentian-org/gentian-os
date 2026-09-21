# Security gap closing

What [security-principles.md](../security-principles.md) requires and the
code does not yet do, found by reading `internal/`, `crossplane/`, `kernel/`
and the console's BFF rather than the design docs — which describe several
controls as present that are not. Each gap names its evidence, the principle
it violates, the fix, and the wave it belongs to. Roadmap §1 numbers are
given where the gap is already tracked there. Direction-setting decisions
made while closing gaps are recorded in
[architectural-decisions.md](architectural-decisions.md).

## 1. Gaps, from code

| # | Gap | Evidence | Principle | Fix | Wave |
| --- | --- | --- | --- | --- | --- |
| G1 | App lifecycle API is unauthenticated and holds git-push authority | `internal/applifecycle/http.go` — no auth middleware; caller sets `X-Gentian-Actor` | 1, 3 | Director (see [operator-split-plan.md](operator-split-plan.md)); token verified, FGA checked, header removed | 0 |
| G2 | No policy-enforcement point calls OpenFGA. Tuples are written (bridge, `app_grant_reconciler`) and never read | BFF: `app/core/openfga_client.py` has `check()`, no route calls it; operator: `Check` exists in `internal/authz/openfga_client.go`, one caller in tests | 3, 4 | Director checks on every write (wave 1); gateway ext-auth checks on every route (wave 1). The console checks nothing: it renders what the director returns for the caller's relations ([ui-restructure.md](ui-restructure.md)) | 1 |
| G3 | No gateway authentication. Every app authenticates itself or not at all; `llm.<domain>` is public. The only `authMode` in the tree is on `BrowserProxyRoute`, a shell-proxy field with two values and a default | no `SecurityPolicy` in `kernel/`, `crossplane/`, `internal/`; `BackendTrafficPolicy` carries timeouts only; `AppProfile.spec.ingress` (`IngressSpec`) has no `authMode` field; `appprofile_types.go` `BrowserProxyRoute.AuthMode` is `+kubebuilder:default=forward-bearer` | 3, 6 | Envoy Gateway `SecurityPolicy` per HTTPRoute: JWT (Keycloak JWKS) + ext-auth to an AuthZEN shim over OpenFGA (roadmap 1.15). The field is `ComponentProfile.spec.expose[].authMode`, mandatory and defaultless, on **both** surfaces (AD-6): a `gateway` entry gets the `SecurityPolicy`, a `perimeter` entry carries its `authMode` into the DMZ proxy, which is where `none`, `basic` and `signature` actually live ([namespace-cleanup.md](namespace-cleanup.md) §2.6). A fix scoped to `ingress` would reach neither | 1 |
| G4 | LiteLLM virtual keys are predictable and unauthenticated at the edge | `app_reconciler.go:242` — `sk-gentian-<tenant>-<app>` | 1, 8 | Random keys from OpenBao; route behind G3; per-key budgets (roadmap 2.15 decides ownership) | 0 |
| G5 | Redis ACL grants every tenant app every key and channel | `cache_reconciler.go:405` — `allkeys allchannels` | 6, 8 | `~<tenant>:<app>:*` key pattern + `&<tenant>:<app>:*` channels; prefix injected through `valueMapping.cache`; apps that cannot prefix get a dedicated instance | 0 |
| G6 | MariaDB dynamic-creation grant is root | `mariadb_reconciler.go:200` — `GRANT ALL ON *.* … WITH GRANT OPTION` | 8 | wildcard grant on the tenant prefix: ``GRANT ALL ON `<prefix>\_%`.*``; no `GRANT OPTION` | 0 |
| G7 | The console acts as one ServiceAccount for every tenant; separation lives in Python | roadmap 1.28; `k8s_*` services use `load_incluster_config()` with no impersonation | 1, 3 | The console has no Kubernetes identity at all: reads and writes are director calls with the user's token, filtered by the caller's relations; its `rbac.yaml` has zero rules ([ui-restructure.md](ui-restructure.md) §2). No impersonation — roadmap 1.28 is superseded | 1 |
| G8 | Workloads have no identity; east-west traffic is unauthenticated | no SPIFFE, no mesh, no audience-bound tokens except the OpenBao Kubernetes-auth path | 1, 6 | Projected SA tokens with per-consumer audiences first; SPIRE + mTLS when an app-to-app contract needs it (roadmap 1.2) | 2 |
| G9 | Agents share human or service credentials | `agentic-ai.md §8` is design; no token-exchange client, no `agent:` tuples written | 1, 5 | Keycloak token exchange with `act`; `agent`/`task` FGA types with TTL conditions; MCP gateway as PEP (roadmap 1.14) | 2 |
| G10 | No decision log; audit covers console actions only | BFF `audit_log.py` records admin actions to SQL; nothing records FGA decisions; operator has no audit output | 7 | FGA check wrapper logs `(request id, subject, relation, object, decision)` in director, gateway shim and console; Keycloak event export; one request id propagated as a header | 3 |
| G11 | Nothing in the supply chain is signed or verified | no `signatureKeys` on the AppProject, no cosign in `.github/workflows/ci.yaml`, provider-helm pulls whatever chart an AppProfile names with cluster-admin (roadmap 1.16) | 7, 9 | Director-signed commits + Argo `AppProject.spec.sourceIntegrity` (GnuPG, mode `head`; `signatureKeys` is deprecated upstream — limits in [artefacts/roadmap-additions.md](artefacts/roadmap-additions.md)); image signing in CI + admission verification; provider-helm scoped per tenant namespace | 3 |
| G12 | OpenBao policies are per tenant, not per app | `tenant-default.yaml` composes one `<tenant>-tenant-policy`; `app-default.yaml` composes none. `security.md §5` says per (tenant, app) | 6, 8 | Per-app policy on `tenants/<t>/apps/<app>/*`, bound to the app's ServiceAccount via Kubernetes auth roles | 4 |
| G13 | Rotation does not reach app workloads | `reloader.stakater.com` carried by the operator Deployment and a few kernel services (`keycloak-idp`, `infra-redis`); no composition adds it, so no tenant app is rolled | 8 | `app-default` annotates every Release; document the opt-out | 4 |
| G14 | No admission guard against literal secrets in `Release.set` | `security.md §8` claims one; `kernel/security/kyverno/policies/` has pod-security rules only | 8 | Kyverno rule: deny `helm.crossplane.io/Release` with `spec.forProvider.set[].value` matching a secret key pattern | 4 |
| G15 | No rate limiting at the edge | `BackendTrafficPolicy` builder emits timeouts only | 6 | Default per-route limits in `BackendTrafficPolicy`; override is an ingress annotation (a diff, per principle 8) | 4 |
| G16 | Keycloak sessions lack refresh-token rotation and revocation | roadmap 1.7; nothing in `kernel/services/keycloak-config` sets it | 1 | Realm defaults: rotation on, offline tokens off, idle/max timeouts set in the realm script | 2 |
| G17 | Secrets are not encrypted at rest in etcd | roadmap 1.18 | 8 | KMS provider or `EncryptionConfiguration`; installer step with a check() | 4 |
| G27 | Profile-declared egress has no approval path. A profile grants itself outbound network by writing a field; a pod-security exception needs an administrator | `netpolicy/internal.go:85` assigns `profile.Spec.Security.Egress` into the NetworkPolicy spec verbatim, gated only on `len(...) > 0` (`build.go:54`). `PlatformSecurityPolicySpec` carries `allowedMacWaivers` and nothing else, and no caller intersects egress against it — compare `mac_waiver_reconciler.go:135` | 8, 9 | Both become `requires.privileges` with one approval path against the cluster policy (AD-5, [component-profile.md](component-profile.md) §3); `PlatformSecurityPolicy` grows the egress half of the allowlist. Until then the asymmetry inverts principle 8: the weaker control is the one with no diff to refuse | 1 |
| G28 | Kernel, system and shared namespaces have no platform-authored NetworkPolicy. Default-deny stops at the tenant boundary | every builder in `internal/kernel/netpolicy/` writes into the tenant namespace (`nsName`) or `binding.Namespace` — `KernelAccessNetworkPolicy` despite its name is the tenant-side egress allow. The only `kind: NetworkPolicy` in the tree are vendored Bitnami templates: `charts/infra/minio` (`networkPolicy.enabled: true`) and `charts/infra/redis` (`false`). Nothing under `kernel/` or `crossplane/` | 6 | A default-deny baseline per `gentianos.io/tier`, opened by the same `requires`/`integrations` derivation that L5 already describes ([networking.md](networking.md) §2); the vendored per-chart policies retire into it rather than being enabled one at a time. Kernel is the trust root, not an exempt layer | 4 |

Not gaps, verified present: NetworkPolicy default-deny egress — a
namespace-wide ingress+egress object with an empty `podSelector`
(`internal/kernel/netpolicy/baseline.go`) — **in tenant namespaces only**,
and with the holes G18 names; every other tier is G28. Pod-security
admission, which is cluster-wide (`gentian-baseline.yaml`: privileged, host
namespaces, non-root, hostPath, capabilities, privilege escalation); the
credential manager's token exchange (the reference implementation of
principle 1); console admin-action audit.
The Keycloak-group → OpenFGA sync is present but not carried forward: AD-12
replaces the 5-minute admin-credential poll with an event-fed projection.

## 2. Waves

Ordered by blast radius per unit of work. A wave is done when its challenge
list passes as scripted tests, not when the code merges.

### Wave 0 — stop the bleeding (no architecture change)

G1 (authentication only — the FGA check waits for the director), G4, G5, G6.
Four small changes that close the findings an attacker would use first:
anyone on the network can push config; anyone on the internet can spend a
tenant's LLM budget; any tenant app can read every other tenant's cache; a
tenant with dynamic databases is MariaDB root.

*Challenge:* unauthenticated `POST /v1/tenants/x/apps/y` → 401; `redis-cli`
as tenant A's app user `KEYS tenant-b:*` → empty; tenant user `SHOW GRANTS`
shows no `*.*`; a guessed `sk-gentian-…` key → 401.

### Wave 1 — the enforcement points (principles 2, 3, 4)

Director with FGA `Check` on every write (G1, G2); gateway `SecurityPolicy`
with JWT + ext-auth on every tenant route, `authMode` mandatory on every
`expose[]` entry with `none` as an explicit value (G3); console with no
Kubernetes identity — reads and writes through the director (G7). Model v1
as [artefacts/model.fga](artefacts/model.fga) with its tests. Privileges —
egress and MAC waivers both — become `requires.privileges`, approved against
the cluster policy by the same director write (G27): an enforcement point
that only covers one of two escape hatches is not one.

*Challenge:* tenant-A admin token on tenant-B route → 403 at the gateway,
never reaching the pod; a route with `authMode: none` appears in `kubectl
gentian audit routes`; removing a user from a group revokes their Keycloak
sessions and the next request re-authenticates without the role
(networking.md §4); deleting a stored tuple (a grant, an entitlement)
changes the next `Check`; the console's ServiceAccount is bound to no Role
or ClusterRole (roles-and-authorizations.md §2, invariant 1); a profile
declaring egress the cluster policy does not allow is refused, exactly as an
unapproved MAC waiver already is.

### Wave 2 — every principal has an identity (principles 1, 5)

Workload audiences (G8), agent token exchange and `agent`/`task` types with
TTL (G9), Keycloak session hardening (G16). This is where the derived
ceiling stops being a diagram.

*Challenge:* an agent token for user U cannot read a document U cannot; the
same request after `acting_for` deletion → 403; a workload token presented
to the wrong audience → rejected; refresh token reuse → session revoked.

### Wave 3 — audit and supply chain (principles 7, 9)

Decision log with request-id correlation across director, gateway shim,
console and Keycloak events (G10); signed commits and images with
verification at Argo and admission, provider-helm scoped per tenant (G11).

*Challenge:* one request id yields the issuer event, the FGA decision and
the resulting commit; an unsigned commit on `main` does not sync; an unsigned
image does not admit; an AppProfile naming a chart that creates a
`ClusterRoleBinding` fails at provider-helm.

### Wave 4 — depth in the data plane (principles 6, 8)

Per-app OpenBao policies (G12), rotation reaching workloads (G13), the
literal-secret admission guard (G14), edge rate limits (G15), etcd
encryption (G17), and default-deny carried into the kernel, system and
shared tiers (G28) — the layer principle 6 currently skips.

*Challenge:* app X's ServiceAccount token cannot read app Y's OpenBao path in
the same tenant; a `Release` with `set: [{name: password, value: …}]` is
refused; rotating a credential in OpenBao rolls the consuming pod without a
human; `etcdctl get` on a Secret shows ciphertext; a pod in `kernel-gitops`
cannot open a connection to `kernel-secrets`, and every namespace carrying
`gentianos.io/tier` has a baseline policy.

## 3. Rules for the work

- A wave's tests live in `crossplane/tests/e2e` or `scripts/tools` and run
  against a real cluster; a gap without a failing test first is not started.
- A design doc that describes a control marks it *implemented*, *partial* or
  *target* — `design/security.md §3.0` carries that table; keep it true when a
  wave lands.
- New CRD kinds, routes and endpoints added during the work follow
  [security-principles.md](../security-principles.md)'s closing checklist in
  their PR description.

## 4. Roadmap items folded into the cleanup

Open roadmap items the cleanup either makes cheap to close or cannot leave
open without contradicting itself. Same table shape as §1; the wave is where
each lands.

| # | Gap | Roadmap | Principle | Fix | Wave |
| --- | --- | --- | --- | --- | --- |
| G18 | Tenant egress reaches the whole service CIDR on 443 and any DNS server; the policy builder fails open on a missing grant | 1.1 | 6, 8 | Egress to CoreDNS and the API-server endpoint only; fail closed in `grants.go`/`integration.go`. §1's "default-deny verified present" is true of the baseline object, not of these holes | 0 |
| G19 | The tenant admin password is a literal env value in a Job spec and echoed to its log | 1.33 | 8 | Pass by `secretKeyRef` as the Dovecot path does; print only the retrieve hint | 0 |
| G20 | Every Keycloak provisioning Job and `provider-keycloak` authenticate as the bootstrap `master` administrator | 1.16 (Keycloak half) | 1, 5 | Per-realm service-account clients with named `realm-management` roles for the Jobs; a `create-realm`-scoped master service account for the provider; the bootstrap account rotated and kept as break-glass. The realm-adoption change for `tenant-platform` (AD-10) touches the same code | 1 |
| G21 | Director and gateway shim would reach OpenFGA with the pre-shared key the bridge used | 1.2 (sub-item, carried over) | 1 | OpenFGA `authn.method: oidc` against the cluster's issuer; callers present projected ServiceAccount tokens bound to OpenFGA's audience, nothing stored | 1 |
| G22 | ~20 settings reach the cluster as installer-written Helm parameters that bypass the Cluster claim | 1.22 | 9 | The claim is the director's source of truth; anything Argo needs before the composition runs is derived from the claim by the bootstrap chart, and `make verify-claim-applied` covers all of them | 1 |
| G23 | A vanity domain (`Tenant.spec.domain`) is bound with no proof of ownership | 2.10 | 6, 8 | DNS TXT challenge issued by the director on `can_expose`, verified by the operator before a listener or certificate is created (networking.md §6) | 1 |
| G24 | A tenant's only administrator is a derived bootstrap account shared by everyone who administers it | 3.4 | 1, 7 | Invitations through the director's identity endpoint (`can_manage_users`); tenant-admin by named account; the bootstrap account disabled or retained as break-glass with its use audited. Required by "administrator ≠ member" (roles §1) | 2 |
| G25 | Profiles can name any registry and any digest; the AppProfile webhook validates categories only | 1.13 | 9 | Registry allowlist and digest check at materialise-on-reference in the director, and as a CEL rule on `ComponentProfile.package` | 3 |
| G26 | App-internal secrets are derived `sha256(xrName:app:secret)` | 1.8 | 8 | Generated with `crypto/rand`, stored once (component-profile.md §4) | 4 |

Not folded in, decided elsewhere: 1.28 (superseded by G7 as rewritten),
1.26 (the per-tenant desktop replaces the BFF client), 2.4 and 2.8
(superseded by AD-3). The per-app `can_use` relation is no longer out of scope: `app#entitled` is in model.fga, so tenant membership alone reaches no app. Additions the
cleanup creates for the roadmap itself are in
[artefacts/roadmap-additions.md](artefacts/roadmap-additions.md).
