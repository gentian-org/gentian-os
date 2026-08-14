#!/usr/bin/env bash
# step: 24-credential-catalogue
# phase: platform
# requires: 16-cluster-xr
# provides: CredentialRequirement catalogue and its ESO satisfaction probes
# mutates: cluster-scoped CredentialRequirement objects, ExternalSecrets in gentian-system

# The on-cluster half of the catalogue. credentials.yaml travels with the
# installer; these are the same content as API objects, so the credential
# manager and any gating Composition can read them.
#
# Each requirement gets an ExternalSecret with creationPolicy: None. ESO
# resolves the remote reference and reports SecretSynced without creating a
# Secret, which makes satisfaction observable as a Kubernetes condition without
# materialising cluster-wide credential material into a namespace that has no
# use for it. That is what lets §4 claim "no controller".

_catalogue_file() { echo "${SCRIPT_DIR}/kernel/credentials/credential-requirements.yaml"; }

check() {
    kubectl get crd credentialrequirements.gentianos.io >/dev/null 2>&1 || return 1
    # Every requirement in the bundled catalogue must exist on the cluster.
    local name
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 || return 1
    done < <(catalogue_names)
    return 0
}

apply() {
    gentian_run kubectl apply -f "$(_catalogue_file)"
}

destroy() {
    kubectl delete -f "$(_catalogue_file)" --ignore-not-found=true 2>/dev/null || true
}
