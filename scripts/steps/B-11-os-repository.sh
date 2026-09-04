#!/usr/bin/env bash
# step: B-11-os-repository
# phase: secrets
# requires: B-10-seed-secrets
# provides: Repository claim for gentian-os — Path-B credential source, not yet handed off
# mutates: Repository claim in crossplane-system

# os is the one repository role with a Path-A bootstrap bridge
# (_apply_argocd_repo_creds, applied by install_argocd() at A-09):
# B-01-openbao-transit needs ArgoCD to already authenticate to a private or
# mirrored osRepo before OpenBao exists to back Path B (ESO-managed, the
# mechanism every other credentialed repository uses exclusively). This step
# only applies the Path-B Repository claim — same shape as
# B-09-deployments-repository, apply-and-move-on, no readiness wait.
#
# The wait for credentialSatisfied and the bootstrap bridge Secret's deletion
# live in C-07-os-repository-handoff, not here: this claim's
# CredentialRequirement composes against the credentialrequirements.gentianos.io
# CRD, which C-06-credential-catalogue installs by hand in phase C — a whole
# phase after this step. Polling for satisfaction here would always time out
# against a CRD that cannot exist yet, the same way it did before this file was
# split (see C-07's header for the mechanics of the fix).

check() {
    kubectl get repository.gentianos.io os -n crossplane-system >/dev/null 2>&1
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
}

destroy() {
    kubectl delete repository.gentianos.io os -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
