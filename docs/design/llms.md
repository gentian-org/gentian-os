# LLM Serving Integration Design

This document details the architecture for integrating Large Language Model (LLM) serving capabilities into Gentian OS.

## What is built

§1–§4 are the original design and its first-stage plan. What runs today differs:

*   **The model gateway** is LiteLLM (`litellm-proxy`), with its own PostgreSQL
    (`litellm-db`) and Redis (`redis-llm`), in the namespace `system-llm`, which
    exists only where the Cluster claim sets `spec.llm.enabled`
    (`kernel/services/llm/manifests`, delivered by the `gentian-llm`
    ApplicationSet). On a cluster without GPUs the same chart runs a mock
    model server (`vllm-inference`).
*   **No request passes the edge.** The gateway has no public address unless the
    console is switched on (§5, "Operator access"). There is no Envoy AI
    Gateway, no token validation in front of the gateway and no per-model
    check in OpenFGA: an app presents its key to LiteLLM, and that is the
    whole check.
*   **A component gets access by declaring it** (`requires.services.llm` in its
    `ComponentProfile`). The operator then generates a key for that tenant and
    component, registers it at the gateway, writes the Secret
    `llm-credentials-<component>` (`OPENAI_API_BASE`, `OPENAI_API_BASE_URL`,
    `OPENAI_API_KEY`) in the tenant's namespace, and opens the component's
    network path to the gateway's port
    (`internal/controller/model_access_reconciler.go`, `internal/modelgateway`).
    A component that does not declare it gets none of these.
*   **One LiteLLM team per tenant**, created by the tenant reconciler
    (`internal/controller/litellm_team.go`). The operator sets no budget and no
    rate limit on a team or a key.
*   **The gateway's models are the Cluster claim's** (§5). The gateway's chart
    writes its configuration file from `spec.llm.instances` and
    `spec.llm.providers`, and the claim reaches the chart as a values file of
    the `gentian-llm` ApplicationSet (`kernel/appsets/raw/09c-llm.yaml`,
    `kernel/services/llm/manifests/templates/gateway-config.yaml`). Nothing
    registers a model through the gateway's API, and the gateway takes none
    from its database.
*   **The cluster's administrator changes them in the administration
    console** (§5, "Changing the models"), or with `kubectl gentian models`.
    Both ask the director, which commits the claim. The console flags a model
    that cannot answer: one the cluster would serve itself, and a provider's
    model whose token is missing.
*   **Not built:** starting the vLLM instances themselves. The gateway offers a
    model for each entry of `spec.llm.instances` and calls it at
    `vllm-<name>-inference.system-llm`, port 8000; the chart that holds that
    workload (`kernel/services/llm/chart`) is delivered by nothing, so such a
    model is listed and does not answer until somebody runs the instance (§6).
    LocalAI was never added.

---

## 1. Architectural Overview

To deliver high-performance, cost-effective, and secure AI capabilities, Gentian OS splits the LLM serving stack between the shared system **Kernel** and isolated **Tenant Land**.

```mermaid
flowchart TB
    classDef client  fill:#dbeafe,stroke:#3b82f6,color:#1e3a5f
    classDef kernel  fill:#f1f5f9,stroke:#94a3b8,color:#1e293b
    classDef gateway fill:#ede9fe,stroke:#7c3aed,color:#3b0764
    classDef engine  fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef tenant  fill:#dcfce7,stroke:#16a34a,color:#14532d

    subgraph Client [User Space / Tenant Land]
        App[Tenant App<br/>Nextcloud / OpenProject]:::client
    end

    subgraph Edge [Kernel Space / Ingress]
        EAG["Envoy AI Gateway v1.0<br/>JWT Auth & Ext Authz"]:::gateway
    end

    subgraph Auth [Kernel Space / Suze Platform]
        KC[Keycloak]:::kernel
        OFG[OpenFGA]:::kernel
    end

    subgraph Serving [Kernel Space / LLM Infrastructure]
        LLP["LiteLLM Router Proxy<br/>virtual keys & budgets"]:::gateway
        vLLM["vLLM GPU Engine<br/>High-perf FP16/AWQ"]:::engine
        LAI["LocalAI CPU Engine<br/>GGUF Fallback"]:::engine
    end

    App -->|1. Bearer JWT / API Request| EAG
    EAG <-->|2. Validate OIDC Token| KC
    EAG <-->|3. Model Access Check| OFG
    EAG -->|4. Forward to Proxy| LLP
    LLP -->|5. Multi-Tenant Route| vLLM
    LLP -->|5. Fallback Route| LAI
```

---

## 2. Kernel vs. Tenant Land Split

