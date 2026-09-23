# Work packages

The running order, what is done and what blocks what, is in
[implementation-plan.md](implementation-plan.md). This document says what each
package is.

Everything the architecture cleanup has to build, change or remove, grouped
by component. Each item names the plan that specifies it. Sequencing across
packages follows the waves of [security-gap-closing.md](security-gap-closing.md)
and the cutover steps of [operator-split-plan.md](operator-split-plan.md) §6;
where an item belongs to one of those it says so. The decisions behind the
packages are [architectural-decisions.md](architectural-decisions.md).

Repositories: `os` = gentian-os, `apps` = gentian-apps, `ui` = gentian-ui,
`deploy` = gentian-deployments, `store` = the App Store, a service outside
the cluster in its own repository (WP-14 is its reference implementation).

## Sequence at a glance

The dev cluster is purged and rebuilt in the target layout; release 4.1
preserves the old installer for any cluster that has not been rebuilt.
That removes the migration choreography from the critical path: the
side-by-side cutover (operator-split-plan.md §6 B) and the decommission
step (§6 D) apply to existing clusters only, and a fresh cluster goes
straight to the target. The installer is not rebuilt last — its skeleton
comes first, because every cluster-dependent package needs the new layout
to be tested against, and each package then brings its own step.

| Phase | Packages | Needs | Gate |
| --- | --- | --- | --- |
| 0 — no cluster | WP-1 cutover A (director against a bare repo, static JWKS, OpenFGA in a container); WP-3 model v1, tests, vocabulary check; WP-5 CRD schemas, CEL rules, profile conversion tooling; WP-6 store contract and grant format; WP-7 desktop and console against a mocked director; WP-3 the event listener provider, the director's ingestion endpoint and the membership tuple writer (code only — no Keycloak needed to build or unit-test them) | nothing | contract tests green |
| 1 — installer skeleton, and the store's counterpart | the parts of WP-8 and WP-10 that produce an *empty* cluster in the target shape: labelled `kernel-*` namespaces, tier-0 operators, `kernel-data` with `kernel-postgres`, Keycloak and OpenFGA in their namespaces, OpenBao and the seal, the two Gateways; the step framework kept, step contents rewritten; ACME staging issuers while iterating. **In parallel, WP-14**: the store reference implementation, built against `director-dev` — it needs no cluster, so it fills this phase without competing for the one being rebuilt, and every director call the store makes is exercised before phase 2 wires the operator behind it | the purged cluster (WP-8/10); nothing (WP-14) | `install.sh` stands up the empty layout repeatably; `--dry-run` and `--status` true; the store's flow test passes against `director-dev` |
| 2 — packages on the fresh cluster | WP-1 deployed (no side-by-side), WP-2, WP-4, WP-5 on-cluster parts and materialise-on-reference against WP-14's catalogue source, WP-9 wave 0 and signing, WP-8 remaining namespaces — each adding its installer step as it lands; **WP-3's event listener deployed and wired**, because the director starts checking here and an empty membership projection denies every write, including phase 3's handover commit | phase 1 | each package's tests; the step's `check()` honest |
| 3 — handover and challenge | WP-10 `E-05`, credential split, challenge lists; WP-11 deployments layout; WP-13 toggles verified off and on | phase 2 | the challenge list passes as scripted tests on a fresh install |
| 4 — identities, audit, depth | WP-3 reconcile hardening and drift reporting (the feed itself landed in phase 2), WP-9 identities and agents, WP-4 log store and exposure view, WP-9 data-plane depth (gap-plan waves 2–4) | phase 3 | audit joins on one request id |
| existing clusters | operator-split-plan.md §6 B and D, or a rebuild from the recovery kit (AD-11) | a passing fresh install | per cluster |

## WP-1 Director — new binary (`os`)

Specified in [operator-split-plan.md](operator-split-plan.md) §3, §5, §6.

- [ ] **The director writes git and nothing else.** As built it also writes
      authorization state: it creates the OpenFGA store and model at start,
      projects cluster roles from the claim and tenant structure from the
      tenant manifests, applies Keycloak membership events, records session
      revocations and writes entitlement tuples. None of that was asked for;
      it was written into these plans and into the code in the same pass.
      Inventory in `tmp/director-today.md`, outside the repository. The director's
      job is to read OpenFGA to decide whether a caller may make a call, and
      to write git. Decide where each of the five writes goes instead — the
      operator, which already turns git into cluster state and already holds a
      cluster credential, is the obvious home for the projections — then take
      the OpenFGA write capability away from the director's token, so the
      rule is enforced by the credential and not by care.
- [ ] **The tile catalogue leaves the director.** It serves the kernel
      consoles from a file baked into its image. Which consoles exist is the
      operator's knowledge, since the operator routes them and refuses to
      route them before the zone exists. The permission question stays with
      the director. Tiles are not a security boundary either way: a tile
      nobody may open is refused at the edge.
- [ ] **Step 0 (wave 0, G1):** bearer verification on the existing
      `internal/applifecycle/http.go` against kernel and tenant realms;
      `X-Gentian-Actor` ignored; BFF and CLI send the user's token.
- [ ] **`kubectl gentian login`** ships with step 0, or the CLI has no token
      to send: device authorization grant (RFC 8628) against a `gentian-cli`
      public client in the kernel realm, audience including the director;
      credentials at `~/.config/gentian/credentials` mode 0600 keyed by
      cluster; refresh on expiry; `logout` revokes and deletes
      (operator-split-plan §3.9). The Keycloak client ships with it.
- [x] `cmd/director`, `internal/director/{api,authn,authz,gitops}`; no
      controller-runtime. Contract tests: `go test ./internal/director/...`
      (decisions from a table) and `make test-director-contract` (the same
      tests, a real OpenFGA with model v1 deciding).
- [ ] Chart: plain Deployment, N replicas, rendered but not enabled.
- [x] Authentication: JWKS verification for kernel and tenant realms —
      any realm under the one configured issuer base, per-realm key cache,
      asymmetric algorithms only, audience required, access tokens only.
- [ ] **Sender-constrained tokens (DPoP, RFC 9449) for non-browser callers
      only** — the CLI, the App Store's install call, agent tokens — where a
      credential is held over time by something that can keep a key. Browser
      traffic is out of scope: the edge cookie never reaches JavaScript and
      the token stops at the gateway (AD-13). Scope before committing:
      Keycloak DPoP support, client configuration, and proof generation in
      the CLI and the store.
- [x] Authorization: OpenFGA `Check` per verb against a pinned model v1
      (WP-3); decision log entry per check with the request id; an
      unreachable OpenFGA denies; one id mapping for users and groups.
- [x] Git backend: copy of `applifecycle/gitops*.go`; commits authored as
      the human, committed by the director; trailer with the decision and
      request id; a rejected push re-applies the edit to the new remote state
      (operator-split-plan §3.7); a failed push leaves nothing behind.
