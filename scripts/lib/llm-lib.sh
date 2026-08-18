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

# Renders kernel/services/llm/chart once per
# entry in VLLM_INSTANCES (cluster instance data — never gentian-os
# defaults) and applies each. One gentian-os cluster can run several named
# vLLM instances at once (e.g. a small always-on model plus a larger
# on-demand one) — each gets its own PVC/Deployment/Service, and whatever
# was previously deployed but is no longer in VLLM_INSTANCES gets pruned
# (Deployment+Service only; PVCs are kept — see the warn below for why).
render_and_apply_vllm_gpu_manifest() {
    local instances="${VLLM_INSTANCES:-}"

    # Values, then one helm release, always. The loop moved into the chart.
    #
    # This rendered the manifest once per instance with sed and applied each,
    # then called prune_stale_vllm_instances to delete what the loop no longer
    # produced — because `kubectl apply` cannot remove what it was not given.
    # A release tracks its own objects, so removing an instance from the values
    # removes its Deployment and Service, and the prune has nothing left to do.
    local values
    values="$(mktemp)"
    printf 'gpuTimeSliceReplicas: %s\ninstances:\n' "${GPU_TIME_SLICE_REPLICAS:-1}" > "${values}"

    local instance count=0
    # No instances when GPU acceleration is off: the release still applies, so
    # GPU time-slicing stays configured and any instance from a previous run is
    # removed by the upgrade rather than by a separate sweep.
    [[ "${GPU_ACCELERATION:-false}" == "true" ]] || instances=""
    for instance in ${instances}; do
        if ! _vllm_instance_is_valid "${instance}"; then
            warn "Skipping invalid VLLM_INSTANCES entry '${instance}' — must be letters/digits/underscore, starting with a letter or underscore."
            continue
        fi

        local instance_upper; instance_upper="$(to_upper "${instance}")"
        local instance_k8s;   instance_k8s="$(_vllm_instance_k8s_name "${instance}")"

        local model_id_var="VLLM_${instance_upper}_MODEL_ID"
        local gpu_mem_var="VLLM_${instance_upper}_GPU_MEMORY_UTILIZATION"
        local max_len_var="VLLM_${instance_upper}_MAX_MODEL_LEN"
        local cache_var="VLLM_${instance_upper}_MODEL_CACHE_SIZE"
        local tag_var="VLLM_${instance_upper}_IMAGE_TAG"
        local tool_parser_var="VLLM_${instance_upper}_TOOL_CALL_PARSER"

        info "Serving vLLM instance '${instance}': ${!model_id_var:-Qwen/Qwen2.5-7B-Instruct}"
        # Quoted scalars: a model tag like 0.85 or 8192 is a string to the chart,
        # and an unquoted one would reach the manifest as a number.
        {
            printf '  - name: %s\n'                 "${instance_k8s}"
            printf '    modelId: "%s"\n'            "${!model_id_var:-Qwen/Qwen2.5-7B-Instruct}"
            printf '    gpuMemoryUtilization: "%s"\n' "${!gpu_mem_var:-0.85}"
            printf '    maxModelLen: "%s"\n'        "${!max_len_var:-8192}"
            printf '    modelCacheSize: "%s"\n'     "${!cache_var:-60Gi}"
            printf '    imageTag: "%s"\n'           "${!tag_var:-latest}"
            printf '    toolCallParser: "%s"\n'     "${!tool_parser_var:-}"
        } >> "${values}"
        count=$((count + 1))
    done

    if (( count == 0 )) && [[ "${GPU_ACCELERATION:-false}" == "true" ]]; then
        warn "GPU_ACCELERATION=true but no valid vLLM instance — the release will carry none."
    fi

    gentian_run helm upgrade --install gentian-llm "${SCRIPT_DIR}/kernel/services/llm/chart" \
        --namespace platform-kernel -f "${values}"
    rm -f "${values}"
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
# was already removed with the gentian-llm release).
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

        local instance_upper; instance_upper="$(to_upper "${instance}")"
        local instance_k8s
        instance_k8s="$(_vllm_instance_k8s_name "${instance}")"
        local model_id_var="VLLM_${instance_upper}_MODEL_ID"
        local model_id="${!model_id_var:-Qwen/Qwen2.5-7B-Instruct}"
        # Deterministic, reproducible from the model id — not a hand-picked
        # nickname — so re-running with the same model is a true no-op.
        local model_name
        model_name="$(printf '%s' "${model_id}" | tr '[:upper:]/' '[:lower:]-')"
        local api_base="http://vllm-${instance_k8s}-inference.platform-kernel.svc.cluster.local:8000/v1"

        # api_key is a required-but-unchecked field: LiteLLM's openai/
        # provider integration refuses to even build a client without a
        # non-empty api_key, regardless of whether the actual backend (vLLM
        # here) enforces auth at all — confirmed live, chat completions
        # 500'd with litellm.AuthenticationError until this was added.
        desired_json="$(jq -c --arg name "${model_name}" --arg model "openai/${model_id}" --arg base "${api_base}" \
            '. + [{"model_name":$name,"api_base":$base,"model":$model,"api_key":"not-needed"}]' <<<"${desired_json}")"
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
              # No -f: LiteLLM's /model/info returns HTTP 500 (not an empty
              # list) when zero models are registered yet — a real state on
              # a fresh proxy, not a fatal error. jq below treats anything
              # whose .data isn't an array (that 500's body is
              # {"detail":{"error":...}}) as an empty model list.
              INFO=\$(curl -s -H "\${AUTH}" "\${BASE}/model/info")

              ACTUAL=\$(printf '%s' "\${INFO}" | jq -c \\
                'if (.data | type) == "array" then
                   [.data[] | select((.litellm_params.api_base // "") | test("^http://vllm-[a-z0-9-]+-inference.platform-kernel.svc.cluster.local:8000/v1\$")) | {id: .model_info.id, model_name: .model_name, api_base: .litellm_params.api_base, model: .litellm_params.model, api_key: (.litellm_params.api_key // "")}]
                 else [] end')

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
                wbase=\$(printf '%s' "\${want}" | jq -r '.api_base')
                wmodel=\$(printf '%s' "\${want}" | jq -r '.model')
                wkey=\$(printf '%s' "\${want}" | jq -r '.api_key')

                match=\$(printf '%s' "\${ACTUAL}" | jq -c --arg base "\${wbase}" '[.[] | select(.api_base==\$base)] | first // empty')
                if [ -n "\${match}" ]; then
                  mname=\$(printf '%s' "\${match}" | jq -r '.model_name')
                  mmodel=\$(printf '%s' "\${match}" | jq -r '.model')
                  mkey=\$(printf '%s' "\${match}" | jq -r '.api_key')
                  mid=\$(printf '%s' "\${match}" | jq -r '.id')
                  if [ "\${mname}" = "\${wname}" ] && [ "\${mmodel}" = "\${wmodel}" ] && [ "\${mkey}" = "\${wkey}" ]; then
                    echo "LiteLLM model '\${wname}' already up to date (id=\${mid})"
                    continue
                  fi
                  echo "vLLM instance at \${wbase} changed (model/name/api_key) — removing stale entry '\${mname}' (id=\${mid})"
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${BASE}/model/delete" -d "{\"id\":\"\${mid}\"}" >/dev/null
                fi

                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${BASE}/model/new" -d "{\"model_name\":\"\${wname}\",\"litellm_params\":{\"model\":\"\${wmodel}\",\"api_base\":\"\${wbase}\",\"api_key\":\"\${wkey}\"}}" >/dev/null
                echo "Registered LiteLLM model '\${wname}' -> \${wmodel} (\${wbase})"
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
