#!/usr/bin/env bash
# =============================================================================
# scripts/lib/argocd.sh — Argo CD install, bootstrap Applications, and verification.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# gentian_argocd_namespace — where Argo CD actually runs.
#
# v4 installs it into a namespace called argocd; v5's layout puts it in the
# gitops namespace that kernel/namespaces.yaml names. The literal was fine
# while there was one layout and is not any more: the bootstrap repo-creds
# bridge below landed in `argocd` on a v5 cluster, where nothing reads it, and
# the Applications that need it could not resolve their source.
# =============================================================================
gentian_argocd_namespace() {
    if [[ -n "${ARGOCD_NAMESPACE:-}" ]]; then
        echo "${ARGOCD_NAMESPACE}"
        return 0
    fi
    ns_kernel gitops
}

# =============================================================================
# _apply_argocd_repo_creds <role> <repo_var> <auth_var> <user_var> <token_var>
#
# Registers a prefix-matched ArgoCD repo-creds Secret directly from the
# shell-collected credential — the one Path-A exception in an otherwise
# ESO/OpenBao-managed credential design (see repository-default.yaml). Exists
# only for repositories a bootstrap Application needs before OpenBao is
# reachable; today that is gentian-os alone, called from install_argocd() above.
#
# url is a PREFIX match in an ArgoCD repo-creds Secret (secret-type:
# repo-creds, as opposed to the exact-match secret-type: repository the
# Composition emits later) — an exact repo URL here matches only that repo, so
# it does not need trimming or wildcarding.
#
# Skipped, not an error, when auth is "none" (public repo, nothing to
# authenticate) or when the credential was never collected — collect_bootstrap_credentials
# only gathers it when _requirement_applies() gated it in, which mirrors the
# same GENTIAN_OS_AUTH check here.
# =============================================================================
_apply_argocd_repo_creds() {
    local role="$1" repo_var="$2" auth_var="$3" user_var="$4" token_var="$5"
    local repo="${!repo_var:-}"
    local auth="${!auth_var:-none}"
    [[ -n "${repo}" && "${auth}" != "none" ]] || return 0

    local user="${!user_var:-}" token="${!token_var:-}"
    if [[ -z "${token}" ]]; then
        warn "No credential collected for ${role} repository (${auth_var}=${auth}); skipping bootstrap ArgoCD repo-creds Secret."
        return 0
    fi

    local ns; ns="$(gentian_argocd_namespace)"
    info "Registering bootstrap ArgoCD repo-creds for ${role} (${repo}) in ${ns}..."
    if [[ "${auth}" == "bearer" ]]; then
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: argocd-repo-creds-bootstrap-${role}
  namespace: ${ns}
  labels:
    argocd.argoproj.io/secret-type: repo-creds
stringData:
  type: git
  url: ${repo}
  bearerToken: "${token}"
EOF
    else
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: argocd-repo-creds-bootstrap-${role}
  namespace: ${ns}
  labels:
    argocd.argoproj.io/secret-type: repo-creds
stringData:
  type: git
  url: ${repo}
  username: "${user}"
  password: "${token}"
EOF
    fi
}

# =============================================================================
# 4. Install ArgoCD + AppProject
# =============================================================================
resolve_argocd_url() {
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        echo "https://argocd.${KERNEL_DOMAIN}"
        return 0
    fi
    local ingress_host svc_type node_port lb_host lb_ip

    _pick_node_ip() {
        local detected
        if [[ -n "${NODE_IP:-}" ]]; then
            if _is_testnet_ip "${NODE_IP}"; then
                warn "NODE_IP=${NODE_IP} looks like documentation/testnet IP; auto-detecting real node IP instead." >&2
            else
                echo "${NODE_IP}"
                return 0
            fi
        fi

        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        echo "<node-ip>"
        return 0
    }

    ingress_host=$(kubectl get ingress -n argocd \
        -o jsonpath='{.items[0].spec.rules[0].host}' 2>/dev/null || true)
    if [[ -n "$ingress_host" ]]; then
        echo "https://${ingress_host}"
        return 0
    fi

    svc_type=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{.spec.type}' 2>/dev/null || true)
    node_port=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{range .spec.ports[?(@.name=="https")]}{.nodePort}{end}' 2>/dev/null || true)
    if [[ -z "$node_port" ]]; then
        node_port=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{range .spec.ports[0]}{.nodePort}{end}' 2>/dev/null || true)
    fi

    if [[ "$svc_type" == "LoadBalancer" ]]; then
        lb_host=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
        lb_ip=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        if [[ -n "$lb_host" ]]; then
            echo "https://${lb_host}"
            return 0
        fi
        if [[ -n "$lb_ip" ]]; then
            echo "https://${lb_ip}"
            return 0
        fi
    fi

    if [[ "$svc_type" == "NodePort" && -n "$node_port" ]]; then
        echo "https://$(_pick_node_ip):${node_port}"
        return 0
    fi

    # ClusterIP or unresolved external endpoint.
    echo "kubectl port-forward -n argocd svc/argocd-server 8080:443"
}

