#!/bin/bash
# LiteLLM Teams reconciliation — one Team per Gentian Tenant CR.
# Sourced from scripts/lib/load.sh; called from install_llm_serving
# (install.sh Step 13c) and op_llm_serving (update.sh --llm).

# Renders kernel/services/llm/manifests/${env}/vllm-gpu.yaml.tmpl with
# cluster instance data (cluster-settings.env — never gentian-os defaults)
# and applies it.
render_and_apply_vllm_gpu_manifest() {
    local manifests_dir="$1"

    local model_id="${VLLM_MODEL_ID:-Qwen/Qwen2.5-7B-Instruct}"
    local gpu_mem_util="${VLLM_GPU_MEMORY_UTILIZATION:-0.85}"
    local max_model_len="${VLLM_MAX_MODEL_LEN:-8192}"
    local cache_size="${VLLM_MODEL_CACHE_SIZE:-60Gi}"
    local image_tag="${VLLM_IMAGE_TAG:-latest}"

    info "Serving model ${model_id} (gpu-memory-utilization=${gpu_mem_util}, max-model-len=${max_model_len}, image tag=${image_tag})"

    local rendered
    rendered="$(mktemp)"
    sed -e "s|%VLLM_MODEL_ID%|${model_id}|g" \
        -e "s|%VLLM_GPU_MEMORY_UTILIZATION%|${gpu_mem_util}|g" \
        -e "s|%VLLM_MAX_MODEL_LEN%|${max_model_len}|g" \
        -e "s|%VLLM_MODEL_CACHE_SIZE%|${cache_size}|g" \
        -e "s|%VLLM_IMAGE_TAG%|${image_tag}|g" \
        "${manifests_dir}/vllm-gpu.yaml.tmpl" >"${rendered}"
    kubectl apply -f "${rendered}"
    rm -f "${rendered}"
}

# Renders kernel/services/llm/manifests/${env}/gpu-sharing.yaml.tmpl with
# cluster instance data (cluster-settings.env — never gentian-os defaults)
# and applies it. How many virtual slices to carve each physical GPU into
# is specific to this cluster's hardware.
render_and_apply_gpu_sharing_manifest() {
    local manifests_dir="$1"

    local replicas="${GPU_TIME_SLICE_REPLICAS:-4}"

    local rendered
    rendered="$(mktemp)"
    sed -e "s|%GPU_TIME_SLICE_REPLICAS%|${replicas}|g" \
        "${manifests_dir}/gpu-sharing.yaml.tmpl" >"${rendered}"
    kubectl apply -f "${rendered}"
    rm -f "${rendered}"
}

