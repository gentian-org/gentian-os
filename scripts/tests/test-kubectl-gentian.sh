#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-kubectl-gentian.sh
# =============================================================================
# kubectl-gentian's commands for a tenant's own domain and for what a tenant
# has on the internet. Both change where something answers or whether it
# answers at all, and both are one request to the director, so what is held
# here is the request and what comes before it:
#
#   - the method, the path and the body sent are the director's route, and
#     nothing typed can turn them into another one;
#   - binding or removing a domain says what moves and what has to be true
#     first, and sends nothing until it is confirmed -- typed at a terminal,
#     or --yes; without a terminal and without --yes nothing is asked at all;
#   - a refusal is the director's own words and a non-zero exit;
#   - `exposures requests` lists what a tenant's apps ask to have on the
#     internet, as the director reads it;
#   - `exposures approve` reads the entry from the director and shows its
#     address, its paths and who can reach it before it asks; sends nothing
#     for an entry the director does not list; and approves an entry for the
#     cluster's main address only after printing the director's rule in full
#     and only with --acknowledge-main-address-rule, which --yes does not say;
#   - `exposures publish` is not a command and sends nothing.
#
# The plugin itself runs, against a kubectl and a curl that stand in for the
# cluster and the director and write down every request. No cluster, no
# network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/bin" "${SANDBOX}/home/.gentian" "${SANDBOX}/replies"
CALLS="${SANDBOX}/calls"

# Signed in already: a token that outlives the test.
printf '{"access_token":"t","refresh_token":"r","expires_at":%s,"refresh_expires_at":%s}\n' \
    "$(( $(date +%s) + 3600 ))" "$(( $(date +%s) + 3600 ))" > "${SANDBOX}/home/.gentian/cli-token-c1.json"

# kubectl: one director, in gentian-system, serving cluster c1; a port-forward
# that forwards nothing and waits to be ended.
cat > "${SANDBOX}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    "config current-context") echo test ;;
    "get deploy -A -l app.kubernetes.io/component=director"*) echo "gentian-system director" ;;
    "get deploy director -n gentian-system -o json")
        echo '{"spec":{"template":{"spec":{"containers":[{"env":[
            {"name":"DIRECTOR_ISSUER_BASE_URL","value":"https://id.k.example"},
            {"name":"GENTIAN_DEPLOYMENTS_CLUSTER_ID","value":"c1"}]}]}}}}' ;;
    port-forward*) exec sleep 60 ;;
    *) echo "kubectl stub: unexpected: $*" >&2; exit 1 ;;
esac
STUB

# curl: the director. Every request but the health probe is written to
# ${CALLS} as "<METHOD> <path> <body>", and answered from the file the test
# laid down for that method and path -- its first line the status, the rest
# the body -- or with 404.
cat > "${SANDBOX}/bin/curl" <<'STUB'
#!/usr/bin/env bash
out="" method="GET" body="" url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        -X) method="$2"; shift 2 ;;
        -d) body="$2"; shift 2 ;;
        -w|-H|--max-time) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
path="/${url#http://*/}"
[[ "${path}" == "/healthz" ]] && exit 0
printf '%s %s %s\n' "${method}" "${path}" "${body}" >> "${STUB_CALLS}"
reply="${STUB_REPLIES}/${method}$(printf '%s' "${path}" | tr '/' '_')"
if [[ -f "${reply}" ]]; then
    tail -n +2 "${reply}" > "${out}"
    head -n 1 "${reply}"
else
    echo '{"error":"the test laid down no answer for this request"}' > "${out}"
    echo 404
fi
STUB
chmod +x "${SANDBOX}/bin/kubectl" "${SANDBOX}/bin/curl"

# reply <METHOD> <path> <status> <body> : what the director answers.
reply() {
    printf '%s\n%s\n' "$3" "$4" > "${SANDBOX}/replies/$1$(printf '%s' "$2" | tr '/' '_')"
}
fresh() { rm -f "${SANDBOX}/replies/"*; : > "${CALLS}"; }

# gentian <args...> : the plugin, with no terminal. Output in OUT, exit in RC.
OUT=""; RC=0
gentian() {
    OUT="$(env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" \
        STUB_CALLS="${CALLS}" STUB_REPLIES="${SANDBOX}/replies" \
        bash "${REPO}/scripts/kubectl-gentian" "$@" 2>&1 </dev/null)"
    RC=$?
}

