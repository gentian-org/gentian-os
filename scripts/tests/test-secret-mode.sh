#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-secret-mode.sh
# =============================================================================
# secretMode decides how the installer makes a kernel credential: derived from
# the master password and the salt, or random. Two things have to hold whatever
# the mode, and neither is visible in a single run:
#
#   - a credential that exists is not made again. Derived values hide a
#     mistake here, because making one again gives the same value; a random
#     one does not, and the mistake is then a database whose password nothing
#     holds any more.
#   - derived values are exactly what they were. A cluster's databases and
#     clients were created with them, so the values are written down here as
#     text, worked out separately from the code under test.
#
# Held here:
#
#   - scripts/bootstrap/seed-openbao.sh, run for real against a stand-in for
#     the vault's KV API: derived gives the written-down values on every run;
#     random gives other values and the same ones on the second run, also for
#     gentian-os/kernel/llm, the one path that script rewrites on every run;
#     a vault that holds derived values keeps them when the mode becomes
#     random; and an llm path that cannot be read stops the run under random
#     instead of being filled with new keys.
#   - the client secrets the installer makes for Argo CD, Headlamp and the
#     model gateway's console: derived gives the written-down values; random
#     gives the secret the vault holds, or else the one the Kubernetes Secret
#     holds, and draws a new one only where neither has any.
#   - the kernel realm's mail login is the same value however often it is
#     asked for, in both modes.
#
# No cluster, no network beyond the loopback interface.
# =============================================================================
# Every check is a string handed to eval, so what it names is expanded there
# and the written-down values are used there.
# shellcheck disable=SC2016,SC2034
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

echo ""
echo "secretMode: a credential is made once, and derived values are what they were"
echo ""
for tool in python3 openssl curl jq; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
        echo "  ${YELLOW}skipped${NC}: ${tool} is not installed."
        exit 0
    fi
done

SB="$(mktemp -d "${TMPDIR:-/tmp}/gsm.XXXXXX")"
KV_PID=""
trap '[[ -n "${KV_PID}" ]] && kill "${KV_PID}" 2>/dev/null; rm -rf "${SB}"' EXIT

# A made-up master password and salt, and what HMAC-SHA256 gives for them --
# worked out apart from the installer, so a change to the derivation shows
# here as a difference and not as two copies agreeing.
MASTER='golden-master-password-1'
SALT='0123456789abcdef0123456789abcdef'
G_HEADLAMP='51fe6cc4e5470311e5e344d96a6146ac3eb5694cbacdadc03442ca9b54f4bd82'
G_ARGOCD='a8d07eb2af501d0577ac348dedbe8c789d29b034cda43e8228caca179c907112'
G_LITELLM_SSO='b5e970fbe8a988132684e51c9034b5b313dc72657a41a61d9de13991c9f878d8'
G_SMTP='fb099f558ee6cbf9d38ba74c3165ff5592d14bea930d08785e7578f1adea3646'
G_PG='a5e45ae804d214d8648eedaf543190eb694e527ecb3e3b9a92c370739dc6bdcd'
G_CNPG='0857fcaf0fe529928af5b4e09067a1801efbd1104803bd24b197e67fae5183f7'
G_LLM_MASTER='sk-2d46b757022c125f870c153f6b76fb74c858d2251b03cddeec47a435f1ce0daa'
G_LLM_DB='2f40aa6bab967a2933f237973a6acb2bf865b42abbd2164334bf3f7f1dd49a2b'
G_LLM_REDIS='8d2d2a6f65188b05868a89869b4e13d41009986f1c14d388b4c525f90e154828'
G_LLM_VLLM='c5f9f7c721693e191ccf846244813ddbbc0ab3e07cb7f53c3b73955a478c2594'
G_LLM_UI='027beca492940e1be343d2c5999e6dddf44d4dbdfa2b45916ae3425034b5de22'

# -----------------------------------------------------------------------------
# A stand-in for the vault: KV v2 reads and writes under /v1/secret/data/, kept
# in a file per store so a test can start from an empty one. A path listed in
# <store>.deaf answers 500, as a vault does that is there and cannot answer.
# -----------------------------------------------------------------------------
cat > "${SB}/kv.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

