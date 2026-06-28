# Gentian Cloud OS — Security Architecture & Nubus Replacement Plan

**Status:** Draft v0.2 · Architecture reference (Stage 0 progress tracked against [roadmap.md](../roadmap.md))
**Scope:** Identity, authorization, and isolation for a fully cloud-based, Kubernetes-native sovereign cloud OS, replacing the Univention Nubus IAM stack.

---

## 1. Purpose

Gentian needs an identity and access layer that is (a) simpler and more modern than Nubus, (b) fully open-source and sovereignty-friendly, (c) first-class for **four principal types** — humans, AI agents, applications/workloads, and assets — and (d) designed so that a *compromised principal of any type does the least possible damage*.

This document defines the security principles, the concrete Keycloak + OpenFGA architecture, how application permissions are modeled (AppProfile declaration, IntegrationBinding wiring, and the planned AppGrant ReBAC layer), and a staged plan to stand it up and retire Nubus.

The guiding idea, borrowed from Android's sandbox model: **least privilege is not a single access-control model — it is the intersection of several independent layers, each enforcing a different concern, so that breaching one layer does not collapse the others.**

---

## 2. Security principles

### 2.1 Core principle — defense in depth as an intersection

Android's "a compromised app can do little" property does not come from one mechanism. It comes from stacking independent enforcement layers: kernel DAC (each app is its own UID, owns its own files), a capability manifest (declared + granted permissions), and SELinux MAC (a system-wide label policy that even root cannot override). An app's effective reach is the **intersection** of sandbox ∩ granted permissions ∩ mandatory policy.

Gentian ports the *layering*, not any single model. Each of the four classic access-control models is assigned to the layer it is best at:

| Layer | Model | Job | Android analogue |
|---|---|---|---|
| **Mandatory isolation backbone** | **MAC** | Tenant boundaries, default-deny egress, "agent ≤ human" ceiling — true regardless of any authZ config | SELinux + inet GID |
| **Authorization plane** | **ReBAC** | Fine-grained "who may touch which resource," sharing, delegation | Binder caller-UID checks + file ownership |
| **Conditioning** | **ABAC** | Time, device posture, risk, data classification | Runtime permission prompts |
| **Ergonomic grouping** | **RBAC** | Human-friendly bundles of grants | Permission groups |
| **Per-task grant** | **Capabilities** | Short-lived, attenuated, audience-bound authority for an app/agent | The permission manifest + grant |

**Rule:** MAC is the backbone, ReBAC is the authorization plane, ABAC conditions it, RBAC is an ergonomic veneer over ReBAC, and capability tokens are the per-task least-privilege grant. **Effective access = the intersection of all layers; the most restrictive layer wins.**

### 2.2 The composition principle

Android composes identity: `effectiveUID = androidUserId × 100000 + appId`. The same app in a work profile vs. a personal profile gets two different sandboxes.

Gentian generalizes this to a **principal chain**:

```
tenant T  →  human U  →  agent A  →  workload W
```

Each layer checks its slice:
- **Tenant T** isolation is **MAC** (namespaces, network policy).
- **Human U** rights are a **ReBAC** ceiling.
- **Agent A** task scope is a **capability ⊆ U's rights**.
- **Workload W** reachability is **SPIFFE/mesh** identity.

Effective permission = the intersection of the whole chain.

### 2.3 The derived-ceiling invariant (most important single idea)

An agent or app acting for a user must be **mathematically incapable of exceeding that user's rights**. This is expressed natively in ReBAC by *deriving* the agent's access through the user rather than granting it independently:

```
# OpenFGA-style schema sketch
type document
  relations
    define reader: [user, group#member]
    define acting_for: [user]
    define valid_task: [task]              # task object carries TTL via Condition
    define can_read: reader or (valid_task and can_read from acting_for)
```

The agent reads a document **only if** it is the user's agent (`acting_for`), the task is still valid (TTL), **and** the user can read it. Consequences:
- The "agent ≤ human" ceiling is an **invariant of the model**, not a policy someone must remember to configure.
- **Revocation is one tuple delete** on `acting_for` — it transitively collapses all downstream access.
- Full auditability: every action attributes to the agent identity *and* the delegating human.

### 2.4 Blast-radius containment per principal type

**Compromised human user** — ReBAC confines to actual relationships; ABAC forces step-up auth on sensitive ops; MAC guarantees the tenant boundary holds regardless of tuple errors; short sessions cap the window.

**Compromised agent** (the dangerous case — autonomous and prompt-injectable):
1. Each agent *instance* is its own principal — never a shared service account (the "each app is its own UID" port).
2. Holds only a task-scoped, time-boxed capability token.
3. Rights derived from the delegating human (§2.3) — can never exceed them.
4. **Default-deny egress** — the most underrated Android lesson. Most agent damage is exfiltration or calling out; a NetworkPolicy egress allowlist is the inet-GID.

