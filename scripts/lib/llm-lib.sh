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
    local succeeded failed job_failed active
    succeeded="$(kubectl get "job/${job}" -n "${ns}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo 0)"
    failed="$(kubectl get "job/${job}" -n "${ns}" -o jsonpath='{.status.failed}' 2>/dev/null || echo 0)"
    if [[ "${succeeded:-0}" -ge 1 ]]; then
        info "  ${job} completed after the ${timeout} wait expired."
        return 0
    fi
    # failed>=1 alone is not a verdict: it counts pods, and a Job whose first
    # pod exhausted its in-script wait retries with a fresh pod under its
    # backoffLimit. Reporting "failed" here while pod #2 was mid-poll is
    # exactly how a sync that went on to register its model got announced as
    # an install error. The Job has failed only when its Failed condition
    # says so — backoffLimit spent.
    job_failed="$(kubectl get "job/${job}" -n "${ns}" \
        -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || echo "")"
    if [[ "${job_failed}" == "True" ]]; then
        error "  ${job} failed (${failed} failed pod(s), retry budget spent)."
        return 1
    fi
    # 2, not 1: still-in-flight and definitively-failed are different answers,
    # and the callers word their reports from them — "failed — retry" over a
    # Job that went on to complete minutes later is how a healthy cold start
    # kept reading as a broken one.
    active="$(kubectl get "job/${job}" -n "${ns}" -o jsonpath='{.status.active}' 2>/dev/null || echo 0)"
    if [[ "${active:-0}" -ge 1 || "${failed:-0}" -ge 1 ]]; then
        warn "  ${job} is still retrying after ${timeout} (${failed:-0} failed pod(s) so far); not waiting further."
        warn "  It keeps running in-cluster: kubectl get job ${job} -n ${ns}"
        return 2
    fi
    warn "  ${job} is still running after ${timeout}; not waiting further."
    return 2
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
    elif [[ $? -eq 2 ]]; then
        # Not a failure: the LLM stack's cold start (its own database's
        # initdb, then the proxy's migrations) genuinely outlasts this wait
        # on a fresh cluster, and the Job's in-script polling carries the
        # sync home once the proxy answers — observed completing ~10 minutes
        # after this point, every cold install. E-02 re-runs the sync anyway.
        kubectl logs -n "${ns}" "job/${job_name}" --tail=15 2>/dev/null || true
        return 2
    else
        warn "LiteLLM model sync did not complete."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=30 2>/dev/null || true
        return 1
    fi
}