ROOT = sys.argv[1]
PREFIX = "/v1/secret/data/"


def store_file(headers):
    return os.path.join(ROOT, headers.get("X-Vault-Token", "none") + ".json")


def load(path):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, code, body):
        # Compact, as the vault writes it: the script under test looks for
        # '"data":{' to tell an existing path from an absent one.
        raw = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def deaf(self, key):
        try:
            with open(store_file(self.headers)[:-5] + ".deaf") as f:
                return key in f.read().split()
        except FileNotFoundError:
            return False

    def do_GET(self):
        if not self.path.startswith(PREFIX):
            return self.answer(404, {"errors": []})
        key = self.path[len(PREFIX):]
        if self.deaf(key):
            return self.answer(500, {"errors": ["internal error"]})
        data = load(store_file(self.headers))
        if key not in data:
            return self.answer(404, {"errors": []})
        self.answer(200, {"data": {"data": data[key]}})

    def do_POST(self):
        key = self.path[len(PREFIX):]
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        path = store_file(self.headers)
        data = load(path)
        data[key] = body["data"]
        with open(path, "w") as f:
            json.dump(data, f)
        self.answer(200, {"data": {"version": 1}})


server = HTTPServer(("127.0.0.1", 0), Handler)
with open(os.path.join(ROOT, "port"), "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY
mkdir -p "${SB}/kv"
python3 "${SB}/kv.py" "${SB}/kv" &
KV_PID=$!
for _ in $(seq 1 50); do [[ -s "${SB}/kv/port" ]] && break; sleep 0.1; done
if [[ ! -s "${SB}/kv/port" ]]; then
    bad "the stand-in vault did not start"
    exit 1
fi
BAO_ADDR="http://127.0.0.1:$(cat "${SB}/kv/port")"

# seed <store> <mode> — run the seeding script against one store.
seed() {
    env -i PATH="${PATH}" HOME="${SB}" BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="$1" SECRET_MODE="$2" \
        DERIVATION_SALT="${SALT}" KERNEL_DOMAIN=example.test MAIL_SERVICE_MODE=system \
        bash "${REPO}/scripts/bootstrap/seed-openbao.sh" "${MASTER}" > "${SB}/seed.log" 2>&1
}
# held <store> <path> <field> — what a store holds.
held() { jq -r --arg p "gentian-os/kernel/$2" --arg f "$3" '.[$p][$f] // empty' "${SB}/kv/$1.json" 2>/dev/null; }
# generated <store> — every generated value of a store, one per line, in a fixed order.
generated() {
    local f
    for f in database/cnpg:superuser_password database/portal-shell:password \
        database/postgresql:postgres_password database/postgresql:keycloak_user_password \
        database/mariadb:root_password cache/redis:auth_password storage/minio:root_password \
        identity/keycloak-bootstrap:admin_password authz/openfga:preshared_key \
        mail/dovecot:doveadm_password mail/dovecot:oidc_client_secret \
        llm:vllm_api_key llm:litellm_master_key llm:litellm_db_password \
        llm:litellm_redis_password llm:litellm_ui_password; do
        printf '%s=%s\n' "${f}" "$(held "$1" "${f%%:*}" "${f##*:}")"
    done
}

echo "  seed-openbao.sh"
if seed derived derived; then
    first="$(generated derived)"
    check "derived: the database passwords are the written-down values" \
        '[[ "$(held derived database/postgresql postgres_password)" == "${G_PG}" && "$(held derived database/cnpg superuser_password)" == "${G_CNPG}" ]]' "${first}"
    check "derived: the model gateway's keys are the written-down values" \
        '[[ "$(held derived llm litellm_master_key)" == "${G_LLM_MASTER}" && "$(held derived llm litellm_db_password)" == "${G_LLM_DB}" && "$(held derived llm litellm_redis_password)" == "${G_LLM_REDIS}" && "$(held derived llm vllm_api_key)" == "${G_LLM_VLLM}" && "$(held derived llm litellm_ui_password)" == "${G_LLM_UI}" ]]' "${first}"
    check "derived: nothing generated is empty" '! grep -q "=$" <<<"${first}"' "${first}"
    seed derived derived
    check "derived: a second run changes nothing" '[[ "$(generated derived)" == "${first}" ]]' "$(generated derived)"
    # The vault that holds derived values, with the mode changed under it.
    seed derived random
    check "derived, then random: every credential that existed is kept" \
        '[[ "$(generated derived)" == "${first}" ]]' "$(generated derived)"
else
    bad "seed-openbao.sh failed in derived mode" "$(tail -5 "${SB}/seed.log")"
fi

if seed random random; then
    first="$(generated random)"
    check "random: nothing generated is empty" '! grep -q "=$" <<<"${first}"' "${first}"
    check "random: no value is the derived one" \
        '! grep -qF -e "${G_PG}" -e "${G_CNPG}" -e "${G_LLM_DB}" -e "${G_LLM_REDIS}" -e "${G_LLM_VLLM}" -e "${G_LLM_UI}" -e "${G_LLM_MASTER#sk-}" <<<"${first}"' "${first}"
    check "random: the model gateway's master key carries its prefix" \
        '[[ "$(held random llm litellm_master_key)" == sk-* ]]'
    check "random: no two values are the same" \
        '[[ -z "$(cut -d= -f2 <<<"${first}" | sort | uniq -d)" ]]' "${first}"
    seed random random
    check "random: a second run changes nothing, the model gateway's keys included" \
        '[[ "$(generated random)" == "${first}" ]]' "$(diff <(echo "${first}") <(generated random))"
    check "random: the addresses beside the keys still follow the settings" \
        '[[ "$(held random llm litellm_proxy_base_url)" == "https://llm.example.test" ]]'
    seed other random
    check "random: another cluster with the same master password holds other values" \
        '[[ "$(held other llm litellm_db_password)" != "$(held random llm litellm_db_password)" && "$(held other database/cnpg superuser_password)" != "$(held random database/cnpg superuser_password)" ]]'
    # A vault that is there and cannot answer for the path.
    echo "gentian-os/kernel/llm" > "${SB}/kv/random.deaf"
    if seed random random; then
        bad "random: a model gateway path that cannot be read stops the run"
    else
        ok "random: a model gateway path that cannot be read stops the run"
    fi
    rm -f "${SB}/kv/random.deaf"
    check "random: and its keys are the ones it held" '[[ "$(generated random)" == "${first}" ]]'
else
    bad "seed-openbao.sh failed in random mode" "$(tail -5 "${SB}/seed.log")"
fi

# -----------------------------------------------------------------------------
# The installer's own library, with a vault and a cluster that answer from
# files: VAULT/<field> is what identity/portal-admin holds, K8S/<secret>.<key>
# what a Kubernetes Secret holds. An empty VAULT_UP is a shell with no token.
# -----------------------------------------------------------------------------
lib() {
    local mode="$1" script="$2"
    # shellcheck disable=SC2016 # expanded by the child shell
    env -i HOME="${SB}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=example.test \
        SECRET_MODE="${mode}" MASTER_PASSWORD="${MASTER}" DERIVATION_SALT="${SALT}" \
        VAULT="${SB}/vault" K8S="${SB}/k8s" VAULT_UP="${VAULT_UP:-1}" \
        OBSERVABILITY_NAMESPACE=obs EDGE_NAMESPACE=edge \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1
            source scripts/lib/portal-login-bootstrap.sh >/dev/null 2>&1
            ns_system() { echo "system-$1"; }
            bao() {
                [[ -n "${VAULT_UP}" ]] || return 2
                local field="" arg
                case "$1 $2" in
                    "kv get")
                        for arg in "$@"; do [[ "${arg}" == -field=* ]] && field="${arg#-field=}"; done
                        [[ -f "${VAULT}/${field}" ]] || return 2
                        cat "${VAULT}/${field}" ;;
                    "kv patch"|"kv put")
                        for arg in "$@"; do
                            [[ "${arg}" == *=* && "${arg}" != -* ]] && printf "%s" "${arg#*=}" > "${VAULT}/${arg%%=*}"
                        done ;;
                    *) return 1 ;;
                esac
            }
            kubectl() {
                [[ "$1 $2" == "get secret" ]] || return 1
                local key="${7#jsonpath=\{.data.}"
                key="${key%\}}"
                [[ -f "${K8S}/$3.${key}" ]] || return 1
                base64 -w0 < "${K8S}/$3.${key}"
            }
            '"${script}" 2>&1
}
fresh() { rm -rf "${SB}/vault" "${SB}/k8s"; mkdir -p "${SB}/vault" "${SB}/k8s" "${SB}/home"; }