**Compromised app/workload** — namespace isolation (the UID sandbox); SPIFFE/mTLS identity so it reaches only what mesh policy permits (Binder checks); default-deny egress; per-workload short-lived secrets (no shared God credential); admission control blocking privilege escalation at deploy time (SELinux confining even privileged domains).

---

## 3. Architecture

### 3.1 Component roles

| Component | Role | License |
|---|---|---|
| **Keycloak** | Authentication authority + token issuer (*who you are*). Realms/Organizations for tenancy, service accounts for agents, RFC 8693 Token Exchange for on-behalf-of delegation, SAML/OIDC brokering for legacy public-sector IdPs. | Apache 2.0 |
| **OpenFGA** | ReBAC authorization PDP (*what you may do*). Relationship tuples for humans/agents/apps/assets; Conditions + contextual tuples for ABAC; the derived-ceiling schema. | Apache 2.0 |
| **Provisioning bridge** | Syncs Keycloak identity/group/role/agent events and SCIM into OpenFGA tuples; reconciles `IntegrationBinding` credentials and (future) `AppGrant` into the graph. | (build) |
| **MAC backbone** | K8s namespaces per tenant, Cilium/NetworkPolicy default-deny egress, service mesh + SPIFFE/SPIRE, admission control (Kyverno / OPA Gatekeeper). | Apache 2.0 / OSS |
| **PEP** | App / API gateway (Kong, Envoy, or in-app) calling OpenFGA `Check`, ideally over the OpenID **AuthZEN** Authorization API so PDPs stay swappable. | OSS |
| **ITAM source of truth (optional)** | NetBox (best license fit) / GLPI / Snipe-IT feeding device & asset objects into the graph. | Apache 2.0 / GPL / AGPL |

### 3.2 Why this replaces the Nubus stack

Nubus = OpenLDAP + Keycloak + Univention Directory Manager (UDM) + NATS provisioning + Guardian (RBAC) authorization, shipped as an umbrella Helm chart. For a greenfield, cloud-only sovereign OS:

- **Drop OpenLDAP + UDM.** Keycloak owns identities, backed by its own Postgres. Provisioning is event/SCIM-based, not LDAP replication.
- **Replace Guardian's RBAC with OpenFGA's ReBAC.** One relationship graph models humans, agents, apps, and assets — no role explosion, and (notably) Nubus does not yet wire any component to its own Authorization Service, so there is little to migrate.
- **Result:** fewer moving parts, no LDAP schema friction, first-class non-human identities, and the layered isolation model of §2 that Nubus does not provide.

### 3.3 Reference architecture

```
                ┌─────────────────────────────────────────────────────┐
                │  Administration / Self-Service Portal (Gentian UI)   │
                └───────────────┬─────────────────────────────────────┘
                                │ (SCIM, Admin API, Tenant.spec.apps, IntegrationBindings)
   Humans / Agents login        ▼
   ────────────────►   ┌──────────────────┐   OIDC / OAuth2 / SAML brokering
                       │    KEYCLOAK       │   RFC 8693 Token Exchange (act / may_act)
                       │  (IdP / AuthN)    │───────────────► tokens
                       │  realms/orgs,     │
                       │  clients,         │
                       │  service accounts │
                       └───┬───────────┬───┘
            events/SCIM    │           │ (optional) SPIFFE/SPIRE → SVIDs (mTLS)
        (users, groups,    │           │ for autonomous workload agents
         roles, agents)    ▼           ▼
                  ┌────────────────┐   ┌─────────────────────┐
                  │ Provisioning   │   │  Agents / Workloads  │
                  │ bridge:        │   └─────────┬───────────┘
                  │ KC→OpenFGA     │             │ acts via OBO token (≤ user)
                  │ + SCIM         │             ▼
                  │ + Integration  │   ┌─────────────────────┐
                  │   Binding      │   │  Apps / API Gateway  │ ◄── PEP
                  │ + AppGrant     │   │  (Kong/Envoy/app)    │
                  │   (future)     │   └─────────┬───────────┘
                  │ + ITAM conn.   │             │ AuthZEN Check
                  └───────┬────────┘             │
                          │ writes               │
                          ▼ tuples               ▼
                  ┌────────────────────────────────────────────┐
                  │              OPENFGA  (ReBAC PDP)            │
                  │  user:* agent:* app:* group:* tenant:*       │
                  │  document/db:* device:* task:* contract:*    │
                  │  Conditions (TTL / ABAC) · derived-ceiling   │
                  └───────────────┬──────────────────────────────┘
                                  ▲ device/asset + contract-consumer edges
                  ┌───────────────┴──────────────┐
                  │  ITAM source of truth (opt.)  │
                  │  (NetBox / GLPI / Snipe-IT)   │
                  └───────────────────────────────┘

        ┌─────────────────────────────────────────────────────────┐
        │  MAC BACKBONE (peer to all of the above, not inside it): │
        │  K8s namespaces/tenant · NetworkPolicy default-deny      │
        │  egress · service mesh + SPIFFE · Kyverno/OPA admission  │
        └─────────────────────────────────────────────────────────┘
```

