#!/usr/bin/env bash
# step: D-01-operator
# phase: platform
# requires: C-02-appsets
# provides: the gentian-os operator in the control namespace, its CRDs and webhook, and the kernel Gateway it reconciles in the edge namespace
# mutates: the gentian-os Application in the gitops namespace; the operator's Deployment, RBAC and webhook; the Gateway and its routes

# shellcheck source=scripts/steps-v5/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps-v5/B-01-bootstrap-apps.sh"

check() {
    _v5_delivered "$(ns_kernel gitops)" gentian-os &&
        kubectl get deployment gentian-os -n "$(ns_kernel control)" >/dev/null 2>&1 &&
        [[ "$(kubectl get deployment gentian-os -n "$(ns_kernel control)" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" != "" ]]
}

apply() {
    banner "Operator"
    V5_APPSETS=true V5_OPERATOR=true _v5_render | kubectl apply -f - >/dev/null
    local ns t
    ns="$(ns_kernel gitops)"
    info "waiting for gentian-os to be Synced and Healthy"
    t=$((SECONDS + 900))
    until _v5_delivered "${ns}" gentian-os; do
        if (( SECONDS > t )); then
            error "gentian-os is not Synced and Healthy after 15m:"
            kubectl get application gentian-os -n "${ns}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
            return 1
        fi
        sleep 10
    done
    success "the operator is running in $(ns_kernel control)"
}

destroy() {
    kubectl delete application gentian-os -n "$(ns_kernel gitops)" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