The components are allocated as follows:

### Kernel Space (Shared Infrastructure)
*   **Ingress Security (Envoy AI Gateway):** Runs as a cluster-wide service in the `envoy-gateway-system` namespace. By upgrading Envoy Gateway to the AI Gateway edition, the platform terminates TLS, performs JWT validation via Keycloak, and executes per-model access checks before requests leave the ingress boundary.
*   **Inference Engines (vLLM & LocalAI):** Deployed in a shared namespace (e.g., `gentian-llm`). Because GPUs are costly and model weights (10GB–70GB+) require massive storage, running centralized engines with shared PVC weight storage allows the platform to use NVIDIA GPU Operator time-slicing or MIG (Multi-Instance GPU) to multiplex compute safely.
*   **Router & Budgeting (LiteLLM Proxy):** Runs as a central service in the kernel backed by shared kernel Postgres (CloudNativePG) and Redis. It translates calls, routes to the appropriate model engine, handles fallback routing, and manages Virtual Keys to enforce tenant-level token budgets and rate limits.

### Tenant Land (Isolated Workspaces)
*   **Tenant Applications:** Downstream applications (e.g., Nextcloud) run in isolated tenant namespaces. During application bootstrapping via Crossplane `App` claims, the operator injects the base URL of the Envoy Gateway and a tenant-specific virtual key as standard credentials (`OPENAI_API_BASE` and `OPENAI_API_KEY`).
*   **Optional Tenant Proxies:** If a specific tenant requires custom model endpoints or private caching, they can install a local LiteLLM instance in their own namespace that redirects requests to the shared kernel engines.

---

## 3. Authentication & Request Lifecycle

```mermaid
sequenceDiagram
    actor App as Tenant Application
    participant EAG as Envoy AI Gateway
    participant KC as Keycloak (Suze)
    participant OFG as OpenFGA (Suze)
    participant LLP as LiteLLM Proxy
    participant Backend as vLLM / LocalAI

    App->>EAG: 1. Send Request (Bearer JWT)
    EAG->>KC: 2. Validate JWT (JWKS signature)
    KC-->>EAG: Token claims returned
    EAG->>OFG: 3. Check model permission (User, Model, Tenant)
    OFG-->>EAG: Allowed / Denied

    alt Denied
        EAG-->>App: 403 Forbidden
    else Allowed
        EAG->>LLP: 4. Forward Request + Virtual-Key Header
        LLP->>LLP: 5. Apply Token Rate Limits & Spend Budgets
        LLP->>Backend: 6. Inference Request (OpenAI-compatible)
        Backend-->>LLP: Return Streamed Tokens
        LLP-->>App: Return Streamed Response
    end
```

---

## 4. Stage 1 Rollout Plan: Single-GPU Authenticated Endpoint

Stage 1 focuses on establishing the core loop: running a single-GPU server, securing the endpoint via Envoy AI Gateway, and routing requests through LiteLLM.

### Task 1: Setup GPU Infrastructure & Inference
*   **Action:** Install the **NVIDIA GPU Operator** via Helm into the cluster.
*   **Configuration:** For A100/H100 instances, configure MIG partition profiles. For lower-tier cards (A10G, L4), enable time-slicing in the operator configuration:
    ```yaml
    # gpu-sharing-config.yaml
    sharing:
      timeSlicing:
        resources:
        - name: nvidia.com/gpu
          replicas: 4
    ```
*   **Deployment:** Deploy a vLLM instance serving a lightweight model (e.g., `Qwen/Qwen2.5-7B-Instruct` or `meta-llama/Llama-3.1-8B-Instruct`) in the `gentian-llm` namespace, mapping standard model weights to a shared PersistentVolumeClaim (PVC).

### Task 2: Deploy LiteLLM Router & State Store
*   **Action:** Deploy the LiteLLM Proxy using the official Helm chart (`deploy/charts/litellm-helm`).
*   **State Stores:**
    *   **PostgreSQL:** Use the existing **CloudNativePG** operator in the kernel to spin up a dedicated database cluster (`litellm-db`) to store virtual keys, audit logs, and budgets.
    *   **Redis:** Deploy a lightweight Redis cluster for query caching and cluster-wide rate limiting.
*   **Configuration:** Mount the vLLM backend address in the LiteLLM `config.yaml` mapping.