**Decision flow:** (1) principal authenticates to Keycloak → OIDC token (agents via client-credentials or Token Exchange carrying `act`). (2) Identity/role/group/agent events + SCIM flow through the bridge → relationship tuples in OpenFGA; `IntegrationBinding` reconciles cross-app credentials today; (future) `AppGrant` reconciles tenant-approved ReBAC edges. (3) PEP receives request + token, calls OpenFGA `Check` (over AuthZEN), passing token claims as contextual tuples for session context. (4) OpenFGA traverses the graph (principal → group/org → resource/device, plus task-scoped delegation with TTL Conditions, plus derived-ceiling) → allow/deny. (5) Independently, the MAC backbone enforces tenant isolation and egress *regardless* of the authZ result. (6) Sensitive ops use consistent reads; the Watch API streams tuple changes to an audit log.

### 3.4 Application permissions — catalogue contracts and grants

Cross-app and kernel access in Gentian is declared in **`AppProfile`**, wired by **`IntegrationBinding`**, and (once Stage 2 ReBAC is live) constrained by a future **`AppGrant`**. This mirrors Android's manifest (`<uses-permission>` = intent) vs. the separate platform/user grant — **the app declares; it never grants itself access to another tenant or app.**

Full CRD field reference and deployment flow: [app-catalogue.md](app-catalogue.md).

#### Terminology — manifest language vs CRD fields

This document and older drafts used *consumes* / *publishes*. The **implemented** `AppProfile` CRD uses different field names:

| Concept (this doc) | `AppProfile` CRD field | Type |
|---|---|---|
| Contracts the app **provides** to peers | `spec.provides[]` | `{ name, protocol? }` |
| Contracts the app **may consume** from peers | `spec.optionalIntegrations[]` | `{ contract, provider?, capabilities? }` |
| Kernel services (OIDC, Postgres, S3, …) | `spec.kernelRequirements` | Separate from integration contracts |

Contract **names** (e.g. `file-store`, `project-management`) are shared vocabulary. Definitions live under `gentian-apps/contracts/` (when present) and are referenced by name only in profiles — the profile does not embed the full contract schema.

#### Three layers — declaration, wiring, authorization

| Layer | CRD / object | Scope | Author | Status |
|---|---|---|---|---|
| **Declaration** | `AppProfile` | Cluster (one per catalogue entry) | Catalogue maintainer (`gentian-apps/profiles/`) | **Implemented** |
| **Wiring** | `IntegrationBinding` | Namespace (per tenant, per provider↔consumer pair) | gentian-os operator (auto when peers match) | **Implemented** |
| **Grant (ReBAC)** | `AppGrant` (planned) | Per tenant install | Tenant admin at install | **Planned** |

Do not conflate them:

- **`kernelRequirements`** — what the **platform kernel** must provision (OIDC client, database, mail, …). Validated at admission; secrets injected via `valueMapping` + OpenBao. Not a cross-app contract.
- **`provides` / `optionalIntegrations`** — what the app **offers to or may use from other catalogue apps**. Optional until peer apps are installed.
- **`IntegrationBinding`** — the **runtime wire** when both provider and consumer are present in `Tenant.spec.apps`: credentials in OpenBao, OIDC token exchange, capability list. Owned by the `Tenant`; garbage-collected on delete.

#### 1. Declaration — `AppProfile` (developer-authored, static)

`AppProfile` is **cluster-scoped** — one YAML per app type in the catalogue, shared across all tenants. It is the *upper bound* of what the app can request, not an authorization decision.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppProfile
metadata:
  name: openproject                    # cluster-scoped catalogue id
spec:
  displayName: "OpenProject"

  # Kernel — platform-provisioned services (NOT integration contracts)
  kernelRequirements:
    identity:
      oidc:
        clientId: opendesk-openproject
        accessType: CONFIDENTIAL
    database:
      engine: postgresql
      databasePerTenant: true

  # Integration contracts this app PROVIDES to other apps
  provides:
    - name: project-management         # kebab-case; matches contract definition name
      protocol: http-json

  # Integration contracts this app MAY CONSUME when a provider is installed
  optionalIntegrations:
    - contract: file-store
      provider: nextcloud              # expected provider profile name (optional hint)
      capabilities: [webdav:read, webdav:write]
    - contract: central-navigation
      provider: portal
      capabilities: [navigation:register]

  chart:
    repository: oci://registry.example/charts
    name: openproject
    version: "14.2.0"

  valueMapping:                        # maps kernel outputs → Helm keys (Pattern A secrets)
    oidc:
      issuerKey: "oidc.issuer"
      clientIdKey: "oidc.clientId"
      clientSecretKey: "oidc.clientSecret"
    # … database, s3, smtp, ldap, cache …
