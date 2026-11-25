#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-platform-role-groups.sh
# =============================================================================
# Every cluster role is held by name on the Cluster claim
# (spec.platformAccess.roleHolders); none is held by a group of the kernel
# realm. What the install writes and makes is held still here:
#
#   - a new Cluster claim names no group for any role;
#   - the kernel realm's bootstrap Job makes no group and puts nobody in one.
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
roles() { /usr/bin/python3 -c 'import sys, yaml; print(sorted(yaml.safe_load(open(sys.argv[1]))["spec"].get("platformRoles") or []))' "${CLAIM}" 2>/dev/null; }
if [[ -f "${CLAIM}" ]]; then
    is "a new claim names no group for a role" "$(roles)" "[]"
else
    bad "step 0 wrote no Cluster claim" "$(tail -n 20 "${SANDBOX}/scaffold.log")"
fi

# --- a claim written before ---------------------------------------------------
# A claim that names a group for a role loses the section and its comment, and
# keeps everything around it; a second pass changes nothing.
OLD="${SANDBOX}/old-claim.yaml"
cat > "${OLD}" <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: Cluster
spec:
  # The cluster's own id.
  clusterUID: abc
  # Who administers this cluster, by the Keycloak group they are in.
  platformRoles:
    admin: gentian:platform:admin
    breakGlass: gentian:platform:break-glass

  # Where this cluster runs.
  platform: self-hosted
EOF
# shellcheck disable=SC2016 # $1 is the inner shell's, given after the script.
env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=k.example \
    bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
        ensure_claim_names_no_role_group "$1"; ensure_claim_names_no_role_group "$1"' _ "${OLD}" >"${SANDBOX}/old.log" 2>&1
is "an earlier claim loses the role groups and nothing else" "$(cat "${OLD}")" "apiVersion: gentianos.io/v1alpha1
kind: Cluster
spec:
  # The cluster's own id.
  clusterUID: abc

  # Where this cluster runs.
  platform: self-hosted"

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
elif grep -q 'gentian:platform:\|SECURITY_GROUP\|ADMIN_GROUP\|/groups' "${KUBECTL_APPLIED}"; then
    bad "the bootstrap Job still makes or fills a group"
else
    ok "the bootstrap Job makes no group"
fi

finish
