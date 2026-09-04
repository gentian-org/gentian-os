#!/usr/bin/env bash
# step: B-12-apps-repository
# phase: secrets
# requires: B-08-cluster-xr
# provides: Repository claim for gentian-apps — private app-catalogue credential, ESO/OpenBao-managed
# mutates: Repository claim in crossplane-system

# apps has no bootstrap-window exception: nothing before ArgoCD's own
# Application/ApplicationSet machinery consumes it, so Path B (this claim,
# resolved by ESO from OpenBao) is the whole of it — mirrors
# B-09-deployments-repository.sh, minus the tier-0 write-access grant.

check() {
    kubectl get repository.gentianos.io apps -n crossplane-system >/dev/null 2>&1
}

apply() {
    [[ -n "${GENTIAN_APPS_REPO:-}" ]] || { error "GENTIAN_APPS_REPO is unset."; return 1; }
    local auth; auth="$(_repo_auth_for gentian-apps-repository)"
    echo "     + kubectl apply — Repository/apps → ${GENTIAN_APPS_REPO} (auth=${auth})"
    [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]] && return 0

    # Two full heredocs rather than one with an interpolated credential-block
    # variable: validate_step_calls (scripts/lib/driver.sh) strips heredoc
    # BODIES before scanning a step for undefined calls, but has no such
    # allowance for YAML sitting in an ordinary multi-line string assignment
    # — every "key:" line in one reads as a 4-space-indented bare command.
    if [[ "${auth}" != "none" ]]; then
        kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: apps
  namespace: crossplane-system
spec:
  type: git
  role: apps
  endpoints:
    inCluster: ${GENTIAN_APPS_REPO}
  branch: ${GENTIAN_APPS_BRANCH:-main}
  writable: false
  credential:
    vaultPath: gentian-os/kernel/repositories/apps
    displayName: "Gentian Apps Repository Access"
    phase: bootstrap
    optional: true
    authType: ${auth}
    validate:
      type: git-https
EOF
    else
        kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: apps
  namespace: crossplane-system
spec:
  type: git
  role: apps
  endpoints:
    inCluster: ${GENTIAN_APPS_REPO}
  branch: ${GENTIAN_APPS_BRANCH:-main}
  writable: false
EOF
    fi
}

destroy() {
    kubectl delete repository.gentianos.io apps -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
