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
        prune_stale_vllm_instances ""
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

    prune_stale_vllm_instances "${instances}"
}

# Removes the Deployment+Service for any vLLM instance that was previously
# applied but is no longer in the given desired-instances list — pass ""
# to remove every real vLLM instance (e.g. GPU_ACCELERATION flipped back
# to false; the mock backend's fixed-name Deployment doesn't collide with
# any of these, so nothing prunes them automatically otherwise). PVCs are
# deliberately left behind (orphaned, not deleted) — cached model weights
# can be tens of GB and take many minutes to redownload (see the HF_TOKEN
# rate-limit note in agentic-ai.md §10.2); re-adding the same instance ID
# later picks the cache back up instead of paying that cost again. Remove
# stale PVCs manually if you want the disk space back:
#   kubectl delete pvc -n platform-kernel -l gentianos.io/vllm-instance=<id>
prune_stale_vllm_instances() {
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
    warn "  PVCs kept (see prune_stale_vllm_instances comment) — delete manually if you want the cached weights gone too."
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

# Registers/updates every VLLM_INSTANCES entry as a LiteLLM model (one
# shared LiteLLM proxy in front of however many vLLM instances exist — see
# llm-services.yaml), so editing VLLM_INSTANCES + `./update.sh --llm` is
# enough on its own — no Admin Console / manual `/model/new` step. Each
# instance is keyed on its own api_base (vllm-<id>-inference.platform-
# kernel.svc.cluster.local — one Service per instance, never shared): any
# existing LiteLLM entry pointed at that api_base gets deleted and
# recreated under the current model's name if the served model changed,
# so a swap never leaves a stale entry claiming to be the old model. Any
# LiteLLM entry pointed at a vllm-*-inference api_base that ISN'T one of
# the current VLLM_INSTANCES gets removed entirely (the instance itself
# was already pruned by prune_stale_vllm_instances).
ensure_litellm_vllm_model() {
    local ns="platform-kernel"
    local job_name="litellm-vllm-model-sync"
    local instances="${VLLM_INSTANCES:-}"

    if ! kubectl get secret llm-sensitive-values -n "${ns}" >/dev/null 2>&1; then
        warn "llm-sensitive-values Secret not found — skipping LiteLLM model sync (run after the LLM ExternalSecret syncs)."
        return 0
    fi

    # Build the desired-state JSON array on the host (jq is a required
    # tool — see check_prereqs) rather than parsing a delimited string
    # inside the Job's alpine/busybox shell.
    local desired_json="[]"
    local instance
    for instance in ${instances}; do
        _vllm_instance_is_valid "${instance}" || continue

        local instance_upper="${instance^^}"
        local instance_k8s
        instance_k8s="$(_vllm_instance_k8s_name "${instance}")"
        local model_id_var="VLLM_${instance_upper}_MODEL_ID"
        local model_id="${!model_id_var:-Qwen/Qwen2.5-7B-Instruct}"
        # Deterministic, reproducible from the model id — not a hand-picked
        # nickname — so re-running with the same model is a true no-op.
        local model_name
        model_name="$(printf '%s' "${model_id}" | tr '[:upper:]/' '[:lower:]-')"
        local api_base="http://vllm-${instance_k8s}-inference.platform-kernel.svc.cluster.local:8000/v1"

        desired_json="$(jq -c --arg name "${model_name}" --arg id "${model_id}" --arg base "${api_base}" \
            '. + [{"model_name":$name,"model_id":$id,"api_base":$base}]' <<<"${desired_json}")"
    done

    # Note: deliberately NOT returning early when desired_json is still
    # "[]" (no vLLM instances configured) — the Job below also removes any
    # LiteLLM entry left pointing at a vllm-*-inference api_base that isn't
    # in the desired set, so this still needs to run to clean up
    # registrations for instances that were removed entirely (or
    # GPU_ACCELERATION flipped back to false — see the mock branch in
    # install.sh/update.sh, which calls this unconditionally).
    if [[ "${desired_json}" == "[]" ]]; then
        info "No vLLM instances configured — checking for stale LiteLLM registrations to remove."
    else
        info "Syncing LiteLLM model registrations for instances: ${instances}"
    fi
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
              DESIRED='${desired_json}'
              INFO=\$(curl -sf -H "\${AUTH}" "\${BASE}/model/info")

              ACTUAL=\$(printf '%s' "\${INFO}" | jq -c \\
                '[.data[] | select((.litellm_params.api_base // "") | test("^http://vllm-[a-z0-9-]+-inference\\.platform-kernel\\.svc\\.cluster\\.local:8000/v1\$")) | {id: .model_info.id, model_name: .model_name, api_base: .litellm_params.api_base}]')

              printf '%s' "\${ACTUAL}" | jq -c --argjson desired "\${DESIRED}" \\
                '.[] | select(.api_base as \$b | ([\$desired[].api_base] | index(\$b)) == null)' | \\
              while IFS= read -r stale; do
                [ -z "\${stale}" ] && continue
                sid=\$(printf '%s' "\${stale}" | jq -r '.id')
                sname=\$(printf '%s' "\${stale}" | jq -r '.model_name')
                echo "Removing stale LiteLLM model '\${sname}' (id=\${sid}) — vLLM instance no longer in VLLM_INSTANCES"
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${BASE}/model/delete" -d "{\"id\":\"\${sid}\"}" >/dev/null
              done

              printf '%s' "\${DESIRED}" | jq -c '.[]' | while IFS= read -r want; do
                wname=\$(printf '%s' "\${want}" | jq -r '.model_name')
                wid=\$(printf '%s' "\${want}" | jq -r '.model_id')
                wbase=\$(printf '%s' "\${want}" | jq -r '.api_base')

                match=\$(printf '%s' "\${ACTUAL}" | jq -c --arg base "\${wbase}" '[.[] | select(.api_base==\$base)] | first // empty')
                if [ -n "\${match}" ]; then
                  mname=\$(printf '%s' "\${match}" | jq -r '.model_name')
                  mid=\$(printf '%s' "\${match}" | jq -r '.id')
                  if [ "\${mname}" = "\${wname}" ]; then
                    echo "LiteLLM model '\${wname}' already up to date (id=\${mid})"
                    continue
                  fi
                  echo "vLLM instance at \${wbase} now serves a different model — removing stale entry '\${mname}' (id=\${mid})"
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${BASE}/model/delete" -d "{\"id\":\"\${mid}\"}" >/dev/null
                fi

                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${BASE}/model/new" -d "{\"model_name\":\"\${wname}\",\"litellm_params\":{\"model\":\"openai/\${wid}\",\"api_base\":\"\${wbase}\"}}" >/dev/null
                echo "Registered LiteLLM model '\${wname}' -> \${wid} (\${wbase})"
              done
          env:
            - name: LITELLM_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: llm-sensitive-values
                  key: litellm_master_key
EOF

    if kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=120s; then
        kubectl logs -n "${ns}" "job/${job_name}" --tail=30 2>/dev/null || true
        success "LiteLLM model registrations synced."
    else
        warn "LiteLLM model sync Job failed or timed out."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=30 2>/dev/null || true
        return 1
    fi
}