echo ""
echo "  the installer's client secrets and the kernel realm's mail login"
fresh
check "derived: the three client secrets are the written-down values" \
    '[[ "$(lib derived "for f in _headlamp_derive_secret _argocd_oidc_derive_secret _litellm_sso_derive_secret; do echo \"\$(\$f)\"; done")" == "${G_HEADLAMP}"$'"'"'\n'"'"'"${G_ARGOCD}"$'"'"'\n'"'"'"${G_LITELLM_SSO}" ]]' \
    "$(lib derived "for f in _headlamp_derive_secret _argocd_oidc_derive_secret _litellm_sso_derive_secret; do echo \"\$(\$f)\"; done")"
check "derived: nothing is written to the vault for them" '[[ -z "$(ls "${SB}/vault")" ]]'
check "the kernel realm's mail login is the written-down value in both modes, however often asked" \
    '[[ "$(lib derived "_derived smtp password; _derived smtp password")" == "${G_SMTP}"$'"'"'\n'"'"'"${G_SMTP}" && "$(lib random "_derived smtp password; _derived smtp password")" == "${G_SMTP}"$'"'"'\n'"'"'"${G_SMTP}" ]]'
check "derived: _derive gives the written-down value" '[[ "$(lib derived "_derive postgres postgres_user")" == "${G_PG}" ]]'
check "random: _derive gives a value of its own on each call" \
    '[[ "$(lib random "_derive postgres postgres_user")" != "${G_PG}" && "$(lib random "_derive a b")" != "$(lib random "_derive a b")" ]]'

