#!/usr/bin/env bash
# step: C-06-credential-catalogue
# phase: platform
# requires: B-07-cluster-xr
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
    # The CRD before the objects that need it. It ships in the operator chart's
    # crds/ directory, and the operator is D-01 — a whole phase after this step,
    # so waiting for Helm to install it would mean this step can never succeed
    # on a first install. Applying the same file Helm would is idempotent:
    # Helm's crds/ handling installs a CRD only when it is absent.
    local crd="${SCRIPT_DIR}/charts/gentian-os/crds/gentianos.io_credentialrequirements.yaml"
    if [[ -f "${crd}" ]]; then
        gentian_run kubectl apply --server-side --force-conflicts -f "${crd}"
        # Established, not merely created: the objects below are rejected by a
        # CRD the API server has not finished registering.
        kubectl wait --for=condition=Established \
            crd/credentialrequirements.gentianos.io --timeout=60s >/dev/null 2>&1 || true
    else
        error "CredentialRequirement CRD not found at ${crd}"
        return 1
    fi

    gentian_run kubectl apply -f "$(_catalogue_file)"
}

destroy() {
    kubectl delete -f "$(_catalogue_file)" --ignore-not-found=true 2>/dev/null || true
}
