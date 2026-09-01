#!/bin/bash
# LiteLLM Teams reconciliation — one Team per Gentian Tenant CR.
# Sourced from scripts/lib/load.sh; called from install_llm_serving
# (./install.sh --step D-05-llm-serving) and E-02-litellm-reconcile.

# Renders kernel/services/llm/chart once per entry in the claim's
# llm.instances (clusters/<id>/kernel/claims/cluster.yaml — cluster
# instance data, never gentian-os defaults) and applies each. One
# gentian-os cluster can run several named vLLM instances at once (e.g. a
# small always-on model plus a larger on-demand one) — each gets its own
# PVC/Deployment/Service, and whatever was previously deployed but is no
# longer on the claim gets pruned (Deployment+Service only; PVCs are
# kept — see the warn below for why).
# =============================================================================
# _time_slicing_is_foreign — is GPU sharing already somebody else's?
#
# The chart templates time-slicing-config in gpu-operator-resources: the
# ConfigMap the NVIDIA GPU operator reads GPU sharing from. It is cluster-wide
# and shared with every GPU workload on the node, not only this platform's.
#
# A cluster that already has one has already decided how its GPUs are carved up,
# usually when the GPU operator was installed and often long before this
# platform existed. Taking it over is wrong twice: Helm refuses to adopt an
# object it did not create, and if it did, the key name differs — the operator's
# own convention is `any`, this chart writes `time-slicing-config.yaml` — so
# adoption would rewrite the node's GPU configuration rather than inherit it.
#
# So: detect it, leave it alone, and say what it says.
#
# Echoes a human description of the existing configuration; returns 1 when the
# ConfigMap is absent or already ours, in which case this release manages it.
# =============================================================================
_time_slicing_is_foreign() {
    local cm="time-slicing-config" ns="gpu-operator-resources" json
    json="$(kubectl get configmap "${cm}" -n "${ns}" -o json 2>/dev/null)" || return 1
    [[ -n "${json}" ]] || return 1

    local owner
    owner="$(jq -r '.metadata.annotations["meta.helm.sh/release-name"] // ""' <<<"${json}" 2>/dev/null)"
    [[ "${owner}" == "gentian-llm" ]] && return 1

    # Replicas out of whichever key the existing config uses — the operator's
    # `any`, this chart's `time-slicing-config.yaml`, or a per-node key.
    local replicas
    replicas="$(jq -r '.data // {} | to_entries[].value' <<<"${json}" 2>/dev/null \
        | grep -oE 'replicas:[[:space:]]*[0-9]+' | grep -oE '[0-9]+' | head -1)"

    local desc="${ns}/${cm}"
    [[ -n "${owner}" ]] && desc="${desc}, owned by the Helm release '${owner}'"
    [[ -n "${replicas}" ]] && desc="${desc}, ${replicas} replica(s) per GPU"
    echo "${desc}"
    return 0
}

