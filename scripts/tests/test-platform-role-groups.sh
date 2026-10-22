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
#   - a claim that names its security officers the earlier way
#     (platformAccess.securityOfficers) is rewritten as role holders;
#   - a claim that names a group for a role now held by name, and a tenant
#     that let its administrators approve (spec.perimeter.adminsApprove), are
#     carried over to role holders from Keycloak's members, and whoever
#     cannot be is named in a warning;
#   - the kernel realm's bootstrap Job makes no security officers' group.
#
# Runs the library's own functions under set -u, with kubectl replaced by a
# stand-in and Keycloak by a local fake. No cluster.
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

# --- a claim that names its security officers the earlier way ---------------
# securityOfficers and brokeredSecondFactor are not in the schema; step 0
# rewrites the list as role holders with role securityOfficer.
if [[ -f "${CLAIM}" ]]; then
    cat >> "${CLAIM}" <<'EOF'
  platformAccess:
    brokeredSecondFactor: required
    securityOfficers:
      - tenant: test
        subject: 7d0f3c2e
        username: "admin@test.example"
EOF
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=k.example \
        GENTIAN_DEPLOYMENTS_PATH="${CO}" GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1 GENTIAN_OS_BRANCH=develop \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; scaffold_cluster_deployment' \
        >"${SANDBOX}/scaffold2.log" 2>&1
    holders="$(/usr/bin/python3 -c 'import sys, yaml; a = yaml.safe_load(open(sys.argv[1]))["spec"]["platformAccess"]; print(sorted(a), a["roleHolders"])' "${CLAIM}" 2>&1)"
    is "the security officers become role holders" "${holders}" "['roleHolders'] [{'role': 'securityOfficer', 'tenant': 'test', 'subject': '7d0f3c2e', 'username': 'admin@test.example'}]"
fi

# --- the carry-over: groups and the tenant's switch, as role holders ---------
# A fake Keycloak: the master realm's token, and per realm the groups and
# their members below.
cat > "${SANDBOX}/kc.py" <<'PY'
import json, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
DATA = {
    ("kernel", "gentian:platform:security"): [{"id": "s-1", "username": "sam@k.example", "email": "sam@k.example"}],
    ("kernel", "gentian:tenant:platform:perimeter"): [{"id": "p-1", "username": "petra@k.example"}],
    ("acme", "gentian:tenant:acme:admins"): [{"id": "a-1", "username": "ann@acme.example"}, {"id": "a-2", "username": "al@acme.example"}],
    ("acme", "gentian:tenant:acme:perimeter"): [{"id": "a-1", "username": "ann@acme.example"}, {"id": "m-1", "username": "mo@acme.example"}],
    ("solo", "gentian:tenant:solo:admins"): [{"id": "o-1", "username": "olga@solo.example"}],
}
class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def reply(self, body):
        b = json.dumps(body).encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b)
    def do_POST(self):
        self.reply({"access_token": "t"})
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        parts = u.path.split("/")
        realm = parts[4]
        if parts[5:] == ["groups"]:
            name = urllib.parse.parse_qs(u.query)["search"][0]
            self.reply([{"id": realm + "|" + name, "name": name}] if (realm, name) in DATA else [])
        else:
            self.reply(DATA[tuple(urllib.parse.unquote(parts[6]).split("|", 1))])
s = HTTPServer(("127.0.0.1", 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
CC="${SANDBOX}/carry"
mkdir -p "${CC}/tenants/platform" "${CC}/tenants/acme" "${CC}/tenants/solo"
cat > "${CC}/claim.yaml" <<'EOF'
spec:
  # The security officers approve what escapes the default posture: a group
  # of their own, never the administrators', made empty by the install.
  platformRoles:
    admin: gentian:platform:admin
    securityOfficer: gentian:platform:security
  kernelDomain: k.example
EOF
printf 'spec:\n  isolation:\n    keycloakRealm: kernel\n' > "${CC}/tenants/platform/tenant.yaml"
printf 'spec:\n  displayName: Acme\n  # Whether its admins approve.\n  perimeter:\n    adminsApprove: true\n  apps: []\n' > "${CC}/tenants/acme/tenant.yaml"
printf 'spec:\n  displayName: Solo\n' > "${CC}/tenants/solo/tenant.yaml"
carry() { /usr/bin/python3 scripts/lib/carry-over-role-holders.py "$@"; }
coproc KC { exec /usr/bin/python3 "${SANDBOX}/kc.py"; }
read -r port <&"${KC[0]}"
out="$(KC_BASE="http://127.0.0.1:${port}/auth" KC_USER=u KC_PASS=p carry apply "${CC}/claim.yaml" "${CC}/tenants" 2>&1)"
kill "${KC_PID}" 2>/dev/null
got="$(/usr/bin/python3 -c 'import sys, yaml; s = yaml.safe_load(open(sys.argv[1]))["spec"]; print(s["platformRoles"]); [print(h["role"], h["scope"], h["tenant"], h["subject"], h["reason"]) for h in s["platformAccess"]["roleHolders"]]' "${CC}/claim.yaml" 2>&1)"
is "the group's members and the tenant's administrators become role holders" "${got}" "{'admin': 'gentian:platform:admin'}
securityOfficer cluster platform s-1 carried over at upgrade
perimeterApprover tenant:acme acme a-1 carried over at upgrade
perimeterApprover tenant:acme acme a-2 carried over at upgrade"
is "the tenant's switch goes, with its comment" "$(cat "${CC}/tenants/acme/tenant.yaml")" "spec:
  displayName: Acme
  apps: []"
is "the claim's old comment goes" "$(grep -c 'security officers' "${CC}/claim.yaml")" "0"
is "a perimeter member who administers nothing is named, not dropped" \
    "$(grep -c '^warn: mo@acme.example approved public addresses of tenant acme' <<<"${out}")" "1"
is "and so is the platform tenant's" \
    "$(grep -c '^warn: petra@k.example approved public addresses of tenant platform' <<<"${out}")" "1"
if carry check "${CC}/claim.yaml" "${CC}/tenants"; then
    bad "a carried-over cluster is carried over again"
else
    ok "once carried over, there is nothing left to carry"
fi
# A cluster with no Keycloak yet: nobody is in any group, the fields go.
printf 'spec:\n  platformRoles:\n    admin: gentian:platform:admin\n    auditor: gentian:platform:auditor\n' > "${CC}/claim.yaml"
KC_BASE="" carry apply "${CC}/claim.yaml" "${CC}/tenants" >/dev/null 2>&1
is "without Keycloak the retired fields go and nobody is named" "$(cat "${CC}/claim.yaml")" "spec:
  platformRoles:
    admin: gentian:platform:admin"
# A Keycloak that does not answer changes nothing.
printf 'spec:\n  platformRoles:\n    auditor: gentian:platform:auditor\n' > "${CC}/claim.yaml"
before="$(cat "${CC}/claim.yaml")"
KC_BASE="http://127.0.0.1:9/auth" carry apply "${CC}/claim.yaml" "${CC}/tenants" >/dev/null 2>&1
rc=$?
is "a Keycloak that does not answer changes nothing" "${rc} $(cat "${CC}/claim.yaml")" "2 ${before}"

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

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sall %d passed%s\n' "${GREEN}" "${pass}" "${NC}"
