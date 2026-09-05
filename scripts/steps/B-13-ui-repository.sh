#!/usr/bin/env bash
# step: B-13-ui-repository
# phase: secrets
# requires: B-08-cluster-xr
# provides: Repository claim for gentian-ui — portal chart/branding credential, ESO/OpenBao-managed
# mutates: Repository claim in crossplane-system

# ui has no bootstrap-window exception, same reasoning as
# B-12-apps-repository.sh: nothing before ArgoCD's own multi-source
# Application (gentian-portal.yaml, applied at D-06) consumes it, so Path B
# is the whole of it.

check() {
    kubectl get repository.gentianos.io ui -n crossplane-system >/dev/null 2>&1
}

apply() {
    [[ -n "${GENTIAN_UI_REPO:-}" ]] || { error "GENTIAN_UI_REPO is unset."; return 1; }
    local auth; auth="$(_repo_auth_for gentian-ui-repository)"
    echo "     + kubectl apply — Repository/ui → ${GENTIAN_UI_REPO} (auth=${auth})"
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
  name: ui
  namespace: crossplane-system
spec:
  type: git
  role: ui
  endpoints:
    inCluster: ${GENTIAN_UI_REPO}
  branch: ${GENTIAN_UI_BRANCH:-develop}
  writable: false
  credential:
    vaultPath: gentian-os/kernel/repositories/ui
    displayName: "Gentian UI Repository Access"
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
  name: ui
  namespace: crossplane-system
spec:
  type: git
  role: ui
  endpoints:
    inCluster: ${GENTIAN_UI_REPO}
  branch: ${GENTIAN_UI_BRANCH:-develop}
  writable: false
EOF
    fi
}

destroy() {
    kubectl delete repository.gentianos.io ui -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
