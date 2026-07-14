# Open-Source LLM Serving on Kubernetes with an Authenticated Gateway

## TL;DR

- **Build a two-layer stack: vLLM (GPU) + LocalAI (CPU) as OpenAI-compatible inference backends, fronted by a single LiteLLM proxy as the unified gateway.** This gives one authenticated OpenAI-compatible endpoint serving unlimited downstream apps.
- **For your Keycloak + OpenFGA stack, do NOT rely on LiteLLM's built-in JWT/OIDC auth — it is an Enterprise (paid) feature.** Instead, upgrade the existing Envoy Gateway v1.2.5 to Envoy AI Gateway v1.0 in Stage 1 and wire its ext_authz filter to Keycloak OIDC + OpenFGA for per-model access control, while using LiteLLM's free virtual keys for per-app budgets, rate limits, and model allowlists.
- **Small models (7B–32B) run on a single GPU node (FP16 for 7–8B, AWQ-INT4 for 14–32B). 70B+ requires either tensor parallelism across multiple GPUs or a high-VRAM single card (48–80 GB).**

---

## Overall Architecture

```mermaid
%%{init: {'theme': 'neutral', 'themeVariables': {'background': '#ffffff', 'mainBkg': '#f8fafc', 'nodeBorder': '#94a3b8', 'clusterBkg': '#f1f5f9', 'clusterBorder': '#cbd5e1', 'titleColor': '#1e293b', 'edgeLabelBackground': '#ffffff', 'lineColor': '#64748b'}}}%%
flowchart TB
    classDef client  fill:#dbeafe,stroke:#3b82f6,color:#1e3a5f
    classDef auth    fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef gateway fill:#ede9fe,stroke:#7c3aed,color:#3b0764
    classDef gpu     fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef cpu     fill:#e0f2fe,stroke:#0284c7,color:#0c4a6e
    classDef large   fill:#fce7f3,stroke:#db2777,color:#831843
    classDef store   fill:#e2e8f0,stroke:#64748b,color:#1e293b

    CLIENTS["🖥  Clients\nBrowser · Nextcloud AI · OpenProject\nOpenWebUI · M2M services"]:::client

    EAG["🛡  Envoy AI Gateway v1.0\n(upgrade from Envoy GW v1.2.5)\next_authz · JWT validation\nKeycloak OIDC · OpenFGA per-model check"]:::auth

    LL["⚡  LiteLLM Proxy\nOpenAI-compatible endpoint\nvirtual keys · budgets · routing · spend"]:::gateway
    PG[("PostgreSQL")]:::store
    RD[("Redis")]:::store

    GPU["🟡  GPU Cluster\nvLLM · 7–8B FP16\nvLLM · 14–32B AWQ-INT4\nvLLM · Embeddings + Rerank"]:::gpu

    CPU["🔵  CPU-only Clusters\nLocalAI · GGUF Q4_K_M\nOllama · dev / low-traffic"]:::cpu

    LARGE["🟣  Large Models — optional 70B+\nvLLM TP=2 · multi-GPU tensor parallel\nKServe + llm-d · multi-node cluster"]:::large

    CLIENTS --> EAG
    EAG --> LL
    LL --- PG
    LL --- RD
    LL --> GPU
    LL --> CPU
    LL -.->|fallback / large models| LARGE
```

**Why this shape:** Gentian OS already runs Envoy Gateway v1.2.5. Stage 1 upgrades it to Envoy AI Gateway v1.0, which builds on the same CNCF Envoy Gateway foundation and adds LLM-specific features: token-aware rate limiting, per-model routing policies, and a clean ext_authz integration for Keycloak OIDC + OpenFGA. No new ingress component is introduced — it is a drop-in upgrade. LiteLLM then handles routing to the right backend and applies per-app virtual-key budgets.

---

## Key Findings

### Inference Backends

**vLLM is the clear production default** for GPU serving. It is OpenAI-compatible (chat, completions, embeddings, plus `/rerank` and `/score` endpoints), delivers the highest throughput via PagedAttention + continuous batching, and supports tensor parallelism for 70B+ models. Independent benchmarks measured vLLM at ~793 tokens/s peak vs Ollama's ~41 tokens/s on identical hardware, with vLLM's throughput scaling smoothly with concurrency while Ollama's flattens almost immediately.

