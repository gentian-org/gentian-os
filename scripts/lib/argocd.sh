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

# -----------------------------------------------------------------------------
# The bootstrap repository credentials
#
# Every repository credential reaches Argo CD from OpenBao, through the
# repository's Repository claim: the claim composes an ExternalSecret, and
# External Secrets writes the Secret repo-<name> that Argo CD reads. Two
# repositories are read before that can happen:
#
#   gentian-os    B-01's Applications read it before OpenBao is initialised.
#
#   deployments   Its Repository claim is a file IN the deployments
#                 repository (clusters/<id>/kernel/claims), delivered by the
#                 gentian-claims Application -- which reads that repository.
#                 Where it is private, the claim that would supply the
#                 credential can only arrive once the credential is there: C-02
#                 waited fifteen minutes for gentian-claims and failed. The
#                 catalogue and the tenants ApplicationSets read the same
#                 repository and stood beside it.
#
# So for those two the installer hands Argo CD the credential it collected,
# as a repo-creds Secret (A-06), and takes it away again once the claim's own
# Secret holds a login (C-05). The two never collide: they have different
# names and different types, and Argo CD uses a credential template only for
# a repository whose own Secret carries no login, so from the moment
# repo-<name> exists it is the one that is used.
# -----------------------------------------------------------------------------

# argocd_bridged_repositories -- the requirements A-06 bridges and C-05 hands
# over, in the order they are first read.
argocd_bridged_repositories() {
    echo "gentian-os-repository deployments-repository"
}

# argocd_bootstrap_repo_credential_name <requirement> -- the bridge's Secret.
argocd_bootstrap_repo_credential_name() {
    local name
    name="$(_repo_credential "$1" vault)" || return 1
    echo "argocd-repo-creds-bootstrap-${name}"
}

# argocd_repo_needs_credential <requirement> -- whether Argo CD needs a login
# to read that repository at all: it authenticates, and it has an address.
# False for the public default, which is why a plain install registers
# nothing.
argocd_repo_needs_credential() {
    local repo_var
    repo_var="$(_repo_credential "$1" repo)" || return 1
    [[ "$(_repo_credential_mode "$1")" != "none" && -n "${!repo_var:-}" ]]
}

# argocd_claim_repo_credential_present <requirement> -- whether the Secret the
# Repository claim composes for Argo CD exists AND carries a login.
#
# Asked of the Secret rather than of the claim's status. The claim reports
# credentialSatisfied when the vault path has a value, which is before
# External Secrets has written the Secret Argo CD reads; and a claim that
# declares no credential composes a Secret with an address and no login.
# Neither is a credential Argo CD can use yet. The value is tested for being
# there and is never printed.
argocd_claim_repo_credential_present() {
    local name
    name="$(_repo_credential "$1" vault)" || return 1
    [[ -n "$(kubectl get secret "repo-${name}" -n "$(gentian_argocd_namespace)" \
        -o jsonpath='{.data.password}{.data.bearerToken}' 2>/dev/null)" ]]
}

# argocd_repo_credential_ok <requirement> -- whether Argo CD has what this run
# could give it to read that repository: nothing needed, the claim's own
# Secret, or the bootstrap bridge.
#
# Two cases are decided by whether this run holds a token:
#
#   No credential on the cluster and a token in hand is not ok, and the next
#   apply() registers the bridge. With no token in hand there is nothing an
#   apply() could register, so it is not reported: a check that stays
#   unsatisfied whatever its step does names the wrong step, and --status and
#   --dry-run collect no credentials at all.
#
#   A bridge that holds a different address or token from the ones this run
#   was given is not ok: the token was rotated, or the repository moved, while
#   the bridge was still the credential in use, and the next apply() writes
#   the current ones over it. A run that holds no token has nothing to
#   compare and takes the bridge as it finds it.
#
# The values are compared and never printed.
argocd_repo_credential_ok() {
    local req="$1" bridge ns repo_var token_var have
    argocd_repo_needs_credential "${req}" || return 0
    argocd_claim_repo_credential_present "${req}" && return 0
    token_var="$(_repo_credential "${req}" token)"
    [[ -n "${!token_var:-}" ]] || return 0
    bridge="$(argocd_bootstrap_repo_credential_name "${req}")"
    ns="$(gentian_argocd_namespace)"
    kubectl get secret "${bridge}" -n "${ns}" >/dev/null 2>&1 || return 1
    repo_var="$(_repo_credential "${req}" repo)"
    have="$(kubectl get secret "${bridge}" -n "${ns}" -o jsonpath='{.data.url}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [[ "${have}" == "${!repo_var}" ]] || return 1
    have="$(kubectl get secret "${bridge}" -n "${ns}" \
        -o jsonpath='{.data.password}{.data.bearerToken}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [[ "${have}" == "${!token_var}" ]]
}

