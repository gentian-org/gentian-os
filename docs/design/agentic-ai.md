# Agentic AI Layer

**Companion to:** [architecture.md](../architecture.md)

---

## Scope: what the kernel carries, and what it leaves to a component

The kernel carries what every participant needs and nobody should rebuild:
the protocol apps expose their operations through (MCP), a gateway to
language models, and an assistant a person can ask while they are at the
desktop.

The kernel does not carry a program that **acts for a person who is not
there**: one that works under a standing permission, on a timer or a
trigger, and changes data without the person clicking. That needs a record
of what the person allowed, a check on every call, and a journal, and it is
the work of a component a cluster may or may not install. The kernel offers
such a component generic hooks and no more:

| Hook | What it gives a component | Where |
|---|---|---|
| The rights check | One question: may this person use that app of my tenant. No credential of the authorization store. | `requires.services.rights`, [custom-catalogues.md](../custom-catalogues.md) |
| The model gateway | A key of its own at the gateway. | `requires.services.llm` |
| Vouching | A token of a person who is away, for one app, from the tenant's realm, and only for a person who linked themselves to the component with their own token. | `requires.services.vouching`, [custom-catalogues.md](../custom-catalogues.md) |
| A token made out to one app | For a signed-in person, at the front door: the session's token exchanged for one whose audience is the app. | exposure `exchangeToken` |
| Contracts between apps | A declared, grantable relation to another app of the tenant. | `provides`, `integrations`, `AppGrant` (§2) |
| Adapters | A class of component that turns one app's abilities into tools, reached only by the tenant's components that vouch for people, and reaching only the app it adapts. | `classes: [adapter]`, `spec.adapter`, [custom-catalogues.md](../custom-catalogues.md) |

A cluster with no such component installed runs no unattended agent, and
loses nothing else described here.

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

## 2. Contracts, MCP and Automation Hooks

A `ComponentProfile` declares what a component needs and offers in one place:

| Layer | Purpose | Consumer |
|---|---|---|
| **Service requirements** (`spec.requires.services`) | Identity, database, storage, cache, mail, model gateway | Fulfilled by the platform |
| **App contracts** (`spec.provides` / `spec.integrations`) | App-to-app integration (e.g., OpenProject ↔ Nextcloud), granted per tenant with an `AppGrant` and wired as an `IntegrationBinding` | Other apps |
| **MCP** (`spec.requires.services.mcp`) | Agent-readable operations | The declaration only (§3) |

Contracts are machine-to-machine via stable APIs; MCP is agent-to-machine.

## 3. MCP as a Kernel Requirement

An app declares that it serves MCP in its `ComponentProfile`:

```yaml
spec:
  requires:
    services:
      mcp:
        enabled: true
        endpoint: /mcp              # path on the app's main service
        auth: oidc                  # oidc | none
```

The operator accepts the declaration and does nothing with it.

Apps without MCP support simply omit the `mcp:` block; the platform
treats them as agent-opaque but still useful.

## 4. Shell AI Assistant

The desktop has an assistant: a person asks, a language model behind the
model gateway answers. It works **while the person is at the desktop** and
ends with their session.

Acting while the person is away, changing data unattended and running on a
trigger are not the assistant's and not the kernel's (see Scope).

## 5. Security Model

A component that acts for a person who is away holds itself to that person's
privileges with the rights check: the person's own right to use the app is
asked on every call, of the same store the edge asks.

## 6. What This Is Not

- Not a runtime for AI models. Models live wherever the user/tenant
  chooses (cloud LLM, on-prem inference, etc.).
- Not a competitor to MCP server implementations. Apps bring their own
  MCP servers.
- Not a workflow engine.

## 7. LLM Serving: Admin Console & vLLM Operations

The model gateway ([llms.md](llms.md)) has its own
platform-admin console, separate from the tenant-facing MCP/agent
surface described above (§4). This section is operational notes for
the cluster admin, not an architecture doc — see
[llms.md](llms.md) for the design.

### 7.1 LiteLLM admin console

`https://llm.<KERNEL_DOMAIN>` exists only where the Cluster claim switches the
console on (`spec.llm.console.enabled`, off by default; see
[llms.md](llms.md), "Operator access"). It is then a kernel-level `HTTPRoute`
(`kernel_gateway_routes.go`) fronting the shared `litellm-proxy` Deployment,
behind the kernel sign-in. Login is Keycloak OIDC SSO via a
`litellm-dashboard` client that only exists in the **kernel realm** —
tenant users (separate per-tenant realms) structurally cannot reach it,
which is what keeps LLM administration platform-admin-only.
LiteLLM's native SSO is free for ≤5 users on OSS (v1.76.0+); the
JWT/OIDC/SCIM/`enforce_rbac` features that require an Enterprise license
are a different feature (`enable_jwt_auth`, for authenticating
*inference API calls*, not the admin UI) and are intentionally unused.

**Teams:** one free/OSS LiteLLM Team is created per `Tenant` CR by the
`TenantReconciler` (`internal/controller/litellm_team.go`), during the
shared-kernel stage of the tenant's reconcile. Nothing has to be re-run after
adding a tenant, and a cluster without LiteLLM simply has no team to create —
the step is non-fatal and retries on the next reconcile.

