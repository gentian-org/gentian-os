#!/usr/bin/env bash
# step: A-08-eso
# phase: control-plane
# requires: A-03-namespaces
# provides: External Secrets Operator — the only runtime read path for secrets
# mutates: namespace external-secrets, ESO CRDs
# pins: external-secrets

check() {
    kubectl get deployment external-secrets -n external-secrets >/dev/null 2>&1 &&
        kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1
}

apply() {
    install_eso
}

destroy() {
    # Clear every ESO-owned object first, cluster-wide, while ESO is still able
    # to be bypassed by a finalizer patch.
    #
    # externalsecrets.external-secrets.io/externalsecret-cleanup is removed only
    # by ESO. Uninstalling ESO while any ExternalSecret still exists strands that
    # object permanently and its namespace can never terminate — and because ESO
    # is torn down early in the reverse order, the failure surfaces much later,
    # in whichever step owns that namespace, by which point nothing in the
    # cluster can clear it. One ExternalSecret in crossplane-system is enough to
    # wedge A-01.
    local kind
    for kind in \
        externalsecrets.external-secrets.io \
        clusterexternalsecrets.external-secrets.io \
        pushsecrets.external-secrets.io \
        secretstores.external-secrets.io \
        clustersecretstores.external-secrets.io; do
        kubectl get crd "${kind}" >/dev/null 2>&1 || continue
        _strip_and_delete_crd_instances "${kind}"
    done

    if helm status external-secrets -n external-secrets >/dev/null 2>&1; then
        gentian_run helm uninstall external-secrets -n external-secrets || true
    fi
    _delete_namespace external-secrets
}