### Task 3: Upgrade Ingress to Envoy AI Gateway v1.0
*   **Action:** Update the Helm chart version for Envoy Gateway in [certs.sh](https://github.com/gentian-org/gentian-os/blob/develop/scripts/lib/certs.sh) to target version `v1.0.0` of the Envoy AI Gateway.
*   **Extension Policy Configuration:** Define an `EnvoyExtensionPolicy` targeting the external auth endpoint (`openfga/openfga-envoy`) to handle token parsing and OIDC enforcement:
    ```yaml
    apiVersion: gateway.envoyproxy.io/v1alpha1
    kind: EnvoyExtensionPolicy
    metadata:
      name: llm-gateway-auth
      namespace: envoy-gateway-system
    spec:
      targetRefs:
        - group: gateway.networking.k8s.io
          kind: HTTPRoute
          name: llm-route
      extAuthz:
        - grpc:
            authority: openfga-envoy.suze.svc.cluster.local
            port: 50051
    ```

### Task 4: Integration with Tenant Provisioning
*   **Action:** Update the `App` composition files in the operator to support injection of LLM environment variables for any catalogue application that requests the `ai-assistant` integration contract.

---

## 5. External Providers

A cluster does not have to serve its own weights. `spec.llm.providers` on the
Cluster claim declares external, OpenAI-compatible endpoints, and they route
through the same LiteLLM gateway as the vLLM instances — so the keys and
per-tenant teams apply to them unchanged. It is independent of
`gpuAcceleration`: a CPU-only cluster with no `instances` is the case these
exist for.

```yaml
spec:
  llm:
    enabled: true
    providers:
      - name: infomaniak
        displayName: Infomaniak AI Services
        apiBase: https://api.infomaniak.com/2/ai/<product-id>/openai/v1
        apiKeyProperty: infomaniak_api_key
        models:
          - name: gemma-4-31b
            model: google/gemma-4-31B-it
            maxTokens: 8192
```

Each `models` entry becomes one model of the gateway named
`<provider>/<model.name>` — `infomaniak/gemma-4-31b` above. The prefix is
deliberate: upstream ids collide across providers, and a tenant reading a model
list should be able to see who serves what. `mode` says what the model does
(`chat`, the default, `completion` or `embedding`); `maxTokens` is the context
window the gateway advertises.

**The claim is the only way in.** The gateway's model list is its
configuration file, and the file is written from the claim: a commit that adds
an entry adds the model, one that removes it removes the model, and each
replaces the gateway's pods, which read the file when they start. The gateway
runs with `STORE_MODEL_IN_DB` off, so it loads no model from its database and
refuses to store one — nothing typed into its console or sent to its API
becomes a model. No step of the installer, no Job and no controller is
involved; Argo CD syncs the claim and the chart together.

### Changing the models

The cluster's administrator changes them on the **Models** tab of the
administration console, or by command:

```bash
kubectl gentian models list                  # each model, and what the claim says of it
kubectl gentian models show > models.json    # the settings, as `set` takes them
kubectl gentian models set -f models.json    # make the claim declare exactly these
```

Both ask the director, and the director is the only writer:

| Route | Who may | What it does |
|---|---|---|
| `GET /v1/clusters/{c}/models` | `can_audit` on the cluster | The settings (`enabled`, `gpuAcceleration`, `instances`, `providers`) and each model under the gateway's name, with what the claim says of it |
| `PUT /v1/clusters/{c}/models` | `can_configure` on the cluster | Replaces the settings in the claim, as one signed commit in the person's name |

A tenant's administrator holds neither relation, and the routes exist under
`/v1/clusters` only. The console and the command write nothing to git and do
not reach the gateway.

The `PUT` carries the whole of the settings. A model, an instance or a provider
that the body does not name is removed from the claim, and with it from the
gateway, whoever wrote it there. The director holds the body to the schema of
`spec.llm` it was built with — names, `https` addresses, the modes — and also
refuses an empty model id, text over more than one line, and two entries that
would give the gateway one model name twice. A body with any other field is
refused unread: the gateway's console switch (`llm.console.enabled`), GPU time
slicing and a provider's token are not set here. The rest of the claim, its
comments included, stays as it was; a comment written inside the list of
instances or of providers is lost when that list changes.

Two things an administrator who has `can_configure` can do here, which follow
from the claim being theirs to write: point a provider at any `https` address,
and name any property of the shared provider credential as its token. The
gateway then sends that token to that address.

**A provider named for the first time has no credential to enter its token
under.** The credential requirement of a provider is declared in this
repository (`credentials.yaml`, one provider today) and reaches a cluster with
the installer (below). The console says so on the provider's models and does
nothing about it.

### Whether a model works

The console shows it per model, and asks neither the gateway nor any pod of
`system-llm`:

| Model | Shown as | Known from |
|---|---|---|
| An instance's, with `gpuAcceleration` true | **Not served** | The director, from one fact: the platform starts no vLLM instance (§6). Every such model is flagged, also one whose instance somebody runs by hand |
| An instance's, with `gpuAcceleration` false; any model with `enabled` false | **Not offered** | The director, from the claim: the gateway does not list it |
| A provider's, with no credential `llm-provider-<name>` on the cluster, or one that lacks the property `apiKeyProperty` names | **No credential** / **Property missing** | The custodian's list of credentials, which the console reads already |
| A provider's, whose credential is not satisfied | **Token missing** | The same list |
| A provider's, whose token is there | **Token supplied** | The same list. Not "works": nothing probes the token or the address |

`kubectl gentian models list` shows the director's part only.

A status that says a model answers would need somebody to ask the gateway
(its `/health` or `/v1/models` with a key) or to read the instance's
Deployment, and to publish the answer where the usher can read it. Nothing
does: the operator registers keys at the gateway and reads no model state.
That reader would be a new path into `system-llm` and is not built.

### Seeing what the gateway was given

To see what the gateway was given, and what an app is offered:

```bash
# The file the gateway reads.
kubectl -n system-llm get configmap litellm-config -o jsonpath='{.data.config\.yaml}'

# The list an app sees, with the app's own key, from the app's pod.
kubectl -n <tenant namespace> exec deploy/<app> -- sh -c \
  'curl -s -H "Authorization: Bearer $OPENAI_API_KEY" "$OPENAI_API_BASE/models"'
```

The second needs an app that declared the gateway (`requires.services.llm`)
and whose image has `curl`; the variables are the ones the Secret
`llm-credentials-<app>` carries, under the names the app's profile maps them
to.

### The API key is a credential, the product id is not

Each provider has its own `llm-provider-<name>` credential declaring one field,
and they all share the OpenBao path `gentian-os/kernel/llm-providers` (see
[`credentials.yaml`](../../credentials.yaml)); `apiKeyProperty` on the claim names
which property to read. Supply the token in the **administration console** under
that credential — the write happens as your own OpenBao token, merge-patches the
path so it cannot clobber another provider's key, and tells the ExternalSecret to
resync immediately rather than at the end of its refresh interval.

One requirement per provider rather than one with a field each, because
`checkFields` requires every declared field in a single write: a combined
credential would make the console demand every provider's token at once and
refuse a single rotation. Adding a provider is two edits in git — a requirement
there and an entry here. `credentials.yaml` is this repository's and declares
one provider today, Infomaniak; another provider needs its requirement added
there before its token can be entered in the console. The requirement of a
provider reaches a cluster with the installer (`C-04`), and only for the
providers its claim names at that run: after naming a provider for the first
time, run `./install.sh --only C-04` for its credential to appear in the
console.

**The token never leaves its Secret.** The ExternalSecret
`llm-provider-credentials` (rendered when the claim names a provider) copies
the vault path into a Secret of the gateway's namespace, the gateway's
container mounts it, and sets one environment variable per property when it
starts, `LLM_PROVIDER_KEY_<apiKeyProperty>`. The configuration file names the
variable, not the token, and nothing writes the token to the gateway's
database.

**A model whose token is missing is listed and does not answer.** Until the
token is supplied a call to such a model comes back as an authentication
error from the gateway. The administration console flags it on the Models tab
(above); nothing on the cluster compares the claim's `apiKeyProperty` with the
credential requirements, and no status or alert reports the gap. When the token is
supplied, rotated or removed, the gateway's container restarts by itself to
read it: a probe compares the mounted Secret with what the container started
with. That takes up to a few minutes — the ExternalSecret's resync, the
kubelet bringing the Secret into the pod, the probe's period — and both
replicas may restart together, so the gateway can be away for the time it
takes to start.

> These credentials declare `validate: noop`, so the console stores the token
> without probing it — the validator enum has no generic bearer-token check.
> Nothing else probes it either: a wrong token, a token without the provider's
> scope and an `apiBase` that is not reachable from the cluster all show as a
> failing call, not as a report.

The product id in `apiBase` is not secret: it selects which product is billed and
it is part of the endpoint, so it belongs on the claim where it can be reviewed.
For Infomaniak it is the AI Tools product id (`GET /1/ai` returns it), not an
account or user id, and the token needs the `ai-tools` scope — one without it
authenticates and then refuses every AI endpoint.

### Where a provider can be reached from

Only from the gateway. The policies on `system-llm` restrict who may connect to
its pods and say nothing about egress, so LiteLLM reaches the internet; a tenant
namespace is denied egress by default (`tenant-isolation`), so a tenant app
configured to call a provider directly gets a timeout its UI usually renders as
an empty model list. That asymmetry is the design: the gateway is what holds the
credential.

> Adding an egress NetworkPolicy to `system-llm` to "allow 443" would be a
> regression, not a hardening: the namespace has no egress policy today, and the
> first one switches the selected pods to deny-by-default, breaking LiteLLM's
> DNS, Postgres and Redis unless all of it is enumerated in the same policy.

### Operator access

LiteLLM's Admin Console is **off unless the Cluster claim switches it on**:

```yaml
spec:
  llm:
    enabled: true
    console:
      enabled: true     # default false
```

Off, the default, there is no route and no published host for
`llm.<kernelDomain>`, and the gateway's NetworkPolicy has no rule for the edge:
the gateway is reached from inside the cluster only, by the apps and desktops
that declared it and by the operator. Nothing the platform does needs the
console. Models come from the claim, an app's key and a tenant's team are
registered by the operator, and a provider's token is entered in the
administration console.

On, `llm.<kernelDomain>` is routed to the gateway behind the kernel sign-in for
accounts that may configure the cluster (`can_configure`;
`kernel_gateway_routes.go`), the host is published, the edge's Envoy pods are
admitted to the gateway's port, and such an administrator's desktop shows a
**Model gateway** tile, projected from the route like the other kernel
consoles' (`tile_projection_reconciler.go`). Platform administrators only,
because the routing and budgets there apply to every tenant. The models are
still the claim's and the console cannot add one; it is for inspecting them,
keys and spend. The host label `llm` stays reserved either way.

Three things to know before switching it on:

- **It is a departure from "system services have no public route"**, taken by
  the cluster's owner for this cluster. The route forwards `/`, so the
  gateway's whole API is on that host as well as its pages — for a signed-in
  platform administrator only, and LiteLLM's own key check still applies behind
  the session.
- **The console signs in to LiteLLM by itself.** The kernel session opens the
  host; LiteLLM then asks for its own administrator sign-in (the master key),
  and its pages send the key that gives them in the `Authorization` header.
  The edge leaves that header alone on this route, as it does on Keycloak's
  administration console, and passes no token of the platform's to LiteLLM
  ([routing.md §4.1](routing.md)). This is held by tests of the rendered policy
  and the bouncer's table, not by a run against a live LiteLLM.
- **An upgrade takes the console away.** A claim written before the setting
  existed does not state it and reads as off, so a cluster that had the console
  loses the route on its next run. The installer says so in one line whenever
  the cluster serves models and the console is off. The route follows the claim
  as soon as it is applied; the NetworkPolicy rule arrives with the installer's
  next run, which is what passes the setting to the gateway's chart. Between
  the two the route exists and answers nothing.

---

## 6. Models the cluster serves itself

`spec.llm.instances` names the models a cluster with GPUs serves from its own
weights, one vLLM instance each. It is read only when `gpuAcceleration` is
true.

```yaml
spec:
  llm:
    enabled: true
    gpuAcceleration: true
    instances:
      - name: qwen
        modelId: Qwen/Qwen2.5-7B-Instruct
```

The gateway offers each entry as a model named after `modelId`, in lower case
with `/` as `-` — `qwen-qwen2.5-7b-instruct` above — and calls it at
`http://vllm-<name>-inference.system-llm.svc.cluster.local:8000/v1`. That is
the same file and the same mechanism as for a provider's models (§5).

**The instance itself is not started by the platform.** The chart with the
Deployment, Service and volume of an instance is in the repository
(`kernel/services/llm/chart`) and no Application delivers it, so on a cluster
installed today the model above is listed and a call to it fails with a
connection error. The administration console and `kubectl gentian models list`
show every such model as not served (§5, "Whether a model works"). What is
missing before the platform can start it:

- an Application that delivers the chart with the claim's `instances` — and
  with them `gpuMemoryUtilization`, `maxModelLen`, `modelCacheSize`,
  `imageTag` and `toolCallParser`, which only that chart reads;
- a NetworkPolicy for the instance's pods, as every other server of
  `system-llm` has (security.md §2.9);
- the pod settings the kernel's admission baseline requires, which the chart's
  Deployment does not carry;
- a named release of the vLLM image: `imageTag` defaults to `latest`;
- a decision on GPU time slicing (`gpuTimeSliceReplicas`), whose ConfigMap is
  the GPU operator's and shared with every GPU workload of the node;
- where the Hugging Face token for gated models comes from.

With `gpuAcceleration` true the mock model server is not started. With it
false the mock runs, and the gateway does not offer it as a model: a cluster
without GPUs and without providers has a gateway with an empty model list.

