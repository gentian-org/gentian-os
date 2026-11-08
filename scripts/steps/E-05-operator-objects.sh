#!/usr/bin/env bash
# step: E-05-operator-objects
# phase: handover
# requires: E-04-user-tenant
# provides: the catalog and every declared tenant synced by Argo CD -- each of its apps syncing gentianos.io objects Synced -- and each declared tenant a Tenant on the cluster
# mutates: Argo CD sync requests for those Applications whose last sync failed

# The last step: tenants, and every other object of the operator's kinds, are
# verified at the end of an install, not trusted to have arrived.
#
# The catalog (gentian-catalogue) and the tenants (gentian-tenants) are
# ApplicationSets of C-02, and the CRDs of what they sync -- ComponentProfile,
# Tenant -- arrive with the operator chart at D-01. On a fresh install their
# first syncs fail with `<Kind>.gentianos.io "" not found`, spend their ten
# retries and are not tried again: automated sync does not repeat a revision
# it failed. A tenant declared in git before the install was never created,
# and no catalog profile reached the cluster, while the install reported
# success. D-01 asks for them again once the CRDs exist; this step asks again
# for whatever still failed, and holds the install to the result.
#
# A tenant other than the platform's is admitted only after the platform
# admin's first sign-in (the handover gate, internal/webhook). Before it, such
# a tenant is reported as held back, and this step stays outstanding: the run
# ends "not fully installed" rather than successful, and a run after the
# sign-in finishes it. After it, a tenant that does not arrive within the wait
# fails the step with Argo CD's message.
#
# Read and asked of Argo CD only: nothing here creates, edits or deletes an
# Application, a Tenant or a file in git. No destroy(): E-01 removes tenants.

# _e05_handover_proven — has the platform admin signed in? As E-04 reads it.
_e05_handover_proven() {
    [[ "$(kubectl get configmap gentian-handover -n "$(ns_kernel control)" \
        -o jsonpath='{.data.writePathProven}' 2>/dev/null || true)" == "true" ]]
}

# _e05_appset_error <ns> <appset> — the ApplicationSet's error, if it reports
# one; "missing" when it does not exist. Empty when it is generating.
_e05_appset_error() {
    local json
    json="$(kubectl get applicationset "$2" -n "$1" -o json 2>/dev/null)" || { echo "missing"; return 0; }
    jq -r '.status.conditions[]? | select(.type == "ErrorOccurred" and .status == "True") | .message' <<<"${json}" 2>/dev/null | head -1 || true
}

# _e05_tenant_of <app> — the Tenant an Application of gentian-tenants
# declares (tenant-<name>), or nothing for any other Application.
_e05_tenant_of() {
    case "$1" in tenant-*) printf '%s' "${1#tenant-}" ;; esac
}

# _e05_leftovers <json> — what an Application reports as on the cluster and
# no longer declared, one "<kind> <namespace>/<name>" a line.
_e05_leftovers() {
    jq -r '.status.resources[]? | select(.requiresPruning == true)
        | "\(.kind) \(if .namespace then .namespace + "/" else "" end)\(.name)"' <<<"$1" 2>/dev/null || true
}

# _e05_done <ns> <app> — Synced, and for a tenant's Application, the Tenant there.
#
# Synced also when the last sync succeeded and all that still differs is what
# Argo CD marks as requiring pruning: the catalog never prunes, so an object
# a profile stopped bringing stays, and the Application stays OutOfSync for as
# long as it does. Waiting would not end that; apply names those objects.
_e05_done() {
    local json tenant
    json="$(kubectl get application "$2" -n "$1" -o json 2>/dev/null)" || return 1
    jq -e '.status.sync.status == "Synced"
        or (.status.sync.status == "OutOfSync" and .status.operationState.phase == "Succeeded"
            and ([.status.resources[]? | select(.status != "Synced")] | length > 0
                 and all(.requiresPruning == true)))' <<<"${json}" >/dev/null 2>&1 || return 1
    tenant="$(_e05_tenant_of "$2")"
    [[ -z "${tenant}" ]] || kubectl get tenants.gentianos.io "${tenant}" >/dev/null 2>&1
}