**Hugging Face TGI entered maintenance mode on December 11, 2025.** HF's Lysandre Debut announced: "text-generation-inference is now in maintenance mode… we contribute to and recommend using going forward: @vllm_project, @sgl_project, as well as local engines such as llama.cpp or MLX." **Do not build new infrastructure on TGI.**

| Backend | GPU | CPU | Concurrency | Best for |
|---------|-----|-----|-------------|----------|
| **vLLM** | ✅ primary | ❌ | Excellent (PagedAttention) | Production GPU serving |
| **LocalAI** | ✅ | ✅ | Moderate | CPU clusters, multi-modal |
| **Ollama** | ✅ | ✅ | Poor (sequential) | Dev / low-traffic |
| **llama.cpp server** | ✅ | ✅ | Poor | Lean CPU/GPU GGUF |
| **SGLang** | ✅ | ❌ | Excellent | Prefix-heavy / agentic workloads |
| **TGI** | ⚠️ | ❌ | — | **Deprecated Dec 2025** |

### Gateway / Orchestration

**LiteLLM proxy** (MIT-licensed) is the recommended gateway: one OpenAI-compatible endpoint routing to 100+ backends, with free virtual keys, per-key/team budgets, rate limits, model allowlists, spend tracking, and load balancing/fallbacks. Handles 1,500+ requests/second in load tests.

**Envoy Gateway v1.2.5** is already the ingress layer in Gentian OS. Stage 1 upgrades it to **Envoy AI Gateway v1.0** (GA June 23, 2026, same CNCF Envoy Gateway base, maintainers include Bloomberg, Tetrate, Tencent, Netflix, and Nutanix), which adds native LLM features: token-aware rate limiting, per-model routing policies, and a clean ext_authz integration for Keycloak + OpenFGA. The upgrade is drop-in — same CRDs, same Gateway API compatibility, no new ingress component.

**KServe + llm-d** is the Kubernetes-native heavyweight (CNCF incubating) for distributed, KV-cache-aware serving of very large models. Appropriate at multi-GPU cluster scale; overkill below that.

### Authentication Integration (the key constraint)

**LiteLLM's virtual API keys are free/open-source** — per-key budgets, RPM/TPM limits, model allowlists, spend tracking, per-team isolation.

**LiteLLM's JWT/OIDC auth (`enable_jwt_auth`), SCIM, and `enforce_rbac` are Enterprise-only.** LiteLLM's marketing ambiguously claims these are "all available on open-source," which contradicts the technical docs. **Trust the docs, not the marketing page.** Since Gentian OS already runs Envoy AI Gateway, enforce Keycloak + OpenFGA there — this is strictly additive to what you already have.

**Recommended fully-OSS auth pattern (fits Gentian OS):**
- **Envoy AI Gateway v1.0 ext_authz → OpenFGA** via `openfga/openfga-envoy`: upgrade Envoy Gateway v1.2.5 to Envoy AI Gateway v1.0 in Stage 1, then add an `EnvoyExtensionPolicy` ext_authz gRPC backend that validates Keycloak JWTs and calls OpenFGA Check per request. LiteLLM virtual keys then apply per-app budgets downstream.

### Authentication & Authorization Flow

```mermaid
sequenceDiagram
    actor User as User / App
    participant KC as Keycloak
    participant EAG as Envoy Gateway / AI GW
    participant OFG as OpenFGA
    participant LL as LiteLLM Proxy
    participant VL as vLLM / Backend

    User->>KC: (1) Authenticate → get JWT
    KC-->>User: JWT (access token)

    User->>EAG: (2) Request with Bearer JWT
    EAG->>KC: (3) Validate JWT via JWKS
    KC-->>EAG: JWT valid, claims extracted

    EAG->>OFG: (4) Check: can {user} use {model}?
    OFG-->>EAG: Allowed / Denied

    alt Denied
        EAG-->>User: 403 Forbidden
    else Allowed
        EAG->>LL: (5) Forward request + virtual-key header
        LL->>LL: (6) Apply budget / RPM / TPM limits\nRoute to backend
        LL->>VL: (7) OpenAI-compatible request
        VL-->>LL: Response (streamed)
        LL-->>User: Response
    end
```