```

**`provides`** entries identify contract names the app implements as a **provider**. **`optionalIntegrations`** entries identify contract names the app can use as a **consumer**, with optional `capabilities` (the requested capability surface, not yet a grant).

Tenant admins select apps by **profile name** in `Tenant.spec.apps` — they do not edit `AppProfile`.

#### 2. Wiring — `IntegrationBinding` (operator-authored, per tenant)

When the gentian-os operator reconciles a `Tenant` and finds both a **provider** (profile with `spec.provides` containing the contract) and a **consumer** (profile with matching `spec.optionalIntegrations[].contract`) in `spec.apps`, it creates an **`IntegrationBinding`** in the tenant namespace:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: IntegrationBinding
metadata:
  name: demo-file-store
  namespace: tenant-demo
spec:
  contract: file-store
  provider:
    app: nextcloud
    namespace: tenant-demo
  consumer:
    app: openproject
    namespace: tenant-demo
  capabilities: [webdav:read, webdav:write]
  auth:
    method: oidc-token-exchange
    vaultPath: gentian-os/tenants/demo/contracts/file-store
status:
  state: Ready
```

This object **provisions credentials and auth method** between two installed apps. It is topology + secret wiring — not user-level ReBAC. Apps receive injected values via Helm/`valueMapping`; they must not implement their own cross-app grant logic (see [app-catalogue.md](app-catalogue.md) §4).

#### 3. Grant — `AppGrant` (tenant-authored, per install) — **planned**

Stage 2 adds a tenant-facing grant that is a **subset** of what `AppProfile` declared — decided at install or by tenant admin, never by the app vendor:

```yaml
# Planned CRD — not yet in gentianos.io/v1alpha1
kind: AppGrant
metadata:
  namespace: tenant-demo
spec:
  app: openproject
  consume:
    - contract: file-store
      granted: [webdav:read]             # webdav:write withheld vs optionalIntegrations
  allowConsumers:                        # publish side — who may call this app's provides
    - app: crm-app
      contract: project-management
      scope: [tasks:read]
```

**Publishing is an authorization surface too.** A provided contract becomes a **resource object** in the ReBAC graph; a consumption grant becomes a **relationship tuple**. `AppProfile.spec.provides` declares the node; (future) `AppGrant.allowConsumers` creates the edge:

```
contract:demo/project-management#consumer@app:crm-app
```

"May CRM read OpenProject tasks?" is then a single OpenFGA `Check`; the tenant controls the edge; revocation is one tuple delete.

#### 4. Runtime authorization — computed at the PEP (per request)

**Today (Stage 0 MAC + legacy IdP):** OIDC authentication (Keycloak via the Nubus stack), tenant MAC isolation, and `IntegrationBinding` wiring. User-level ReBAC on app APIs is not platform-wide until OpenFGA is deployed (Stage 1); first-party apps carry **PEP stubs** (`openfga_client.py` in `gentian-app-template`) that pass through when `OPENFGA_API_URL` is unset.

**Target (Stage 2+):**

```
effective access = declared (AppProfile)
                 ∩ wired (IntegrationBinding exists + credentials valid)
                 ∩ granted (AppGrant subset, future)
                 ∩ acting-user ceiling (ReBAC)
                 ∩ conditions (ABAC)
```

The most restrictive layer wins (§2.1).

### 3.5 Agentic identity

- Each agent is a **distinct first-class identity** — a dedicated Keycloak client/service account and an `agent:` object in OpenFGA — never a shared human credential.
- Tokens are short-lived: client-credentials for autonomous agents; **RFC 8693 Token Exchange** with `act` / `may_act` for on-behalf-of a user.
- Delegation lives in the graph via the **derived-ceiling** schema (§2.3); TTL enforced by OpenFGA **Conditions**; revocation = tuple delete.
- For agent/tool endpoints, adopt the **MCP authorization** model (OAuth 2.1 resource server: validate audience, require PKCE, RFC 8707 resource indicators, no token passthrough). Track **Cross-App Access / ID-JAG** so Keycloak can later mediate agent→app access centrally.
- Add **SPIFFE/SPIRE** only when autonomous in-cluster workload agents need secret-less mTLS identity — a layer *beneath* OAuth/ReBAC, not a replacement.

*Standards note (2025–2026): the industry is converging on extending OAuth/OIDC/SPIFFE rather than inventing agent-specific protocols (IETF WIMSE, `draft-klrc-aiagent-auth`, OpenID AIIM/AuthZEN, NIST agent-identity work). Architect for these primitives; treat the specs as still in flux.*

