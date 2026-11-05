#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-initial-install-approvals.sh
# =============================================================================
# The initial install of a cluster is break-glass authority, and approves the
# privileges of the add-ons it placed (_initial_install_approvals); every
# later run is an update and approves nothing. What is held still:
#
#   - the approvals are the platform tenant's privileges.yaml, listed as a
#     patch, with the installer as approver, every apiExtensions and
#     clusterRoles request of the add-on's profile;
#   - an API extension's approval names the digest the operator computes for
#     the request (internal/security/testdata/api-extension-profile.digest,
#     which a Go test holds the operator's computation to);
#   - an add-on step 0 did not place, a run with --disable-api-extensions or
#     without the extra services, and a privileges file that already exists
#     get none;
#   - the scaffold calls it only in the run that wrote the Cluster claim.
#
# Runs the library's own functions in a throwaway deployments checkout. No
# cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT

CLUSTER=c1
PROFILE="${REPO}/internal/security/testdata/api-extension-profile.yaml"
DIGEST="$(tr -d '[:space:]' < "${REPO}/internal/security/testdata/api-extension-profile.digest")"

# new_checkout <dir> [placed] -- the platform tenant as step 0 writes it, and
# the add-on's profile in the catalog when placed is given.
new_checkout() {
    rm -rf "$1"
    mkdir -p "$1/clusters/${CLUSTER}/tenants/platform" "$1/clusters/${CLUSTER}/catalogue" "$1/clusters/${CLUSTER}/kernel/claims"
    printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- tenant.yaml\n' \
        > "$1/clusters/${CLUSTER}/tenants/platform/kustomization.yaml"
    if [[ -n "${2:-}" ]]; then
        cp "${PROFILE}" "$1/clusters/${CLUSTER}/catalogue/backup-scheduler-ee.yaml"
    fi
}

# run <checkout> <env assignments...> -- <shell>
run() {
    local checkout="$1"; shift
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" \
        GENTIAN_DEPLOYMENTS_PATH="${checkout}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" \
        KERNEL_DOMAIN=k.example ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }

echo ""
echo "Step 0: what the initial install approves"
echo ""

CO="${SANDBOX}/co"
DIR="${CO}/clusters/${CLUSTER}/tenants/platform"
APPROVE='_initial_install_approvals '"${CLUSTER}"' "${GENTIAN_DEPLOYMENTS_PATH}/clusters/'"${CLUSTER}"'/tenants/platform"'

new_checkout "${CO}" placed
out="$(run "${CO}" -- "${APPROVE}")"
file="$(cat "${DIR}/privileges.yaml" 2>/dev/null)"
if [[ "${file}" == *"privilege: apiExtensions/backup-schedules"* && "${file}" == *"digest: ${DIGEST}"* \
    && "${file}" == *"privilege: clusterRoles/tenant-exports"* && "${file}" == *"install: backup-scheduler-ee"* \
    && "${file}" == *"approver: installer"* && "${file}" == *"  name: platform"* && "${out}" != *"unbound variable"* ]]; then
    ok "the add-on's two requests are approved by the installer, the API extension pinned to the operator's digest"
else
    bad "the add-on's two requests are approved by the installer, the API extension pinned to the operator's digest" "${out}"$'\n'"${file}"
fi
if [[ "$(grep -c -- '- path: privileges.yaml' "${DIR}/kustomization.yaml")" == "1" ]]; then
    ok "the approvals are listed as a patch of the platform tenant"
else
    bad "the approvals are listed as a patch of the platform tenant" "$(cat "${DIR}/kustomization.yaml")"
fi
if [[ "$(grep -c 'digest:' "${DIR}/privileges.yaml")" == "1" ]]; then
    ok "only the API extension's approval names a digest"
else
    bad "only the API extension's approval names a digest" "${file}"
fi

before="$(cat "${DIR}/privileges.yaml")"
run "${CO}" -- "${APPROVE}" >/dev/null
if [[ "$(cat "${DIR}/privileges.yaml")" == "${before}" && "$(grep -c -- '- path: privileges.yaml' "${DIR}/kustomization.yaml")" == "1" ]]; then
    ok "a privileges file that exists is left alone"
else
    bad "a privileges file that exists is left alone" "$(cat "${DIR}/privileges.yaml")"
fi

for case in "not placed" "--disable-api-extensions" "--no-extra-services"; do
    new_checkout "${CO}" $([[ "${case}" == "not placed" ]] || echo placed)
    case "${case}" in
        "not placed") run "${CO}" -- "${APPROVE}" >/dev/null ;;
        "--disable-api-extensions") run "${CO}" GENTIAN_DISABLE_API_EXTENSIONS=1 -- "${APPROVE}" >/dev/null ;;
        "--no-extra-services")
            printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1-dev\nspec:\n  catalogue:\n    extraServices: false\n' \
                > "${CO}/clusters/${CLUSTER}/kernel/claims/cluster.yaml"
            run "${CO}" -- "${APPROVE}" >/dev/null ;;
    esac
    if [[ ! -e "${DIR}/privileges.yaml" ]] && ! grep -q privileges "${DIR}/kustomization.yaml"; then
        ok "${case}: nothing is approved"
    else
        bad "${case}: nothing is approved" "$(cat "${DIR}/privileges.yaml" 2>/dev/null)"
    fi
done

new_checkout "${CO}" placed
out="$(run "${CO}" -- '_initial_install_allowlist')"
if [[ "${out}" == *"platformSecurityPolicy:"* && "${out}" == *"- profile: backup-scheduler-ee"* && "${out}" == *"role: tenant-exports"* ]]; then
    ok "the cluster's values permit the add-on's role, for the director to start the allowlist from"
else
    bad "the cluster's values permit the add-on's role, for the director to start the allowlist from" "${out}"
fi

# Structure: only the run that wrote the Cluster claim approves.
body="$(awk '/^scaffold_cluster_deployment\(\)/,/^}/' scripts/lib/bootstrap.sh)"
if grep -q 'initial_install=1' <<< "$(grep -A2 'Scaffolded ${kernel_dir}/claims/cluster.yaml' <<< "${body}")" \
    && grep -q 'if (( initial_install )); then' <<< "${body}" \
    && [[ "$(grep -c '_initial_install_approvals "' <<< "${body}")" == "1" ]] \
    && [[ "$(grep -rc '_initial_install_approvals "' scripts/lib scripts/steps install.sh | awk -F: '{s+=$2} END {print s}')" == "1" ]]; then
    ok "the approvals are made only in the run that writes the Cluster claim"
else
    bad "the approvals are made only in the run that writes the Cluster claim" ""
fi

echo ""
if (( fail )); then
    printf '%s%d failed%s, %d passed.\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%s%d checks passed.%s\n' "${GREEN}" "${pass}" "${NC}"
