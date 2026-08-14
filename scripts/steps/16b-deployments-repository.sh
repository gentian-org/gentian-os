#!/usr/bin/env bash
# step: 16b-deployments-repository
# phase: secrets
# requires: 16-cluster-xr
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
# It is written inline rather than as a .tmpl + envsubst so the YAML an operator
# is asked to trust is visible in the step they are reading, and so Phase 10c
# has one fewer template to delete rather than one more.
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

    kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: deployments
  namespace: crossplane-system
spec:
  type: git
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
    validate:
      type: git-https
EOF
}

destroy() {
    kubectl delete repository.gentianos.io deployments -n crossplane-system \
        --ignore-not-found=true --timeout=60s 2>/dev/null || true
}
