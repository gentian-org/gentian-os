#!/usr/bin/env bash
# step: B-11-os-repository
# phase: secrets
# requires: B-10-seed-secrets
# provides: Repository claim for gentian-os — hands ArgoCD's os-repo credential off from the A-09 bootstrap bridge to OpenBao/ESO
# mutates: Repository claim in crossplane-system; deletes argocd-repo-creds-bootstrap-os once the handoff completes

# os is the one repository role with a Path-A bootstrap bridge
# (_apply_argocd_repo_creds, applied by install_argocd() at A-09):
# B-01-openbao-transit needs ArgoCD to already authenticate to a private or
# mirrored osRepo before OpenBao exists to back Path B (ESO-managed, the
# mechanism every other credentialed repository uses exclusively). This step
# creates the Path-B Repository claim as soon as OpenBao is reachable, then —
# only once its credential is confirmed satisfied FROM OPENBAO, not merely
# applied — deletes the bootstrap bridge Secret. Confirm-then-delete, so
# there is never a window with zero working credential for osRepo.
#
# Positioned after B-10 rather than immediately after B-02 (OpenBao becoming
# reachable, the earliest this could in principle run): B-09's own
# prerequisites already establish that the AppProject, Cluster XR and
# ClusterSecretStore machinery this claim's ExternalSecret needs are only
# reliably up by the end of phase B, and appending here needs no renumbering
# of the existing B-03..B-10 sequence for what is otherwise a small delay in
# closing the bootstrap bridge window.

check() {
    kubectl get repository.gentianos.io os -n crossplane-system >/dev/null 2>&1 &&
        ! kubectl get secret argocd-repo-creds-bootstrap-os -n argocd >/dev/null 2>&1
}

apply() {
    [[ -n "${GENTIAN_OS_REPO:-}" ]] || { error "GENTIAN_OS_REPO is unset."; return 1; }
    local auth; auth="$(_repo_auth_for gentian-os-repository)"
    echo "     + kubectl apply — Repository/os → ${GENTIAN_OS_REPO} (auth=${auth})"
    [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]] && return 0

    # AUTH=none (the public gentian-org default) omits the credential block
    # entirely — see repository-default.yaml's $hasCred branch. Declaring one
    # that nothing will ever write to OpenBao would leave credentialSatisfied
    # false forever.
    local cred_block=""
    if [[ "${auth}" != "none" ]]; then
        cred_block="  credential:
    vaultPath: gentian-os/kernel/repositories/os
    displayName: \"Gentian OS Repository Access\"
    phase: bootstrap
    optional: true
    authType: ${auth}
    validate:
      type: git-https"
    fi

    kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: os
  namespace: crossplane-system
spec:
  type: git
  role: os
  endpoints:
    inCluster: ${GENTIAN_OS_REPO}
  branch: ${GENTIAN_OS_BRANCH:-main}
  writable: false
${cred_block}
EOF

    # Nothing to hand off: no credential declared, or the A-09 bridge was
    # never created (also implied by auth=none — _apply_argocd_repo_creds
    # returns early on the same check).
    [[ "${auth}" == "none" ]] && return 0
    kubectl get secret argocd-repo-creds-bootstrap-os -n argocd >/dev/null 2>&1 || return 0

    info "Waiting for Repository/os credential to be satisfied from OpenBao (up to 2m)..."
    local i=0
    until [[ "$(kubectl get repository.gentianos.io os -n crossplane-system -o jsonpath='{.status.credentialSatisfied}' 2>/dev/null)" == "true" ]]; do
        echo -n "."
        sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || {
            warn "Repository/os not yet credentialSatisfied after 2m — leaving the bootstrap bridge Secret in place; re-run install.sh to retry the handoff."
            echo ""
            return 0
        }
    done
    echo ""

    info "Repository/os credential satisfied from OpenBao — removing the bootstrap ArgoCD repo-creds bridge."
    kubectl delete secret argocd-repo-creds-bootstrap-os -n argocd --ignore-not-found=true
}

destroy() {
    kubectl delete repository.gentianos.io os -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
