# Work packages

Everything the architecture cleanup has to build, change or remove, grouped
by component. Each item names the plan that specifies it. Sequencing across
packages follows the waves of [security-gap-closing.md](security-gap-closing.md)
and the cutover steps of [operator-split-plan.md](operator-split-plan.md) §6;
where an item belongs to one of those it says so. The decisions behind the
packages are [architectural-decisions.md](architectural-decisions.md).

Repositories: `os` = gentian-os, `apps` = gentian-apps, `ui` = gentian-ui,
`deploy` = gentian-deployments, `store` = the external App Store (outside
these repositories; listed because the cluster side depends on it).

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
| 0 — no cluster | WP-1 cutover A (director against a bare repo, static JWKS, OpenFGA in a container); WP-3 model v1, tests, vocabulary check; WP-5 CRD schemas, CEL rules, profile conversion tooling; WP-6 store contract and grant format; WP-7 desktop and console against a mocked director | nothing | contract tests green |
| 1 — installer skeleton | the parts of WP-8 and WP-10 that produce an *empty* cluster in the target shape: labelled `kernel-*` namespaces, tier-0 operators, `kernel-data` with `kernel-postgres`, Keycloak and OpenFGA in their namespaces, OpenBao and the seal, the two Gateways; the step framework kept, step contents rewritten; ACME staging issuers while iterating | the purged cluster | `install.sh` stands up the empty layout repeatably; `--dry-run` and `--status` true |
| 2 — packages on the fresh cluster | WP-1 deployed (no side-by-side), WP-2, WP-4, WP-5 on-cluster parts, WP-9 wave 0 and signing, WP-8 remaining namespaces — each adding its installer step as it lands | phase 1 | each package's tests; the step's `check()` honest |
| 3 — handover and challenge | WP-10 `E-05`, credential split, challenge lists; WP-11 deployments layout; WP-13 toggles verified off and on | phase 2 | the challenge list passes as scripted tests on a fresh install |
| 4 — identities, audit, depth | WP-3 event feed and reconcile, WP-9 identities and agents, WP-4 log store and exposure view, WP-9 data-plane depth (gap-plan waves 2–4) | phase 3 | audit joins on one request id |
| existing clusters | operator-split-plan.md §6 B and D, or a rebuild from the recovery kit (AD-11) | a passing fresh install | per cluster |

## WP-1 Director — new binary (`os`)

Specified in [operator-split-plan.md](operator-split-plan.md) §3, §5, §6.

- [ ] **Step 0 (wave 0, G1):** bearer verification on the existing
      `internal/applifecycle/http.go` against kernel and tenant realms;
      `X-Gentian-Actor` ignored; BFF and CLI send the user's token.
- [ ] `cmd/director`, `internal/director/{api,authn,authz,gitops}`; plain
      Deployment, N replicas, no controller-runtime.
- [ ] Authentication: JWKS verification for kernel and tenant realms.
      **Sender-constrained tokens (DPoP, RFC 9449) for non-browser callers
      only** — the CLI, the App Store's install call, agent tokens — where a
      credential is held over time by something that can keep a key. Browser
      traffic is out of scope: the edge cookie never reaches JavaScript and
      the token stops at the gateway (AD-13). Scope before committing:
      Keycloak DPoP support, client configuration, and proof generation in
      the CLI and the store.
- [ ] Authorization: OpenFGA `Check` per verb against model v1 (WP-3);
      decision log entry per check with the request id.
- [ ] Git backend: copy of `applifecycle/gitops*.go`; commits authored as
      the human, committed by the director, **signed** with a key held in
      OpenBao transit (artefacts/roadmap-additions.md); trailer with the
      decision and request id; non-fast-forward retry as concurrency control.
- [ ] Write API `/v1/tenants/{t}/…`, `/v1/clusters/{c}/…`: apps, addons,
      plans, policies, exposure enablements, shared-app installs, tenant
      deploy/undeploy, raw edit (break-glass); every write returns 202 with
      an operation URL.
- [ ] Read API: tenants, apps, tiles filtered by `can_launch`, operations,
      exposure summary/log/objects (networking §8.4), audit views joining
      issuer log, decision log and git by request id.
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
- [ ] Platform tenant: `Tenant/platform` adopts the kernel realm; the
      identity reconciler adopts rather than creates it; undeletable
      (ui-restructure §2).
- [ ] Retire: `AppCatalogue` singleton and the catalogue ApplicationSet
      (catalogue leaves the cluster, AD-3); `authz_bridge_reconciler`;
      `app-privilege-requested` annotation kick.
- [ ] RBAC: no `argoproj.io` write verbs; `pods/exec` stays for purge.

## WP-3 Authorization — model, feed, groups (`os`)

Specified in [authorization-model.md](authorization-model.md) and
[roles-and-authorizations.md](roles-and-authorizations.md).

- [ ] Model v1 in `artefacts/model.fga`: types per CRD kind, roles via
      `group#member` only, `can_*` per verb, `admin` explicit (no `or
      member`), conditions for time.
- [ ] `tests.fga.yaml`: three cases per relation (grant, neighbouring
      denial, derivation through the parent).
- [ ] `make verify-authz-vocabulary`: every `can_*` and role noun in
      `docs/plans/*.md` exists in the model, and vice versa.
