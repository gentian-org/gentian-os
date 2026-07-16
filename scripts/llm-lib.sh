#!/bin/bash
# LiteLLM Teams reconciliation — one Team per Gentian Tenant CR.
# Sourced from scripts/lib/load.sh; called from install_llm_serving
# (install.sh Step 13c) and op_llm_serving (update.sh --llm).

# A vLLM instance ID doubles as a bash indirect-expansion variable-name
# component (VLLM_<ID>_MODEL_ID etc.) and, lowercased with underscores
# turned into hyphens, a Kubernetes resource-name component
# (vllm-<id>-inference) — so it must be a valid bash identifier.
_vllm_instance_is_valid() {
    [[ "$1" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]
}

_vllm_instance_k8s_name() {
    local id="${1,,}"
    printf '%s' "${id//_/-}"
}

# Renders kernel/services/llm/manifests/${env}/vllm-gpu.yaml.tmpl once per
# entry in VLLM_INSTANCES (cluster instance data — never gentian-os
# defaults) and applies each. One gentian-os cluster can run several named
# vLLM instances at once (e.g. a small always-on model plus a larger
# on-demand one) — each gets its own PVC/Deployment/Service, and whatever
# was previously deployed but is no longer in VLLM_INSTANCES gets pruned
# (Deployment+Service only; PVCs are kept — see the warn below for why).
render_and_apply_vllm_gpu_manifest() {
    local manifests_dir="$1"
    local instances="${VLLM_INSTANCES:-}"

    if [[ -z "${instances}" ]]; then
        warn "GPU_ACCELERATION=true but VLLM_INSTANCES is empty — no vLLM instance to deploy."
        warn "  Set VLLM_INSTANCES (space-separated instance IDs, e.g. \"qwen\") in cluster-settings.env."
        return 0
    fi

    local instance
    for instance in ${instances}; do
        if ! _vllm_instance_is_valid "${instance}"; then
            warn "Skipping invalid VLLM_INSTANCES entry '${instance}' — must be letters/digits/underscore, starting with a letter or underscore."
            continue
        fi

        local instance_upper="${instance^^}"
        local instance_k8s
        instance_k8s="$(_vllm_instance_k8s_name "${instance}")"

        local model_id_var="VLLM_${instance_upper}_MODEL_ID"
        local gpu_mem_var="VLLM_${instance_upper}_GPU_MEMORY_UTILIZATION"
        local max_len_var="VLLM_${instance_upper}_MAX_MODEL_LEN"
        local cache_var="VLLM_${instance_upper}_MODEL_CACHE_SIZE"
        local tag_var="VLLM_${instance_upper}_IMAGE_TAG"

        local model_id="${!model_id_var:-Qwen/Qwen2.5-7B-Instruct}"
        local gpu_mem_util="${!gpu_mem_var:-0.85}"
        local max_model_len="${!max_len_var:-8192}"
        local cache_size="${!cache_var:-60Gi}"
        local image_tag="${!tag_var:-latest}"

        info "Serving vLLM instance '${instance}': ${model_id} (gpu-memory-utilization=${gpu_mem_util}, max-model-len=${max_model_len}, image tag=${image_tag})"

        local rendered
        rendered="$(mktemp)"
        sed -e "s|%VLLM_INSTANCE%|${instance_k8s}|g" \
            -e "s|%VLLM_MODEL_ID%|${model_id}|g" \
            -e "s|%VLLM_GPU_MEMORY_UTILIZATION%|${gpu_mem_util}|g" \
            -e "s|%VLLM_MAX_MODEL_LEN%|${max_model_len}|g" \
            -e "s|%VLLM_MODEL_CACHE_SIZE%|${cache_size}|g" \
            -e "s|%VLLM_IMAGE_TAG%|${image_tag}|g" \
            "${manifests_dir}/vllm-gpu.yaml.tmpl" >"${rendered}"
        kubectl apply -f "${rendered}"
        rm -f "${rendered}"
    done

    _prune_stale_vllm_instances "${instances}"
}

# Removes the Deployment+Service for any vLLM instance that was previously
# applied but is no longer in the current VLLM_INSTANCES list. PVCs are
# deliberately left behind (orphaned, not deleted) — cached model weights
# can be tens of GB and take many minutes to redownload (see the HF_TOKEN
# rate-limit note in agentic-ai.md §10.2); re-adding the same instance ID
# later picks the cache back up instead of paying that cost again. Remove
# stale PVCs manually if you want the disk space back:
#   kubectl delete pvc -n platform-kernel -l gentianos.io/vllm-instance=<id>
_prune_stale_vllm_instances() {
    local desired_instances="$1"
    local ns="platform-kernel"

    local desired_k8s_ids=" "
    local instance
    for instance in ${desired_instances}; do
        _vllm_instance_is_valid "${instance}" || continue
        desired_k8s_ids="${desired_k8s_ids}$(_vllm_instance_k8s_name "${instance}") "
    done

    local live_ids
    live_ids=$(kubectl get deployment -n "${ns}" -l app.kubernetes.io/component=vllm-instance \
        -o jsonpath='{range .items[*]}{.metadata.labels.gentianos\.io/vllm-instance}{"\n"}{end}' 2>/dev/null \
        | sort -u)
    [[ -z "${live_ids}" ]] && return 0

    local stale=""
    local id
    while IFS= read -r id; do
        [[ -z "${id}" ]] && continue
        [[ "${desired_k8s_ids}" == *" ${id} "* ]] || stale="${stale} ${id}"
    done <<< "${live_ids}"
    [[ -z "${stale}" ]] && return 0

    warn "Removing vLLM instance(s) no longer in VLLM_INSTANCES:${stale}"
    warn "  PVCs kept (see _prune_stale_vllm_instances comment) — delete manually if you want the cached weights gone too."
    for id in ${stale}; do
        kubectl delete deployment,service -n "${ns}" -l "gentianos.io/vllm-instance=${id}" --ignore-not-found=true
    done
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