Tenant administrators have no console of the shared gateway.

### 7.2 Configuring vLLM

**Chat UIs (Open WebUI included) cannot install or reconfigure vLLM —
this isolation is structural, not a permission we grant/deny.** Open
WebUI (and any tenant app) only ever talks to vLLM indirectly, through
the shared LiteLLM endpoint with a key of its own that the operator
delivers (`model_access_reconciler.go`) — it calls
`/v1/chat/completions`, nothing that touches how vLLM itself is
deployed or configured. Open WebUI's own "Admin Settings" panel lets
its local admin manage *that instance's* connections/model list/users,
but that's configuring the client, not the server — it has no path to
vLLM's CLI flags, GPU allocation, or Deployment spec. Reconfiguring
vLLM always requires `kubectl`/GitOps access to the cluster, which only
the platform admin has, so no separate access-control mechanism is
needed.

vLLM has no live reconfiguration API for core serving parameters
(model, quantization, parallelism, context length) — these are set via
CLI flags to `vllm serve <model> [flags]` at container startup and
require a redeploy (new pod) to change. The one runtime exception is
LoRA adapters, which can be hot-loaded/unloaded via `POST
/v1/load_lora_adapter` and `/v1/unload_lora_adapter` — vLLM's own docs
flag this as **dev-only**, not for production use.

Cheat-sheet of the flags that matter most in production:

| Flag | Purpose |
| --- | --- |
| `--gpu-memory-utilization` | Fraction of GPU memory vLLM may claim (start ~0.90, tune up) |
| `--max-model-len` | Caps context length → directly controls KV-cache memory reserved |
| `--tensor-parallel-size` | Shard a model across N GPUs (must match GPU count allocated) |
| `--quantization awq` / `--dtype fp8` | AWQ: ~2x throughput, <2% accuracy loss. FP8: one-flag win on H100/Blackwell, no quantization step |
| `--enable-prefix-caching` | Reuse KV-cache across requests sharing a prompt prefix |
| `--enable-chunked-prefill` | Better latency/throughput mixing for concurrent long+short requests |
| `--enable-auto-tool-choice` / `--tool-call-parser <parser>` | Required for `tool_choice="auto"` (Open WebUI's native tool support, agentic clients) — omitted by default, so tool-calling requests 400 clearly instead of silently misparsing. `<parser>` is model-family-specific (`hermes` for Qwen2/Qwen2.5/Hermes-family, `mistral` for Mistral, `llama3_json` for Llama 3) — the instance's `toolCallParser` |

**In gentian-os:** the namespace `system-llm` runs the gateway and, with
`spec.llm.gpuAcceleration` false (the default), a mock OpenAI-compatible
server (`vllm-inference`, `kernel/services/llm/manifests/templates/vllm-mock.yaml`),
both delivered by the `gentian-llm` ApplicationSet.

With `gpuAcceleration` true, each entry of `spec.llm.instances` (name,
`modelId`, `gpuMemoryUtilization`, `maxModelLen`, `modelCacheSize`,
`memoryLimit`, `imageTag`, `toolCallParser`) is a `vllm-<name>-inference` Deployment, Service
and PVC (`kernel/services/llm/manifests/templates/vllm.yaml`) and a model of
the gateway, delivered by the same ApplicationSet ([llms.md](llms.md) §6).

What an instance needs:

- **One GPU per instance.** Each instance requests one `nvidia.com/gpu`, a
  time-sliced share where the node's GPU operator slices the card. `--gpu-memory-utilization` is a fraction of
  one physical card's memory, and instances sharing a card draw from the same
  pool, so their values must sum to comfortably under 1.0. A 24GB card fits
  one 7B-class model at `0.85`; a second needs both quantized.
- **Memory while loading.** Loading the weights takes about the model's size
  on disk in memory, and then falls back. The container is
  limited to `memoryLimit` (20Gi unless the entry says otherwise), so a
  larger model needs a larger value; over it, the instance is killed rather
  than the node's other pods.
- **Gated models and the first download.** The Deployment reads
  `HUGGING_FACE_HUB_TOKEN` from an optional Secret `vllm-hf-token` (key
  `token`) in its namespace, shared by all instances. A gated model (e.g.
  Llama) needs it. It is worth creating for ungated ones too: unauthenticated
  Hugging Face requests are rate-limited, and a first download of several
  gigabytes can outlast the `startupProbe` (about 20 minutes). Weights are
  cached in the instance's PVC, so a second start is fast, and the PVC is kept
  when an instance is removed.
- **Changing a model** is a change to the claim and a new pod; operational
  checks against a running instance are plain HTTP (`GET /health`,
  `GET /v1/models`, `GET /metrics`, `GET /version`).

**Further reading:**

- [vLLM docs — OpenAI-compatible server](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/)
- [vLLM docs — engine args reference](https://docs.vllm.ai/en/latest/serving/engine_args.html)
- [LiteLLM docs — Admin UI SSO](https://docs.litellm.ai/docs/proxy/admin_ui_sso)
- [LiteLLM docs — Team budgets](https://docs.litellm.ai/docs/proxy/team_budgets)
