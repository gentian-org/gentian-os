#!/usr/bin/env bash
# step: D-01-operator
# phase: platform
# requires: C-02-appsets
# provides: the gentian-os operator AND the director in the control namespace, the CRDs and webhook, and the kernel Gateway the operator reconciles in the edge namespace
# mutates: the gentian-os Application in the gitops namespace; the operator's and the director's Deployments, RBAC and webhook; the Gateway and its routes

# One Application, one chart, two workloads.
#
# charts/gentian-os carries the operator and the director, and the bootstrap
# chart sets director.enabled unconditionally -- so the director has no step of
# its own and arrives here. The step said only "operator", which reads as the
# director never being installed, and check() looked only at the operator's
# Deployment: a director that never became ready left this step reporting
# satisfied. Both are named and both are checked now.
#
# They are one release on purpose. The director is not a separate product: it is
# the half of this codebase that decides and writes git, against the half that
# reconciles the cluster, built from one image and released together. Two
# releases would let the two halves differ in version, which is the one thing
# the split must never allow -- they share the CRD types.

# shellcheck source=scripts/steps/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps/B-01-bootstrap-apps.sh"

# Both Deployments of the release. The chart names them <fullname> and
# <fullname>-director, so one release name gives both -- stated once here
# rather than twice, because a rename that moved one and not the other would
# leave this step checking a Deployment that does not exist.
_d01_deployments() { local release="gentian-os"; echo "${release} ${release}-director"; }

check() {
    local ns d
    ns="$(ns_kernel control)"
    _v5_delivered "$(ns_kernel gitops)" gentian-os || return 1
    for d in $(_d01_deployments); do
        kubectl get deployment "${d}" -n "${ns}" >/dev/null 2>&1 || return 1
        [[ "$(kubectl get deployment "${d}" -n "${ns}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" != "" ]] || return 1
    done
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

    # The workload, whether or not the Application still owns it. Deleting the
    # Application with --wait=false leaves Argo CD's prune to finish in the
    # background, and a teardown that returns before it does leaves the
    # operator Running -- which then makes check() report the step satisfied on
    # a torn-down cluster.
    kubectl delete deployment,service,replicaset -n "$(ns_kernel control)" \
        -l app.kubernetes.io/name=gentian-os \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    # The API scaffold: the gentianos.io CRDs and the tenant validating
    # webhook. v4 removed these here and v5 did not, so a v5 teardown left the
    # webhook intercepting PATCH on Tenant CRs with no service behind it --
    # every patch failing with "service not found", which is what E-01 exists
    # to get ahead of and cannot if the webhook outlives this step.
    _delete_gentianos_api_scaffold || true
}
