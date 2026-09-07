#!/usr/bin/env bash
# step: B-09-deployments-repository
# phase: secrets
# requires: B-08-cluster-xr
# provides: Repository claim for gentian-deployments — ArgoCD credential and operator push access
# mutates: Repository claim in crossplane-system

# The tier-0 exception, and the only one.
#
# §2 says no rendered artefact is applied by a script, because a reconciler
# cannot detect drift in it. This claim is the documented exception: a private
# deployments repository cannot describe its own access, so the claim granting
# ArgoCD access to it cannot itself arrive through ArgoCD. Everything that comes
# after this point is declarative precisely because this one object is not.
#
# It is written inline rather than rendered from a chart so the YAML an operator
# is asked to trust is visible in the step they are reading.
#
# The value lives in OpenBao already — the installer's credential prompt wrote
# it. This names the path and nothing else.

check() {
    kubectl get repository.gentianos.io deployments -n crossplane-system >/dev/null 2>&1
}

apply() {
    [[ -n "${GENTIAN_DEPLOYMENTS_REPO:-}" ]] || { error "GENTIAN_DEPLOYMENTS_REPO is unset."; return 1; }
    echo "     + kubectl apply — Repository/deployments → ${GENTIAN_DEPLOYMENTS_REPO}"
    [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]] && return 0

    # GENTIAN_DEPLOYMENTS_AUTH=none (public deployments repo) omits the
    # credential block entirely — see repository-default.yaml's $hasCred
    # branch. Declaring one that nothing will ever write to OpenBao would
    # leave credentialSatisfied false forever and block the repo Secret and
    # the operator's push-access Secret from ever being emitted.
    #
    # Two full heredocs rather than one with an interpolated credential-block
    # variable: validate_step_calls (scripts/lib/driver.sh) strips heredoc
    # BODIES before scanning a step for undefined calls, but has no such
    # allowance for YAML sitting in an ordinary multi-line string assignment
    # — every "key:" line in one reads as a 4-space-indented bare command.
    local auth; auth="$(_repo_auth_for deployments-repository)"
    if [[ "${auth}" != "none" ]]; then
        kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: deployments
  namespace: crossplane-system
spec:
  type: git
  # Not the default. role decides how the API guards a change to this
  # repository: deployments is the source of truth, so repointing it asks for a
  # retype, while apps is an additive catalogue and does not. Omitting it left
  # the cluster's own source of truth protected as though it were an optional
  # app source — the safer-looking value being the weaker guard.
  role: deployments
  endpoints:
    inCluster: ${GENTIAN_DEPLOYMENTS_REPO}
  branch: ${GENTIAN_DEPLOYMENTS_BRANCH:-main}
  # The operator pushes here: installing an app from the store commits a
  # manifest to this repository.
  writable: true
  credential:
    vaultPath: gentian-os/kernel/repositories/deployments
    displayName: "Deployments Repository Access"
    phase: bootstrap
    authType: ${auth}
    validate:
      type: git-https
EOF
    else
        kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: deployments
  namespace: crossplane-system
spec:
  type: git
  role: deployments
  endpoints:
    inCluster: ${GENTIAN_DEPLOYMENTS_REPO}
  branch: ${GENTIAN_DEPLOYMENTS_BRANCH:-main}
  writable: true
EOF
    fi
}

destroy() {
    kubectl delete repository.gentianos.io deployments -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