---

## Inference Backend Selection

```mermaid
%%{init: {'theme': 'neutral', 'themeVariables': {'background': '#ffffff', 'mainBkg': '#f8fafc', 'nodeBorder': '#94a3b8', 'clusterBkg': '#f1f5f9', 'clusterBorder': '#cbd5e1', 'lineColor': '#64748b'}}}%%
flowchart TD
    classDef decision fill:#e2e8f0,stroke:#64748b,color:#1e293b
    classDef gpu      fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef cpu      fill:#e0f2fe,stroke:#0284c7,color:#0c4a6e
    classDef large    fill:#fce7f3,stroke:#db2777,color:#831843
    classDef gw       fill:#dcfce7,stroke:#16a34a,color:#14532d

    Start([Inference request]) --> HasGPU{GPU available?}:::decision

    HasGPU -->|Yes| ModelSize{Model size}:::decision
    HasGPU -->|No| CPU[LocalAI / llama.cpp\nGGUF Q4_K_M]:::cpu

    ModelSize -->|7–8B| FP16[vLLM FP16\ne.g. Llama 3.1 8B]:::gpu
    ModelSize -->|14–32B| AWQ[vLLM AWQ-INT4\nawq_marlin kernel]:::gpu
    ModelSize -->|70B+| Big{Multi-GPU?}:::decision

    Big -->|multi-GPU| TP2[vLLM TP=2]:::large
    Big -->|48–80 GB card| Single[vLLM single node]:::large
    Big -->|cluster scale| KS[KServe + llm-d]:::large

    CPU --> EMB[LocalAI\nEmbeddings + Rerank]:::cpu

    FP16 & AWQ & TP2 & Single & KS & CPU & EMB --> LL[LiteLLM Proxy\nunified endpoint]:::gw
```

---

## Model Sizing Guide

```mermaid
xychart-beta
    title "VRAM required by model size & quantization"
    x-axis ["7B FP16", "14B AWQ-INT4", "32B AWQ-INT4", "70B AWQ-INT4"]
    y-axis "VRAM (GB)" 0 --> 48
    bar [14, 10, 19, 40]
```

> Typical cloud GPU tiers: 16–24 GB (T4/A10G on AWS g4dn/g5), 40 GB (A100 40 GB), 48 GB (L40S/A6000), 80 GB (A100 80 GB / H100). 70B AWQ-INT4 (~40 GB) requires a 40 GB+ card or tensor parallelism across smaller GPUs.

- **7–8B FP16** (~14 GB): fits any A10G or larger; ~146 tok/s on a 24 GB node.
- **14B AWQ-INT4** (`awq_marlin` kernel, ~10 GB): ~135 tok/s, 1.7–2.4× faster than legacy `awq`.
- **32B AWQ-INT4** (~19 GB): ~65 tok/s, constrained to ~8 concurrent sequences on a 24 GB node.
- **70B AWQ-INT4** (~40 GB): requires a 40 GB A100, 48 GB L40S/A6000, or 80 GB A100/H100 — or TP=2 across two smaller GPUs.

Always pair AWQ-INT4 with FP8 KV cache (`--kv-cache-dtype fp8`) to fit more concurrent sequences.

---

## Kubernetes Deployment Topology