- [ ] Commits **signed** with a key held in OpenBao transit
      (artefacts/roadmap-additions.md). Needs OpenBao; lands with cutover C.
- [ ] Write API `/v1/tenants/{t}/…`, `/v1/clusters/{c}/…`: apps, addons,
      plans, policies, exposure enablements, shared-app installs, tenant
      deploy/undeploy, raw edit (break-glass); every write returns 202 with
      an operation URL.
- [ ] Read API — **one read per write, no exceptions** (operator-split-plan
      §3.5). Tenants, installed apps with version, digest, config and addons,
      integrations in force, users and groups, policies, exposure surfaces and
      their enablements, entitlements, plans and usage, cluster security and
      exposure ceilings, operations, tiles filtered by `can_launch`, and audit
      views joining issuer log, decision log and git by request id. Reads are
      authorised by `can_view`/`can_audit`, never by the write relation. Both
      UIs and the App Store render from these: a store that cannot see what is
      installed offers choices the cluster has already made.
- [ ] Materialise-on-reference: `ensureProfile` fetches the bundle at its
      digest from the catalogue repository and applies the profile CR
      **before** the commit; the only cluster write the director has.
- [ ] Request CRs: `TenantExport`, `TenantRestore` created by the director
      on an authenticated request; secrets as ESO references only (G7).
- [ ] Identity writes: users and groups against Keycloak with a scoped
      service identity after `can_manage_users`; on any membership change,
      revoke the user's sessions (networking §4); refuse admin+member on one
      account (roles §1).
- [ ] Entitlements: verify the store's signed grant, commit the fact, write
      `catalogue_entry#entitled` with `expires_at`; pull credentials go to
      OpenBao through the credential manager, never git (ui-restructure §3).
- [ ] OpenFGA projection: receive Keycloak membership events (WP-3), write
      `group#member` tuples, run the reconcile with a `view-users` client;
      create the store and model on first start; rebuild tuples from Keycloak
      and git.
- [ ] Chart, RBAC (read-only + `appprofiles` create/update; no `pods/exec`,
      no `secrets`), NetworkPolicy to git host, OpenFGA, Keycloak JWKS only.
- [ ] Contract tests with no cluster: bare repo, static JWKS, OpenFGA in a
      container (cutover A).
- [ ] Decommission: operator-side `applifecycle` copies, init container,
      `appLifecycle.*` values, `NeedLeaderElection` special case, the CLI's
      `git_commit_push` and `kubectl apply` fallback (cutover D).

## WP-2 Operator — what changes (`os`)

- [ ] **Shared-instance binding in the reconcile loop** (component-profile
      §6.1): when a tenant's `spec.apps` names a profile, check for a
      `shared_instance` offered to that tenant. Bind if there is one — tenant
      objects only, route backend in `shared-<app>`, one NetworkPolicy hop —
      otherwise install a dedicated release, which stays the default.
      `status.fulfilment` records which, and a later offer never moves a
      running app.
- [ ] Purge moves out of the request path into a reconciler driven by
      desired state (app absent from `Tenant.spec.apps`).
- [ ] `provisionAppGroupUsers` becomes a reconcile of `Tenant.spec.apps` →
      Keycloak groups (`app_privilege_reconciler`).
- [ ] Namespace constants (`meta.KernelNamespace`, `netpolicy.Config`,
      `CNPG_CLUSTER_NAME` namespace) become label selectors (WP-8 step 1).
- [ ] `ComponentProfile`/`Component` support: `requires.contracts` fulfilled
      by the requirement reconcilers; `requires.privileges` intersected
      against cluster policy on one approval path; `integrations` bound
      continuously (WP-5).
- [ ] Exposure: `expose[]` → `HTTPRoute` + `SecurityPolicy` per gateway
      entry; enablement → DMZ proxy, route, listener, certificate; removal at
      `expiresAt` (WP-4, networking §8).
- [ ] Per-zone edge OIDC clients with back-channel logout URIs, created
      alongside app clients; per-host clients for vanity domains.
- [ ] Network intent: `Cluster.spec.network.egressAllow`,
      `Tenant.spec.network.{egressAllow,denyKernel}` merged into
      `BuildDesired`; webhook enforces tenant ⊆ cluster.
- [x] Platform tenant: `Tenant/platform` adopts the kernel realm; the
      identity reconciler adopts rather than creates it; undeletable
      (ui-restructure §2). The tenant composition composes only the tenant's
      own groups for a realm it adopts; the webhook admits it before handover
      and refuses its deletion; the installer scaffolds it with the cluster;
      the director attaches every tenant in git to its cluster at start.
- [x] Retire `authz_bridge_reconciler` — safe because WP-3's event feed
      landed in the same phase — and `app_grant_reconciler`'s tuple writes,
      since the director is the store's only writer and rebuilds from git on
      start, which would otherwise delete AppGrant tuples it did not write.
- [ ] Retire: `AppCatalogue` singleton and the catalogue ApplicationSet
      (catalogue leaves the cluster, AD-3); `app-privilege-requested`
      annotation kick.
- [ ] RBAC: no `argoproj.io` write verbs; `pods/exec` stays for purge.

## WP-3 Authorization — model, feed, groups (`os`)

Specified in [authorization-model.md](authorization-model.md) and
[roles-and-authorizations.md](roles-and-authorizations.md).

- [ ] **Decide who projects into OpenFGA**, once the director stops (WP-1).
      Three things need a writer: the store and model at first start,
      structure projected from git (cluster roles, tenants, tenant roles,
      entitlement facts), and membership projected from Keycloak's events.
      All three are "turn a declared state into cluster state", which is what
      the operator does. The fourth, session revocation on back-channel
      logout, is a runtime signal rather than a projection and could belong to
      the edge authorization service that reads it.
- [ ] **A username is an address.** The kernel realm's `email` claim is
      deliberately mapped to the username, so the email *field* can hold the
      recovery address Keycloak mails a reset to. That only works where the
      username is itself an address, and the platform administrator is created
      as the bare name `administrator`, so the claim reads `administrator`.
      Decide the pattern and apply it everywhere a user is created: the
      platform administrator becomes `admin@platform.<kernel>` or
      `admin@<kernel>`, matching the tenant administrators, and nothing
      creates a username that is not an address. Nothing in v0.4 fixes this.
- [x] Model v1 in `authz/model/v1/model.fga` (with `model.json`): types per CRD kind, roles via
      `group#member` only, `can_*` per verb, `admin` explicit (no `or
      member`), conditions for time.
- [x] `authz/model/v1/tests.fga.yaml` — 14 tests, 98 checks, run by
      `make test-policy-authz` for every model version, under docker where
      there is no Go; extend to three cases per relation (grant, neighbouring
      denial, derivation through the parent).
- [x] Membership is **stored**, not contextual: `model.fga`'s header said it
      arrived from the token's groups, which R7, AD-12 and principle 4 each
      forbid and which would leave no tuple writer for membership at all.
