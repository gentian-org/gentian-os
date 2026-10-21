#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-platform-role-groups.sh
# =============================================================================
# Every cluster has two platform roles held from the start, by two groups of
# the kernel realm: its administrators and its security officers. They are
# separate on purpose -- the one who installs is not the one who approves
# exceptions -- so what the install writes and makes is held still here:
#
#   - a new Cluster claim names gentian:platform:admin for admin and
#     gentian:platform:security for securityOfficer, two different groups;
#   - the kernel realm's bootstrap Job makes gentian:platform:security when it
#     is absent and leaves one that exists alone;
#   - it puts nobody in it, the administrator included, and grants it no role
#     of the realm.
#
# Runs the library's own functions under set -u, with kubectl and curl
# replaced by stand-ins. No cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home" "${SANDBOX}/bin"

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
is() { # <what> <output> <exactly>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

echo ""
echo "The platform's two standing roles: administrators and security officers"
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
roles() { /usr/bin/python3 -c 'import sys, yaml; r = yaml.safe_load(open(sys.argv[1]))["spec"]["platformRoles"]; print(r.get(sys.argv[2], ""))' "${CLAIM}" "$1" 2>/dev/null; }
if [[ -f "${CLAIM}" ]]; then
    is "a new claim names the administrators' group" "$(roles admin)" "gentian:platform:admin"
    is "and the security officers' group" "$(roles securityOfficer)" "gentian:platform:security"
else
    bad "step 0 wrote no Cluster claim" "$(tail -n 20 "${SANDBOX}/scaffold.log")"
fi

# --- the bootstrap Job --------------------------------------------------------
# kubectl keeps what is applied from stdin and the arguments a Secret is
# created with, and answers yes to everything else.
cat > "${SANDBOX}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "apply" ]]; then cat >> "${KUBECTL_APPLIED}"; printf -- '---\n' >> "${KUBECTL_APPLIED}"; fi
if [[ "$1" == "create" ]]; then printf '%s\n' "$@" >> "${KUBECTL_APPLIED}.args"; fi
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

# The Secret's value, and the Job's shell as Kubernetes would hand it over.
is "the Job is given the security officers' group" \
    "$(sed -n 's/^--from-literal=platform_security_group=//p' "${KUBECTL_APPLIED}.args" 2>/dev/null)" \
    "gentian:platform:security"
read_applied() {
    /usr/bin/python3 - "${KUBECTL_APPLIED}" "$1" <<'EOF'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if not doc:
        continue
    if sys.argv[2] == "script" and doc.get("kind") == "Job":
        print(doc["spec"]["template"]["spec"]["containers"][0]["command"][2])
EOF
}
read_applied script > "${SANDBOX}/job.sh"
# The part of the Job that is about this group: from its comment to the next
# blank line.
awk '/# The security officers. group: made if absent/{on=1} on && /^[[:space:]]*$/{exit} on' \
    "${SANDBOX}/job.sh" > "${SANDBOX}/security.sh"
if [[ ! -s "${SANDBOX}/security.sh" ]]; then
    bad "the Job makes no security officers' group" "$(tail -n 20 "${SANDBOX}/job.log")"
else
    # curl as Keycloak: the group search answers what KC_GROUPS holds, and
    # every request is recorded.
    cat > "${SANDBOX}/bin/curl" <<'EOF'
#!/usr/bin/env bash
method=GET url=""
while [[ $# -gt 0 ]]; do
    case "$1" in -X) method="$2"; shift ;; -H|-d) shift ;; -*) ;; *) url="$1" ;; esac
    shift
done
printf '%s %s\n' "${method}" "${url}" >> "${CURL_CALLS}"
[[ "${method}" == GET && "${url}" == *"/groups?search="* ]] && printf '%s' "${KC_GROUPS}"
exit 0
EOF
    chmod +x "${SANDBOX}/bin/curl"
    job() { # <groups the realm has, as JSON>
        : > "${SANDBOX}/calls"
        env -i PATH="${SANDBOX}/bin:${PATH}" CURL_CALLS="${SANDBOX}/calls" KC_GROUPS="$1" \
            AUTH="Authorization: Bearer t" KEYCLOAK_BASE=http://kc REALM=kernel \
            PLATFORM_SECURITY_GROUP=gentian:platform:security \
            sh -eu "${SANDBOX}/security.sh" >/dev/null 2>&1
        grep -v '^GET ' "${SANDBOX}/calls"
    }
    is "an absent group is made, and nothing else is written" \
        "$(job '[]')" "POST http://kc/admin/realms/kernel/groups"
    is "a group that exists is left alone" \
        "$(job '[{"id":"g1","name":"gentian:platform:security"}]')" ""
    if grep -Eq 'groups/\\?\$\{?SECURITY_ID|role-mappings.*SECURITY' "${SANDBOX}/job.sh"; then
        bad "the Job puts somebody in the security officers' group, or grants it a role"
    else
        ok "nobody is put in it and it is granted no role of the realm"
    fi
fi

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sall %d passed%s\n' "${GREEN}" "${pass}" "${NC}"