```mermaid
%%{init: {'theme': 'neutral', 'themeVariables': {'background': '#ffffff', 'mainBkg': '#f8fafc', 'nodeBorder': '#94a3b8', 'clusterBkg': '#f1f5f9', 'clusterBorder': '#cbd5e1', 'lineColor': '#64748b'}}}%%
flowchart TB
    classDef ingress fill:#e2e8f0,stroke:#64748b,color:#1e293b
    classDef auth    fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef gw      fill:#ede9fe,stroke:#7c3aed,color:#3b0764
    classDef gpu     fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef cpu     fill:#e0f2fe,stroke:#0284c7,color:#0c4a6e
    classDef store   fill:#e2e8f0,stroke:#64748b,color:#1e293b

    GW["Gateway API\nHTTPRoute"]:::ingress
    EAG["Envoy AI Gateway v1.0\n(upgraded from v1.2.5)\next_authz → OpenFGA"]:::ingress
    KC["Keycloak\n(existing)"]:::store
    OFG["OpenFGA\n+ PostgreSQL"]:::auth
    OAP["oauth2-proxy\n(browser SSO)"]:::auth

    LLP["LiteLLM Proxy\n× N replicas\nns: litellm"]:::gw
    LLPG[("PostgreSQL")]:::store
    LLRD[("Redis")]:::store

    V7B["vLLM 7B\nns: llm-gpu"]:::gpu
    V32B["vLLM 32B AWQ\nns: llm-gpu"]:::gpu
    VEMB["vLLM Embeddings\nns: llm-gpu"]:::gpu
    PVC[("model weights PVC")]:::store

    LAI["LocalAI\nns: llm-cpu"]:::cpu
    LPVC[("GGUF model PVC")]:::store

    GW --> EAG
    EAG <--> OFG
    EAG <--> KC
    EAG --> OAP
    OAP <--> KC
    EAG --> LLP
    LLP --- LLPG
    LLP --- LLRD
    LLP --> V7B
    LLP --> V32B
    LLP --> VEMB
    LLP --> LAI
    V7B & V32B & VEMB --- PVC
    LAI --- LPVC
```

**Helm charts available for every component:** LiteLLM (`deploy/charts/litellm-helm`, Kubernetes 1.21+ / Helm 3.8.0+, optional PostgreSQL + Redis subcharts), Ollama (`otwld/ollama-helm`, GPU/MIG/DRA + model pre-pull), vLLM Production Stack (`vllm/vllm-stack`), NVIDIA GPU Operator, oauth2-proxy, OpenFGA, KServe.

**GPU sharing:** On MIG-capable instances (A100, H100), prefer MIG for hardware-level VRAM isolation between models. On non-MIG instances (A10G, T4, L4), use NVIDIA GPU Operator time-slicing — cap per-pod VRAM with `--gpu-memory-utilization` to avoid OOM between slices.

---

## OpenBao / Secret Flow (Gentian OS Integration)

This fits the existing ESO pattern used throughout Gentian OS — no new secret management approach needed.

```mermaid
%%{init: {'theme': 'neutral', 'themeVariables': {'background': '#ffffff', 'mainBkg': '#f8fafc', 'nodeBorder': '#94a3b8', 'clusterBkg': '#f1f5f9', 'clusterBorder': '#cbd5e1', 'lineColor': '#64748b'}}}%%
flowchart LR
    classDef ob   fill:#e2e8f0,stroke:#64748b,color:#1e293b
    classDef eso  fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef app  fill:#ede9fe,stroke:#7c3aed,color:#3b0764

    OB["OpenBao\ngentian-os/kernel/llm/\nvllm_api_key · litellm_master_key\nlitellm_db_pass · litellm_redis_pass"]:::ob
    ES["ExternalSecret\nllm-secrets"]:::eso
    K8S["Kubernetes Secret\nllm-credentials"]:::eso
    ENV["LiteLLM\nLITELLM_MASTER_KEY\nDATABASE_URL · REDIS_URL"]:::app
    VENV["vLLM\nVLLM_API_KEY"]:::app

    OB -->|sync| ES
    ES -->|creates| K8S
    K8S -->|envFrom secretRef| ENV
    K8S -->|envFrom secretRef| VENV
```

---

## Staged Rollout Plan

```mermaid
gantt
    title LLM Platform Staged Rollout
    dateFormat  YYYY-MM-DD
    axisFormat  %b %d

    section Stage 1 — Single GPU, authenticated endpoint
    NVIDIA GPU Operator + MIG/time-slicing    :s1a, 2026-07-14, 3d
    vLLM 7B + embedding model                 :s1b, after s1a, 4d
    LiteLLM proxy + PostgreSQL + Redis        :s1c, after s1b, 3d
    Upgrade Envoy GW v1.2.5 → AI GW v1.0      :s1d, after s1c, 2d
    ext_authz + OpenFGA wiring                 :s1e, after s1d, 2d
    Per-app virtual keys + budgets             :s1f, after s1e, 1d

    section Stage 2 — CPU fallback + larger models
    LocalAI on CPU nodes (fallback)           :s2a, after s1f, 3d
    AWQ-INT4 14–32B models on GPU             :s2b, after s1f, 2d
    Redis multi-replica rate-limit state      :s2c, after s2a, 2d

    section Stage 3 — Large Models / Scale
    40–80 GB GPU instance (A100/H100/L40S)    :s3a, after s2b, 5d
    vLLM TP=2 for 70B                         :s3b, after s3a, 3d
    KServe + llm-d (multi-node, optional)     :s3c, after s3b, 7d
    Prometheus + Grafana dashboards           :s3d, after s2c, 5d
```