### 3.6 Physical assets & ITAM

Model devices as plain **resource objects** now: `type device` with relations `owner`, `assigned_user`, `operator`, `maintainer`, inheriting org scope (`device:printer-3f#can_print@user:alice`; agents the same way via `#operator@agent:print-bot`). Evolve toward full ITAM only when inventory grows: add **NetBox** (Apache 2.0, best license fit) / **GLPI** / **Snipe-IT** as the asset source of truth, projected into OpenFGA tuples by the same provisioning bridge. Keep this layer thin.

### 3.7 Automation (n8n-like workflows)

An automation platform is a textbook **confused deputy**: a central engine holding many services' credentials and combining them in flows. Dropped in unmodified it becomes the god-mode lateral-movement engine this architecture exists to prevent. The fix is to decompose it along the same seams as everything else — **never one principal, never a central credential vault.**

| n8n concept | Maps to | Enforcement |
|---|---|---|
| The n8n **platform** | Catalogue `AppProfile` + tenant `App` install (§3.4) | `kernelRequirements` + MAC-confined namespace + **default-deny egress** allowlisted to declared connector endpoints |
| Cross-app **connectors** | `optionalIntegrations` → `IntegrationBinding` | Operator-wired credentials; per-step token exchange — not a shared vault |
| A **workflow** | First-class principal / agent instance (§3.5) — *one identity per workflow*, the "each app is its own UID" port | Own `workflow:` (or `agent:`) identity; no shared vault |
| A **workflow execution** | `task:` object with TTL Condition | `user → owns → workflow → executes_as → task(ttl)`; revocation = delete `acting_for` |
| **Credentials** | JIT short-lived scoped tokens | Requested per-step from Keycloak via **RFC 8693** token exchange — no stored long-lived secrets |
| A user-owned workflow | Derived-ceiling delegation (§2.3) | `workflow ≤ owning-user` — cannot touch what the owner can't |
| A system/scheduled workflow | Machine identity | Keycloak **client-credentials** + explicit narrow grant (no human ceiling) |
| Each **node/step** touching a resource | A PEP `Check` (§3.3) | Per-step authorization, each bounded by the ceiling and the egress allowlist |
| A step needing access beyond its grant | **Human-in-the-loop** approval | AuthZEN Access Request & Approval Profile → time-boxed elevated grant |

**The two inversions from vanilla n8n:** (1) replace the central credential vault with **just-in-time, scoped, attenuated tokens**; (2) replace the single platform identity with **one identity per workflow**, each running under the derived-ceiling. A "read CRM contacts → post to Slack" flow then becomes two PEP checks plus two egress-allowlist gates, every action attributed to (workflow identity + owning user) via the `act` chain — instead of one over-privileged deputy with standing access to everything.

---

## 4. Implementation plan — replacing Nubus

A staged path. Each stage is independently useful and leaves a working system.

### Stage 0 — Foundations (MAC backbone first)

*Rationale:* the isolation backbone must exist before identities and grants, so tenant boundaries and egress limits hold independently of any later authZ config. This is the layer Nubus does not provide.

**Prototype exit criteria (met on `feat/new-security`):** a tenant namespace exists; default-deny egress holds; kernel and contract egress are operator-managed; Kyverno blocks privileged/host-namespace workloads in tenant namespaces.

