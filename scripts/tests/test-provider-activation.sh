#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-provider-activation.sh
# =============================================================================
# Crossplane installs only the provider resource types the platform uses.
# Three parts have to agree for that to be safe, and each is held here:
#
#   A-04  passes the chart an empty default activation list -- as JSON, since
#         helm's `--set x={}` is a list holding one empty string -- and the
#         chart's own "*" when CROSSPLANE_ACTIVATE_ALL=true; with that switch
#         its check() asks for the policy the switch creates, so the way back
#         reaches a cluster that was installed without it.
#   B-05  applies crossplane/providers/activation.yaml before the providers,
#         records the file's checksum on it, waits for every type it names,
#         and says which type is missing and what to do when one never
#         appears; its check() is not satisfied by a list that has changed.
#   lint  scripts/lint/lint-provider-activation.py passes on this repository,
#         and fails on a Composition that composes a type the list does not
#         name, on a kind it cannot resolve, on a list entry no pinned package
#         ships, and on type lists read from another version than is installed.
#
# kubectl and helm are stand-ins; no cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
BIN="${SANDBOX}/bin"; mkdir -p "${BIN}" "${SANDBOX}/home"

# helm: every call is logged; `list` answers with no releases, or with the
# pinned Crossplane release when ${STATE}/helm-installed exists.
cat > "${BIN}/helm" <<'STUB'
#!/usr/bin/env bash
printf 'helm' >> "${STATE}/calls.log"; printf ' %s' "$@" >> "${STATE}/calls.log"; echo >> "${STATE}/calls.log"
case "${1:-}" in
    list)
        if [[ -f "${STATE}/helm-installed" ]]; then cat "${STATE}/helm-installed"; else echo '[]'; fi ;;
    status) [[ -f "${STATE}/helm-installed" ]] || exit 1 ;;
esac
exit 0
STUB

# kubectl: every call is logged. A CRD is established when it is a line of
# ${STATE}/crds; any other object exists when ${STATE}/objects names it as
# "<kind>/<name>"; an annotation read answers with ${STATE}/sha.
cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
printf 'kubectl' >> "${STATE}/calls.log"; printf ' %s' "$@" >> "${STATE}/calls.log"; echo >> "${STATE}/calls.log"
verb="${1:-}"; shift || true
jsonpath=""; file=""; args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|-l) shift ;;
        -o) [[ "$2" == jsonpath=* ]] && jsonpath="${2#jsonpath=}"; shift ;;
        -f) file="$2"; shift ;;
        --*) ;;
        -A) ;;
        *) args+=("$1") ;;
    esac
    shift
