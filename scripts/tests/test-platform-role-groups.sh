#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-platform-role-groups.sh
# =============================================================================
# Two platform roles are held by a group of the kernel realm, admin and
# breakGlass; every other is held by name on the Cluster claim
# (spec.platformAccess.roleHolders). What the install writes and makes is
# held still here:
#
#   - a new Cluster claim names gentian:platform:admin for admin, and no group
#     for any other role;
#   - the kernel realm's bootstrap Job makes no security officers' group.
#
# Runs the library's own functions under set -u, with kubectl replaced by a
# stand-in. No cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home" "${SANDBOX}/bin"

is() { # <what> <output> <exactly>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

echo ""
echo "Platform roles: held by group, and held by name"
echo ""

# --- the claim step 0 writes --------------------------------------------------
CO="${SANDBOX}/co"
g init --bare "${SANDBOX}/origin.git"
g init "${CO}"
g -C "${CO}" remote add origin "${SANDBOX}/origin.git"
g -C "${CO}" commit --allow-empty -m seed
env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=k.example \
    GENTIAN_DEPLOYMENTS_PATH="${CO}" GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1 GENTIAN_OS_BRANCH=develop \
    bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; scaffold_cluster_deployment' \
    >"${SANDBOX}/scaffold.log" 2>&1
CLAIM="${CO}/clusters/c1/kernel/claims/cluster.yaml"
roles() { /usr/bin/python3 -c 'import sys, yaml; r = yaml.safe_load(open(sys.argv[1]))["spec"]["platformRoles"]; print(sorted(r))' "${CLAIM}" 2>/dev/null; }
if [[ -f "${CLAIM}" ]]; then
    is "a new claim names the administrators' group and no other" "$(roles)" "['admin']"
else
    bad "step 0 wrote no Cluster claim" "$(tail -n 20 "${SANDBOX}/scaffold.log")"
fi

# --- the bootstrap Job --------------------------------------------------------
# kubectl keeps what is applied from stdin, and answers yes to everything else.
cat > "${SANDBOX}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "apply" ]]; then cat >> "${KUBECTL_APPLIED}"; printf -- '---\n' >> "${KUBECTL_APPLIED}"; fi
exit 0
EOF
chmod +x "${SANDBOX}/bin/kubectl"
export KUBECTL_APPLIED="${SANDBOX}/applied.yaml"
: > "${KUBECTL_APPLIED}"
env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=k.example \
    KUBECTL_APPLIED="${KUBECTL_APPLIED}" \
    bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; source scripts/lib/portal-login-bootstrap.sh >/dev/null 2>&1; trap - ERR; set +e
        ensure_argocd_oidc_secret() { echo argocd-secret; }
        ensure_headlamp_oidc_secret() { echo headlamp-secret; }
        gentian_job_logs() { :; }
        run_keycloak_portal_bootstrap_job' >"${SANDBOX}/job.log" 2>&1
if ! grep -q 'kind: Job' "${KUBECTL_APPLIED}"; then
    bad "the bootstrap Job was not applied" "$(tail -n 20 "${SANDBOX}/job.log")"
elif grep -q 'platform:security\|SECURITY_GROUP' "${KUBECTL_APPLIED}"; then
    bad "the bootstrap Job still makes a security officers' group"
else
    ok "the bootstrap Job makes no security officers' group"
fi

finish