# argocd_bootstrap_repo_credential <requirement>
#
# Hands Argo CD the credential the installer collected for one repository, as
# a repo-creds Secret, before OpenBao can serve it.
#
# A repo-creds Secret matches by URL prefix, so the repository's own URL
# matches that repository and nothing else.
#
# Does nothing for a repository that does not authenticate, and warns without
# failing when it does but no token was collected. The token goes from the
# environment into the Secret through jq and a pipe: it is not echoed, and it
# is in no command line.
argocd_bootstrap_repo_credential() {
    local req="$1" name mode repo_var user_var token_var
    name="$(_repo_credential "${req}" vault)" || {
        error "argocd_bootstrap_repo_credential: '${req}' is not a repository credential."
        return 1
    }
    mode="$(_repo_credential_mode "${req}")"
    repo_var="$(_repo_credential "${req}" repo)"
    user_var="$(_repo_credential "${req}" username)"
    token_var="$(_repo_credential "${req}" token)"

    case "${mode}:${!repo_var:+repo}:${!token_var:+token}" in
        none:*|*::*) return 0 ;;
        *:repo:)
            warn "The ${name} repository authenticates (${mode}) but no token was collected; no bootstrap repo-creds Secret for Argo CD."
            return 0 ;;
    esac

    local ns; ns="$(gentian_argocd_namespace)"
    info "Registering bootstrap ArgoCD repo-creds for ${name} (${!repo_var}) in ${ns}..."
    # Built by jq and applied as JSON, so a token is quoted by something that
    # knows how, whatever characters it holds. The two modes differ only in
    # the keys Argo CD reads the login from.
    #
    # The token is exported to jq for the one call and read there from the
    # environment (env.T), not passed with --arg: an argument is in the
    # process list for as long as jq runs.
    #
    # The username falls back to x-access-token, the same fallback the vault
    # is seeded with (seed_repository_credentials), so the bridge and the
    # claim's Secret that replaces it hold the same login.
    GENTIAN_BRIDGE_TOKEN="${!token_var}" jq -n \
        --arg name "argocd-repo-creds-bootstrap-${name}" --arg ns "${ns}" \
        --arg url "${!repo_var}" --arg mode "${mode}" \
        --arg user "${!user_var:-x-access-token}" '
        { kind: "Secret", apiVersion: "v1",
          metadata: { namespace: $ns, name: $name,
                      labels: { "argocd.argoproj.io/secret-type": "repo-creds" } },
          stringData: ( { url: $url, type: "git" }
                        + if $mode == "bearer" then { bearerToken: env.GENTIAN_BRIDGE_TOKEN }
                          else { username: $user, password: env.GENTIAN_BRIDGE_TOKEN } end ) }' |
        kubectl apply -f -
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

    # 2. Patch argocd-cm with OIDC settings and external URL, and switch the
    # local admin account off.
    #
    # Argo CD ships an "admin" with a generated password, which the installer
    # used to print in its summary. That account is everything Keycloak is
    # not: no MFA, no activation link, no record of who used it, and a
    # password sitting in terminal scrollback. Once Argo CD signs in against
    # the realm, a platform administrator has role:admin through the group
    # below, and anyone with kubectl in the gitops namespace can turn the
    # account back on -- so nothing is lost by turning it off.
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
    \"admin.enabled\": \"false\",
    \"oidc.config\": $(jq -R -s '.' <<<"${oidc_config}")
  }
}"
    # The generated password goes with the account. Argo CD keeps only its
    # hash in argocd-secret; this Secret holds it in the clear and exists for
    # the first sign-in, which is Keycloak's now.
    kubectl delete secret argocd-initial-admin-secret -n "${ns}" --ignore-not-found >/dev/null

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

    # An operation still Running, but pinned to a revision Argo CD refuses:
    # it retries that revision with backoff and never looks at the commit that
    # replaced it -- an unsigned head followed by a signed one (AD-2) left the
    # claims Application retrying the unsigned commit indefinitely. Stop it
    # and refresh; automated sync starts again on the current head.
    if [[ "${phase}" == "Running" ]] && \
       jq -e '(.status.operationState.message // "") | test("GIT/GPG|Failed verifying revision")' <<<"${json}" >/dev/null 2>&1; then
        info "${app}: its sync is pinned to a revision Argo CD refuses; stopping it and refreshing."
        kubectl patch application "${app}" -n "${ns}" --type merge -p '{"operation":null}' >/dev/null 2>&1 || true
        kubectl annotate application "${app}" -n "${ns}" argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
        return 0
    fi
    # A sync that SUCCEEDED and an Application that is still OutOfSync: what
    # Argo CD applied changed nothing, and it goes on reporting a difference.
    # Seen when a CRD gained a field in the same sync as an object using it:
    # the diff computed at that moment had the object without the field,
    # Argo CD caches diffs by the object's version, and every later apply was
    # a no-op -- so the version never moved, the cached diff was served for
    # ever, and restarting the controller did not help because the cache is
    # not in it. A hard refresh recomputes without the cache. It changes
    # nothing in the cluster, and is asked at most once a minute.
    if [[ "${phase}" == "Succeeded" && "${sync}" == "OutOfSync" ]] && \
       [[ "$(jq -r '.operation // "" | type' <<<"${json}")" == "string" ]]; then
        local now=${SECONDS} last_var="_ARGO_HARD_REFRESH_${app//[^A-Za-z0-9]/_}"
        if (( now - ${!last_var:--60} >= 60 )); then
            printf -v "${last_var}" '%s' "${now}"
            info "${app}: synced and still OutOfSync; asking Argo CD to recompute the comparison."
            kubectl annotate application "${app}" -n "${ns}" argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
        fi
        return 0
    fi
    [[ "${phase}" == "Failed" || "${phase}" == "Error" ]] || return 0
    [[ "${sync}" != "Synced" ]] || return 0
    # An operation still in flight has its own phase; only a finished one is
    # ours to replace.
    [[ "$(jq -r '.operation // "" | type' <<<"${json}")" == "string" ]] || return 0

    # An ExternalSecret that failed while its store was not ready yet is not
    # retried by External Secrets until its refresh interval -- an hour -- so
    # a new sync would only meet the same failure. Nudge the Application's
    # unready ExternalSecrets first; a cold install met this on every run.
    local es_ns es_name
    while read -r es_ns es_name; do
        [[ -n "${es_name}" ]] || continue
        [[ "$(kubectl get externalsecret "${es_name}" -n "${es_ns}" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] && continue
        info "  ${app}: refreshing ExternalSecret ${es_ns}/${es_name}, which failed earlier."
        kubectl annotate externalsecret "${es_name}" -n "${es_ns}" \
            force-sync="$(date +%s)" --overwrite >/dev/null 2>&1 || true
    done < <(jq -r '.status.resources[]? | select(.kind=="ExternalSecret") | "\(.namespace) \(.name)"' <<<"${json}")

    info "${app}: its last sync failed and automated retries are exhausted; asking for another."
    kubectl patch application "${app}" -n "${ns}" --type merge \
        -p '{"operation":{"initiatedBy":{"username":"gentian-installer"},"sync":{"syncStrategy":{"hook":{}}}}}' \
        >/dev/null 2>&1 || warn "  ${app}: could not request a sync."
}

# =============================================================================
# Summary — portal admin credentials for install output
# =============================================================================
