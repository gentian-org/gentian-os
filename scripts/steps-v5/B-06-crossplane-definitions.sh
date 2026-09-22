#!/usr/bin/env bash
# step: B-06-crossplane-definitions
# phase: secrets
# requires: B-05-crossplane-providers
# provides: the XRDs and Compositions of crossplane/, established, from this checkout
# mutates: CompositeResourceDefinitions and Compositions (cluster-scoped)

# Applied from the checkout, as v4's A-02 does before Argo CD owns them. The
# claim a cluster is scaffolded with carries spec.layout, and the compositions
# read it; a checkout and a claim of different layouts meet at C-01, which
# refuses the mismatch.

# The files hold several documents (the XRD and the ClusterRoles it needs);
# name only the XRDs and Compositions.
_v5_xrd_names()         { _gentian_yq 'select(.kind == "CompositeResourceDefinition") | .metadata.name' "$1"; }
_v5_composition_names() { _gentian_yq 'select(.kind == "Composition") | .metadata.name' "$1"; }
_v5_sha()               { sha256sum "$1" | cut -c1-16; }

# Existence is not enough: a Composition edited in the checkout and not
# re-applied is exactly how a cluster keeps composing with the old one while
# the step reports satisfied. The applied file's checksum is recorded on the
# object and compared.
_v5_current() {
    local kind="$1" name="$2" file="$3"
    [[ "$(kubectl get "${kind}" "${name}" -o jsonpath='{.metadata.annotations.gentianos\.io/source-sha}' 2>/dev/null)" == "$(_v5_sha "${file}")" ]]
}

check() {
    local f name
    for f in "${SCRIPT_DIR}"/crossplane/xrds/*.yaml; do
        for name in $(_v5_xrd_names "${f}"); do
            [[ "$(kubectl get xrd "${name}" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)" == "True" ]] || return 1
            _v5_current xrd "${name}" "${f}" || return 1
        done
    done
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        for name in $(_v5_composition_names "${f}"); do
            _v5_current composition "${name}" "${f}" || return 1
        done
    done
    return 0
}

apply() {
    banner "Crossplane definitions"
    local f
    local name
    for f in "${SCRIPT_DIR}"/crossplane/xrds/*.yaml; do
        _kubectl_retry apply -f "${f}"
        for name in $(_v5_xrd_names "${f}"); do
            kubectl wait xrd "${name}" --for=condition=Established --timeout=120s >/dev/null
            kubectl annotate xrd "${name}" --overwrite "gentianos.io/source-sha=$(_v5_sha "${f}")" >/dev/null
        done
    done
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        _kubectl_retry apply -f "${f}"
        for name in $(_v5_composition_names "${f}"); do
            kubectl annotate composition "${name}" --overwrite "gentianos.io/source-sha=$(_v5_sha "${f}")" >/dev/null
        done
    done
}

destroy() {
    local f
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do kubectl delete -f "${f}" --ignore-not-found >/dev/null 2>&1 || true; done
    for f in "${SCRIPT_DIR}"/crossplane/xrds/*.yaml; do kubectl delete -f "${f}" --ignore-not-found >/dev/null 2>&1 || true; done
}