| Item | Status | Implementation / notes |
|------|--------|-------------------------|
| Per-tenant **K8s namespaces** | **Done** | `tenant-default` Composition; operator seeds tenant shell |
| Default-deny **NetworkPolicy** (tenant MAC floor) | **Done** | `tenant-isolation` — DNS + kube-API egress only; ingress from gateway/ingress namespace ([`internal/kernel/netpolicy/baseline.go`](../../internal/kernel/netpolicy/baseline.go)) |
| **Kernel egress** from `AppProfile.kernelRequirements` | **Done** | `kernel-access-{app}` policies ([`kernel.go`](../../internal/kernel/netpolicy/kernel.go)) |
| **Contract egress** from active `IntegrationBinding` | **Done** | `contract-{binding}` policies; all declared binding capabilities allowed until AppGrant ([`integration.go`](../../internal/kernel/netpolicy/integration.go)) |
| **Kyverno admission** (privileged, host namespaces, non-root) | **Done** | `kernel/appsets/05-admission.yaml`, `kernel/security/kyverno/policies/gentian-baseline.yaml`; install Step 13c |
| **Cilium** (FQDN/L7 egress, Hubble) | **Deferred** → [roadmap § Cilium](../roadmap.md#cilium-planned) | Standard Kubernetes NetworkPolicy is sufficient for Stage 0 prototype |
| **Service mesh + SPIFFE/SPIRE** | **Deferred** → [roadmap § Service mesh](../roadmap.md#service-mesh--spiffespire-planned) / **Stage 3** below | Edge TLS is Envoy Gateway; workload mTLS is not required yet |
| **AppGrant-governed contract allowlists** | **Deferred** → **Stage 2** | Bindings allow full declared capability set today |
| **Image provenance / cosign / SLSA at admission** | **Deferred** → [roadmap § Horizon A](#horizon-a--harden-immediately-post-rollout) | Not in baseline Kyverno policies yet; see [app-catalogue-security.md](app-catalogue-security.md) |
| **Required workload labels / tier enforcement** | **Partial** → [roadmap § App catalogue security](../roadmap.md#app-catalogue-security) | Baseline pod security only; `catalogue-tier` webhook and prod Kyverno tier rules not shipped |
| **Egress drift detection (managed as code)** | **Deferred** → [roadmap § Horizon A](#horizon-a--harden-immediately-post-rollout) | Policies are reconciled by the operator but not continuously audited |

Stage 0 is **complete for dev/homelab**. Remaining rows are optional upgrades (Cilium, mesh) or post-rollout hardening — not blockers for starting Stage 1.

### Stage 1 — Identity + authorization core

Deploy the **Keycloak + OpenFGA** pair and the first **provisioning bridge** so authorization is graph-backed rather than LDAP-group + app-local checks alone.

**Naming:** the Crossplane composite for this pair is **Suze** (after the gentian-root bitter) — claim kind `Suze`, composite `XSuze`, claim `dev-suze`. It sits alongside **InfraData** for shared databases. Component Helm releases remain `gentian-idp-keycloak` and `gentian-openfga`.

| Item | Status | Notes |
|------|--------|-------|
| **Keycloak** (realms, OIDC clients, SAML brokering) | **Done (Stage 1 path)** | Standalone Keycloak via **Suze** XR (`gentian-idp-keycloak`); OpenDesk Nubus deploy commented out in `install.sh` |
| Drop **OpenLDAP + UDM**; Keycloak authoritative | **In progress** | `IDENTITY_MODE=keycloak-native` skips LDAP/UDM provisioning; OpenDesk stack deploy commented out in `install.sh` |
| **`provider-keycloak` Realm MRs** (drift-safe tenant realms) | **Blocked** → [roadmap § Keycloak](../roadmap.md#keycloak--provider-keycloak-consolidation) | Tenant realms still provisioned via manifest-bridge Jobs |
| Deploy **OpenFGA** + Postgres store | **Done** | **Suze** XR + `kernel/appsets/09-suze.yaml`; shared Postgres `openfga` database |
| Author **base authorization model** (`user`, `group`, `tenant`, derived-ceiling) | **Done** | `authz/model/v0/model.fga`, `model.json`, `tests.fga.yaml`; embedded in operator bootstrap |
| **Keycloak→OpenFGA event publisher** + SCIM sync | **Done (v0 reconcile)** | `AuthzBridgeReconciler` periodic Keycloak user → OpenFGA tuple sync (no SCIM yet) |
| **PEP** calling OpenFGA `Check` on real requests | **Done (reference path)** | `gentian-ui` `/api/v1/apps` uses `require_shell_launch()` when `OPENFGA_*` env is set |
| OIDC validation in first-party apps | **Partial** | Template + `gentian-ui` dogfood JWT validation; not enforced catalogue-wide |

**Stage 1 exit criteria** (from original plan): a human authenticates via Keycloak and a PEP makes a correct `Check` against OpenFGA for a real resource — **met on the default install path** (`install.sh` Steps 14–15).

#### What is missing to start / finish Stage 1?

Stage 1 **minimum work package is implemented** on `feat/new-security` (`install.sh` Steps 14–15). Remaining polish (not blockers for Stage 2):

1. **SCIM / event-driven sync** — bridge uses periodic reconciliation; Keycloak event publisher can replace polling.
2. **provider-keycloak Realm MRs** — tenant realms still use shell Jobs until upstream gaps close ([roadmap § Keycloak](../roadmap.md#keycloak--provider-keycloak-consolidation)).
3. **Gateway-level AuthZEN** — PEP is wired in `gentian-ui` BFF; Envoy external auth is Stage 2.
4. **App-template PEP parity** — copy `require_shell_launch()` pattern into `gentian-app-template` for catalogue apps.

**Not required for Stage 1** (correctly deferred): AppGrant CRD (Stage 2), AuthZEN standardization (Stage 2), agent service accounts / Token Exchange (Stage 2), SPIFFE (Stage 3), ITAM (Stage 3), cosign admission (Horizon A).

**Already in place and reused by Stage 1:** Stage 0 MAC (tenant isolation holds regardless of authZ bugs), `IntegrationBinding` credential wiring (Stage 2 will add ReBAC on top), app-template auth/OIDC stubs.

### Stage 2 — App permissions, agents, and the PEP

| Item | Status |
|------|--------|
| **`AppProfile` + `IntegrationBinding`** | **Done** (§3.4) |
| **`AppGrant` CRD** + OpenFGA tuple reconciliation | **Planned** |
| **AuthZEN** PEP↔PDP interface | **Planned** |
| **Agent identities** (Keycloak SA, RFC 8693, OpenFGA Conditions) | **Planned** |

- Extend the provisioning bridge to reconcile binding health and credentials into observability, and introduce the planned **`AppGrant`** CRD so tenant-approved subsets — including publish-side `allowConsumers` — become OpenFGA tuples.
- Standardize the **PEP↔PDP** interface on the OpenID **AuthZEN** Authorization API so gateways/apps aren't coupled to OpenFGA.
- Make agents first-class: `agent:` object type, Keycloak service accounts, **RFC 8693** Token Exchange with `act`/`may_act`, TTL via OpenFGA **Conditions**, revocation via tuple delete.
- *Exit criteria:* an app installed into a tenant operates within `AppProfile` declaration ∩ `IntegrationBinding` wiring ∩ (future) `AppGrant` ∩ user-ceiling; an agent acting for a user provably cannot exceed that user; revoking delegation is a single tuple delete.

### Stage 3 — Assets, ITAM, hardening
- Model devices as resource objects; if inventory warrants, introduce **NetBox/GLPI/Snipe-IT** and feed the graph via the bridge.
- Adopt **MCP OAuth 2.1** resource-server discipline for agent/tool endpoints; evaluate **Cross-App Access/ID-JAG** with Keycloak as issuer.
- Add **SPIFFE/SPIRE** workload identity for autonomous in-cluster agents where secret-less mTLS is needed.
- Turn on consistent reads for security-critical writes; wire the OpenFGA **Watch API** to an immutable audit log.

### Migrating off Nubus specifically
1. **Identities:** export users/groups from OpenLDAP/UDM; import into Keycloak (its own Postgres store). Decommission OpenLDAP and UDM once Keycloak is authoritative.
2. **Provisioning:** replace NATS/UDM provisioning with the event/SCIM bridge → OpenFGA.
3. **Authorization:** there is little to migrate from Guardian (RBAC, and not yet wired into Nubus components); author the equivalent grants directly as ReBAC tuples and (future) `AppGrant` objects atop existing `IntegrationBinding` wiring.
4. **Run in parallel** during cutover: Keycloak can broker to the legacy IdP while apps are migrated app-by-app behind the AuthZEN PEP.

---

## 5. Roadmap — after the rollout

Section 4 gets the architecture *running*. This section is what turns a running system into a defensible, sovereign-grade one. The ordering reflects leverage: the first three items are cheap, high-impact, and shippable immediately after Stage 1–2; the later horizons require real investment and are sequenced before you onboard production public-sector tenants.

### The five most important next steps (in priority order)

1. **Policy-as-code testing in CI (do this first).** Treat the OpenFGA model like application code: an assertion suite (`.fga.yaml`) with an emphasis on **negative tests** — "an agent must *not* read a doc unless `acting_for` a reader," "tenant A must *not* reach tenant B" — run as a merge gate on every model change. This converts the derived-ceiling and tenant-isolation invariants from *hopes* into *checked properties*, and it directly addresses the silent-authz-bug weakness. Cheapest, highest-leverage item on the list.
2. **Supply-chain integrity at admission.** Enforce **Sigstore/cosign signature verification + SLSA provenance + SBOMs** so only signed, provenance-attested images run. For a product assembling many third-party components (openDesk, Odoo, n8n, …) this is the most likely real-world compromise vector — and a genuine sovereignty/GTM differentiator, not just hygiene.
3. **Continuous authorization (close the revocation window).** Adopt **Shared Signals / CAEP** so Keycloak pushes revocation and session-change events to resource servers in near-real-time, instead of waiting for token expiry. This matters most for agents, which act fast: it closes the gap between "revoked in Keycloak" and "token still valid" that bridge-lag and TTLs otherwise leave open.
4. **Detection on the decision logs (assume prevention fails).** Everything in §2–§4 is preventive. Add anomaly detection on OpenFGA decision logs (an agent checking 10× more objects, odd-hours access, a workflow touching resource types it never has) and runtime threat detection (Falco) on the MAC layer. The Watch API audit log should *alert*, not just record — this is what catches the incomplete-mediation breach, which otherwise fails silently.
5. **Break-glass + operator-side PAM.** Define an emergency-access path for when the IdP itself is down/misconfigured (separate credentials, loud alerting), and constrain *Gentian's own operators* with just-in-time admin elevation, dual-control for sensitive ops, and session recording. The sovereignty pitch is "even we can't quietly access your data" — that claim lives or dies on operator-side controls, so this is essential before real tenants, even if it can lag in the prototype.

### Horizon A — Harden (immediately post-rollout)
Items 1–3 above, plus: drift detection on egress/NetworkPolicy (the erodible MAC floor) managed as code; consistent reads enabled for security-critical writes; mTLS-required mesh and **no direct datastore access** made structural (all data reached through contract-mediated APIs that pass the PEP) so there are no off-PEP paths to mediate.

### Horizon B — Detect & respond (before first production tenant)
Items 4–5 above, plus: an incident runbook tied to the audit log; tabletop a "compromised agent" and a "compromised operator" scenario; verify revocation actually propagates end-to-end (tuple delete → CAEP signal → token rejected) under test.

### Horizon C — Sovereign data tiering (as data sensitivity demands)
Implement the **tiered encryption** model. Encryption at rest and in transit are unconditional baselines. Above them, two tiers routed per dataset by the question *"does this need server-side features?"*:
- **Vault tier** — client-held keys with **envelope encryption / per-recipient wrapped DEKs**, where a relationship tuple and a wrapped key are two faces of the same grant (`document:X#reader@user:alice` ↔ `DEK_X` wrapped for Alice). Agents become genuine crypto endpoints via DEKs wrapped under their ephemeral session key (the cryptographic expression of the derived-ceiling). Accepts loss of server-side features for that tier. Hardens incomplete mediation: an off-PEP path hits ciphertext it has no key for.
- **Collaborative tier** — **confidential computing (TEE/enclave: AMD SEV-SNP, Intel TDX) + BYOK/HYOK**, where the server processes plaintext (search, co-editing, agents) but inside attested hardware the operator cannot introspect. This is the realistic sovereign answer for feature-rich data.
- Adopt a per-principal **key hierarchy** (tenant → user → per-object DEK) mirroring the composition chain, giving **crypto-shredding** as a revocation primitive alongside tuple-delete. Plan key recovery/escrow explicitly — public-sector clients will not accept "lose your key, lose your data."

*Known limits to design around (not solvable by crypto): server-side features still need a plaintext-holding recipient; agent access means the agent reads plaintext (least-privilege access, not zero-access); revocation can't reach already-decrypted data; and wrapped-key recipients still leak access **metadata** (who-can-read-what) even when content is opaque.*

### Horizon D — Evolve with the standards (ongoing)
Track and adopt as they stabilize: **ID-JAG / Cross-App Access** with Keycloak as issuer for central agent→app mediation; **IETF WIMSE** for workload identity; **ReBAC delegation overlays** for runtime sessions, agent-to-agent, and workflow-scoped authority (the `agent`/`session`/`task` types built in Stage 2 are the hooks). Keep betting on OAuth/OIDC/SPIFFE primitives rather than any single proprietary agent framework. Optionally encode the two or three load-bearing invariants (tenant isolation, agent ≤ human) as explicitly verified properties — the principle behind formally-verified engines like Cedar — even while staying on OpenFGA.

---

## 6. Licensing & sovereignty summary

- **Apache 2.0 (ideal):** Keycloak, OpenFGA, SpiceDB, Ory core, OPA, NetBox, Cilium, SPIRE.
- **AGPL-3.0 (copyleft — disclose service-side modifications):** Zitadel v3+, Permify, Snipe-IT.
- **GPL:** GLPI.
- **Recommendation:** the Keycloak + OpenFGA core is fully Apache 2.0 — the cleanest fit for an open-core commercial product where you may ship a *modified* IdP/authZ engine as part of a managed Gentian offering. Self-host on EU/Swiss infrastructure for full vendor independence. Zitadel remains the strong sovereignty-branded alternative if native multi-tenancy outweighs the AGPL constraint and you don't need to *consume* upstream SAML.

---

## 7. Open questions / caveats

- **Dual-write consistency:** syncing identities into a separate graph introduces a consistency window (the Zanzibar zookie problem). Use event-driven sync with reconciliation; consistent reads for sensitive checks.
- **Agent-identity standards are in flux (2025–2026):** ID-JAG, the IETF agent-auth draft, OIDC-A, NIST guidance are early. Architect for OAuth/OIDC/SPIFFE primitives, not any single proprietary agent framework.
- **ReBAC schemas need extension for advanced delegation** (runtime sessions, agent-to-agent, workflow-scoped authority) — active research. Build `agent`/`session`/`task` as explicit graph types now to adopt overlays later.
- **Operational cost:** Keycloak (JVM) + OpenFGA + MAC backbone + mesh is more moving parts than a single binary. Budget DevOps capacity; SPIRE adds further weight when adopted.