# gentian_typing <answer> <args...> : the same at a terminal, with <answer>
# typed at the first prompt.
gentian_typing() {
    local typed="$1"; shift
    OUT="$(env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" \
        STUB_CALLS="${CALLS}" STUB_REPLIES="${SANDBOX}/replies" TYPED="${typed}" \
        python3 -c '
import os, pty, sys
pid, fd = pty.fork()
if pid == 0:
    os.execvp("bash", ["bash"] + sys.argv[1:])
os.write(fd, (os.environ["TYPED"] + "\n").encode())
seen = b""
while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    seen += chunk
sys.stdout.write(seen.decode(errors="replace"))
sys.exit(os.waitstatus_to_exitcode(os.waitpid(pid, 0)[1]))
' "${REPO}/scripts/kubectl-gentian" "$@" 2>&1)"
    RC=$?
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
is() { # <what> <got> <exactly>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}
has() { # <what> <text> <part>
    if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1" "    missing: $3
    in: $2"; fi
}
refused() { # <what> : the last command ended non-zero
    if [[ "${RC}" -ne 0 ]]; then ok "$1"; else bad "$1" "    it exited 0: ${OUT}"; fi
}
# What was asked of the director that changes something: every request but a read.
writes() { grep -v '^GET ' "${CALLS}" || true; }

TENANTS='{"cluster":"c1","tenants":[{"name":"aluvian","apps":[],"protected":false},{"name":"acme","apps":[],"protected":false,"customDomain":"acme.example"}]}'
DOMAIN_ROUTE="/v1/clusters/c1/tenants/aluvian/domain"

echo ""
echo "kubectl gentian tenants domain"
echo ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain
refused "no tenant named is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain acme
has "a name alone shows the domain the tenant is bound to" "${OUT}" "bound to the domain acme.example"
is "... and only reads" "$(cat "${CALLS}")" "GET /v1/clusters/c1/tenants "
gentian tenants domain aluvian
has "a tenant with none is said to have none" "${OUT}" "bound to no domain of its own"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain nobody example.org --yes
refused "a tenant git does not declare is refused"
has "... by name" "${OUT}" "no tenant named nobody"
is "... and nothing is sent" "$(writes)" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain aluvian aluvian.io
refused "binding without a terminal and without --yes is refused"
has "... and says how to confirm" "${OUT}" "not a terminal: confirm with --yes"
is "... before the director is asked anything" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants domain aluvian aluvian.io --yes
is "binding with --yes succeeds" "${RC}" "0"
is "... as one PUT of the domain to the director's route" "$(writes)" "PUT ${DOMAIN_ROUTE} {
  \"domain\": \"aluvian.io\"
}"
has "... after saying where the desktop moves" "${OUT}" "desktop.aluvian.io"
has "... and the admin console" "${OUT}" "admin.aluvian.io"
has "... that the wildcard has to resolve to the cluster" "${OUT}" "*.aluvian.io resolves to this cluster"
has "... that the issuer has to answer for the zone" "${OUT}" "_acme-challenge"
has "... and that nobody checks either" "${OUT}" "The director checks none of it, and neither does this command"
has "... and it names the commit" "${OUT}" "committed (01234567)"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants domain aluvian Aluvian.IO --yes
has "a domain typed in capitals is shown in the spelling that is recorded" "${OUT}" "desktop.aluvian.io"
has "... and sent in it" "$(writes)" '"domain": "aluvian.io"'

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian_typing "aluvian.io" tenants domain aluvian aluvian.io
is "at a terminal, the domain typed out confirms" "${RC}" "0"
has "... and the request is sent" "$(writes)" "PUT ${DOMAIN_ROUTE}"
: > "${CALLS}"
gentian_typing "yes" tenants domain aluvian aluvian.io
refused "anything else typed does not"
has "... and says nothing was changed" "${OUT}" "not confirmed; nothing was changed"
is "... and nothing is sent" "$(writes)" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 422 '{"error":"invalid custom domain: t.k.example is on the kernel domain, where every tenant already is"}'
gentian tenants domain aluvian t.k.example --yes
refused "a domain the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 422: invalid custom domain: t.k.example is on the kernel domain, where every tenant already is"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT /v1/clusters/c1/tenants/acme/domain 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants domain acme acme.example --yes
has "the domain a tenant already has changes nothing" "${OUT}" "already bound to acme.example"
is "... and nothing is sent" "$(writes)" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain acme other.example --remove --yes
refused "--remove with a domain is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain acme --remove
refused "removing without a terminal and without --yes is refused"
is "... before the director is asked anything" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply DELETE /v1/clusters/c1/tenants/acme/domain 202 '{"status":"updated","commit":"fedcba9876543210"}'
gentian tenants domain acme --remove --yes
is "removing with --yes succeeds" "${RC}" "0"
is "... as one DELETE with no body" "$(writes)" "DELETE /v1/clusters/c1/tenants/acme/domain "
has "... after saying nothing answers at the domain any more" "${OUT}" "nothing of it answers at acme.example any more"
: > "${CALLS}"
gentian_typing "acme.example" tenants domain acme --remove
refused "at a terminal, removing is confirmed with the tenant's name and nothing else"
is "... and nothing is sent" "$(writes)" ""
gentian_typing "acme" tenants domain acme --remove
is "... and the name typed out confirms" "$(writes)" "DELETE /v1/clusters/c1/tenants/acme/domain "

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain aluvian --remove --yes
has "removing from a tenant with no domain changes nothing" "${OUT}" "nothing was changed"
is "... and nothing is sent" "$(writes)" ""

fresh
gentian tenants domain 'aluvian/../x' example.org --yes
refused "a tenant name that is not a name is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

echo ""
echo "kubectl gentian exposures"
echo ""

REGISTRY='{"tenant":"aluvian","live":[
  {"install":"aluvian-store","exposureName":"api","owner":"u-1","reviewAt":"2027-10-09T10:00:00Z","publishedAt":"2026-10-09T10:00:00Z","reason":"the store other clusters read"},
  {"install":"website","exposureName":"site","owner":"u-2","reviewAt":"2026-01-01T00:00:00Z","publishedAt":"2025-01-01T00:00:00Z","apex":true}],
 "expired":[{"install":"cloud","exposureName":"share","owner":"u-3","reviewAt":"2026-03-01T00:00:00Z","expiresAt":"2026-03-01T00:00:00Z"}],
 "reviewDue":[{"install":"website","exposureName":"site","owner":"u-2","reviewAt":"2026-01-01T00:00:00Z"}]}'