# _e05_report_leftovers <ns> — the objects the Applications no longer declare
# and keep, said once, with where they are removed.
_e05_report_leftovers() {
    local app json left
    for app in $(argo_operator_kind_apps "$1"); do
        json="$(kubectl get application "${app}" -n "$1" -o json 2>/dev/null)" || continue
        left="$(_e05_leftovers "${json}")"
        [[ -n "${left}" ]] || continue
        warn "${app}: no longer declared and still on the cluster (Argo CD does not prune them):"
        while read -r line; do warn "  ${line}"; done <<<"${left}"
        case "${app}" in
            gentian-catalogue-*)
                warn "  To review them, and remove one at a time:"
                warn "    kubectl gentian catalogues residue" ;;
        esac
    done
}

# _e05_held <app> — a tenant the handover gate still holds back.
_e05_held() {
    local tenant
    tenant="$(_e05_tenant_of "$1")"
    [[ -n "${tenant}" && "${tenant}" != "platform" ]] && ! _e05_handover_proven
}

# _e05_explain <ns> <app> — what Argo CD says about one that is not done.
_e05_explain() {
    local json tenant
    json="$(kubectl get application "$2" -n "$1" -o json 2>/dev/null)" || { error "  Application $2 does not exist."; return 0; }
    error "  $2: $(jq -r '"sync \(.status.sync.status // "unknown"), last sync \(.status.operationState.phase // "none"): \(.status.operationState.message // "no message")"' <<<"${json}")"
    tenant="$(_e05_tenant_of "$2")"
    if [[ -n "${tenant}" ]] && ! kubectl get tenants.gentianos.io "${tenant}" >/dev/null 2>&1; then
        error "    Tenant/${tenant} does not exist."
    fi
}

check() {
    local ns set app
    ns="$(ns_kernel gitops)"
    for set in gentian-catalogue gentian-tenants; do
        [[ -z "$(_e05_appset_error "${ns}" "${set}")" ]] || return "${CHECK_MISSING}"
    done
    for app in $(argo_operator_kind_apps "${ns}"); do
        _e05_done "${ns}" "${app}" || return "${CHECK_MISSING}"
    done
    return 0
}

apply() {
    local ns set app msg deadline timeout="${GENTIAN_OPERATOR_OBJECTS_WAIT_SECS:-600}"
    local pending held
    ns="$(ns_kernel gitops)"
    info "Waiting for the catalogue and every declared tenant to be synced (up to $(( timeout / 60 )) min)..."
    deadline=$((SECONDS + timeout))
    while :; do
        pending=""; held=""
        for set in gentian-catalogue gentian-tenants; do
            msg="$(_e05_appset_error "${ns}" "${set}")"
            [[ -z "${msg}" ]] || pending="${pending} applicationset:${set}"
        done
        for app in $(argo_operator_kind_apps "${ns}"); do
            _e05_done "${ns}" "${app}" && continue
            if _e05_held "${app}"; then
                held="${held} ${app}"
                continue
            fi
            request_argo_sync_if_stalled "${ns}" "${app}" || true
            pending="${pending} ${app}"
        done
        [[ -n "${pending}" ]] || break
        if (( SECONDS > deadline )); then
            error "Not synced after $(( timeout / 60 )) minutes -- what the deployments repository declares is not on the cluster:"
            for app in ${pending}; do
                case "${app}" in
                    applicationset:*)
                        set="${app#applicationset:}"
                        error "  ApplicationSet ${set}: $(_e05_appset_error "${ns}" "${set}")" ;;
                    *) _e05_explain "${ns}" "${app}" ;;
                esac
            done
            error "  To look:   kubectl get applications -n ${ns}"
            error "  Then:      ./install.sh --only E-05"
            return 1
        fi
        sleep 10
    done

    if [[ -n "${held}" ]]; then
        warn "Tenants held back until the platform admin's first sign-in at https://platform.${KERNEL_DOMAIN:-<kernel-domain>}/:"
        for app in ${held}; do warn "  Tenant/$(_e05_tenant_of "${app}") (Application ${app})"; done
        warn "  The cluster admits no tenant but the platform's before it. Afterwards, finish with:"
        warn "    ./install.sh --only E-05"
        return 0
    fi
    _e05_report_leftovers "${ns}"
    success "The catalogue and every declared tenant are synced."
}