render_and_apply_vllm_gpu_manifest() {
    # The instance list comes from the claim, and the claim's shape is the
    # chart's shape — same field names, same defaults. There is no translation
    # step because there is nothing to translate.
    #
    # It used to be assembled from VLLM_<ID>_MODEL_ID and five siblings per
    # instance, read by indirect expansion. That put what a cluster serves in
    # variables no reviewer could find and no schema could check: a typo in an
    # instance id silently produced a default model, and grepping for a reader
    # found none.
    local claim_file
    claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"

    local values
    values="$(mktemp)"
    printf 'gpuTimeSliceReplicas: %s\n' "${GPU_TIME_SLICE_REPLICAS:-1}" > "${values}"

    # No instances when GPU acceleration is off: the release still applies, so
    # GPU time-slicing stays configured and any instance from a previous run is
    # removed by the upgrade rather than by a separate sweep.
    if [[ "${GPU_ACCELERATION:-false}" != "true" ]]; then
        printf 'instances: []\n' >> "${values}"
    elif [[ -r "${claim_file}" ]]; then
        # A straight projection of the claim, not a translation of it: the
        # chart's value names ARE the claim's field names, so the list is copied
        # across and nothing in between can rename or drop a field. Per-instance
        # defaults live in the chart, which is what reads them.
        #
        # python3 rather than yq because both yq flavours are seen in the wild
        # with incompatible syntax — the same reason yq_get exists — and this
        # needs to emit a list rather than read one scalar.
        python3 -c '
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
items = (((doc.get("spec") or {}).get("llm") or {}).get("instances")) or []
yaml.safe_dump({"instances": items}, sys.stdout, default_flow_style=False, sort_keys=False)
' "${claim_file}" >> "${values}"
    else
        printf 'instances: []\n' >> "${values}"
    fi

    local count
    # Indentation-agnostic: PyYAML writes a list under a key unindented, and a
    # hand-written values file usually indents it.
    count="$(grep -cE '^[[:space:]]*-[[:space:]]+name:' "${values}" || true)"
    if [[ "${count}" == "0" && "${GPU_ACCELERATION:-false}" == "true" ]]; then
        warn "llm.gpuAcceleration is true but the claim lists no instances under llm.instances."
        warn "  The release will carry none. Add them to claims/cluster.yaml."
    else
        info "Serving ${count} vLLM instance(s) from the claim."
    fi

    # GPU sharing is the node's, not this release's, when something else already
    # configured it.
    local manage_slicing="true" existing
    if existing="$(_time_slicing_is_foreign)"; then
        manage_slicing="false"
        info "GPU time-slicing is already configured on this cluster — leaving it alone."
        info "  ${existing}"
        local want="${GPU_TIME_SLICE_REPLICAS:-1}"
        local have="${existing##*, }"; have="${have%% *}"
        if [[ -n "${have}" && "${have}" =~ ^[0-9]+$ && "${have}" != "${want}" ]]; then
            warn "  llm.gpuTimeSliceReplicas is ${want} on the claim, but the cluster is set to ${have}."
            warn "  The cluster's value wins. Set the claim to ${have}, or change it where it is owned."
        fi
    fi

    gentian_run helm upgrade --install gentian-llm "${SCRIPT_DIR}/kernel/services/llm/chart" \
        --namespace platform-kernel \
        --set "manageTimeSlicing=${manage_slicing}" -f "${values}"
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
# ensure_litellm_teams was here. Per-tenant LiteLLM Teams are the
# TenantReconciler's now (internal/controller/litellm_team.go), for the same
# reason tenant realm SMTP moved there in e29db18e: per-tenant state converged
# by a script only converges when somebody re-runs the installer, so a tenant
# created afterwards had no Team until then. A controller reconciles it when the
# Tenant appears, and retries when LiteLLM is not up yet.


# Registers/updates every claim llm.instances entry as a LiteLLM model
# (one shared LiteLLM proxy in front of however many vLLM instances exist
# — see llm-services.yaml), so editing the claim + `./install.sh --step
# D-05-llm-serving` is enough on its own — no Admin Console / manual
# `/model/new` step. Each
# instance is keyed on its own api_base (vllm-<id>-inference.platform-
# kernel.svc.cluster.local — one Service per instance, never shared): any
# existing LiteLLM entry pointed at that api_base gets deleted and
# recreated under the current model's name if the served model changed,
# so a swap never leaves a stale entry claiming to be the old model. Any
# LiteLLM entry pointed at a vllm-*-inference api_base that ISN'T one of
# the claim's current llm.instances gets removed entirely (the instance
# itself was already removed with the gentian-llm release).
# =============================================================================
# _wait_for_job — complete, failed, or still running; a timeout is none of them.
#
# `kubectl wait --timeout` returns non-zero when the deadline passes, which is
# not the same as the Job failing: a pod that is slow to schedule or pull an
# image finishes a minute later and succeeds. Treating the timeout as a failure
# reported "sync failed" over a Job that had done its work, and the operator was
# told to re-run something that had already run.
#
# So the deadline is a prompt to look, not a verdict. Returns 0 when the Job
# succeeded — whether it did so before or after the wait gave up.
# =============================================================================
_wait_for_job() {
    local job="$1" ns="$2" timeout="${3:-120s}"
    kubectl wait "job/${job}" -n "${ns}" --for=condition=complete --timeout="${timeout}" >/dev/null 2>&1 && return 0

    # Deadline passed. Ask the Job what actually happened.
    if ! kubectl get "job/${job}" -n "${ns}" >/dev/null 2>&1; then
        error "  ${job} does not exist in ${ns}."
        return 1
    fi
    local succeeded failed
    succeeded="$(kubectl get "job/${job}" -n "${ns}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo 0)"
    failed="$(kubectl get "job/${job}" -n "${ns}" -o jsonpath='{.status.failed}' 2>/dev/null || echo 0)"
    if [[ "${succeeded:-0}" -ge 1 ]]; then
        info "  ${job} completed after the ${timeout} wait expired."
        return 0
    fi
    if [[ "${failed:-0}" -ge 1 ]]; then
        error "  ${job} failed (${failed} failed pod(s))."
        return 1
    fi
    warn "  ${job} is still running after ${timeout}; not waiting further."
    return 1
}