### Stage 1 — Single GPU node, one authenticated endpoint

1. Install NVIDIA GPU Operator (Helm); enable MIG on A100/H100 or time-slicing on other instance types.
2. Deploy vLLM for a 7–8B model (e.g. Llama 3.1 8B or Qwen2.5 7B, FP16) and a bge/Qwen3 embedding model.
3. Deploy LiteLLM (Helm) with PostgreSQL; register vLLM backends under unified model names; issue per-app virtual keys.
4. Upgrade Envoy Gateway v1.2.5 to Envoy AI Gateway v1.0 (drop-in, same CRDs and Gateway API compatibility), then add an `EnvoyExtensionPolicy` ext_authz filter backed by `openfga/openfga-envoy` for Keycloak JWT validation and per-model OpenFGA access control.

### Stage 2 — CPU fallback + larger models

1. Add CPU nodes running LocalAI/llama.cpp (quantized GGUF) as fallback backends in the same LiteLLM config.
2. Graduate to AWQ-INT4 14–32B models when 7–8B quality is insufficient.
3. Add Redis for multi-replica LiteLLM routing and rate-limit coordination.

### Stage 3 — Large models / production multi-GPU

1. For 70B+, use a 40–80 GB GPU instance (A100, H100, L40S on AWS/Infomaniak) or TP=2 across two smaller GPU nodes; serve with vLLM.
2. Adopt KServe + llm-d only when you have multiple GPU nodes and need cluster-wide scheduling. Below that threshold, plain vLLM + LiteLLM is simpler and sufficient.
3. Add Prometheus/Grafana via the vLLM Production Stack dashboards.

---

## Production-Readiness Assessment

| Component | Status | Notes |
|-----------|--------|-------|
| vLLM | ✅ Production-ready | Clear GPU serving default |
| LiteLLM proxy + virtual keys | ✅ Production-ready | MIT, free key management |
| Envoy AI Gateway v1.0 | ✅ Production-ready | GA June 2026; drop-in upgrade from v1.2.5 |
| OpenFGA | ✅ Production-ready | CNCF incubating |
| oauth2-proxy | ✅ Production-ready | CNCF sandbox, browser SSO only |
| NVIDIA GPU Operator | ✅ Production-ready | MIG (A100/H100) preferred; time-slicing for others |
| LocalAI | ✅ Production-ready | CPU clusters, multi-modal |
| Ollama | ⚠️ Dev/low-traffic only | No concurrent batching |
| KServe + llm-d | ✅ Production-ready (complex) | Multi-GPU cluster scale only |
| vLLM Production Stack | ✅ Production-ready | Heavier reference deployment |
| LiteLLM JWT/OIDC / SSO (>5) / SCIM | ❌ Enterprise (paid) | Handled at Envoy layer instead |
| TGI | ❌ Deprecated Dec 2025 | Do not use for new builds |

---

## Caveats

- **LiteLLM auth licensing is the crux:** virtual keys are free, but JWT/OIDC / SSO (>5 users) / SCIM / `enforce_rbac` are paid. Upgrading Envoy Gateway v1.2.5 to Envoy AI Gateway v1.0 and enforcing auth via ext_authz → OpenFGA avoids this constraint entirely with no new ingress component.
- **Time-slicing has no memory isolation** — a misconfigured pod can OOM its neighbours. Prefer MIG on A100/H100 instances.
- **Ollama does not scale under concurrency** — keep it for dev/CPU/low-traffic only.
- **OpenFGA integration is DIY** — there is no turnkey LiteLLM+OpenFGA product; you build the Check call into the Envoy ext_authz filter via `openfga/openfga-envoy`.
- **Benchmark numbers** vary widely by hardware, quantization, context length, and concurrency; validate on your own workload before committing.