# Configure ArgoCD OIDC settings and group mapping.
configure_argocd_oidc() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    # Where Argo CD runs, and which realm signs the tokens it must accept.
    # Both were literals: the namespace made this unusable under any other
    # layout, and the realm silently disagreed with every other caller here,
    # which honours KERNEL_REALM -- a cluster whose realm is not called kernel
    # got an issuer nothing had ever issued a token for.
    local ns="${GITOPS_NAMESPACE:-$(ns_kernel gitops)}"
    local realm="${KERNEL_REALM:-kernel}"
    info "Configuring ArgoCD OIDC (Keycloak integration)..."

    # 1. Trust the wildcard-tls CA (self-signed or staging issuer support)
    local ca_cert
    ca_cert=$(kubectl get secret wildcard-tls -n "${ns}" -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 -d || true)
    if [[ -z "$ca_cert" ]]; then
        ca_cert=$(kubectl get secret wildcard-tls -n "${ns}" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d || true)
    fi
    if [[ -n "$ca_cert" ]]; then
        info "Registering gateway CA certificate in argocd-tls-certs-cm..."
        kubectl patch configmap argocd-tls-certs-cm -n "${ns}" --type merge \
            --patch "{\"data\":{\"id.${kernel_domain}\":$(jq -R -s '.' <<<"${ca_cert}")}}"
    fi

    # 2. Patch argocd-cm with OIDC settings and external URL
    local oidc_config
    oidc_config=$(cat <<EOF
name: Keycloak
issuer: https://id.${kernel_domain}/auth/realms/${realm}
clientID: gentian-argocd
clientSecret: \$oidc.keycloak.clientSecret
requestedScopes: ["openid", "profile", "email", "groups"]
EOF
)
    kubectl patch configmap argocd-cm -n "${ns}" --type merge -p "
{
  \"data\": {
    \"url\": \"https://argocd.${kernel_domain}\",
    \"oidc.config\": $(jq -R -s '.' <<<"${oidc_config}")
  }
}"

    # 3. Patch argocd-rbac-cm to map group to admin role
    #
    # Both spellings, because the claim carries the FULL path.
    #
    # The kernel realm's groups mapper is configured full.path=true, which
    # OpenBao needs: its roles bind /group-name with a leading slash and the
    # bare name matches nothing. So the token Argo CD receives says
    # "/gentian:platform:admin", and a policy naming the bare form matches no
    # subject at all -- which does not look like a permissions problem from
    # the outside. It looks like an empty Argo CD: "No applications available
    # to you just yet", for a platform administrator who holds everything.
    #
    # Naming both costs nothing and survives the mapper being changed back.
    local platform_admin_group="${PLATFORM_ADMIN_GROUP:-gentian:platform:admin}"
    local policy_csv
    policy_csv="g, ${platform_admin_group}, role:admin
g, /${platform_admin_group}, role:admin"
    kubectl patch configmap argocd-rbac-cm -n "${ns}" --type merge -p "
{
  \"data\": {
    \"policy.csv\": $(jq -R -s '.' <<<"${policy_csv}"),
    \"scopes\": \"[groups]\"
  }
}"

    # 4. Restart ArgoCD server to pick up new configurations
    #
    # A restart triggered within the same second as an earlier one is refused:
    # the annotation kubectl writes carries a timestamp, and two in one second
    # are the same value, so there is nothing to patch. It means a restart is
    # already on its way, which is what this wanted -- but unhandled it ended
    # the whole install one line before the step's last success message.
    kubectl rollout restart deployment argocd-server -n "${ns}" \
        || warn "  argocd-server restart already in flight; the new configuration comes up with it."
    kubectl rollout status deployment argocd-server -n "${ns}" --timeout=90s 2>/dev/null || true
    success "ArgoCD OIDC configuration completed."
}


