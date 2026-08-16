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
    # Every requirement in the bundled catalogue must exist on the cluster,
    # along with the ExternalSecret that makes its satisfaction observable.
    #
    # Both halves, because they have different lifetimes: the requirements are
    # cluster-scoped and survive almost anything, while the probes live in
    # gentian-system — a namespace the Cluster XR composes, so it can be removed
    # and recreated underneath them. Testing only the requirements left this
    # step reporting satisfied with every probe gone, which is exactly the state
    # `make check-credentials` reads and reports as six missing credentials.
    # Existence is not enough — the probe has to ask for the fields the
    # catalogue declares. An ExternalSecret left over from an earlier catalogue
    # still exists while querying a field nobody writes, so it reports
    # SecretSyncedError forever and check-credentials calls the credential
    # missing; a check testing only that the object is there skips the apply
    # that would correct it.
    local name want have
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 || return 1

        want="$(catalogue_field_keys "${name}" | sort | tr '\n' ' ')"
        have="$(kubectl get externalsecret "credreq-${name}" -n gentian-system \
            -o jsonpath='{range .spec.data[*]}{.remoteRef.property}{"\n"}{end}' \
            2>/dev/null | sort | tr '\n' ' ')" || return 1
        [[ -n "${have}" && "${want}" == "${have}" ]] || return 1
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