fresh
gentian exposures list
refused "list without --tenant is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/tenants/aluvian/exposures 200 "${REGISTRY}"
gentian exposures list --tenant aluvian
is "list reads the tenant's registry and nothing else" "$(cat "${CALLS}")" "GET /v1/tenants/aluvian/exposures "
has "... an entry in force is published" "$(grep aluvian-store <<<"${OUT}")" "published"
has "... with who published it and its review date" "$(grep aluvian-store <<<"${OUT}" | tr -s ' ')" "u-1 2026-10-09 2027-10-09"
has "... one past its review date is review due" "$(grep website <<<"${OUT}")" "review due"
has "... one that ended is expired" "$(grep cloud <<<"${OUT}")" "expired"

fresh; reply GET /v1/tenants/aluvian/exposures 200 '{"tenant":"aluvian","live":[],"expired":[],"reviewDue":[]}'
gentian exposures list --tenant aluvian
has "an empty registry is said to be empty" "${OUT}" "has published nothing to the internet"

fresh; reply GET /v1/tenants/aluvian/exposures 403 '{"error":"you may not see this tenant"}'
gentian exposures list --tenant aluvian
refused "a read the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 403: you may not see this tenant"

WITHDRAW_ROUTE="/v1/tenants/aluvian/exposures/aluvian-store/api"

fresh
gentian exposures withdraw aluvian-store --tenant aluvian
refused "withdraw without an entry is a usage error"
gentian exposures withdraw aluvian-store api
refused "withdraw without --tenant is refused"
gentian exposures withdraw aluvian-store 'api?x=1' --tenant aluvian
refused "an entry that is not a name is refused"
gentian exposures withdraw 'a/b' api --tenant aluvian
refused "an app instance that is not a name is refused"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply DELETE "${WITHDRAW_ROUTE}" 202 '{"status":"updated","commit":"aaaabbbbccccdddd"}'
gentian exposures withdraw aluvian-store api --tenant aluvian
is "withdraw succeeds" "${RC}" "0"
is "... as one DELETE of the entry, with no body" "$(cat "${CALLS}")" "DELETE ${WITHDRAW_ROUTE} "
has "... and names the commit" "${OUT}" "committed (aaaabbbb)"

fresh; reply DELETE "${WITHDRAW_ROUTE}" 200 '{"status":"unchanged"}'
gentian exposures withdraw aluvian-store api --tenant aluvian
has "withdrawing what is not published says so" "${OUT}" "is not published by aluvian; nothing was committed"

fresh; reply DELETE "${WITHDRAW_ROUTE}" 403 '{"error":"publishing and withdrawing is the perimeter approver'"'"'s"}'
gentian exposures withdraw aluvian-store api --tenant aluvian
refused "a withdrawal the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 403: publishing and withdrawing is the perimeter approver's"