verify_argocd_apps() {
    banner "Verify — ArgoCD Applications"

    # Restart the application-controller once to clear any stale resource
    # health cached during the OpenBao seal-migration window (when ESO
    # transiently couldn't read secrets). Without this, Applications
    # whose underlying resources are now healthy can stay reported as
    # Degraded indefinitely because ArgoCD doesn't re-evaluate cached
    # resource health unless the resource generation changes.
    info "Restarting argocd-application-controller to clear stale health cache..."
    kubectl rollout restart statefulset -n argocd argocd-application-controller \
        >/dev/null 2>&1 || true
    kubectl rollout status  statefulset -n argocd argocd-application-controller \
        --timeout=120s >/dev/null 2>&1 || warn "application-controller rollout did not become ready in 120s; continuing."

    local timeout=${VERIFY_TIMEOUT:-600}
    local interval=15
    local elapsed=0
    local total synced healthy bad_lines
    info "Waiting up to ${timeout}s for all Applications to become Synced+Healthy..."

    while true; do
        # If no Applications exist yet, keep waiting (root ApplicationSet may
        # still be generating children).
        total=$(kubectl get applications -n argocd --no-headers 2>/dev/null | wc -l)
        if [[ "$total" -eq 0 ]]; then
            if [[ $elapsed -ge $timeout ]]; then
                warn "No ArgoCD Applications appeared within ${timeout}s."
                export VERIFY_STATUS="empty"
                return 1
            fi
            printf "  …no Applications yet (%ds/%ds)\n" "$elapsed" "$timeout"
            sleep "$interval"; elapsed=$((elapsed + interval))
            continue
        fi

        synced=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.sync.status=="Synced")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)
        # Bootstrap operator / ApplicationSet parent: kube-defaulted fields or
        # Argo tracking annotations can leave apps OutOfSync while Healthy.
        while IFS= read -r _app; do
            [[ -n "$_app" ]] && synced=$((synced + 1))
        done < <(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.sync.status=="OutOfSync" && @.status.health.status=="Healthy")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | grep -E '^(gentian-os|gentian-appsets)$' || true)
        healthy=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.health.status=="Healthy")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)

        printf "  apps=%d synced=%d healthy=%d (%ds/%ds)\n" \
            "$total" "$synced" "$healthy" "$elapsed" "$timeout"

        if [[ "$synced" -eq "$total" && "$healthy" -eq "$total" ]]; then
            success "All ${total} ArgoCD Applications are Synced and Healthy."
            export VERIFY_STATUS="ok"
            export VERIFY_TOTAL="$total"
            return 0
        fi

        if [[ $elapsed -ge $timeout ]]; then
            bad_lines=$(kubectl get applications -n argocd \
                -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status' \
                --no-headers 2>/dev/null | awk '$2!="Synced" || $3!="Healthy"')
            warn "Timed out after ${timeout}s with ${total} Applications, ${synced} Synced, ${healthy} Healthy."
            echo "  Degraded / out-of-sync Applications:"
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "    $line"
            done <<< "$bad_lines"
            export VERIFY_STATUS="degraded"
            export VERIFY_TOTAL="$total"
            export VERIFY_BAD="$bad_lines"
            return 1
        fi

        sleep "$interval"; elapsed=$((elapsed + interval))
    done
}