done
kind="${args[0]:-}"; name="${args[1]:-}"
[[ "${kind}" == */* ]] && { name="${kind#*/}"; kind="${kind%%/*}"; }
case "${verb}" in
    get)
        case "${kind}" in
            crd)
                [[ -z "${name}" ]] && exit 0
                grep -qxF "${name}" "${STATE}/crds" 2>/dev/null || exit 1
                [[ "${jsonpath}" == *Established* ]] && printf 'True'
                exit 0 ;;
            managedresourceactivationpolicies.apiextensions.crossplane.io)
                grep -qxF "mrap/${name}" "${STATE}/objects" 2>/dev/null || exit 1
                [[ "${jsonpath}" == *source-sha* ]] && cat "${STATE}/sha" 2>/dev/null
                exit 0 ;;
            provider.pkg.crossplane.io|function.pkg.crossplane.io)
                [[ "${jsonpath}" == *Healthy* ]] && printf 'True'
                exit 0 ;;
            providerconfig.*) exit 0 ;;
            managed) exit 0 ;;
        esac
        exit 0 ;;
    apply)
        # Only a manifest piped in is read; a file is named and left alone.
        if [[ "${file}" == "-" ]]; then cat >/dev/null; fi
        exit 0 ;;
    *) exit 0 ;;
esac
STUB
chmod +x "${BIN}/helm" "${BIN}/kubectl"

new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    : > "${STATE}/calls.log"; : > "${STATE}/crds"; : > "${STATE}/objects"
}

# run <step> <shell> [VAR=value ...] -- the step's verbs in a fresh shell with
# the installer's libraries loaded, the way the driver loads a step.
run() {
    local step="$1" script="$2"; shift 2
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SANDBOX}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        STEP="${step}" SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh"
            trap - ERR; set +e
            sleep() { :; }
            source "${SCRIPT_DIR}/scripts/steps/${STEP}.sh"
            eval "${SCRIPT}"
        ' 2>&1
}

ACTIVATION="${REPO}/crossplane/providers/activation.yaml"
LINT="${REPO}/scripts/lint/lint-provider-activation.py"

echo ""
echo "Crossplane installs only the provider resource types the platform uses"
echo ""

# --- A-04 ---------------------------------------------------------------------
new_state
out="$(run A-04-crossplane 'apply; echo "rc=$?"')"
if [[ "${out}" == *"rc=0"* ]] && grep -qF -- '--set-json provider.defaultActivations=[]' "${STATE}/calls.log"; then
    ok "A-04: the chart is given an empty default activation list, as JSON"
else
    bad "A-04: the chart is given an empty default activation list, as JSON" "${out}$(cat "${STATE}/calls.log")"
fi
if ! grep -qE -- '--set(-string)? provider\.defaultActivations' "${STATE}/calls.log"; then
    ok "A-04: never with --set, which would pass a list holding one empty string"
else
    bad "A-04: never with --set, which would pass a list holding one empty string" "$(cat "${STATE}/calls.log")"
fi

new_state
out="$(run A-04-crossplane 'apply; echo "rc=$?"' CROSSPLANE_ACTIVATE_ALL=true)"
if [[ "${out}" == *"rc=0"* ]] && grep -qF -- '--set-json provider.defaultActivations=["*"]' "${STATE}/calls.log" \
    && [[ "${out}" == *"CROSSPLANE_ACTIVATE_ALL=true"* ]]; then
    ok "A-04: CROSSPLANE_ACTIVATE_ALL=true passes the chart's own \"*\", and says so"
else
    bad "A-04: CROSSPLANE_ACTIVATE_ALL=true passes the chart's own \"*\", and says so" "${out}$(cat "${STATE}/calls.log")"
fi

new_state
out="$(run A-04-crossplane 'apply; echo "rc=$?"' CROSSPLANE_ACTIVATE_ALL=yes)"
if grep -qF -- '--set-json provider.defaultActivations=[]' "${STATE}/calls.log"; then
    ok "A-04: only the word true turns it on"
else
    bad "A-04: only the word true turns it on" "${out}"
fi

# check(): the release answers as installed at the pinned version; what
# differs is whether the "default" policy exists.
installed_release() {
    local version ns
    version="$(bash "${REPO}/scripts/lib/versions.sh" crossplane chart)"
    ns="$(run A-04-crossplane 'ns_kernel provisioning')"
    printf '[{"name":"crossplane","namespace":"%s","status":"deployed","chart":"crossplane-%s","app_version":"%s"}]' \
        "${ns}" "${version}" "${version}" > "${STATE}/helm-installed"
    echo "compositeresourcedefinitions.apiextensions.crossplane.io" >> "${STATE}/crds"
}
new_state; installed_release
out="$(run A-04-crossplane 'check; echo "rc=$?"')"
if [[ "${out}" == *"rc=0"* ]]; then
    ok "A-04 check: an installed Crossplane without the default policy is satisfied"
    new_state; installed_release
    out="$(run A-04-crossplane 'check; echo "rc=$?"' CROSSPLANE_ACTIVATE_ALL=true)"
    if [[ "${out}" == *"rc=1"* ]]; then
        ok "A-04 check: with CROSSPLANE_ACTIVATE_ALL=true and no default policy it is not, so the way back is applied"
    else
        bad "A-04 check: with CROSSPLANE_ACTIVATE_ALL=true and no default policy it is not, so the way back is applied" "${out}"
    fi
    echo "mrap/default" >> "${STATE}/objects"
    out="$(run A-04-crossplane 'check; echo "rc=$?"' CROSSPLANE_ACTIVATE_ALL=true)"
    if [[ "${out}" == *"rc=0"* ]]; then
        ok "A-04 check: and satisfied once the default policy exists"
    else
        bad "A-04 check: and satisfied once the default policy exists" "${out}"
    fi
else
    bad "A-04 check: an installed Crossplane without the default policy is satisfied" "${out}"
fi

# --- B-05 ---------------------------------------------------------------------
# Every CRD the step waits for answers as established.
all_crds() {
    # shellcheck disable=SC2016 # for the child shell to expand
    run B-05-crossplane-providers '_b05_activated_types; printf "%s\n" "${_V5_PROVIDER_CRDS[@]}"' >> "${STATE}/crds"
}
activation_sha() { run B-05-crossplane-providers '_b05_activation_sha'; }

new_state
types="$(run B-05-crossplane-providers '_b05_activated_types')"
listed="$(python3 -c '
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d: print("\n".join(d["spec"]["activate"]))' "${ACTIVATION}")"
if [[ -n "${types}" && "${types}" == "${listed}" ]]; then
    ok "B-05 reads every entry of activation.yaml ($(wc -l <<<"${types}" | tr -d ' ') types)"
else
    bad "B-05 reads every entry of activation.yaml" "${types}"
fi

new_state; all_crds
out="$(run B-05-crossplane-providers 'apply; echo "rc=$?"')"
policy_line="$(grep -n -- "kubectl apply -f ${ACTIVATION}" "${STATE}/calls.log" | head -1 | cut -d: -f1)"
providers_line="$(grep -n -- 'kubectl apply -f -' "${STATE}/calls.log" | head -1 | cut -d: -f1)"
if [[ "${out}" == *"rc=0"* && -n "${policy_line}" && -n "${providers_line}" ]] && (( policy_line < providers_line )); then
    ok "B-05: the activation policy is applied before the providers"
else
    bad "B-05: the activation policy is applied before the providers" "${out}$(cat "${STATE}/calls.log")"
fi
if [[ "$(grep -c -- 'kubectl apply -f -' "${STATE}/calls.log")" == "3" ]]; then
    ok "B-05: then the providers, their RBAC and their ProviderConfigs"
else
    bad "B-05: then the providers, their RBAC and their ProviderConfigs" "$(cat "${STATE}/calls.log")"
fi
if grep -qF -- "annotate managedresourceactivationpolicies.apiextensions.crossplane.io gentian-platform --overwrite gentianos.io/source-sha=$(activation_sha)" "${STATE}/calls.log"; then
    ok "B-05: the file's checksum is recorded on the policy"
else
    bad "B-05: the file's checksum is recorded on the policy" "$(grep annotate "${STATE}/calls.log")"
fi
if grep -qF -- 'delete provider.pkg.crossplane.io provider-http' "${STATE}/calls.log"; then
    ok "B-05: provider-http, which nothing uses, is removed from a cluster that still has it"
else
    bad "B-05: provider-http, which nothing uses, is removed from a cluster that still has it"
fi
if ! grep -q 'provider-http' "${REPO}/crossplane/providers/providers.yaml"; then
    ok "providers.yaml no longer installs provider-http"
else
    bad "providers.yaml no longer installs provider-http"
fi

# One activated type never appears.
new_state; all_crds
missing="secretv2s.kv.vault.upbound.io"
grep -vxF "${missing}" "${STATE}/crds" > "${STATE}/crds.new"; mv "${STATE}/crds.new" "${STATE}/crds"
out="$(run B-05-crossplane-providers 'apply; echo "rc=$?"' GENTIAN_ACTIVATION_TIMEOUT=0)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"${missing}"* && "${out}" == *"CROSSPLANE_ACTIVATE_ALL=true"* && "${out}" == *"kubectl get mrd"* ]]; then
    ok "B-05: a type that never appears stops the step, named, with the way back"
else
    bad "B-05: a type that never appears stops the step, named, with the way back" "${out}"
fi
# providers.yaml is the one manifest piped to kubectl before the wait; the
# providers' RBAC and ProviderConfigs come after it.
if [[ "$(grep -c -- 'kubectl apply -f -' "${STATE}/calls.log")" == "1" ]]; then
    ok "B-05: and neither the providers' RBAC nor their ProviderConfigs are applied after it"
else
    bad "B-05: and neither the providers' RBAC nor their ProviderConfigs are applied after it" "$(cat "${STATE}/calls.log")"
fi

# check()
new_state; all_crds
echo "mrap/gentian-platform" >> "${STATE}/objects"; activation_sha > "${STATE}/sha"
out="$(run B-05-crossplane-providers 'check; echo "rc=$?"')"
if [[ "${out}" == *"rc=0"* ]]; then
    ok "B-05 check: policy from this checkout, every type established: satisfied"
else
    bad "B-05 check: policy from this checkout, every type established: satisfied" "${out}"
fi
echo "0000000000000000" > "${STATE}/sha"
out="$(run B-05-crossplane-providers 'check; echo "rc=$?"')"
if [[ "${out}" == *"rc=1"* ]]; then
    ok "B-05 check: a policy applied from another version of the list is not"
else
    bad "B-05 check: a policy applied from another version of the list is not" "${out}"
fi
activation_sha > "${STATE}/sha"; : > "${STATE}/objects"
out="$(run B-05-crossplane-providers 'check; echo "rc=$?"')"
if [[ "${out}" == *"rc=1"* ]]; then
    ok "B-05 check: no policy at all is not"
else
    bad "B-05 check: no policy at all is not" "${out}"
fi
echo "mrap/gentian-platform" >> "${STATE}/objects"
grep -vxF "roles.role.keycloak.crossplane.io" "${STATE}/crds" > "${STATE}/crds.new"; mv "${STATE}/crds.new" "${STATE}/crds"
out="$(run B-05-crossplane-providers 'check; echo "rc=$?"')"
if [[ "${out}" == *"rc=1"* ]]; then
    ok "B-05 check: an activated type that is not a CRD is not"
else
    bad "B-05 check: an activated type that is not a CRD is not" "${out}"
fi

# The ProviderConfig CRDs B-05 waits for before it applies the configs are
# kinds Crossplane installs whatever is activated.
new_state
unknown=""
# shellcheck disable=SC2016 # for the child shell to expand
for crd in $(run B-05-crossplane-providers 'printf "%s\n" "${_V5_PROVIDER_CRDS[@]}"'); do
    grep -A4 -xF "  - name: ${crd}" "${REPO}"/crossplane/providers/types/*.yaml | grep -q 'managed: false' || unknown+="${crd} "
done
if [[ -z "${unknown}" ]]; then
    ok "B-05: the ProviderConfig CRDs it waits for are plain CRDs in the pinned packages, not activated types"
else
    bad "B-05: the ProviderConfig CRDs it waits for are plain CRDs in the pinned packages, not activated types" "${unknown}"
fi

# --- the lint -------------------------------------------------------------------
if out="$(GENTIAN_APPS_DIR="${SANDBOX}/none" python3 "${LINT}" 2>&1)"; then
    ok "lint: every provider type this repository uses is activated"
else
    bad "lint: every provider type this repository uses is activated" "${out}"
fi

# A Composition that composes a type the list does not name.
TREE="${SANDBOX}/tree"; mkdir -p "${TREE}/crossplane/compositions"
cat > "${TREE}/crossplane/compositions/app.yaml" <<'EOF'
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
spec:
  pipeline:
    - step: render
      input:
        inline:
          template: |
            {{- if .observed.composite.resource.spec.mount }}
            ---
            apiVersion: vault.vault.upbound.io/v1alpha1
            kind: Mount
            metadata:
              name: {{ $name }}-mount
            {{- end }}
            ---
            apiVersion: kv.vault.upbound.io/v1alpha1
            kind: SecretV2
            metadata:
              name: {{ $name }}-secret
EOF
out="$(python3 "${LINT}" --tree "${TREE}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"mounts.vault.vault.upbound.io"* && "${out}" == *"crossplane/compositions/app.yaml:11"* \
      && "${out}" == *"- mounts.vault.vault.upbound.io"* && "${out}" != *"secretv2s"* ]]; then
    ok "lint: a Composition composing an unlisted type fails, naming the type, the line and the entry to add"
else
    bad "lint: a Composition composing an unlisted type fails, naming the type, the line and the entry to add" "${out}"
fi

# The same tree passes once the type is in the list.
LIST="${SANDBOX}/activation.yaml"
sed 's|^    - policies.vault.vault.upbound.io$|&\n    - mounts.vault.vault.upbound.io|' "${ACTIVATION}" > "${LIST}"
if out="$(python3 "${LINT}" --tree "${TREE}" --activation "${LIST}" 2>&1)"; then
    ok "lint: and passes once the list names it"
else
    bad "lint: and passes once the list names it" "${out}"
fi

# A kind that cannot be read.
cat > "${TREE}/crossplane/compositions/app.yaml" <<'EOF'
            apiVersion: kv.vault.upbound.io/v1alpha1
            kind: {{ $kind }}
EOF
out="$(python3 "${LINT}" --tree "${TREE}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"templated"* ]]; then
    ok "lint: a templated kind beside a provider apiVersion fails rather than passing unseen"
else
    bad "lint: a templated kind beside a provider apiVersion fails rather than passing unseen" "${out}"
fi
cat > "${TREE}/crossplane/compositions/app.yaml" <<'EOF'
apiVersion: kv.vault.upbound.io/v1alpha1
kind: SecretV3
EOF
out="$(python3 "${LINT}" --tree "${TREE}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"SecretV3.kv.vault.upbound.io is not a type the pinned provider package ships"* ]]; then
    ok "lint: a kind the pinned package does not ship fails"
else
    bad "lint: a kind the pinned package does not ship fails" "${out}"
fi

# Go and shell.
rm -f "${TREE}/crossplane/compositions/app.yaml"; mkdir -p "${TREE}/internal" "${TREE}/scripts"
cat > "${TREE}/internal/x.go" <<'EOF'
package x

// +kubebuilder:rbac:groups=user.keycloak.crossplane.io,resources=users,verbs=get;list;watch
var gvk = schema.GroupVersionKind{Group: "defaults.keycloak.crossplane.io", Version: "v1alpha1", Kind: "Roles"}
EOF
printf '%s\n' 'kubectl get mount.vault.vault.upbound.io x' > "${TREE}/scripts/x.sh"
out="$(python3 "${LINT}" --tree "${TREE}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"users.user.keycloak.crossplane.io"* && "${out}" == *"roles.defaults.keycloak.crossplane.io"* \
      && "${out}" == *"mounts.vault.vault.upbound.io"* ]]; then
    ok "lint: a type named in Go (marker, GroupVersionKind) or in a kubectl call is checked too"
else
    bad "lint: a type named in Go (marker, GroupVersionKind) or in a kubectl call is checked too" "${out}"
fi
rm -rf "${TREE}/internal" "${TREE}/scripts"

# A list entry no pinned package ships.
sed 's|^    - policies.vault.vault.upbound.io$|&\n    - polices.vault.vault.upbound.io|' "${ACTIVATION}" > "${LIST}"
out="$(python3 "${LINT}" --tree "${TREE}" --activation "${LIST}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"polices.vault.vault.upbound.io matches no type"* ]]; then
    ok "lint: a list entry no pinned provider package ships fails"
else
    bad "lint: a list entry no pinned provider package ships fails" "${out}"
fi

# Type lists read from another version than providers.yaml installs.
PROVIDERS="${SANDBOX}/providers.yaml"
sed 's|provider-keycloak:v[0-9.]*|provider-keycloak:v99.0.0|' "${REPO}/crossplane/providers/providers.yaml" > "${PROVIDERS}"
out="$(python3 "${LINT}" --tree "${TREE}" --providers "${PROVIDERS}" 2>&1)"; rc=$?
if [[ ${rc} -ne 0 && "${out}" == *"provider-keycloak"* && "${out}" == *"make refresh-provider-types"* ]]; then
    ok "lint: a provider pin that moved without its type list being refreshed fails"
else
    bad "lint: a provider pin that moved without its type list being refreshed fails" "${out}"
fi

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} of $((pass + fail)) checks failed.${NC}"
exit 1