# =============================================================================
# ensure_litellm_provider_models — the claim's spec.llm.providers, registered.
#
# The sibling of ensure_litellm_vllm_model, for models this cluster does not
# serve itself. Same shape for the same reason: the claim is the declaration, a
# Job reconciles LiteLLM's registry against it, and what is not on the claim is
# removed. Editing the Admin Console instead produces an entry the next run
# deletes.
#
# Three things differ from the vLLM sync, and each is why this is its own
# function rather than a branch in that one:
#
#   Ownership cannot be read off the api_base. A vLLM entry is recognisable by
#   its api_base matching vllm-*-inference.platform-kernel; a provider's api_base
#   is whatever the operator declared, and a rule broad enough to match it would
#   also match, and then delete, anything a human registered by hand. So these
#   entries carry model_info.gentian_managed, and only entries carrying it are
#   ever pruned. LiteLLM's ModelInfo is extra="allow", so custom keys round-trip.
#
#   The api_key comes back masked from /model/info, so it cannot be compared to
#   decide whether a registration is current — comparing it would delete and
#   recreate every model on every run. A fingerprint (sha256, first 8 hex, of a
#   high-entropy token) goes in model_info instead, which makes a rotated key
#   detectable without reading the key back.
#
#   The provider might not be reachable. vLLM is a Service in the same
#   namespace; a provider is on the internet, behind whatever egress policy the
#   cluster has. Registering a model whose endpoint cannot be reached produces
#   the failure this whole feature exists to avoid — a model that lists fine and
#   fails at the first request — so each provider is probed from inside the
#   cluster first, and a provider that fails the probe keeps whatever
#   registrations it already has rather than gaining broken ones.
# =============================================================================
ensure_litellm_provider_models() {
    local ns="platform-kernel"
    local job_name="litellm-provider-model-sync"
    local secret_name="llm-provider-credentials"

    local claim_file
    claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"

    if ! kubectl get secret llm-sensitive-values -n "${ns}" >/dev/null 2>&1; then
        warn "llm-sensitive-values Secret not found — skipping LiteLLM provider sync (run after the LLM ExternalSecret syncs)."
        return 0
    fi

    local desired_json="[]"
    if [[ -r "${claim_file}" ]]; then
        # model_name is provider/model rather than the upstream id: the upstream
        # ids collide across providers (two of them serve google/gemma-4-31B-it)
        # and say nothing about who is being billed for the call. The prefix is
        # what a tenant sees in Open WebUI's model list.
        desired_json="$(python3 -c '
import sys, json, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
providers = (((doc.get("spec") or {}).get("llm") or {}).get("providers")) or []
out = []
for p in providers:
    name, base, prop = p.get("name"), p.get("apiBase"), p.get("apiKeyProperty")
    if not (name and base and prop):
        continue
    for m in (p.get("models") or []):
        mname, model = m.get("name"), m.get("model")
        if not (mname and model):
            continue
        entry = {
            "provider": name,
            "model_name": f"{name}/{mname}",
            # openai/ regardless of who serves it: the contract is the
            # OpenAI-compatible one, which is what makes these interchangeable.
            "model": f"openai/{model}",
            "api_base": base.rstrip("/"),
            "key_property": prop,
            "mode": m.get("mode") or "chat",
        }
        if m.get("maxTokens"):
            entry["max_tokens"] = int(m["maxTokens"])
        out.append(entry)
json.dump(out, sys.stdout)
' "${claim_file}")" || {
            warn "Could not read spec.llm.providers from ${claim_file} — skipping provider sync."
            return 1
        }
    fi

    # Not returning early on "[]": the Job also removes registrations for
    # providers deleted from the claim, which is exactly the state that leaves
    # desired empty. Same reasoning as the vLLM sync.
    if [[ "${desired_json}" == "[]" ]]; then
        info "No external LLM providers on the claim — checking for stale provider registrations to remove."
    else
        info "Syncing LiteLLM provider registrations from the claim ($(jq -r 'length' <<<"${desired_json}") model(s))."
    fi

    # Mounted, not envFrom: the property names come from the claim, so the shell
    # would have to build a variable name from data and dereference it. A
    # directory of files is the same lookup without the indirection.
    local secret_optional="true"
    kubectl get secret "${secret_name}" -n "${ns}" >/dev/null 2>&1 || \
        warn "${secret_name} Secret not found — providers will be reported as missing their API key until the ExternalSecret syncs."

    kubectl delete job "${job_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: litellm-provider-model-sync