# =============================================================================
# unstick_argo_hook_job <argocd_namespace> <application>
#
# Self-heal for a sync operation parked forever on a Helm hook.
#
# Argo CD maps Helm's post-install/post-upgrade hooks onto its own PostSync
# phase and then waits for the hook Job to finish before the operation ends.
# A Job whose pod can never start — an image that no longer pulls is the case
# this exists for, and a Job's pod template is immutable, so a corrected chart
# cannot repair it — parks the operation indefinitely. While an operation is
# Running, Argo CD starts no new sync, so the Application stays OutOfSync no
# matter what the repository now says and every step waiting on it times out.
#
# Removing the Job lets the hook resolve and the operation end; the next sync
# renders from the current desired state. Only ever acts on a Job that has no
# pod able to make progress, so a hook that is genuinely working is untouched.
# =============================================================================
unstick_argo_hook_job() {
    local ns="$1" app="$2" json hook job_ns job started

    json="$(kubectl get application "${app}" -n "${ns}" -o json 2>/dev/null)" || return 0
    [[ "$(jq -r '.status.operationState.phase // ""' <<<"${json}")" == "Running" ]] || return 0

    hook="$(jq -r '[.status.operationState.syncResult.resources[]?
                    | select(.kind == "Job" and .hookType != null and .hookPhase == "Running")][0]
                   | select(. != null) | "\(.namespace) \(.name)"' <<<"${json}")"
    [[ -n "${hook}" ]] || return 0
    read -r job_ns job <<<"${hook}"

    # A pull that is merely slow deserves the benefit of the doubt.
    started="$(jq -r '.status.operationState.startedAt // ""' <<<"${json}")"
    if [[ -n "${started}" ]]; then
        local age
        age=$(( $(date -u +%s) - $(date -u -d "${started}" +%s 2>/dev/null || echo 0) ))
        (( age > 300 )) || return 0
    fi

    # Progress means a pod that runs or has already finished. Anything else
    # waiting on its image or its configuration will not resolve by itself.
    local pods stuck
    pods="$(kubectl get pods -n "${job_ns}" -l "batch.kubernetes.io/job-name=${job}" -o json 2>/dev/null)" || return 0
    if jq -e '[.items[]? | select(.status.phase == "Running" or .status.phase == "Succeeded")] | length > 0' \
        <<<"${pods}" >/dev/null 2>&1; then
        return 0
    fi
    stuck="$(jq -r '[.items[]?.status.containerStatuses[]?.state.waiting.reason
                     | select(. == "ImagePullBackOff" or . == "ErrImagePull"
                              or . == "InvalidImageName" or . == "CreateContainerConfigError")]
                    | first // ""' <<<"${pods}")"
    [[ -n "${stuck}" ]] || return 0

    warn "${app}: its sync is parked on hook Job ${job_ns}/${job}, whose pod cannot start (${stuck})."
    warn "  Argo CD runs no further sync while an operation waits, so the Application"
    warn "  cannot pick up the current chart. Removing the Job so the operation ends."
    kubectl delete job "${job}" -n "${job_ns}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    # Terminating what is left releases the operation even when Argo CD has
    # already stopped watching the Job. The Application CRD carries no status
    # subresource, so this is a plain merge patch.
    kubectl patch application "${app}" -n "${ns}" --type merge \
        -p '{"status":{"operationState":{"phase":"Terminating"}}}' >/dev/null 2>&1 || true
    success "${app}: parked hook released."
}

# =============================================================================
# request_argo_sync_if_stalled <argocd_namespace> <application>
#
# Self-heal for an Application whose automated sync has given up.
#
# Automated sync retries a failed attempt a fixed number of times and then
# stops. That is correct -- retrying a bad manifest forever helps nobody -- but
# it also means an Application whose cause has since been fixed stays OutOfSync
# with a failure from before the fix, and a refresh does not restart it. Every
# step waiting on that Application then waits out its whole timeout and reports
# a fault that was repaired minutes earlier.
#
# Asking for a sync is the documented way to say "try again now". Only ever
# asked when the last attempt FAILED and nothing is running, so a sync in
# progress is never disturbed and a healthy Application is never touched.
# =============================================================================
request_argo_sync_if_stalled() {
    local ns="$1" app="$2" json phase sync

    json="$(kubectl get application "${app}" -n "${ns}" -o json 2>/dev/null)" || return 0
    phase="$(jq -r '.status.operationState.phase // ""' <<<"${json}")"
    sync="$(jq -r '.status.sync.status // ""' <<<"${json}")"
    [[ "${phase}" == "Failed" || "${phase}" == "Error" ]] || return 0
    [[ "${sync}" != "Synced" ]] || return 0
    # An operation still in flight has its own phase; only a finished one is
    # ours to replace.
    [[ "$(jq -r '.operation // "" | type' <<<"${json}")" == "string" ]] || return 0

    info "${app}: its last sync failed and automated retries are exhausted; asking for another."
    kubectl patch application "${app}" -n "${ns}" --type merge \
        -p '{"operation":{"initiatedBy":{"username":"gentian-installer"},"sync":{"syncStrategy":{"hook":{}}}}}' \
        >/dev/null 2>&1 || warn "  ${app}: could not request a sync."
}

# =============================================================================
# Summary — portal admin credentials for install output
# =============================================================================
