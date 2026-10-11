#!/usr/bin/env bash
# step: C-04-credential-catalogue
# phase: platform
# requires: C-01-cluster-claim
# provides: CredentialRequirement catalogue and its ESO satisfaction probes
# mutates: cluster-scoped CredentialRequirement objects, ExternalSecrets in the control namespace

# The on-cluster half of the catalogue. credential-requirements.yaml travels
# with the installer; these are the same content as API objects, so the
# custodian and any gating Composition can read them.
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
# The manifest is generated with the probes in the control namespace
# (scripts/gen/gen-credential-requirements.py reads kernel/namespaces.yaml).

_cc_ns() { ns_kernel control; }

_catalogue_file() { echo "${SCRIPT_DIR}/kernel/credentials/credential-requirements.yaml"; }

# The catalogue with this cluster's addresses in it.
#
# The repository hosts are substituted, because a git-https probe has no
# endpoint of its own -- username and password do not carry one -- so a
# requirement without a host cannot be validated at all, and a write that
# asked to be validated was refused rather than stored.
#
# The addresses are this cluster's, which is why they are not in the file.
# Defaults match the public repositories, so a cluster that mirrors none of
# them still gets a probe that reaches something.
_cc_render() {
    sed -e "s#__GENTIAN_DEPLOYMENTS_REPO__#${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments}#" \
        -e "s#__GENTIAN_OS_REPO__#${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}#" \
        -e "s#__GENTIAN_UI_REPO__#${GENTIAN_UI_REPO:-https://github.com/gentian-org/gentian-ui}#" \
        "$(_catalogue_file)"
}

# What this cluster asks for, not the whole catalogue.
#
# The catalogue describes the platform -- every DNS provider, every source
# repository, every LLM provider -- and applying all of it filled the
# custodian with forms nothing on the cluster would ever read: six
# DNS providers on a Cloudflare cluster, NOT SET under each. An entry is
# applied when _requirement_applies says this cluster uses it (the same gate
# the installer's prompts use), and not when a composed requirement -- a
# Repository claim's -- already declares the same OpenBao path, which listed
# the deployments credential twice.
_cc_composed_paths() {
    kubectl get credentialrequirements -o json 2>/dev/null \
        | jq -r '.items[] | select((.metadata.ownerReferences // []) | length > 0) | .spec.vaultPath' \
        2>/dev/null || true
}

_cc_wanted() {
    local name path composed
    composed="$(_cc_composed_paths)"
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        _requirement_applies "${name}" || continue
        path="$(catalogue_get "${name}" vaultPath)"
        if [[ -n "${path}" ]] && grep -qxF -- "${path}" <<<"${composed}"; then
            continue
        fi
        echo "${name}"
    done < <(catalogue_names)
}

# _cc_select <names> — the rendered catalogue, reduced to these requirements
# and their probes. A probe is named credreq-<requirement>.
_cc_select() {
    # shellcheck disable=SC2016 # awk's own fields
    awk -v keep="$1" '
        BEGIN { n = split(keep, k, "\n"); for (i = 1; i <= n; i++) if (k[i] != "") want[k[i]] = 1 }
        function flush() {
            if (doc != "" && (name in want)) printf "---\n%s", doc
            doc = ""; name = ""; meta = 0
        }
        /^---$/                { flush(); next }
        /^metadata:/           { meta = 1 }
        /^[a-z]/ && !/^metadata:/ { meta = 0 }
        meta && /^  name: /    { name = $2; sub(/^credreq-/, "", name) }
        { doc = doc $0 "\n" }
        END { flush() }
    '
}

# _cc_prune <names> — remove catalogue requirements this cluster no longer
# asks for, and their probes. Only what the catalogue applied: a requirement
# with an owner was composed by something else and is that thing's to remove.
# The declaration goes; a value already stored in OpenBao stays.
_cc_prune() {
    local wanted="$1" name ns
    ns="$(_cc_ns)"
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        grep -qxF -- "${name}" <<<"${wanted}" && continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 || continue
        [[ -z "$(kubectl get credentialrequirement "${name}" -o jsonpath='{.metadata.ownerReferences}' 2>/dev/null)" ]] || continue
        info "  ${name}: not requested on this cluster; removing its declaration (a stored value stays in OpenBao)"
        kubectl delete credentialrequirement "${name}" --ignore-not-found >/dev/null 2>&1 || true
        kubectl delete externalsecret "credreq-${name}" -n "${ns}" --ignore-not-found >/dev/null 2>&1 || true
    done < <(catalogue_names)
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
    local name want have ns wanted
    ns="$(_cc_ns)"
    wanted="$(_cc_wanted)"
    # And nothing it no longer asks for: a requirement left from before shows
    # a form nobody needs, so its presence is as unsatisfied as an absence.
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        grep -qxF -- "${name}" <<<"${wanted}" && continue
        [[ -n "$(kubectl get credentialrequirement "${name}" -o jsonpath='{.metadata.ownerReferences}' 2>/dev/null)" ]] && continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 && return 1
    done < <(catalogue_names)
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl get credentialrequirement "${name}" >/dev/null 2>&1 || return 1

        want="$(catalogue_field_keys "${name}" | sort | tr '\n' ' ')"
        have="$(kubectl get externalsecret "credreq-${name}" -n "${ns}" \
            -o jsonpath='{range .spec.data[*]}{.remoteRef.property}{"\n"}{end}' \
            2>/dev/null | sort | tr '\n' ' ')" || return 1
        [[ -n "${have}" && "${want}" == "${have}" ]] || return 1
    done <<<"${wanted}"
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

    # A placeholder that survived substitution would become a host nothing
    # can reach, and the probe would then report a good credential as bad --
    # which is worse than not probing at all.
    local rendered wanted
    wanted="$(_cc_wanted)"
    rendered="$(_cc_render | _cc_select "${wanted}")"
    if grep -q '__GENTIAN_' <<<"${rendered}"; then
        error "The credential catalogue still holds an unsubstituted placeholder:"
        grep -o '__GENTIAN_[A-Z_]*__' <<<"${rendered}" | sort -u | while IFS= read -r ph; do
            error "  ${ph}"
        done
        return 1
    fi
    if [[ -n "${rendered}" ]]; then
        printf '%s\n' "${rendered}" | gentian_run kubectl apply -f -
    fi
    _cc_prune "${wanted}"
}

destroy() {
    _cc_render | kubectl delete -f - --ignore-not-found=true 2>/dev/null || true
}