- [x] `tenant#admin` derives `or admin from operated_by` (the consent tuple
      below), so the platform-tenant bootstrap tuple goes: a copied tuple is
      what R4 exists to prevent, and it is one nobody has to remember to
      remove.
- [x] `tenant#can_approve_privilege` for tenant-scope privilege grants.
- [ ] `app#entitled` — **move the existing rule, do not change it.** The
      groups and the rule are already in place: the operator creates
      `gentian:tenant:<t>:app:<profile>` and gentian-ui's `shell_apps.py`
      already says "entitlement is membership of the app's own group, and
      nothing else". What is missing is that only the tile list applies it, so
      an app's hostname bypasses it. Write the tuple at install and have the
      gateway check `can_use`. Two behaviours to preserve verbatim, both
      currently in Python: a base with activated addons is entitled by the
      *addon* groups and not its own, and an admin account sees admin tiles
      only. The access review becomes honest as a consequence, not as the
      goal.
- [ ] `app#can_write_credential` (modelled and tested) and the credential manager's `Check` before
      any OpenBao write; OpenBao policy bounds the path, not the decision.
- [x] `shared_instance` with `offered_to`/`can_bind`, so a shared install is
      offered to a tenant and still invisible until that tenant installs it.
- [x] `contract` type with `provider`/`consumer`/`can_consume`, written from
      `AppGrant`. No PEP asks it until G8; the documents stop claiming that
      deleting the tuple revokes a delivered credential.
- [ ] `tenant#operated_by` (modelled and tested; the writers are open): platform administration of a tenant is a
      removable consent tuple, written at deploy; `session#revoked` written
      by the director on back-channel logout and read by the shim.
- [ ] App-admin groups become per app — `gentian:tenant:<t>:app:<p>:admins` —
      and are created only for profiles declaring a `privilegedRole`. The
      cross-app group made a Nextcloud administrator an Odoo administrator.
- [x] `tenant#can_administer` for admin tiles, and `tenant#can_enter` widened
      with `can_audit from cluster` and `can_approve from cluster`. Without
      both, `app#can_use`'s deliberate exclusion of admin accounts makes every
      tile in `tenant-platform` unreachable for the platform admins it is
      built for, and the security officer and auditor cannot enter the console
      at all.
- [ ] Default perimeter approver: the director writes
      `tenant:<t>#perimeter_approver@group:gentian/tenant/<t>/admins#member`
      at tenant deploy. Publishing stays its own relation and its own audit
      line; a tenant that staffs the role separately removes that tuple.
- [x] `make verify-authz-vocabulary` (also run by `make test-policy-authz`):
      every `can_*` in `docs/plans/*.md` and the security principles exists in
      the model, every `can_*` in the model is in authorization-model.md, and
      every relation the director names exists in the model; planned relations
      are listed with their reason in `authz/model/v1/planned.txt`.
- [ ] Keycloak groups, exactly as `authz/model/v1/model.fga` names them — the
      vocabulary check fails otherwise:
      `gentian:platform:{admin,security,auditor,service-admin,shared-apps-admin,break-glass}`
      and `gentian:tenant:<t>:{admins,members,perimeter}`, plus per app
      `…:app:<p>` and, where the profile declares a `privilegedRole`,
      `…:app:<p>:admins`; realm
      script and console.
- [x] Director ingestion endpoint and membership tuple writer
      (`internal/director/membership`, `POST /v1/events/keycloak`): Ed25519
      signature over timestamp and body, five-minute window, replay dropped by
      event id and by per-user event time; events state a user's complete
      group set, so applying one is a comparison and a lost event is repaired
      by the next; **a realm speaks only for its own tenant** — a tenant realm
      naming `gentian:platform:*` or another tenant's group is refused and
      logged; a failed apply answers 503 and the retry is accepted.
- [x] **Keycloak event listener** — source, tests and image build in
      `kernel/extensions/keycloak-event-listener`: an event-listener SPI
      provider that states a user's complete top-level group set, signed
      (Ed25519), after commit, on membership and user admin events, on group
      rename, on login and registration, and on the model's group- and
      user-removal events; one background sender, bounded queue, retried for
      a minute. It holds no credential beyond the signing key.
- [ ] Listener deployed: CI image job, init container on the Keycloak pod in
      `kernel-authentication`, key pair in OpenBao with the public half in the
      director's configuration, `eventsListeners` and admin events enabled on
      every realm (kernel realm and the tenant realm template). Inventory row
      in namespace-cleanup §2.1. `keycloak.version` in its `pom.xml` pinned to
      the deployed Keycloak.
- [ ] Reconcile: on start, on a failed event as a targeted re-read of that
      subject, and as a rolling per-realm sweep completing cluster-wide within
      **15 minutes** — `view-users` client only, corrects toward Keycloak,
      reports drift, flags admin+member accounts. A lost *removal* is the
      asymmetric failure: it leaves access in place silently, and the sweep is
      what bounds it (authorization-model §2).
- [ ] Freshness: the director records the last accepted event and the last
      completed sweep; no sweep in 30 minutes raises an alert. A stale
      projection does not fail checks closed — an issuer hiccup must not
      become a platform outage — it fails loudly.
- [ ] Ext-auth shim polls OpenFGA's `ReadChanges` changelog and evicts
      cached decisions (WP-4).
- [ ] OpenFGA on its own CNPG cluster or pooled database with a reserved
      connection limit, isolated from Keycloak's login load (AD-8).
- [ ] Reverse queries and access-review export for the auditor (roadmap
      1.12).
- [ ] Wave 2: `agent`, `task` types with TTL conditions; MCP gateway checks
      `can_act`.

## WP-4 Networking — edges, DMZ, exposure (`os`)

Specified in [networking.md](networking.md).

- [x] Two `Gateway` objects, `authenticated` and `perimeter`, in
      `kernel-edge` under `mergeGateways`; tenant listeners stay per-zone.
- [x] `SecurityPolicy` per route for the kernel zone: OIDC session with
      `gentian-edge-kernel` (no groups scope, director in its audience,
      back-channel logout at the director), ext-auth for reachability. Envoy
      Gateway binds a session's cookies to the policy that made it, so a
      component's oidc routes share one policy.
- [ ] `SecurityPolicy` per route for tenant zones (edge clients from WP-2)
      and JWT for bearer routes.
- [x] **Ext-auth shim** — `edge-authz` in `kernel-edge`, shipped in the
      operator image: verifies the token, asks the route's relation, caches
      per `(sub, sid, route)`, evicts on every `ReadChanges` entry, fails
      closed with cached allows carrying, denies a revoked `sid`; gRPC. Envoy
      Gateway runs ext_authz before its OIDC filter, so on an oidc route a
      request with no session passes to the OIDC filter with no identity and
      is refused only on a bearer route.