# What a tenant's apps ask to have on the internet, and approving it.
RULE='Any script that runs in a page on the main address can set cookies for the whole domain. The rule: publish here only a site whose scripts your organisation itself controls -- no third-party scripts and no pages uploaded by users.'
REQUESTS='{"tenant":"acme","live":[],"expired":[],"reviewDue":[],"entries":[
  {"install":"cloud","exposureName":"shares","state":"requested","host":"share.acme.example","paths":["/public.php/","/s/"],"denyPaths":["/s/admin/"],"authMode":"none","anyoneWithoutSignIn":true,"access":"Reachable by anyone on the internet without sign-in.","mainAddress":false},
  {"install":"feeds","exposureName":"hook","state":"approved","host":"hook.acme.example","paths":["/in/"],"authMode":"bearer","anyoneWithoutSignIn":false,"access":"Nobody signs in at the platform'"'"'s edge.","mainAddress":false,
   "approval":{"install":"feeds","exposureName":"hook","owner":"u-7","reviewAt":"2027-05-01T00:00:00Z","publishedAt":"2026-05-01T00:00:00Z","expiresAt":"2027-01-01T00:00:00Z"}},
  {"install":"website","exposureName":"site","state":"requested","host":"k.example","paths":["/"],"authMode":"none","anyoneWithoutSignIn":true,"access":"Reachable by anyone on the internet without sign-in.","mainAddress":true,"mainAddressRule":"'"${RULE}"'"},
  {"install":"blog","exposureName":"site","state":"requested","paths":["/"],"authMode":"none","anyoneWithoutSignIn":true,"access":"Reachable by anyone on the internet without sign-in.","mainAddress":true,"note":"the main address is already held by website/site"},
  {"install":"ghost","exposureName":"api","state":"unmatched","mainAddress":false,"note":"tenant acme has no app instance named ghost installed","approval":{"install":"ghost","exposureName":"api","owner":"u-9","reviewAt":"2027-05-01T00:00:00Z"}}]}'
READ="/v1/tenants/acme/exposures"
APPROVE_ROUTE="/v1/tenants/acme/exposures/cloud/shares"

fresh
gentian exposures requests
refused "requests without --tenant is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian exposures requests --tenant acme
is "requests succeeds" "${RC}" "0"
is "... as one read of the tenant's entries" "$(cat "${CALLS}")" "GET ${READ} "
has "... an entry nobody approved is requested, with its address" "$(grep '^cloud' <<<"${OUT}" | tr -s ' ')" "cloud shares requested https://share.acme.example /public.php/ /s/ none: anyone"
has "... an approved one says by whom and until when" "$(grep '^feeds' <<<"${OUT}" | tr -s ' ')" "approved https://hook.acme.example /in/ by the app (bearer) u-7 2027-05-01 2027-01-01"
has "... one that matches nothing is marked" "$(grep '^ghost' <<<"${OUT}")" "unmatched"
has "... with the director's reason" "${OUT}" "ghost/api: tenant acme has no app instance named ghost installed"
has "... and an entry with no address says why" "${OUT}" "blog/site: the main address is already held by website/site"

fresh; reply GET "${READ}" 200 '{"tenant":"acme","live":[],"expired":[],"reviewDue":[],"entries":[]}'
gentian exposures requests --tenant acme
has "no entries is said in a sentence" "${OUT}" "No app installed in tenant acme asks to have anything on the internet"