# Keycloak Admin API calls run in-cluster (Job) because litellm-proxy is a
# ClusterIP Service and is not reachable from the install host.
ensure_litellm_teams() {
    local ns="platform-kernel"
    local job_name="litellm-teams-sync"
    local tenants

    tenants=$(kubectl get tenants.gentianos.io -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    if [[ -z "${tenants}" ]]; then
        info "No tenants found — skipping LiteLLM team sync."
        return 0
    fi

    if ! kubectl get secret llm-sensitive-values -n "${ns}" >/dev/null 2>&1; then
        warn "llm-sensitive-values Secret not found — skipping LiteLLM team sync (run after the LLM ExternalSecret syncs)."
        return 0
    fi

    local tenant_csv
    tenant_csv=$(echo "${tenants}" | paste -sd, -)

    info "Syncing LiteLLM teams for tenants: ${tenant_csv}"
    kubectl delete job "${job_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: litellm-teams-sync
spec:
  ttlSecondsAfterFinished: 3600
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: teams-sync
          image: alpine:3.20
          command:
            - /bin/sh
            - -ec
            - |
              apk add --no-cache --quiet curl jq >/dev/null
              set -eu
              BASE="http://litellm-proxy.${ns}.svc.cluster.local:4000"
              AUTH="Authorization: Bearer \${LITELLM_MASTER_KEY}"
              EXISTING=\$(curl -sf -H "\${AUTH}" "\${BASE}/team/list")
              echo "\${TENANTS}" | tr ',' '\n' | while IFS= read -r tenant; do
                [ -z "\${tenant}" ] && continue
                FOUND=\$(printf '%s' "\${EXISTING}" | jq -r --arg t "\${tenant}" '.[] | select(.team_alias==\$t) | .team_id' | head -1)
                if [ -n "\${FOUND}" ] && [ "\${FOUND}" != "null" ]; then
                  echo "Team '\${tenant}' already exists (team_id=\${FOUND})"
                else
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${BASE}/team/new" -d "{\"team_alias\":\"\${tenant}\"}" >/dev/null
                  echo "Created LiteLLM team '\${tenant}'"
                fi
              done
          env:
            - name: TENANTS
              value: "${tenant_csv}"
            - name: LITELLM_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: llm-sensitive-values
                  key: litellm_master_key
EOF

    if kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=120s; then
        kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
        success "LiteLLM teams synced for: ${tenant_csv}"
    else
        warn "LiteLLM team sync Job failed or timed out."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
        return 1
    fi
}

# Registers/updates the GPU vLLM backend as a LiteLLM model, so changing
# VLLM_MODEL_ID + `./update.sh --llm` is enough on its own — no Admin
# Console / manual `/model/new` step. Keyed on api_base (there is exactly
# one vllm-inference backend — see render_and_apply_vllm_gpu_manifest):
# any existing LiteLLM entry pointed at that api_base gets deleted and
# recreated under the current model's name if the served model changed,
# so a swap never leaves a stale entry claiming to be the old model.
ensure_litellm_vllm_model() {
    local ns="platform-kernel"
    local job_name="litellm-vllm-model-sync"
    local model_id="${VLLM_MODEL_ID:-Qwen/Qwen2.5-7B-Instruct}"
    local api_base="http://vllm-inference.platform-kernel.svc.cluster.local:8000/v1"
    # Deterministic, reproducible from VLLM_MODEL_ID — not a hand-picked
    # nickname — so re-running with the same model is a true no-op.
    local model_name
    model_name="$(printf '%s' "${model_id}" | tr '[:upper:]/' '[:lower:]-')"

    if ! kubectl get secret llm-sensitive-values -n "${ns}" >/dev/null 2>&1; then
        warn "llm-sensitive-values Secret not found — skipping LiteLLM model sync (run after the LLM ExternalSecret syncs)."
        return 0
    fi

    info "Syncing LiteLLM model registration: ${model_name} (${model_id})"
    kubectl delete job "${job_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: litellm-vllm-model-sync
spec:
  ttlSecondsAfterFinished: 3600
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: model-sync
          image: alpine:3.20
          command:
            - /bin/sh
            - -ec
            - |
              apk add --no-cache --quiet curl jq >/dev/null
              set -eu
              BASE="http://litellm-proxy.${ns}.svc.cluster.local:4000"
              AUTH="Authorization: Bearer \${LITELLM_MASTER_KEY}"
              INFO=\$(curl -sf -H "\${AUTH}" "\${BASE}/model/info")

              EXISTING_ID=\$(printf '%s' "\${INFO}" | jq -r --arg base "\${VLLM_API_BASE}" \
                '.data[] | select(.litellm_params.api_base==\$base) | .model_info.id' | head -1)
              EXISTING_NAME=\$(printf '%s' "\${INFO}" | jq -r --arg base "\${VLLM_API_BASE}" \
                '.data[] | select(.litellm_params.api_base==\$base) | .model_name' | head -1)

              if [ -n "\${EXISTING_ID}" ] && [ "\${EXISTING_ID}" != "null" ] && [ "\${EXISTING_NAME}" = "\${MODEL_NAME}" ]; then
                echo "LiteLLM model '\${MODEL_NAME}' already up to date (id=\${EXISTING_ID})"
                exit 0
              fi

              if [ -n "\${EXISTING_ID}" ] && [ "\${EXISTING_ID}" != "null" ]; then
                echo "vLLM backend now serves a different model — removing stale LiteLLM entry '\${EXISTING_NAME}' (id=\${EXISTING_ID})"
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${BASE}/model/delete" -d "{\"id\":\"\${EXISTING_ID}\"}" >/dev/null
              fi

              curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                "\${BASE}/model/new" -d "{\"model_name\":\"\${MODEL_NAME}\",\"litellm_params\":{\"model\":\"openai/\${MODEL_ID}\",\"api_base\":\"\${VLLM_API_BASE}\"}}" >/dev/null
              echo "Registered LiteLLM model '\${MODEL_NAME}' -> \${MODEL_ID}"
          env:
            - name: MODEL_NAME
              value: "${model_name}"
            - name: MODEL_ID
              value: "${model_id}"
            - name: VLLM_API_BASE
              value: "${api_base}"
            - name: LITELLM_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: llm-sensitive-values
                  key: litellm_master_key
EOF

    if kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=120s; then
        kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
        success "LiteLLM model registration synced: ${model_name}"
    else
        warn "LiteLLM model sync Job failed or timed out."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
        return 1
    fi
}
