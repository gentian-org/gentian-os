# Agentic AI Layer

**Companion to:** [architecture-crossplane.md](../architecture-crossplane.md)

---

## 1. Why MCP Belongs in the Kernel

Gentian OS treats the **Model Context Protocol (MCP)** the same way a
desktop OS treats system APIs: a stable, discoverable surface that
applications expose so other applications — including AI assistants —
can act on user data without bespoke integration.

Without an OS-level capability layer, every AI integration is
N×M: every agent must be taught every app's API, with separate
authentication, rate limits, and schemas. MCP collapses this to N+M:
each app exposes one MCP endpoint; each agent talks one protocol.

This is the agentic-era equivalent of what desktop OSes did for
clipboard, file pickers, and inter-process messaging — a shared
contract apps participate in, owned by the OS.

## 2. Contracts vs MCP

The platform already has two layers of inter-app contracts. MCP is
the third:

| Layer | Purpose | Consumer |
|---|---|---|
| **Kernel requirements** (`AppProfile.spec.kernel`) | Identity, storage, mail, DB | Provisioned by the platform |
| **App contracts** (`AppProfile.spec.contracts`) | App-to-app integration (e.g., OpenProject ↔ NextCloud) | Other apps |
| **MCP capabilities** (`AppProfile.spec.mcp`) | Agent-readable operations (`searchTasks`, `createIssue`) | AI agents, shell assistant, cross-app workflows |

A single `AppProfile` declares all three. Contracts are
machine-to-machine via stable APIs; MCP is agent-to-machine via a
discoverable, semantic interface.

## 3. MCP as a Kernel Requirement

Apps declare their MCP surface in `AppProfile.spec.mcp`:

```yaml
spec:
  mcp:
    endpoint: /mcp                # path on the app's main service
    auth: oidc                    # uses kernel identity, no separate creds
    capabilities:
      - name: searchTasks
        description: Search tasks by query
        scope: read
      - name: createIssue
        description: Create new issue
        scope: write
      - name: updateStatus
        description: Update task status
        scope: write
```

When the Composition reconciles the app, it also:

1. Registers the app's MCP endpoint with the **MCP registry**
   (a kernel service exposing the catalogue of all live MCP
   endpoints in the cluster, scoped per tenant).
2. Configures the OIDC client so MCP calls authenticate via Keycloak
   token exchange — agents present the user's token, the app trusts
   the kernel issuer.
3. Publishes the capability list under the tenant's namespace,
   discoverable via `kubectl get mcpcapabilities -n tenant-{name}`.

Apps without MCP support simply omit the `mcp:` block; the platform
treats them as agent-opaque but still useful.

## 4. Shell AI Assistant

A built-in **shell assistant** (Univention Portal extension) can be
enabled per tenant. When a user asks "show me my open tasks across
all my apps", the assistant:

1. Queries the MCP registry for the tenant's apps with `read`-scope
   capabilities matching `tasks`.
2. Performs OIDC token exchange to obtain per-app access tokens
   on behalf of the user.
3. Calls `searchTasks` on each matching app via MCP.
4. Aggregates and returns the results.

The assistant is itself an agent that runs **as the user** — it has
no privileges the user doesn't have. Cross-tenant queries are
structurally impossible because the registry, OIDC issuer, and
network policies are all tenant-scoped.

## 5. Cross-App Agent Orchestration

MCP enables genuine workflow automation across apps without
point-to-point integrations. Examples:

- **"Invoice arrived in OX Mail → create task in OpenProject →
  notify finance channel in Element."** Three apps, three MCP
  capabilities (`getAttachment`, `createTask`, `sendMessage`),
  composed by an agent — no shared schema, no custom integration code.
- **"Customer signed contract in NextCloud Sign → provision their
  account in OpenProject + invite them to a Jitsi room."** A
  workflow agent registered with the MCP registry watches for
  signing events and triggers downstream capabilities.
- **"Daily summary: open issues, calendar conflicts, pending docs
  needing review."** A scheduled agent walks read-scope capabilities
  and produces a digest in the user's preferred channel.

These workflows are tenant-defined (lives in the tenant's own
namespace, uses the tenant's identity, scoped to that tenant's apps)
— the platform provides the substrate, not the workflows.

## 6. AI-Assisted Platform Operations

The same MCP fabric powers operator-side automation:

- **AppProfile generation:** an agent reads a Helm chart's
  `values.yaml`, infers the kernel requirements (does it need OIDC?
  S3? mail?), and proposes an `AppProfile` — a human reviews and
  commits to `gentian-apps`.
- **Tenant provisioning assistant:** "spin up a new tenant for ACME
  Corp with NextCloud, OpenProject, Element, mail mode external,
  isolation namespace" — produces the Tenant CR for review.
- **Health monitoring agent:** continuously walks
  `kubectl get tenants,integrationbindings,applications` outputs,
  correlates with metrics (see [operations.md](operations.md)), and
  raises summaries in the operator chat — "tenant `beta-inc` has
  binding `nextcloud↔openproject` degraded for 12m; root cause:
  NextCloud OIDC client secret rotation didn't roll OpenProject pods
  (Reloader annotation missing)".
- **Migration planner:** for kernel version upgrades, an agent walks
  the diff between two kernel versions and predicts which tenants
  need attention.

These are agents that the **platform team** runs against the
cluster's read-scope MCP surface. They are bound by the same OIDC
identity and RBAC model as any human operator.

## 7. Implementation Roadmap

| Phase | Capability | Rationale |
|---|---|---|
| **v1** | MCP registry + per-app `mcp:` block in AppProfile + 2–3 reference apps (NextCloud, OpenProject, Element) exposing read-scope capabilities | Establish the contract, prove the loop end-to-end |
| **v2** | Shell AI assistant (Portal extension) using OIDC token exchange + cross-app aggregation queries | Demonstrate user-facing value with the registry |
| **v3** | Workflow agents (scheduled + event-driven), AppProfile generator, tenant provisioning assistant | Open the platform to external agents and AI-assisted operations |

## 8. Security Model

- **No agent has privileges the calling user lacks.** OIDC token
  exchange enforces this end-to-end.
- **Capability scopes** (`read` / `write` / `admin`) are declared
  per capability and enforced by the app, with the platform validating
  the declaration matches the underlying API surface.
- **Audit log:** every MCP call is logged with (user, app,
  capability, agent identity, tenant) — the same audit pipeline that
  records human API calls.
- **Tenant isolation:** the MCP registry is per-tenant; agents
  cannot discover or call MCP endpoints in other tenants.
- **Rate limits** apply per (user, capability) — an out-of-control
  agent cannot DoS an app for other users.

## 9. What This Is Not

- Not a runtime for AI models. Models live wherever the user/tenant
  chooses (cloud LLM, on-prem inference, etc.).
- Not a competitor to MCP server implementations. Apps still bring
  their own MCP servers; the platform provides the registry,
  identity, and tenant-scoping.
- Not magic: apps that don't expose MCP remain opaque to agents.
  The value scales with catalogue adoption.
