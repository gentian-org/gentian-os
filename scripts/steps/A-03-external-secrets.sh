#!/usr/bin/env bash
# step: A-03-external-secrets
# phase: control-plane
# requires: A-01-namespaces
# provides: External Secrets Operator and its CRDs in the secrets namespace
# mutates: the secrets namespace, ESO CRDs, cluster-scoped RBAC
# pins: external-secrets

check() {
    helm_pinned_ok external-secrets external-secrets secrets &&
        kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1 &&
        helm_deployments_available secrets external-secrets external-secrets-cert-controller external-secrets-webhook
}

apply() {
    banner "External Secrets Operator"
    helm_pinned external-secrets external-secrets secrets --set installCRDs=true
}

# The ExternalSecrets, released while their controller still runs.
#
# Each carries externalsecret-cleanup, a finalizer only the controller
# clears. Uninstalled first, the controller left them for ever, and every
# namespace holding one stayed Terminating -- which the next install met as
# "unable to create new content in namespace kernel-edge". So: delete them
# all, give the controller a bounded wait, strip what is left, then uninstall.
_a03_release_external_secrets() {
    local kinds="externalsecrets.external-secrets.io pushsecrets.external-secrets.io" kind obj ns
    for kind in ${kinds}; do
        kubectl get "${kind}" -A -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' 2>/dev/null \
            | while read -r ns obj; do
                [[ -n "${obj}" ]] || continue
                kubectl delete "${kind}" "${obj}" -n "${ns}" --wait=false >/dev/null 2>&1 || true
            done
    done
    wait_until_empty 60 5 kubectl get externalsecrets.external-secrets.io,pushsecrets.external-secrets.io -A -o name && return 0
    for kind in ${kinds}; do
        kubectl get "${kind}" -A -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' 2>/dev/null \
            | while read -r ns obj; do
                [[ -n "${obj}" ]] || continue
                strip_finalizers "${kind}" "${obj}" -n "${ns}" >/dev/null
            done
    done
}

destroy() {
    _a03_release_external_secrets
    helm uninstall external-secrets -n "$(ns_kernel secrets)" >/dev/null 2>&1 || true
}