fresh
a="$(lib random "_headlamp_derive_secret")"
b="$(lib random "_headlamp_derive_secret")"
check "random: a client secret is drawn once and then read from the vault" \
    '[[ -n "${a}" && "${a}" == "${b}" && "${a}" != "${G_HEADLAMP}" && "$(cat "${SB}/vault/headlamp_client_secret")" == "${a}" ]]' "${a} / ${b}"
c="$(lib random "_argocd_oidc_derive_secret")"
check "random: each client has a secret of its own" '[[ -n "${c}" && "${c}" != "${a}" ]]'

# A shell with no vault token, which is every run resumed after the seeding
# step: the secret the reader already mounts is the secret.
fresh
printf 'held-by-the-cluster' > "${SB}/k8s/headlamp-oidc.OIDC_CLIENT_SECRET"
printf 'argocd-held' > "${SB}/k8s/gentian-argocd.client_secret"
printf 'console-held' > "${SB}/k8s/litellm-dashboard-sso.client_secret"
check "random, no vault token: the secret the Kubernetes Secret holds is the answer, each time" \
    '[[ "$(VAULT_UP="" lib random "for f in _headlamp_derive_secret _headlamp_derive_secret _argocd_oidc_derive_secret _litellm_sso_derive_secret; do echo \"\$(\$f)\"; done")" == "held-by-the-cluster"$'"'"'\n'"'"'"held-by-the-cluster"$'"'"'\n'"'"'"argocd-held"$'"'"'\n'"'"'"console-held" ]]' \
    "$(VAULT_UP="" lib random "for f in _headlamp_derive_secret _headlamp_derive_secret _argocd_oidc_derive_secret _litellm_sso_derive_secret; do echo \"\$(\$f)\"; done")"
check "random, vault back: that secret is the one written to it" \
    '[[ "$(lib random "_headlamp_derive_secret")" == "held-by-the-cluster" && "$(cat "${SB}/vault/headlamp_client_secret")" == "held-by-the-cluster" ]]'
printf 'in-the-vault' > "${SB}/vault/headlamp_client_secret"
check "random: where the vault and the cluster both hold one, the vault's stands" \
    '[[ "$(lib random "_headlamp_derive_secret")" == "in-the-vault" ]]'

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}All ${pass} checks passed.${NC}"