ensure_litellm_vllm_model() {
    local ns="platform-kernel"
    local job_name="litellm-vllm-model-sync"

    # The same claim the vLLM release is rendered from, so LiteLLM advertises
    # exactly what is being served. Reading a second source here is how the
    # gateway and the backends came to disagree about which models exist.
    local claim_file
    claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"

    if ! kubectl get secret llm-sensitive-values -n "${ns}" >/dev/null 2>&1; then
        warn "llm-sensitive-values Secret not found — skipping LiteLLM model sync (run after the LLM ExternalSecret syncs)."
        return 0
    fi

    # Build the desired-state JSON array on the host (jq is a required
    # tool — see check_prereqs) rather than parsing a delimited string
    # inside the Job's alpine/busybox shell.
    local desired_json="[]"
    if [[ "${GPU_ACCELERATION:-false}" == "true" && -r "${claim_file}" ]]; then
        # model_name is derived from the model id rather than being a hand-picked
        # nickname, so re-running with the same model is a true no-op.
        #
        # api_key is a required-but-unchecked field: LiteLLM's openai/ provider
        # refuses to build a client without a non-empty api_key, regardless of
        # whether vLLM enforces auth at all — chat completions 500'd with
        # litellm.AuthenticationError until this was added.
        desired_json="$(python3 -c '
import sys, json, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
items = (((doc.get("spec") or {}).get("llm") or {}).get("instances")) or []
out = []
for i in items:
    name, model_id = i.get("name"), i.get("modelId")
    if not name or not model_id:
        continue
    out.append({
        "model_name": model_id.lower().replace("/", "-"),
        "api_base": f"http://vllm-{name}-inference.platform-kernel.svc.cluster.local:8000/v1",
        "model": f"openai/{model_id}",
        "api_key": "not-needed",
    })
json.dump(out, sys.stdout)
' "${claim_file}")"
    fi

    # Note: deliberately NOT returning early when desired_json is still
    # "[]" (no vLLM instances configured) — the Job below also removes any
    # LiteLLM entry left pointing at a vllm-*-inference api_base that isn't
    # in the desired set, so this still needs to run to clean up
    # registrations for instances that were removed entirely (or
    # GPU_ACCELERATION flipped back to false — see the mock branch in
    # install_llm_serving, which calls this unconditionally).
    if [[ "${desired_json}" == "[]" ]]; then
        info "No vLLM instances configured — checking for stale LiteLLM registrations to remove."
    else
        info "Syncing LiteLLM model registrations from the claim ($(jq -r 'length' <<<"${desired_json}") model(s))."
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
              # litellm-proxy is typically still booting when this Job is
              # created on a fresh install — its own CNPG database first,
              # then schema migrations. The first request used to be the
              # sync itself, so curl died with exit 7 (connection refused)
              # under set -e, three fast pod retries, Job dead inside two
              # minutes — on every fresh install, regardless of whether
              # anything was actually wrong. Wait for the proxy to answer
              # at all before syncing.
              lp_tries=0
              until curl -s -o /dev/null "\${BASE}/health/liveliness"; do
                lp_tries=\$((lp_tries + 1))
                if [ "\${lp_tries}" -ge 60 ]; then
                  echo "litellm-proxy did not answer within 10 minutes" >&2
                  exit 1
                fi
                echo "litellm-proxy not answering yet (attempt \${lp_tries}/60); retrying in 10s..."
                sleep 10
              done
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
                echo "Removing stale LiteLLM model '\${sname}' (id=\${sid}) — vLLM instance no longer on the claim's llm.instances"
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

    if _wait_for_job "${job_name}" "${ns}" 720s; then
        kubectl logs -n "${ns}" "job/${job_name}" --tail=30 2>/dev/null || true
        success "LiteLLM model registrations synced."
    else
        warn "LiteLLM model sync did not complete."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=30 2>/dev/null || true
        return 1
    fi
}
