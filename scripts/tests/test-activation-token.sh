#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-activation-token.sh
# =============================================================================
# issue_admin_activation hands an administrator's account to its holder. It
# talks to Keycloak with a token of the master realm, which lives for 60
# seconds, and it stops to ask a person for an address. A token drawn before
# the question was refused after it: the user tenant's administrator got
# neither the mail nor the link, and the reason given was a guess.
#
# Held here, against a stand-in curl that plays Keycloak and counts tokens:
#
#   - a call refused with 401 is said to be the token having run out, a new
#     token is drawn, and the call is made again -- for the link and for the
#     mail;
#   - when nothing can be handed over, what to run instead is printed, with
#     kubectl gentian login before a tenant's command and the address that was
#     given carried along;
#   - a failure that is not a 401 is reported with what Keycloak answered and
#     is not retried.
#
# No cluster and no network.
# =============================================================================
# Every check is a string handed to eval, so what it names is expanded there.
# shellcheck disable=SC2016,SC2034
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

echo ""
echo "activation: a token that ran out is said so, replaced, and the call made again"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gat.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"

# kubectl hands over the admin Secret's two fields.
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    *"get secret keycloak-admin"*) printf 'eA==' ;;
esac
exit 0
STUB

# curl plays Keycloak. Every token it issues is numbered; a call listed in
# REFUSE_ONCE is answered 401 the first time it is made and served after
# that, and one listed in FAIL_ALWAYS is answered 500 every time. The answer
# ends in "\n<status>" when -w asks for the status after a body, and is the
# status alone when the body goes to /dev/null.
cat >"${SB}/bin/curl" <<'STUB'
#!/usr/bin/env bash
url=""; want_status=0; to_null=0; method=GET
while [[ $# -gt 0 ]]; do
    case "$1" in
        -w) want_status=1; shift ;;
        -o) [[ "$2" == /dev/null ]] && to_null=1; shift ;;
        -X) method="$2"; shift ;;
        -H|-d|--data-urlencode|--max-time) shift ;;
        http*) url="$1" ;;
    esac
    shift
done
answer() {   # answer <status> <body>
    if (( to_null )); then printf '%s' "$1"
    elif (( want_status )); then printf '%s\n%s' "$2" "$1"
    else printf '%s' "$2"; fi
}
what=""
case "${method} ${url}" in
    *"/protocol/openid-connect/token") n=$(( $(cat "${STATE}/tokens" 2>/dev/null || echo 0) + 1 )); echo "${n}" > "${STATE}/tokens"
        printf '{"access_token":"t%s"}' "${n}"; exit 0 ;;
    "GET "*"/users?username="*)        printf '[{"id":"u1"}]'; exit 0 ;;
    "GET "*"/users/u1/credentials")    printf '[]'; exit 0 ;;
    "GET "*"/clients?clientId="*)      printf '[{"rootUrl":"https://desktop.example.org"}]'; exit 0 ;;
    "GET "*"/users/u1")                printf '{"id":"u1"}'; exit 0 ;;
    "GET "*"/admin/realms/user")       printf '{"smtpServer":{"host":"%s","from":"%s"}}' "${SMTP_HOST:-}" "${SMTP_HOST:+a@b}"; exit 0 ;;
    "PUT "*"/execute-actions-email"*)  what=mail ;;
    "PUT "*"/users/u1")                what=save ;;
    "POST "*"/gentian-activation/"*)   what=link ;;
esac
echo "${what}" >> "${STATE}/calls"
if [[ " ${FAIL_ALWAYS:-} " == *" ${what} "* ]]; then answer 500 '{"errorMessage":"the mail server refused the message"}'; exit 0; fi
if [[ " ${REFUSE_ONCE:-} " == *" ${what} "* && ! -f "${STATE}/refused-${what}" ]]; then
    : > "${STATE}/refused-${what}"; answer 401 ''; exit 0
fi
case "${what}" in
    link) answer 200 '{"link":"https://id.example.org/activate/abc","expiresAt":""}' ;;
    *)    answer 204 '' ;;
esac
STUB
chmod +x "${SB}/bin/kubectl" "${SB}/bin/curl"

# run <recovery-email> [VAR=value ...] — the user tenant's administrator, the
# way E-04 hands the account over. Prints what was printed, the number of
# tokens drawn and the calls made.
run() {
    local email="$1"; shift
    STATE="$(mktemp -d "${SB}/state.XXXXXX")"
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" STATE="${STATE}" EMAIL="${email}" \
        GENTIAN_NONINTERACTIVE=1 "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1
source scripts/lib/portal-login-bootstrap.sh; trap - ERR; set +e
gentian_service_addr() { echo "http://keycloak.test"; }
issue_admin_activation user user-admin@example.org false "${EMAIL}" "kubectl gentian tenants activate-admin user"
echo "rc=$?"' 2>&1
    echo "tokens=$(cat "${STATE}/tokens")"
    echo "calls=$(tr '\n' ' ' < "${STATE}/calls")"
}

out="$(run "" REFUSE_ONCE=link)"
check "the link refused with 401: said to be the token, loudly" \
    'grep -q "KEYCLOAK REFUSED THE INSTALLER.S TOKEN (HTTP 401) while asking for the link" <<<"${out}"' "${out}"
check "a new token is drawn and the link asked for again, and handed over" \
    'grep -q "tokens=2" <<<"${out}" && grep -q "calls=link link " <<<"${out}" && grep -q "https://id.example.org/activate/abc" <<<"${out}" && grep -q "rc=0" <<<"${out}"' "${out}"

out="$(run owner@example.org SMTP_HOST=postfix REFUSE_ONCE="save mail")"
check "the mail refused with 401 at either call: a new token each time, and the mail goes out" \
    'grep -q "tokens=3" <<<"${out}" && grep -q "calls=save save mail mail " <<<"${out}" && grep -q "Activation link mailed to owner@example.org" <<<"${out}" && grep -q "rc=0" <<<"${out}"' "${out}"

out="$(run owner@example.org SMTP_HOST=postfix FAIL_ALWAYS=mail)"
check "the mail fails for another reason: what Keycloak answered is said, and it is not retried" \
    'grep -q "HTTP 500 the mail server refused the message" <<<"${out}" && grep -q "calls=save mail link " <<<"${out}" && ! grep -q "REFUSED THE INSTALLER" <<<"${out}"' "${out}"
check "and the link is shown here instead" 'grep -q "https://id.example.org/activate/abc" <<<"${out}" && grep -q "rc=0" <<<"${out}"' "${out}"

out="$(run owner@example.org SMTP_HOST=postfix FAIL_ALWAYS="mail link")"
check "nothing can be handed over: said so, and the install is told it failed" \
    'grep -q "NO ACTIVATION LINK WAS HANDED OVER for user-admin@example.org" <<<"${out}" && grep -q "rc=1" <<<"${out}"' "${out}"
check "what to run instead: sign in first, then the command with the address that was given" \
    'grep -q "kubectl gentian login" <<<"${out}" && grep -q "kubectl gentian tenants activate-admin user --recovery-email owner@example.org" <<<"${out}"' "${out}"

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} checks passed.${NC}"