- [ ] Keycloak groups: `gentian:platform:{security,auditor,service-operator,shared-apps}`,
      `gentian:tenant:<t>:perimeter`; realm script and console.
- [ ] **Keycloak event listener** — a new kernel component in
      `kernel-authentication`: an event-listener SPI provider (or the
      community webhook listener, pinned and reviewed) that pushes signed
      membership and user events to the director's ingestion endpoint;
      replay protection by event id; the director is its only receiver; it
      holds no credential beyond the signing key. Inventory row in
      namespace-cleanup §2.1.
- [ ] Reconcile: periodic and on start, `view-users` client only, corrects
      toward Keycloak, reports drift; flags admin+member accounts.
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

- [ ] Two `Gateway` objects, `authenticated` and `perimeter`, in
      `kernel-edge` under `mergeGateways`; tenant listeners stay per-zone.
- [ ] `SecurityPolicy` per route: OIDC session per tenant zone (edge
      clients from WP-2), JWT for bearer routes, ext-auth for reachability;
      the edge client's scope emits no groups.
- [ ] **Ext-auth shim** — new component in `kernel-edge`: verifies the
      token, asks `can_use`/`can_enter`, caches per `(sub, sid, route)`,
      evicts by subject on a `ReadChanges` poll (OpenFGA has no push stream),
      denies on a `Check` transport error while previously cached allows
      carry until they expire; **receives Keycloak's back-channel logout** for
      every zone client and denies a revoked `sid` at L2 (AD-13);
      stateless, gRPC, topology-aware.
- [ ] DMZ publishing proxy image: generic Envoy/nginx with config rendered
      per surface — path allow/deny, `authMode` adapter (basic via the
      broker's passdb, bearer via JWKS, signature via HMAC from OpenBao),
      rate and body limits, one backend and port, one credential; Coraza
      with the OWASP core rules for `none` surfaces.
- [ ] Structured proxy access logs with tokens hashed; the cluster log
      store (G10); retention set by the security officer.
- [ ] Drift job: routed listeners, DMZ routes, DNS records and certificates
      reconciled against enablements.
- [ ] Vanity hosts: listener and HTTP-01 certificate per enabled host; DNS
      guidance; cloudflared hostnames from the same enablement.
- [ ] `system-mail` split: Postfix, spam filter and an optional Dovecot
      proxy in `system-mail-dmz`; store and DKIM milter in `system-mail`;
      `TCPRoute`s on the perimeter Gateway; external IMAP/submission a
      default-off surface.
- [ ] `system-turn` (coturn or SFU) as a DMZ-tier service with a `UDPRoute`
      and per-session HMAC credentials, when the first conferencing profile
      requires it.
- [ ] Default per-route rate limits in `BackendTrafficPolicy`; WebSocket
      max connection duration (G15).
- [ ] Keycloak route on the perimeter Gateway with the `/realms/*` path
      allowlist; `/admin`, `master`, metrics on an internal hostname
      (roadmap 1.6); brute-force detection per realm (G16).
- [ ] Exposure API and console view: summary, condensed log (most requests,
      most recent incl. first-seen, most bytes, rejections), public objects
      via the contract, complete log (networking §8.4).
- [ ] Retire `browserProxy`, `additionalIngresses`, the portal session
      bridges' routes.

## WP-5 Catalogue — `ComponentProfile` (`os`, `apps`)

Specified in [component-profile.md](component-profile.md).

- [ ] CRDs `ComponentProfile` and `Component`; CEL rules (§7); admission
      policies for who may create which tenancy where.
- [ ] Two-level tenancy; `trustTier` in spec; `requires` absorbing
      `kernelRequirements`, `optionalIntegrations`, `security`;
      `integrations`; `provides`; `secrets`; `expose[]` with mandatory
      `authMode` and `surface`; `extensions`; `hooks`.
- [ ] `ExposureEnablement` on the instance; cluster exposure policy on the
      Cluster claim (§5.1); `exposure-policy` contract (§5.2).
- [ ] Fulfiller selection: default per contract on the Cluster claim,
      mapping to the per-engine `system-*` namespaces (§9.1, AD-9).
- [ ] Conversion of the 31 `AppProfile`s in `apps/profiles/` to
      `ComponentProfile`; presentation fields move to the store; `apps`
      profiles gain `tenancy: [tenant]` and `expose[]` entries.
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
- [ ] Signed entitlement grants (`entitlement_grant`, `signing_key`);
      delivery to the director; single-use fetch token; pull credential
      handed to the credential manager as the tenant admin.
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
- [ ] All writes through the director with the user's token; all reads
      through the director's read API; `rbac.yaml` empty; `k8s_*` services
      removed.
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

- [ ] Step 1, labels first: `gentianos.io/tier`, `gentianos.io/function`,
      `gentianos.io/tenant` on every namespace; NetworkPolicy generation,
      Kyverno scoping and operator selectors use labels.
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

- [ ] Wave 0: G4 random LiteLLM keys from OpenBao behind the gateway; G5
      Redis ACL key and channel prefixes via `valueMapping.cache`; G6
      MariaDB wildcard grant on the tenant prefix, no `GRANT OPTION`.
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
      selectors for the modular kernel, and the `compliance` block of WP-13.
- [ ] Namespace label step (WP-8 step 1) and the fresh-install layout.
- [ ] Recovery kit carries the director's signing key material or its
      transit reference.
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