fresh; reply GET "${READ}" 200 '{"tenant":"acme","live":[],"expired":[],"reviewDue":[]}'
gentian exposures requests --tenant acme
refused "a director that lists no entries is not read as a tenant with none"
gentian exposures approve cloud shares --tenant acme --yes
refused "... and nothing is approved through it"
has "... which is said" "${OUT}" "older than this command. Nothing was sent"
is "... and only the read was made" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh
gentian exposures approve cloud --tenant acme --yes
refused "approve without an entry is a usage error"
gentian exposures approve cloud shares --yes
refused "approve without --tenant is refused"
gentian exposures approve 'a/b' shares --tenant acme --yes
refused "an app instance that is not a name is refused"
gentian exposures approve cloud shares --tenant acme
refused "approve with no terminal and no --yes is refused"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${APPROVE_ROUTE}" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian exposures approve cloud shares --tenant acme --yes --reason "shared calendars" --expires 2027-03-31
is "approve succeeds" "${RC}" "0"
is "... as the read and then one PUT of the entry" "$(cat "${CALLS}")" "GET ${READ} 
PUT ${APPROVE_ROUTE} {\"reason\":\"shared calendars\",\"expiresAt\":\"2027-03-31T23:59:59Z\"}"
has "... after showing the address" "${OUT}" "address:  https://share.acme.example"
has "... the paths" "${OUT}" "paths:    /public.php/  /s/"
has "... what is never published" "${OUT}" "refused:  /s/admin/"
has "... that anyone reaches it without sign-in" "${OUT}" "access:   Reachable by anyone on the internet without sign-in."
has "... and the expiry" "${OUT}" "expires:  2027-03-31T23:59:59Z"
has "... and names the commit" "${OUT}" "committed (1234abcd)"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${APPROVE_ROUTE}" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian_typing "cloud/shares" exposures approve cloud shares --tenant acme
is "approve at a terminal succeeds once the entry is typed" "${RC}" "0"
has "... having shown what is approved first" "${OUT%%Type cloud/shares*}" "address:  https://share.acme.example"
is "... and sends an approval with no terms" "$(grep '^PUT' "${CALLS}")" "PUT ${APPROVE_ROUTE} {}"
has "... which stays until it is withdrawn" "${OUT}" "expires:  never"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian_typing "yes" exposures approve cloud shares --tenant acme
refused "approve with anything else typed is refused"
is "... and nothing was approved" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "/v1/tenants/acme/exposures/feeds/hook" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian exposures approve feeds hook --tenant acme --yes
is "approving an approved entry is a review" "${RC}" "0"
has "... and says so, with who approved it" "${OUT}" "is approved already; approving it again is a review"
has "... and until when" "$(grep 'so far' <<<"${OUT}")" "approved by u-7 on 2026-05-01, review 2027-05-01, expires 2027-01-01"

for missing in "cloud caldav" "nothing shares" "ghost api"; do
    fresh; reply GET "${READ}" 200 "${REQUESTS}"
    # shellcheck disable=SC2086  # two words: the app instance and the entry
    gentian exposures approve ${missing} --tenant acme --yes
    refused "approve of ${missing}, which the director does not list as a request, is refused"
    has "... saying so" "${OUT}" "the director lists no request ${missing/ //} for tenant acme"
    is "... with nothing sent" "$(grep -c '^PUT' "${CALLS}")" "0"
done

fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian exposures approve blog site --tenant acme --yes --acknowledge-main-address-rule
refused "approve of an entry that would be published nowhere is refused"
has "... with the director's reason" "${OUT}" "would be published nowhere: the main address is already held by website/site. Nothing was sent"
is "... and nothing sent" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${APPROVE_ROUTE}" 422 '{"error":"cloud declares no entry named shares for the internet. Nothing was changed"}'
gentian exposures approve cloud shares --tenant acme --yes
refused "an approval the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 422: cloud declares no entry named shares for the internet. Nothing was changed"

# The cluster's main address.
SITE_ROUTE="/v1/tenants/acme/exposures/website/site"
fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian exposures approve website site --tenant acme --yes
refused "a website for the main address is not approved by --yes alone"
has "... the director's rule is printed in full" "$(tr -s ' \n' ' ' <<<"${OUT}")" "${RULE}"
has "... and the flag that acknowledges it is named" "${OUT}" "only with --acknowledge-main-address-rule"
is "... and nothing was sent" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${SITE_ROUTE}" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian exposures approve website site --tenant acme --yes --acknowledge-main-address-rule
is "with the acknowledgement it is approved" "${RC}" "0"
has "... the rule printed all the same" "$(tr -s ' \n' ' ' <<<"${OUT}")" "${RULE}"
is "... and the request carries the acknowledgement" "$(grep '^PUT' "${CALLS}")" "PUT ${SITE_ROUTE} {\"apex\":true,\"acknowledgeMainAddressRule\":true}"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian_typing "website/site" exposures approve website site --tenant acme
refused "at a terminal too, typing the entry does not acknowledge the rule"
is "... and nothing was sent" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
gentian exposures approve cloud shares --tenant acme --yes --acknowledge-main-address-rule
refused "the acknowledgement on an entry that is not for the main address is refused"
is "... and nothing was sent" "$(grep -c '^PUT' "${CALLS}")" "0"

fresh
gentian exposures publish cloud shares --tenant acme
refused "publish is not a command"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

echo ""
if [[ "${fail}" -gt 0 ]]; then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%s%d passed%s\n' "${GREEN}" "${pass}" "${NC}"