spec:
  ttlSecondsAfterFinished: 3600
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      volumes:
        - name: provider-keys
          secret:
            secretName: ${secret_name}
            optional: ${secret_optional}
      containers:
        - name: provider-sync
          image: alpine:3.20
          volumeMounts:
            - name: provider-keys
              mountPath: /etc/llm-providers
              readOnly: true
          command:
            - /bin/sh
            - -ec
            - |
              apk add --no-cache --quiet curl jq >/dev/null
              set -eu
              BASE="http://litellm-proxy.${ns}.svc.cluster.local:4000"
              AUTH="Authorization: Bearer \${LITELLM_MASTER_KEY}"
              DESIRED='${desired_json}'
              MARKER="cluster-claim-provider"
              # Collected rather than returned: the loops below run in pipeline
              # subshells, so a variable set inside one is lost at the pipe.
              : >/tmp/degraded
              : >/tmp/failed

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

              # ---- per-provider preflight: egress, then authorization --------
              # From inside the cluster, because that is where it has to work.
              # The same request from an operator's workstation proves nothing
              # about a pod's egress, which is how a provider that answers fine
              # over the desk was configured and then listed no models.
              printf '%s' "\${DESIRED}" | jq -r '[.[] | {provider, api_base, key_property}] | unique_by(.provider) | .[] | @base64' | \\
              while IFS= read -r row; do
                [ -z "\${row}" ] && continue
                d() { printf '%s' "\${row}" | base64 -d | jq -r "\$1"; }
                pname=\$(d '.provider'); pbase=\$(d '.api_base'); pprop=\$(d '.key_property')
                keyfile="/etc/llm-providers/\${pprop}"
                if [ ! -s "\${keyfile}" ]; then
                  echo "PROVIDER \${pname}: no API key — '\${pprop}' is not a property of ${secret_name}."
                  echo "  Supply it in the Admin Console under 'llm-provider-\${pname}', or see credentials.yaml."
                  echo "\${pname}" >>/tmp/degraded; echo 1 >>/tmp/failed
                  continue
                fi
                ptoken=\$(tr -d '\\r\\n' <"\${keyfile}")
                # No "|| echo 000" fallback: -w already prints 000 when the
                # transfer never happened, so the fallback appended a second one
                # and the "000000" fell through to the catch-all branch —
                # swallowing the egress diagnosis this probe exists to give.
                # The "or true" is because a failed transfer is a non-zero
                # exit under set -e, and the status code is the answer we want.
                code=\$(curl -s -o /tmp/probe.out -w '%{http_code}' --max-time 20 \\
                         -H "Authorization: Bearer \${ptoken}" "\${pbase}/models" || true)
                [ -n "\${code}" ] || code=000
                case "\${code}" in
                  200)
                    echo "PROVIDER \${pname}: reachable and authorized (\${pbase}), \$(jq -r '(.data // []) | length' /tmp/probe.out 2>/dev/null || echo '?') model(s) offered upstream."
                    ;;
                  000)
                    echo "PROVIDER \${pname}: UNREACHABLE from platform-kernel — no response from \${pbase} within 20s."
                    echo "  The pod could not open the connection. Check egress for this namespace, then DNS."
                    echo "\${pname}" >>/tmp/degraded; echo 1 >>/tmp/failed
                    ;;
                  401|403)
                    echo "PROVIDER \${pname}: reachable but REFUSED (HTTP \${code}) — the endpoint answered, the token was rejected."
                    echo "  The key in '\${pprop}' is wrong, revoked, or lacks the provider's AI scope."
                    echo "\${pname}" >>/tmp/degraded; echo 1 >>/tmp/failed
                    ;;
                  404)
                    echo "PROVIDER \${pname}: reachable but NO MODEL LIST at \${pbase}/models (HTTP 404) — apiBase is wrong."
                    echo "  It must be the OpenAI-compatible base, ending at the version segment (…/openai/v1)."
                    echo "\${pname}" >>/tmp/degraded; echo 1 >>/tmp/failed
                    ;;
                  *)
                    echo "PROVIDER \${pname}: unexpected HTTP \${code} from \${pbase}/models; not registering its models."
                    echo "\${pname}" >>/tmp/degraded; echo 1 >>/tmp/failed
                    ;;
                esac
              done

              # No -f: /model/info answers 500, not an empty list, on a proxy
              # with nothing registered — a real state on a fresh cluster.
              INFO=\$(curl -s -H "\${AUTH}" "\${BASE}/model/info")
              ACTUAL=\$(printf '%s' "\${INFO}" | jq -c --arg marker "\${MARKER}" \\
                'if (.data | type) == "array" then
                   [.data[] | select((.model_info.gentian_managed // "") == \$marker) | {id: .model_info.id, model_name: .model_name, api_base: (.litellm_params.api_base // ""), model: (.litellm_params.model // ""), fp: (.model_info.gentian_key_fingerprint // "")}]
                 else [] end')

              # ---- prune: managed, and no longer on the claim ---------------
              printf '%s' "\${ACTUAL}" | jq -c --argjson desired "\${DESIRED}" \\
                '.[] | select(.model_name as \$n | ([\$desired[].model_name] | index(\$n)) == null)' | \\
              while IFS= read -r stale; do
                [ -z "\${stale}" ] && continue
                sid=\$(printf '%s' "\${stale}" | jq -r '.id')
                sname=\$(printf '%s' "\${stale}" | jq -r '.model_name')
                echo "Removing LiteLLM model '\${sname}' (id=\${sid}) — no longer on the claim's llm.providers"
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${BASE}/model/delete" -d "{\"id\":\"\${sid}\"}" >/dev/null
              done

              # ---- register or update --------------------------------------
              printf '%s' "\${DESIRED}" | jq -c '.[]' | while IFS= read -r want; do
                wprov=\$(printf '%s' "\${want}" | jq -r '.provider')
                if grep -qxF "\${wprov}" /tmp/degraded 2>/dev/null; then
                  continue
                fi
                wname=\$(printf '%s' "\${want}" | jq -r '.model_name')
                wbase=\$(printf '%s' "\${want}" | jq -r '.api_base')
                wmodel=\$(printf '%s' "\${want}" | jq -r '.model')
                wprop=\$(printf '%s' "\${want}" | jq -r '.key_property')
                wmode=\$(printf '%s' "\${want}" | jq -r '.mode')
                wmax=\$(printf '%s' "\${want}" | jq -r '.max_tokens // empty')
                wkey=\$(tr -d '\\r\\n' </etc/llm-providers/"\${wprop}")
                wfp=\$(printf '%s' "\${wkey}" | sha256sum | cut -c1-8)

                match=\$(printf '%s' "\${ACTUAL}" | jq -c --arg n "\${wname}" '[.[] | select(.model_name==\$n)] | first // empty')
                if [ -n "\${match}" ]; then
                  mid=\$(printf '%s' "\${match}" | jq -r '.id')
                  if [ "\$(printf '%s' "\${match}" | jq -r '.api_base')" = "\${wbase}" ] \\
                     && [ "\$(printf '%s' "\${match}" | jq -r '.model')" = "\${wmodel}" ] \\
                     && [ "\$(printf '%s' "\${match}" | jq -r '.fp')" = "\${wfp}" ]; then
                    echo "LiteLLM model '\${wname}' already up to date (id=\${mid})"
                    continue
                  fi
                  echo "Provider model '\${wname}' changed (endpoint, model or key) — replacing entry id=\${mid}"
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${BASE}/model/delete" -d "{\"id\":\"\${mid}\"}" >/dev/null
                fi

                # model_info carries the marker this function prunes on and the
                # key fingerprint it compares; mode matters because an embedding
                # model routed as chat fails at the first request, not here.
                body=\$(jq -nc --arg name "\${wname}" --arg model "\${wmodel}" --arg base "\${wbase}" \\
                        --arg key "\${wkey}" --arg marker "\${MARKER}" --arg fp "\${wfp}" \\
                        --arg mode "\${wmode}" --arg prov "\${wprov}" --arg max "\${wmax}" \\
                        '{model_name: \$name,
                          litellm_params: {model: \$model, api_base: \$base, api_key: \$key},
                          model_info: ({gentian_managed: \$marker, gentian_provider: \$prov,
                                        gentian_key_fingerprint: \$fp, mode: \$mode}
                                       + (if \$max == "" then {} else {max_tokens: (\$max|tonumber)} end))}')
                if printf '%s' "\${body}" | curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                     "\${BASE}/model/new" -d @- >/dev/null; then
                  echo "Registered LiteLLM model '\${wname}' -> \${wmodel} (\${wbase})"
                else
                  echo "FAILED to register LiteLLM model '\${wname}'" >&2
                  echo 1 >>/tmp/failed
                fi
              done

              if [ -s /tmp/failed ]; then
                echo "\$(wc -l </tmp/failed | tr -d ' ') provider problem(s) above; every healthy provider was still synced." >&2
                exit 3
              fi
          env:
            - name: LITELLM_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: llm-sensitive-values
                  key: litellm_master_key
EOF

    local rc=0
    _wait_for_job "${job_name}" "${ns}" 720s || rc=$?
    case "${rc}" in
        0)
            kubectl logs -n "${ns}" "job/${job_name}" --tail=40 2>/dev/null || true
            success "LiteLLM provider registrations synced."
            ;;
        2)
            # Cold start, same as the vLLM sync: the proxy's migrations outlast
            # this wait on a fresh cluster and the Job's own polling carries it.
            kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
            return 2
            ;;
        *)
            # Exit 3 from the script is "some provider is misconfigured", which
            # is an operator's problem to fix and not a failed install step. The
            # log says which provider and why, so it is printed in full.
            warn "LiteLLM provider sync reported problems — see the provider lines below."
            kubectl logs -n "${ns}" "job/${job_name}" --tail=40 2>/dev/null || true
            return 1
            ;;
    esac
}
