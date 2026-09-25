#!/usr/bin/env bash
# step: C-05-credential-catalogue
# phase: claims
# requires: C-01-cluster-claim
# provides: CredentialRequirement catalogue and its ESO satisfaction probes
# mutates: cluster-scoped CredentialRequirement objects, ExternalSecrets in the control namespace

# The on-cluster half of the catalogue. credential-requirements.yaml travels
# with the installer; these are the same content as API objects, so the
# credential manager and any gating Composition can read them.
#
# Each requirement gets an ExternalSecret with creationPolicy: None. ESO
# resolves the remote reference and reports SecretSynced without creating a
# Secret, which makes satisfaction observable as a Kubernetes condition without
# materialising cluster-wide credential material into a namespace that has no
# use for it.
#
# v5 had no such step, and the consequence was not subtle: `make
# check-credentials` looks for exactly these probes, found none, and reported
# every credential missing on a cluster where most were fine. A genuinely
# absent DNS, registry or repository credential was invisible in that noise
# until its consumer failed.
#
# The manifest is v4's, applied with the layout's namespace substituted, the
# same way B-05 applies the providers. Seventeen `namespace: gentian-system`
# lines in one file are not worth a second copy of the file.

_cc_ns() { ns_kernel control; }

_catalogue_file() { echo "${SCRIPT_DIR}/kernel/credentials/credential-requirements.yaml"; }

_cc_render() {
    sed -e "s/^  namespace: gentian-system$/  namespace: $(_cc_ns)/" "$(_catalogue_file)"
}

check() {
    kubectl get crd credentialrequirements.gentianos.io >/dev/null 2>&1 || return 1
    # Both halves, because they have different lifetimes: the requirements are
    # cluster-scoped and survive almost anything, while the probes live in the
    # control namespace — which the Cluster XR composes, so it can be removed
    # and recreated underneath them.
    #
    # Existence is not enough. A probe left over from an earlier catalogue
    # still exists while querying a field nobody writes, reports
    # SecretSyncedError for ever, and check-credentials calls the credential
    # missing; a check testing only that the object is there would skip the
    # apply that corrects it.
    local name want have ns
    ns="$(_cc_ns)"
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 || return 1

        want="$(catalogue_field_keys "${name}" | sort | tr '\n' ' ')"
        have="$(kubectl get externalsecret "credreq-${name}" -n "${ns}" \
            -o jsonpath='{range .spec.data[*]}{.remoteRef.property}{"\n"}{end}' \
            2>/dev/null | sort | tr '\n' ' ')" || return 1
        [[ -n "${have}" && "${want}" == "${have}" ]] || return 1
    done < <(catalogue_names)
    return 0
}

apply() {
    banner "Credential catalogue"
    # The CRD before the objects that need it. It ships in the operator chart's
    # crds/ directory and the operator is D-01, a phase later, so waiting for
    # Helm to install it would mean this step can never succeed on a first
    # install. Applying the same file Helm would is idempotent: Helm's crds/
    # handling installs a CRD only when it is absent.
    #
    # It matters more on v5 than it did on v4. The Repository claims are
    # composed by C-02, before the operator exists, and the Repository
    # Composition emits a CredentialRequirement unconditionally — into an API
    # group that would otherwise not be there yet.
    local crd="${SCRIPT_DIR}/charts/gentian-os/crds/gentianos.io_credentialrequirements.yaml"
    if [[ ! -f "${crd}" ]]; then
        error "CredentialRequirement CRD not found at ${crd}"
        return 1
    fi
    gentian_run kubectl apply --server-side --force-conflicts -f "${crd}"
    # Established, not merely created: the objects below are rejected by a CRD
    # the API server has not finished registering.
    kubectl wait --for=condition=Established \
        crd/credentialrequirements.gentianos.io --timeout=60s >/dev/null 2>&1 || true

    _cc_render | gentian_run kubectl apply -f -
}

destroy() {
    _cc_render | kubectl delete -f - --ignore-not-found=true 2>/dev/null || true
}
