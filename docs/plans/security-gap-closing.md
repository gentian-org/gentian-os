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
| G2 | No policy-enforcement point calls OpenFGA. Tuples are written (bridge, `app_grant_reconciler`) and never read | BFF: `app/core/openfga_client.py` has `check()`, no route calls it; operator: `Check` exists in `internal/authz/openfga_client.go`, one caller in tests | 3, 4 | Director checks on every write (wave 1); gateway ext-auth checks on every route (wave 1); console checks before every admin action (wave 1) | 1 |
| G3 | No gateway authentication. Every app authenticates itself or not at all; `llm.<domain>` is public | no `SecurityPolicy` in `kernel/`, `crossplane/`, `internal/`; `BackendTrafficPolicy` carries timeouts only | 3, 6 | Envoy Gateway `SecurityPolicy` per HTTPRoute: JWT (Keycloak JWKS) + ext-auth to an AuthZEN shim over OpenFGA; `authMode` on every route, `none` explicit (roadmap 1.15) | 1 |
| G4 | LiteLLM virtual keys are predictable and unauthenticated at the edge | `app_reconciler.go:242` — `sk-gentian-<tenant>-<app>` | 1, 8 | Random keys from OpenBao; route behind G3; per-key budgets (roadmap 2.15 decides ownership) | 0 |
| G5 | Redis ACL grants every tenant app every key and channel | `cache_reconciler.go:402` — `allkeys allchannels` | 6, 8 | `~<tenant>:<app>:*` key pattern + `&<tenant>:<app>:*` channels; prefix injected through `valueMapping.cache`; apps that cannot prefix get a dedicated instance | 0 |
| G6 | MariaDB dynamic-creation grant is root | `mariadb_reconciler.go:200` — `GRANT ALL ON *.* … WITH GRANT OPTION` | 8 | wildcard grant on the tenant prefix: ``GRANT ALL ON `<prefix>\_%`.*``; no `GRANT OPTION` | 0 |
| G7 | The console acts as one ServiceAccount for every tenant; separation lives in Python | roadmap 1.28; `k8s_*` services use `load_incluster_config()` with no impersonation | 1, 3 | Writes move to the director (G1); reads use `Impersonate-User`/`Impersonate-Group` so the API server enforces tenant scope | 1 |
| G8 | Workloads have no identity; east-west traffic is unauthenticated | no SPIFFE, no mesh, no audience-bound tokens except the OpenBao Kubernetes-auth path | 1, 6 | Projected SA tokens with per-consumer audiences first; SPIRE + mTLS when an app-to-app contract needs it (roadmap 1.2) | 2 |
| G9 | Agents share human or service credentials | `agentic-ai.md §8` is design; no token-exchange client, no `agent:` tuples written | 1, 5 | Keycloak token exchange with `act`; `agent`/`task` FGA types with TTL conditions; MCP gateway as PEP (roadmap 1.14) | 2 |
| G10 | No decision log; audit covers console actions only | BFF `audit_log.py` records admin actions to SQL; nothing records FGA decisions; operator has no audit output | 7 | FGA check wrapper logs `(request id, subject, relation, object, decision)` in director, gateway shim and console; Keycloak event export; one request id propagated as a header | 3 |
| G11 | Nothing in the supply chain is signed or verified | no `signatureKeys` on the AppProject, no cosign in `.github/workflows/ci.yaml`, provider-helm pulls whatever chart an AppProfile names with cluster-admin (roadmap 1.16) | 7, 9 | Director-signed commits + Argo `signatureKeys`; image signing in CI + admission verification; provider-helm scoped per tenant namespace | 3 |
| G12 | OpenBao policies are per tenant, not per app | `tenant-default.yaml` composes one `<tenant>-tenant-policy`; `app-default.yaml` composes none. `security.md §5` says per (tenant, app) | 6, 8 | Per-app policy on `tenants/<t>/apps/<app>/*`, bound to the app's ServiceAccount via Kubernetes auth roles | 4 |
| G13 | Rotation does not reach app workloads | `reloader.stakater.com` annotation only on the operator Deployment; compositions do not add it | 8 | `app-default` annotates every Release; document the opt-out | 4 |
| G14 | No admission guard against literal secrets in `Release.set` | `security.md §8` claims one; `kernel/security/kyverno/policies/` has pod-security rules only | 8 | Kyverno rule: deny `helm.crossplane.io/Release` with `spec.forProvider.set[].value` matching a secret key pattern | 4 |
| G15 | No rate limiting at the edge | `BackendTrafficPolicy` builder emits timeouts only | 6 | Default per-route limits in `BackendTrafficPolicy`; override is an ingress annotation (a diff, per principle 8) | 4 |
| G16 | Keycloak sessions lack refresh-token rotation and revocation | roadmap 1.7; nothing in `kernel/services/keycloak-config` sets it | 1 | Realm defaults: rotation on, offline tokens off, idle/max timeouts set in the realm script | 2 |
| G17 | Secrets are not encrypted at rest in etcd | roadmap 1.18 | 8 | KMS provider or `EncryptionConfiguration`; installer step with a check() | 4 |

Not gaps, verified present: per-tenant NetworkPolicy default-deny egress
(`internal/kernel/netpolicy/baseline.go`); pod-security admission
(`gentian-baseline.yaml`: privileged, host namespaces, non-root, hostPath,
capabilities, privilege escalation); the credential manager's token exchange
(the reference implementation of principle 1); Keycloak-group → OpenFGA sync;
console admin-action audit.

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
with JWT + ext-auth on every tenant route, `authMode` mandatory in
`AppProfile.spec.ingress` with `none` as an explicit value (G3); console
reads under impersonation, writes through the director (G7). Model v1 with
`cluster`, `catalogue_entry`, `can_*` relations and tests.

*Challenge:* tenant-A admin token on tenant-B route → 403 at the gateway,
never reaching the pod; a route with `authMode: none` appears in `kubectl
gentian audit routes`; deleting one tuple revokes a running session's next
request; `kubectl auth can-i --as=<user>` matches what the console shows.

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
encryption (G17).

*Challenge:* app X's ServiceAccount token cannot read app Y's OpenBao path in
the same tenant; a `Release` with `set: [{name: password, value: …}]` is
refused; rotating a credential in OpenBao rolls the consuming pod without a
human; `etcdctl get` on a Secret shows ciphertext.

## 3. Rules for the work

- A wave's tests live in `crossplane/tests/e2e` or `scripts/tools` and run
  against a real cluster; a gap without a failing test first is not started.
- A design doc that describes a control marks it *implemented*, *partial* or
  *target* — `design/security.md §3` carries that table; keep it true when a
  wave lands.
- New CRD kinds, routes and endpoints added during the work follow
  [security-principles.md](../security-principles.md)'s closing checklist in
  their PR description.