- [ ] Shim: `can_use` on app routes; topology-aware routing to it.
- [ ] DMZ publishing proxy image: generic Envoy/nginx with config rendered
      per surface — path allow/deny, `authMode` adapter (basic via the
      broker's passdb, bearer via JWKS, signature via HMAC from OpenBao),
      rate and body limits, one backend and port, one credential; Coraza
      with the OWASP core rules for `none` surfaces.
- [ ] Structured proxy access logs with tokens hashed; the cluster log
      store (G10); retention set by the security officer.
- [ ] Drift job: routed listeners, DMZ routes, DNS records and certificates
      reconciled against enablements.
- [ ] Vanity hosts: listener and **HTTP-01** certificate per enabled host —
      the platform holds no credential to a customer's zone; DNS guidance;
      cloudflared hostnames from the same enablement.
- [ ] Zone certificates: **no change** — the per-tenant `*.<domain>`
      certificate is already issued by DNS-01 against
      `letsencrypt-dns01-<provider>` (`tenant_edge_tls.go`), and DNS records
      stay external-dns's, from HTTPRoutes on a static-ip cluster and from the
      operator's `DNSEndpoint` on a tunnelled one. Carried here only so the
      package does not silently re-litigate it.
- [ ] `system-mail` split: Postfix, spam filter and an optional Dovecot
      proxy in `system-mail-dmz`; store and DKIM milter in `system-mail`;
      `TCPRoute`s on the perimeter Gateway; external IMAP/submission a
      default-off surface.
- [ ] `system-turn` (coturn or SFU) as a DMZ-tier service with a `UDPRoute`
      and per-session HMAC credentials, when the first conferencing profile
      requires it.
- [ ] Default per-route rate limits in `BackendTrafficPolicy`; WebSocket
      max connection duration (G15).
- [x] Keycloak route on the perimeter Gateway with the `/realms/*` path
      allowlist; `/admin` and `master` refused by a route of their own
      closed by an authorization policy; the console on `id-admin.<kernel>`
      behind the kernel session.
- [ ] Keycloak metrics and health on an internal hostname only (roadmap
      1.6); brute-force detection per realm (G16).
- [ ] Exposure API and console view: summary, condensed log (most requests,
      most recent incl. first-seen, most bytes, rejections), public objects
      via the contract, complete log (networking §8.4).
- [ ] **Publishing proxy as a named enforcement point** (principle 3): it
      verifies `none`, `basic` and `signature`, applies any declared source
      restriction, and **strips every inbound identity header** before
      forwarding, setting only its own. Without the strip, an app-host
      perimeter path is a header-spoofing route to the same pod the
      authenticated Gateway serves.
- [x] Both `Gateway` objects are kernel resources in `kernel-edge`,
      reconciled by the operator from the Cluster claim. Tenants get listeners
      and own `HTTPRoute`s only — under `mergeGateways` listener uniqueness is
      class-wide, so a tenant-owned Gateway could claim another tenant's
      hostname. Zone wildcards by DNS-01 on the kernel domain's existing
      provider credential; HTTP-01 only for tenant-owned vanity hosts
      (networking §7).
- [ ] Retire `browserProxy`, `additionalIngresses`, the portal session
      bridges' routes.

## WP-5 Catalogue — `ComponentProfile` (`os`, `apps`)

Specified in [component-profile.md](component-profile.md).

- [x] CRDs `ComponentProfile` and `Component` with their CEL rules (§7, and
      the ones the instance needs: immutable owner, approver and tenancy; an
      exposure always ends; `forwardToken` only at platform tier and never on
      the perimeter). `component_schema_test.go` runs the generated CRDs'
      OpenAPI schema and CEL the way the API server does — it caught three
      rules that would have refused every valid profile.
- [ ] Admission policies for who may create which tenancy where (needs the
      cluster's namespace tiers: lands with WP-8).
- [x] Two-level tenancy; `trustTier` in spec; `requires` absorbing
      `kernelRequirements`, `optionalIntegrations`, `security`;
      `integrations`; `provides`; `secrets`; `expose[]` with mandatory
      `authMode` and `surface`; `extensions`; `hooks`.
- [ ] **`exposure-policy` adapter for Nextcloud** as an extension container
      — the first one, because with `requireExposurePolicyContract` on no
      `none` surface can be enabled without it; Docmost and the meeting apps
      follow. `forwardToken` on `expose[]`, refused by admission outside the
      desktop profile; `fulfilment: auto|dedicated` on the tenant's app entry.
- [ ] `ExposureEnablement` on the instance; cluster exposure policy on the
      Cluster claim (§5.1); `exposure-policy` contract (§5.2).
- [ ] Fulfiller selection: default per contract on the Cluster claim,
      mapping to the per-engine `system-*` namespaces (§9.1, AD-9).
- [x] Conversion tooling: `scripts/tools/convert-appprofiles.sh <profiles> <out>`
      writes a `ComponentProfile` and a store listing per `AppProfile`, proves
      every result against the CRD, and lists what a person must decide in
      `REVIEW.md`. Against the catalogue today: 34 profiles, all admitted, 45
      review items (egress and waiver reasons, routes that forwarded the
      user's token, apps with no login of their own, dropped ingress
      annotations).
- [ ] Run the conversion in `apps`, settle the review items, commit the
      profiles and hand the listings to the store.
- [ ] Bundle digests; profile bundles in the mirror target of roadmap 1.9;
      `make lint-image-digests` covers them.
- [ ] `Repository` claim: read credential (Argo) split from push credential
      (director); `credential.appliesWhen` generated into the requirement
      CRs; `_requirement_applies()` regenerated from the same field.
- [ ] Retire the `catalogue-<repo>` ApplicationSet, `AppCatalogue`,
      `AppPackage`'s in-cluster role; `app-store-me` profile and its dead
      install paths (`services/gitops.py`, `add_tenant_app`).
- [ ] Tenant desktop as a `ComponentProfile` (`tenancy: [tenant]`, `llm` and
      database requirements, realm client) — the proof of the abstraction.

## WP-6 App Store — external service (`store`, `os`)

Specified in [ui-restructure.md](ui-restructure.md) §3 and
[app-store-schema.sql](app-store-schema.sql).

- [ ] The store service outside the cluster over the schema: listings in
      every locale, editions, plans, subscriptions, catalogue access per
      cluster, cluster registration with a public key.
- [ ] Ingest from the catalogue repository; reject entries not deployable
      as `tenant`.
- [x] **The contract and the cluster's half of it**
      ([store-contract.md](../design/store-contract.md)): statements are
      compact JWS, EdDSA, bound to cluster and tenant, ordered by `iat`, keys
      pinned; `POST`/`GET /v1/tenants/{t}/entitlements` in the director; the
      fact committed to `entitlements.yaml`, the conditional tuple written or
      removed in the order that fails towards less access. Contract-tested
      with OpenFGA evaluating `grant_valid`.
- [ ] Store side: signing (`entitlement_grant`, `signing_key`); single-use
      fetch token; pull credential handed to the credential manager as the
      tenant admin.
- [x] **Revocation on the same path** (cluster side; the pull credential's
      removal waits for the credential manager): a signed record with `granted: false`
      to `POST /v1/tenants/{t}/entitlements`. The director verifies, commits
      the fact and deletes the tuple in one operation, so a later commit
      overrides an earlier `expires_at` (operator-split-plan §3.8). The pull
      credential and its `ExternalSecret` go with it; running pods are
      untouched and the next pod start cannot pull.
- [ ] Store reads cluster state through the director's read API with the
      signed-in admin's token, to render installed apps, enabled addons and
      published surfaces (AD-3: it may trigger and read, never supply).
- [ ] Install trigger: `POST /v1/tenants/{t}/apps/{p}` with the user's
      token or an exchanged token carrying `act`.
- [ ] Cluster side: the director's ingestion endpoint on the kernel gateway,
      bearer only, behind the gateway `SecurityPolicy`, verified again.

## WP-7 UI — desktop and console (`ui`)

Specified in [ui-restructure.md](ui-restructure.md) §1–§2.

- [ ] Desktop per tenant from one image (frontend + BFF), served on the
      tenant's host or vanity host; BFF holds **no OIDC client secret and no
      session** — it consumes the token the edge forwards (AD-13) — only the
      granted `{t}_shell` database; Kubernetes RBAC: none.
- [ ] Tiles from `GET /v1/tenants/{t}/apps?viewer=me`; admin screens shown
      by relation, never by flag.
- [ ] **The admin console works behind the edge.** It was built against the
      portal's own Keycloak admin credential, which the desktop no longer
      holds and must not. Every screen in it is a cluster or tenant setting,
      so each one is a director call that authorises the caller and commits to
      git, exactly as installing an app already is. Until then the console is
      a set of screens with nothing behind them.
- [ ] **The Cluster tile opens signed in.** Headlamp runs its own OIDC flow
      with its own client and keeps the result in its own cookie, and the
      kubeconfig it proxies with has no `id-token` until that flow has run.
      The edge session is deliberately not forwarded to it, and Headlamp would
      not read it if it were: every `/clusters/main/*` call fails with
      "No valid id-token, and cannot refresh without refresh-token". So the
      second sign-in is structural. It costs a click and not a password, since
      the Keycloak session already exists, but the click should go. Either
      start Headlamp's own flow from the tile, or bridge the edge session into
      the cookie it expects.
- [ ] **The Identity tile is not an iframe problem.** Embedding works: the
      realm sends no `X-Frame-Options` and no CSP, the edge injects a
      permissive `frame-ancestors`, the console is same-site so its cookies
      are first-party, and the silent SSO and token exchange both complete
      inside the frame. It then dies on its first Admin REST call, 401, and
      the spinner is what is left. The cause is that `KC_HOSTNAME` and
      `KC_HOSTNAME_ADMIN` differ: tokens are minted by `id.<kernel>` and
      presented to `id-admin.<kernel>`, which Keycloak refuses. Upstream has
      this reported and closed as not planned (keycloak#42264), so opening the
      tile in a tab would fail in exactly the same way. Decide whether to drop
      the split hostname and serve `/auth/admin/` on `id.<kernel>` behind the
      kernel session, which is the only fix that does not wait on upstream.
      Note also what the current setup costs: the realm's clickjacking
      defences are cleared wholesale to make one tile frameable, which removes
      them from every login and account page in the realm.
- [x] First slice, against a mocked director: `app/services/director.py`
      and `/api/v1/director/...` in the BFF forward the person's own token and
      repeat the director's answer, status and request id; no actor header,
      no role test. `docker-compose.dev.yaml` points at `director-dev`
      (`make run-director-dev` in `os`), proven end to end: list, install,
      refusal with request id, attributed commit.
- [ ] All writes through the director with the user's token; all reads
      through the director's read API; `rbac.yaml` empty; `k8s_*` services
      removed; the apps screens switched from `k8s_catalogue` to the routes
      above.
- [ ] Remove: the three session bridges (`portal_`, `openproject_`,
      `matrix_session_bridge.py`), the LiteLLM master key path
      (`routes/llm.py`), Keycloak admin credentials, the `tenants` patch, the
      SQL audit store as the record (the console shows the joined logs).
- [ ] AI widget through the desktop component's own granted LLM credential.
- [ ] Exposure view (WP-4) and audit view.
- [ ] Platform tenant's desktop in `tenant-platform` as the platform-admin
      console.

## WP-8 Namespaces (`os`, `deploy`)

Specified in [namespace-cleanup.md](namespace-cleanup.md).

- [x] Step 1, labels first: `gentianos.io/tier`, `gentianos.io/function`
      on every kernel namespace (`kernel/namespaces.yaml`, `internal/layout`);
      Kyverno's webhook scoped by tier label in the v5 chart.
- [x] `gentianos.io/tier` and `gentianos.io/tenant` on tenant namespaces;
      the operator addresses every platform namespace by function
      (`internal/controller/namespaces.go`: Keycloak Jobs in the
      authentication namespace, each data-plane Job beside its `system-*`
      service, the provisioning ConfigMap in `kernel-provisioning`) and
      `meta.KernelNamespace` is gone.
- [ ] NetworkPolicy generation by label selectors.
- [ ] Stateless renames for fresh installs: `kernel-gitops`,
      `kernel-provisioning`, `kernel-secrets`, `kernel-seal`,
      `kernel-control`, `kernel-edge`, `kernel-admission`, `kernel-data`,
      `kernel-authentication`, `kernel-authorization`; installer, bootstrap
      chart, ApplicationSets, OpenBao auth roles re-issued.
- [ ] `kernel-data`: CNPG `kernel-postgres` for Keycloak, extensions,
      OpenFGA (own cluster or reserved pool), admin console; retire
      `infra-postgresql`.
- [ ] `system-postgresql`, `system-mariadb`, `system-cache`, `system-s3`,
      `system-mail`, `system-mail-dmz`, `system-llm`, `system-turn`; stage
      suffix dropped; `kernel-admin` chart split three ways.
- [ ] `tenant-<t>-dmz` per tenant; tenant name length ≤ 52.
- [ ] MetalLB and metrics-server labelled in place.
- [ ] Recovery-kit path for existing clusters (artefacts/recovery-playbook-addition.md).

## WP-9 Security gaps not covered above (`os`, `apps`)

From [security-gap-closing.md](security-gap-closing.md).

- [ ] **Backup split (decided 2026-09-22).** Local backup, tenant
      export/restore and `BackupPolicy` stay here. Remote targets with key
      escrow, retention across tenants, restore drills with proof, DR and
      tenant migration become a GTC-authored component delivered as an
      entitled catalogue entry; its source leaves this repository when the
      backup code is touched in phase 2. Nothing here references it.
- [ ] Wave 0: G4 random LiteLLM keys from OpenBao behind the gateway; G5
      Redis ACL key and channel prefixes via `valueMapping.cache`; G6
      MariaDB wildcard grant on the tenant prefix, no `GRANT OPTION`.
- [ ] **Privileges are granted, not filtered** (component-profile §3.1):
      `PrivilegeRequest` defined, `PrivilegeGrant` written by the director
      with approver, reason and expiry; scope decided by the kind, never by
      the profile; an ungranted request queues the install instead of being
      dropped. `PlatformSecurityPolicy` keeps its meaning as the ceiling of
      what may be asked for, and stops being mistaken for the approval.
- [ ] **Master password out of the app-install path** (design/security.md §6):
      Composition init Jobs stop reading
      `gentian-os/kernel/internal/master-password` and ask the credential
      manager for the one credential they need. Today any init Job in any
      tenant can derive every credential on the cluster, kernel identity
      included, and no OpenBao policy contains it because the derivation is
      client-side. After this the master password has one reader and can move
      to a KMS without touching the install path.
- [ ] G8 workload identity: projected SA tokens with audiences; SPIRE and
      mTLS when a contract needs it.
- [ ] G9 agents: RFC 8693 exchange with `act`; MCP gateway as PEP.
- [ ] G11 supply chain: director signing key in OpenBao transit; Argo
      `sourceIntegrity` policy for `gentian-deployments`
      (artefacts/roadmap-additions.md); image signing in CI and admission
      verification; provider-helm scoped per tenant (roadmap 1.16).
- [ ] G10 decision log and request-id propagation across director, shim,
      console and Keycloak events; the log store.
- [ ] G12 per-app OpenBao policies bound to the app's ServiceAccount.
- [ ] G13 Reloader annotation on every Release from `app-default`.
- [ ] G14 Kyverno rule against literal secrets in `Release.set`.
- [ ] G16 Keycloak: refresh-token rotation, offline tokens off, idle/max
      timeouts, brute-force detection.
- [ ] G17 etcd encryption at rest with an installer step and check.
- [ ] Kubernetes identities for platform roles (structured auth or
      Pinniped); break-glass bound to `cluster-admin` by group
      (roadmap-additions).
- [ ] Step-up authentication for high-impact permissions (roadmap-additions).

## WP-10 Installer and bootstrap (`os`)

- [ ] **What the v5 step set dropped and has not replaced.** The v4 set is 44
      steps, v5 is 20. Two cold-start races are fixed (B-05 now waits for the
      provider CRDs to be Established and for every Crossplane function to be
      Healthy, because Healthy is not Established and a Composition whose
      function is missing renders nothing). These remain, each with nothing
      doing the work anywhere else:
      - the OpenBao **`oidc` auth mount** and its configuration. The Cluster
        composition already composes auth roles against that mount and says in
        a comment that two installer steps create it; neither exists in v5, so
        the roles attach to a mount that is not there and OpenBao accepts no
        Keycloak login.
      - the four **`Repository` claims** (deployments, os, apps, ui). Nothing
        writes them, so the composition that emits Argo CD's repo Secret, the
        operator's push credential and the catalogue-sync ApplicationSet has
        no claim to compose from. A private deployments repository has no
        credential path at all.
      - the **credential catalogue** (`CredentialRequirement`s and their
        satisfaction probes), which `make check-credentials` reads.
      - **mail** and **LLM serving**: no v5 step and no ApplicationSet, while
        the operator still writes Postfix virtual-domain entries and still
        publishes an `llm.<kernel>` route to a service nothing deploys.
      - the **recovery kit** and the **bootstrap token revocation**. A v5
        install ends with the installer's OpenBao root token still valid and
        the init file still on disk.
      - **tenant teardown**: nothing in v5 reverses tenants, so the webhook
        and the CRDs block a purge.
      - the **public DNS wait**. The first step to reach the cluster from
        outside is now the kernel realm step, so a slow publish surfaces
        inside it rather than at a named step.
- [ ] `B-08-seed-secrets` declares `requires: C-01-cluster-claim` but runs
      before it, because steps run in filename order and nothing validates the
      header against that order. Fix the header or the filename, and make
      `make validate-steps-v5` check the two agree.

- [ ] `D-01` installs the director chart; `E-05-director-handover`: proof
      commit as the installing admin, `signatureKeys`/`sourceIntegrity`
      set, host branch protection verified, push token in one Secret only.
- [ ] `B-09`: two credentials for the deployments repository.
- [ ] `C-06`: retire the unconditional apply; requirements composed from
      the Cluster claim with `appliesWhen`.
- [ ] `D-05` retired: vLLM composed from `Cluster.spec.llm` (AD-9).
- [ ] `D-08`/`D-09`: no catalogue sync; the catalogue-repository claim
      keeps the credential for materialisation only.
- [ ] Cluster claim fields: `exposure` policy, `network.egressAllow`,
      default fulfiller per contract, identity/secrets/database provider
      selectors for the modular kernel, `catalogue.sources[]` (AD-14),
      `store.signingKeys`, and the `compliance` block of WP-13.
- [x] **Phase 1 skeleton, `install.sh --layout v5`** (`scripts/steps-v5/`,
      `kernel/bootstrap-v5/chart`, `kernel/data/kernel-postgres`): one namespace
      list, `kernel/namespaces.yaml`, read by the installer
      (`scripts/lib/namespaces.sh`), by Go (`internal/layout`, whose test pins
      it to the file) and by the bootstrap chart (which refuses to render
      without it); A-01 creates and labels every kernel namespace; A-02…A-07
      install cert-manager, ESO, Crossplane, Envoy Gateway, Argo CD and
      metrics-server by function; B-01 applies the AppProject and the kernel
      Applications — OpenBao in `kernel-secrets` with its seal in
      `kernel-seal`, Reloader, CNPG and `kernel-postgres` in `kernel-data`,
      Kyverno in `kernel-admission` (kernel tier exempt by label), external-dns
      in `kernel-edge`. `make validate-steps-v5` runs the step-contract checks
      and `lint-namespace-layout`, which fails on any namespace named by hand.
      The v4 step set is untouched. **Not run against a cluster yet.**
- [ ] **Milestone M1 — the platform administrator signs in and sees the
      cluster, in the plans' shape.** `install.sh --layout v5` runs end to end
      on the purged cluster; `administrator@<kernel>` signs in once at
      `console.<kernel>` and sees the platform tenant's desktop; the kernel
      consoles are tiles the director answered from that account's relations;
      each opens in a window on the desktop, signed in, with no second login
      and no token to paste. Nothing about it is a stand-in for the plan: the
      desktop is `Tenant/platform`'s, the edge holds the session, the desktop
      holds no authority. In order, each landing with its installer step and
      verified on the cluster before the next:
      1. `[x]` **Vocabulary** (WP-3): the Keycloak groups exactly as model v1
         names them — `gentian:platform:admin` replaces the bootstrap's
         `superadmin` everywhere it is written or read: the identity
         bootstrap, the claim's `platformRoles` default, Argo CD's policy,
         the proxy's binding, the OpenBao OIDC roles, the desktop's constant.
      2. `[x]` **Platform tenant** (WP-2, WP-8): `Tenant/platform` with
         `isolation.keycloakRealm: kernel`, realm adopted and never created,
         disabled or deleted — the tenant composition honours the realm
         override and emits no Realm and no kernel broker for a tenant that
         adopts the kernel realm; tenant namespaces carry
         `gentianos.io/tier: tenant` beside `gentianos.io/tenant`; the v5
         ApplicationSets sync `clusters/<c>/tenants/*`; the installer
         scaffolds `tenants/platform/`; the director writes
         `tenant:platform#cluster` and `#operated_by` at start, as it writes
         the cluster roles.
      3. `[ ]` **The edge** (WP-4): `authenticated` and `perimeter` Gateways
         in `kernel-edge` under `mergeGateways`, reconciled by the operator
         from the Cluster claim; the kernel zone's one confidential client
         (`gentian-edge-kernel`: no groups scope, secret in `kernel-edge`,
         back-channel logout at the director); `SecurityPolicy` OIDC with the
         zone's cookie on `.<kernel>` and ext-auth on every kernel-zone route;
         the ext-auth shim as a new binary in `kernel-edge` — gRPC, verifies
         the token, asks the route's relation, caches per `(sub, sid, route)`,
         evicts on `ReadChanges`, denies `session#revoked`, fails closed with
         cached allows carrying — with its route table written by the
         operator beside the routes; `id.<kernel>` serves `/realms/*` only on
         the perimeter Gateway and `/admin/*` on `id-admin.<kernel>` behind
         the kernel session.
      4. `[x]` **The desktop as a component** (WP-5, WP-2): a
         `ComponentProfile` `desktop` (`tenancy: [tenant]`, `trustTier:
         platform`, a database requirement, one gateway exposure with
         `authMode: oidc` and `forwardToken: true`); a Component reconciler
         that gives every tenant its desktop from that profile — a
         provider-helm Release in `tenant-<t>`, the database fulfilled in
         the tenant's namespace, the route and `SecurityPolicy` from
         `expose[]` — so `tenant-platform` serves `console.<kernel>`.
      5. `[x]` **The desktop without authority** (WP-7, WP-1): the BFF
         consumes the token the edge forwards and runs no code flow; no
         client secret, no Keycloak admin credential, `rbac.yaml` empty; the
         database from the granted requirement; tiles from the director —
         the kernel consoles from `/v1/clusters/{c}/tiles`, the admin tile by
         `can_administer` from a `GET /v1/tenants/{t}/me` relations read.
      6. `[x]` **Kernel UIs behind the kernel session** (WP-4): Argo CD,
         Headlamp and `id-admin` routes carry the zone's `SecurityPolicy` and
         the shim's `can_configure` / `can_audit`; each tool's own OIDC login
         is the silent second factor.
      7. `[x]` **Retire** the portal in `kernel-edge`, `portal.<kernel>`, the
         portal secret and BFF client in the identity bootstrap, and
         `kernelPortalHost`; `www.<kernel>` is an alias of the console.
      8. `[ ]` **Purge and reinstall** — the confirmation cycle; every
         `check()` honest; `--status` true.
      Earlier partial results (kernel tiles served by the director, Headlamp
      through the impersonating proxy, the event listener wired, the claim's
      `platformRoles` projected) stand and are reused; they are not the
      milestone.
- [ ] Phase 1 continued: OpenBao init and the seal token (B-02), ESO stores,
      Crossplane providers, Keycloak and OpenFGA in their namespaces (the Suze
      composition takes the layout's names), the two Gateways from the Cluster
      claim, the director and operator in `kernel-control`; then the first run
      on the purged cluster with ACME staging.
- [ ] Namespace label step (WP-8 step 1) and the fresh-install layout.
- [ ] **API-server audit logging**: an audit policy shipped by the installer,
      flowing to the same store as the decision log. Without it break-glass is
      unaudited — a kubeconfig session raises no Keycloak event and touches no
      director verb, so none of principle 7's three logs sees it, and
      "time-boxed, every action logged" is a claim with no mechanism.
- [ ] Recovery kit carries the director's signing key **material**, decided
      deliberately (2026-09-21) so that recovery works when OpenBao itself is
      gone, which a transit reference cannot survive. The cost is accepted and
      recorded rather than discovered: a kit holder can produce commits Argo
      accepts as the director's, so signature alone no longer distinguishes a
      recovery-time commit from a real one. What still does: kit custody, the
      fact that recovery is an announced event, and API-server audit logging
      covering what break-glass touches. Rotate the director's key after any
      recovery in which the kit was opened.
- [ ] Challenge tests for cutover C and every wave under
      `crossplane/tests/e2e` and `scripts/tools`.

## WP-11 Deployments repository layout (`deploy`)

- [ ] `clusters/<c>/kernel/security/` for `PlatformSecurityPolicy`,
      `PolicyException`s and overlays, synced by one more Application.
- [ ] Exposure enablements and network intent in the tenant and cluster
      claims.
- [ ] Branch protection: only the director's identity pushes `main` after
      handover; humans push during bootstrap only.
- [ ] Per-cluster director signing key registration.

## WP-12 Documentation and verification (`os`)

- [ ] `design/security.md` §3.0 status table kept true per wave;
      `routing.md` for the two edges; `iam.md` for the new groups and the
      platform tenant; `kernel.md` for tier 0 versus `KernelService`;
      `architecture.md` for the director.
- [ ] Threat model re-run against code after wave 1 and wave 3.
- [ ] `make verify-authz-vocabulary` and the challenge lists wired into CI.

## WP-13 Certification readiness — on components already being changed (`os`)

Controls and evidence for SOC 2, ISO 27001, ISAE 3402 and ISO 9001 that fall
out of the director, the realm configuration, the Cluster claim and the
purge reconciler with little extra code. Nothing here adds a component;
what needs one is on [roadmap.md](../roadmap.md) under *Certification*.

Every behaviour is switchable from one block on the Cluster claim, so a
single-node, single-tenant or single-person cluster is not asked to
approve its own changes or attest its own access. Defaults are the
non-intrusive setting; the certifying operator turns them on.

```yaml
compliance:
  fourEyes: false            # a privilege or exposure approval must come from a subject other than the requester
  adminMfa: optional         # optional | required — MFA or passkey for every platform and tenant-admin role
  breakGlass:
    maxDuration: 0h          # 0 = membership does not expire; >0 = auto-removed by the director, reason required
  accessReview:
    interval: 0h             # 0 = off; >0 = the director generates the report and asks for sign-off
  evidence:
    enabled: true            # read-only exports; harmless on any cluster
  records:
    deletion: true           # purge and tenant export write a signed record
  residency: ""              # jurisdiction/region shown to tenants; empty = unstated
```

- [ ] **Evidence exports** on the director's read API (`evidence.enabled`):
      access review per tenant and cluster (holder, role, granted when and by
      whom, from the projection and the change log); configuration changes
      over a period with approver and request id; privileged-access uses
      (break-glass additions and removals); exposure inventory over a period;
      key rotation dates from the credential catalogue. Each is a query over
      the three logs; auditors sample from populations and these are the
      populations.
- [ ] **Four-eyes on approvals** (`fourEyes`): the director refuses a
      privilege or exposure approval whose approver is the requester; the
      trailer records both subjects. Segregation of duties as a test.
- [ ] **Break-glass as a workflow** (`breakGlass.maxDuration`): joining
      `gentian:platform:break-glass` requires a reason, receives an expiry,
      is removed by the director at expiry, and leaves a review item; both
      events are Keycloak events and appear in the exports.
- [ ] **MFA for administrators** (`adminMfa`): realm configuration for the
      kernel realm and every tenant realm's `admins` and `perimeter` groups;
      passkeys offered first. Step-up for high-impact verbs stays a roadmap
      item.
- [ ] **Access-review attestation** (`accessReview.interval`): the director
      generates the review, notifies the tenant administrator and the
      security officer, and records the sign-off as an FGA-checked commit;
      overdue reviews appear in the console and the exports.
- [ ] **Deletion and export records** (`records.deletion`): the purge
      reconciler and tenant export write a signed record — subject, object,
      when, verified gone or verified delivered — to git through the
      director, joined to the request id.
- [ ] **Residency** (`residency`): a Cluster claim field shown in the tenant
      desktop and the exports.
- [ ] **Control catalogue** — `docs/compliance/controls.md`: one row per
      control objective → principle → mechanism → evidence query. The
      auditor's first document and ISAE 3402's system description; the
      roles document is the list of complementary user-entity controls.
- [ ] **Request id everywhere**: the director, the shim, the console and
      the Keycloak listener propagate one id; the exports join on it.

## WP-14 Store reference implementation (`store`)

Specified by [store-contract.md](../design/store-contract.md) (the interface),
[ui-restructure.md](ui-restructure.md) §3 and
[app-store-schema.sql](app-store-schema.sql). WP-6 is the cluster's half of
the contract and is done; this is the other half, built so the whole flow —
listing, entitlement, install, read-back, revocation — can be run and tested
before any of it touches a cluster. It is a reference implementation: the
smallest service that honours the contract, in the shape a product store
would take, and the one the contract tests run against. Billing, plans and
subscriptions stay out until the flow is proven.

The store holds no cluster credential and no identity toward the cluster
(AD-3). Everything it does at the director it does with the signed-in
person's token, or with a statement it signed.

- [x] **Repository and service.** In the store's own repository, one service
      per trust boundary over a shared account library; the store is FastAPI
      over SQLAlchemy (Postgres; SQLite in tests), runnable beside
      `director-dev` from that repository's dev compose. Its signing key is
      one setting with no fallback; without it every entitlement route is 503.
- [x] **Ingest.** `convert-appprofiles.sh` already produces
      `listings/<name>.yaml` per profile; the store ingests a directory of
      them plus the catalogue index (coordinate → profile name, bundle
      digest, source). It refuses a listing whose profile is not deployable
      as `tenant`. The catalogue index is the thing that ties
      `main/nextcloud` to the profile `nextcloud` — today nothing does, and
      the director's materialise-on-reference (WP-5) reads the same index.
- [x] **Listings API.** Locale fallback as the schema describes; per cluster,
      which catalogues it may see. Public: this is the shop window.
- [x] **Cluster registration.** A cluster registers with an id and the store
      publishes its signing keys for the platform administrator to pin on the
      Cluster claim (`store.signingKeys`); rotation is a new key published
      before it signs, the old one retired after.
- [x] **Entitlement statements.** Sign grants and revocations exactly as
      store-contract.md §2 states (compact JWS, EdDSA, `aud`/`sub`/`jti`/`iat`,
      `exp` on grants, `reason` on revocations); record each in
      `entitlement_grant`; deliver a grant through the signed-in tenant
      administrator's browser (`POST /v1/tenants/{t}/entitlements` with their
      token) and a revocation directly, with no token. Retry until the
      director answers `recorded` or `unchanged`; `409` is the store's own
      older statement and is final.
- [x] **OSS and paid, as one mechanism.** An OSS listing is granted on
      request with a far expiry; a paid one on a recorded purchase, and
      revoked on refund. No second code path: the difference is who may
      trigger the grant and when it ends.
- [~] **Install and read-back.** The website's checkout page and account page
      call the store from the browser with the person's GTC token; the
      install trigger and read-back live in the tenant desktop's store screen
      (WP-7), which delivers the grant it is handed. The store triggers
      `POST /v1/tenants/{t}/apps/{p}` with the person's token and the
      coordinate, and renders "installed / addons enabled / entitled until"
      from the director's reads — never from its own tables.
- [ ] **Private catalogue sources** (paid apps): a single-use fetch token
      issued with the grant, for the director to fetch the bundle at the
      digest; the pull credential handed to the credential manager as the
      tenant administrator. Lands with WP-5's materialise-on-reference, not
      before.
- [x] **Flow test**, runnable without a cluster — `tests/test_flow.py` in the
      store, against `director-dev -entitlements -store-keys …`; also run in
      that repository's CI with `director-dev` built from this branch. Passes:: store + `director-dev` (with
      `-entitlements`) — ingest two listings, register the dev cluster, pin
      the key, grant one entry to tenant demo, install it as tom (202),
      install the other (403), revoke, install again (403), replay the old
      grant (409), read the tenant's apps and entitlements as mia (200). The
      same test later runs against a real director in phase 2.
- [ ] **The store is optional (AD-14).** In `os`: `catalogue.sources[]` on
      the Cluster claim with `access: entitled|open` and `tenants`; the
      director reads each source's index, writes `catalogue_entry#source`
      and `catalogue_source#open` tuples from the claim, and serves
      `GET /v1/catalogues[/{source}/entries]` filtered by `can_install` for
      the tenant. `[x]` model v1: `catalogue_source#open`,
      `can_install: entitled or open from source`, tested. In `ui`: the
      store screen renders the director's index — name, version, install —
      when the store is unreachable or not configured, and the store's
      listings on top of it when it is.
- [ ] **Account tier and terms.** Sign-up records the tier (individual,
      organisation under the threshold, corporate or MSP) and the terms
      version accepted; a grant is refused without a current acceptance and
      records the tier and version it was issued under. Listings carry the
      upstream license and "distributed under the store terms"; GTC-authored
      components carry "commercial license". The desktop shows it per entry.
- [ ] **Not in the cluster, ever.** The `app-store-me` profile and its dead
      install paths are retired with WP-5; the cluster keeps the director's
      endpoint and, per tenant, only the installed profiles.

