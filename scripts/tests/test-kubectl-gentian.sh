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
#   - `catalogues add` sends an address or an OCI artifact with the repository
#     it is read with (--oci, --repository), never both, refuses --path (no
#     catalogue is kept in the deployments repository), and `catalogues list`
#     shows which;
#   - `catalogues rename` is one request to the cluster administrator's route,
#     with --tenant as without, carrying the new name and nothing else;
#   - `tenants create` states a switch only when it is typed, and reports
#     what the director wrote;
#   - `exposures requests` lists what a tenant's apps ask to have on the
#     internet, as the director reads it;
#   - `exposures approve` reads the entry from the director and shows its
#     address, its paths and who can reach it before it asks; sends nothing
#     for an entry the director does not list; and approves an entry for the
#     cluster's main address only after printing the director's rule in full
#     and only with --acknowledge-main-address-rule, which --yes does not say;
#   - `exposures publish` is not a command and sends nothing;
#   - a move the platform rolled back is retried by `apps install` of the
#     pinned build and by `apps move-outdated`, which print the director's
#     answer, and `apps outdated` gives the command that retries it.
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

# kubectl: one director, in kernel-control, serving cluster c1; a port-forward
# that forwards nothing and waits to be ended.
cat > "${SANDBOX}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    "config current-context") echo test ;;
    "get deploy -A -l app.kubernetes.io/component=director"*) echo "kernel-control director" ;;
    # A custodian, where credentials are set.
    "get deploy -n kernel-control -l app.kubernetes.io/component=custodian"*) echo "custodian" ;;
    "get deploy custodian -n kernel-control -o jsonpath="*) echo 9444 ;;
    # And one usher beside it, which answers what the cluster holds.
    "get deploy -A -l app.kubernetes.io/component=usher"*) echo "kernel-control usher" ;;
    "get deploy usher -n kernel-control -o jsonpath="*) echo 8080 ;;
    # And the registrar, which answers for people.
    "get deploy -n kernel-control -l app.kubernetes.io/component=registrar"*) echo "registrar" ;;
    "get deploy registrar -n kernel-control -o jsonpath="*) echo 8081 ;;
    "get deploy director -n kernel-control -o json")
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

# gentian_stdin <input> <args...> : the same, with <input> on standard input.
gentian_stdin() {
    local input="$1"
    shift
    OUT="$(printf '%s\n' "${input}" | env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" \
        STUB_CALLS="${CALLS}" STUB_REPLIES="${SANDBOX}/replies" \
        bash "${REPO}/scripts/kubectl-gentian" "$@" 2>&1)"
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
lacks() { # <what> <text> <part that must not be there>
    if [[ "$2" != *"$3"* ]]; then ok "$1"; else bad "$1" "    present: $3
    in: $2"; fi
}
refused() { # <what> : the last command ended non-zero
    if [[ "${RC}" -ne 0 ]]; then ok "$1"; else bad "$1" "    it exited 0: ${OUT}"; fi
}
# What was asked of the director that changes something: every request but a read.
writes() { grep -v '^GET ' "${CALLS}" || true; }

TENANTS='{"cluster":"c1","tenants":[{"name":"globex","apps":[],"protected":false},{"name":"acme","apps":[],"protected":false,"customDomain":"acme.example"}]}'
DOMAIN_ROUTE="/v1/clusters/c1/tenants/globex/domain"

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
gentian tenants domain globex
has "a tenant with none is said to have none" "${OUT}" "bound to no domain of its own"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain nobody example.org --yes
refused "a tenant git does not declare is refused"
has "... by name" "${OUT}" "no tenant named nobody"
is "... and nothing is sent" "$(writes)" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants domain globex globex.example
refused "binding without a terminal and without --yes is refused"
has "... and says how to confirm" "${OUT}" "not a terminal: confirm with --yes"
is "... before the director is asked anything" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants domain globex globex.example --yes
is "binding with --yes succeeds" "${RC}" "0"
is "... as one PUT of the domain to the director's route" "$(writes)" "PUT ${DOMAIN_ROUTE} {
  \"domain\": \"globex.example\"
}"
has "... after saying where the desktop moves" "${OUT}" "desktop.globex.example"
has "... and the admin console" "${OUT}" "admin.globex.example"
has "... that the wildcard has to resolve to the cluster" "${OUT}" "*.globex.example resolves to this cluster"
has "... that the issuer has to answer for the zone" "${OUT}" "_acme-challenge"
has "... and that nobody checks either" "${OUT}" "The director checks none of it, and neither does this command"
has "... and it names the commit" "${OUT}" "committed (01234567)"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants domain globex Globex.EXAMPLE --yes
has "a domain typed in capitals is shown in the spelling that is recorded" "${OUT}" "desktop.globex.example"
has "... and sent in it" "$(writes)" '"domain": "globex.example"'

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian_typing "globex.example" tenants domain globex globex.example
is "at a terminal, the domain typed out confirms" "${RC}" "0"
has "... and the request is sent" "$(writes)" "PUT ${DOMAIN_ROUTE}"
: > "${CALLS}"
gentian_typing "yes" tenants domain globex globex.example
refused "anything else typed does not"
has "... and says nothing was changed" "${OUT}" "not confirmed; nothing was changed"
is "... and nothing is sent" "$(writes)" ""

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
reply PUT "${DOMAIN_ROUTE}" 422 '{"error":"invalid custom domain: t.k.example is on the kernel domain, where every tenant already is"}'
gentian tenants domain globex t.k.example --yes
refused "a domain the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 422: invalid custom domain: t.k.example is on the kernel domain, where every tenant already is"

fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
SINGLE_REFUSAL="this cluster's tenancy mode is single: its one tenant for users is on the cluster's own addresses, and bound to a domain of its own it would leave them and give up the cluster's main address. Binding a domain is for a cluster with many tenants (tenancyMode: multi). Nothing was changed"
reply PUT "${DOMAIN_ROUTE}" 422 "$(jq -n --arg e "${SINGLE_REFUSAL}" '{error: $e}')"
gentian tenants domain globex globex.example --yes
refused "a bind on a single-tenancy cluster ends the command non-zero"
has "... with the director's refusal, word for word" "${OUT}" "the director answered 422: ${SINGLE_REFUSAL}"
has "... after the command said whom it is for" "${OUT}" "This command is for a cluster with many tenants"

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
gentian tenants domain globex --remove --yes
has "removing from a tenant with no domain changes nothing" "${OUT}" "nothing was changed"
is "... and nothing is sent" "$(writes)" ""

fresh
gentian tenants domain 'globex/../x' example.org --yes
refused "a tenant name that is not a name is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

echo ""
echo "kubectl gentian tenants: who approves public addresses, who adds catalogues"
echo ""

SWITCHES='{"cluster":"c1","tenants":[{"name":"globex","apps":[],"protected":false,"adminsApprove":false,"cataloguesDelegated":false},{"name":"acme","displayName":"Acme","apps":["cloud"],"protected":false,"adminsApprove":true,"cataloguesDelegated":false}]}'
APPROVE_ROUTE="/v1/clusters/c1/tenants/globex/perimeter-delegation"
CATALOGUE_ROUTE="/v1/clusters/c1/tenants/globex/catalogue-delegation"

fresh; reply GET /v1/clusters/c1/tenants 200 "${SWITCHES}"
gentian tenants list
has "list names the two switches" "$(head -1 <<<"${OUT}")" "ADMINS-APPROVE"
has "... and each tenant's" "$(grep '^acme' <<<"${OUT}" | tr -s ' ')" "yes no"
has "... off where nothing was switched on" "$(grep '^globex' <<<"${OUT}" | tr -s ' ')" "no no"
# A director that does not answer the fields yet shows them off, never on.
fresh; reply GET /v1/clusters/c1/tenants 200 "${TENANTS}"
gentian tenants list
has "a listing without the fields shows both off" "$(grep '^globex' <<<"${OUT}" | tr -s ' ')" "no no"

fresh; reply GET /v1/clusters/c1/tenants 200 "${SWITCHES}"
gentian tenants show acme
has "show says whether its administrators approve" "${OUT}" "Its administrators may approve public addresses:  yes"
has "... and whether they add catalogues" "${OUT}" "Its administrators may add catalogues:            no"
has "... and who always approves" "${OUT}" "gentian:tenant:acme:perimeter"
is "... and only reads" "$(cat "${CALLS}")" "GET /v1/clusters/c1/tenants "
gentian tenants show nobody
refused "show of a tenant that is not there is refused"

fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef"}'
gentian tenants create globex
is "create names no switch unless one is given" "$(writes | tr -d ' \n')" 'POST/v1/clusters/c1/tenants{"name":"globex","displayName":"","requireMFA":true}'
has "... and says who approves then" "${OUT}" "not by its own administrators"

fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef"}'
gentian tenants create globex --admins-approve-public-addresses --admins-add-catalogues
is "create with both switches states both, as the manifest names them" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"globex","displayName":"","requireMFA":true,"perimeter":{"adminsApprove":true},"catalogue":{"delegated":true}}'
has "... and says so" "${OUT}" "Its administrators may approve its public addresses."

# The user tenant of a single-tenancy cluster: the director turns approving on
# where the request does not say, and the command reports what it wrote.
fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef","adminsApprove":true}'
gentian tenants create user
is "create for the user tenant states no switch either" "$(writes | tr -d ' \n')" 'POST/v1/clusters/c1/tenants{"name":"user","displayName":"","requireMFA":true}'
has "... and says what the director wrote, not what was typed" "${OUT}" "Its administrators may approve its public addresses."

fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef","adminsApprove":false}'
gentian tenants create user --admins-approve-public-addresses=false
is "create states an explicit off" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"user","displayName":"","requireMFA":true,"perimeter":{"adminsApprove":false}}'
has "... and says who approves then" "${OUT}" "not by its own administrators"

fresh
gentian tenants create user --admins-approve-public-addresses=maybe
refused "create refuses a value that is neither true nor false"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef"}'
gentian tenants create globex --admins-add-catalogues
is "create with one switch states that one" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"globex","displayName":"","requireMFA":true,"catalogue":{"delegated":true}}'

# Sign-up: stated only when asked for, and said out loud before the director
# is asked, because it opens the realm to people nobody invited.
fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef","signUp":true}'
gentian tenants create customers --self-sign-up
is "create with sign-up states it, and nothing else" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"customers","displayName":"","requireMFA":true,"signUp":true}'
has "... warns what it opens" "${OUT}" "WARNING: --self-sign-up lets anybody on the internet make an account"
has "... and says what was made" "${OUT}" "Anybody may sign up to it"
fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef"}'
gentian tenants create globex
has "... and a tenant made without it is not warned" "$(grep -c 'self-sign-up' <<<"${OUT}" || true)" "0"

# An outside realm its people sign in at: named, sent as brokeredTo, said.
fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef","brokeredTo":1}'
gentian tenants create acme --brokered-to name=aluvian,redirect=https://id.aluvian.example/auth/realms/customers/broker/acme/endpoint,jwks=https://id.aluvian.example/auth/realms/customers/protocol/openid-connect/certs
is "create with an outside realm sends it as brokeredTo" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"acme","displayName":"","requireMFA":true,"brokeredTo":[{"name":"aluvian","redirectURI":"https://id.aluvian.example/auth/realms/customers/broker/acme/endpoint","jwksURL":"https://id.aluvian.example/auth/realms/customers/protocol/openid-connect/certs"}]}'
has "... and says where its people may sign in" "${OUT}" "Its people may sign in at: aluvian"
fresh
gentian tenants create acme --brokered-to name=aluvian,redirect=https://x.example/e
refused "an outside realm without its keys' address is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

# A country: sent upper case, refused when it is not two letters.
fresh; reply POST /v1/clusters/c1/tenants 202 '{"status":"created","commit":"0123456789abcdef","country":"CH"}'
gentian tenants create acme --country ch
is "create with a country sends it upper case" "$(writes | tr -d ' \n')" \
    'POST/v1/clusters/c1/tenants{"name":"acme","displayName":"","requireMFA":true,"country":"CH"}'
has "... and says it" "${OUT}" "Its country: CH."
fresh
gentian tenants create acme --country Schweiz
refused "a country that is not an alpha-2 code is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

# The other half: a realm of another cluster whose people sign in here, as an
# organization.
fresh; reply PUT /v1/tenants/customers/brokered-from/abc 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants organizations customers add abc --display-name "ABC AG" --issuer https://id.gentian.example/auth/realms/abc --client-id broker-to-aluvian --domain ABC.gentian.example
is "organizations add sends one PUT" "$(writes | tr -d ' \n')" \
    'PUT/v1/tenants/customers/brokered-from/abc{"displayName":"ABCAG","issuer":"https://id.gentian.example/auth/realms/abc","clientID":"broker-to-aluvian","domain":"abc.gentian.example"}'
has "... and says who signs in" "${OUT}" "People of abc.gentian.example sign in to tenant customers as the organization ABC AG"
fresh
gentian tenants organizations customers add abc --display-name "ABC AG" --issuer https://id.gentian.example/auth/realms/abc --client-id broker-to-aluvian
refused "an organization without its domain is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh; reply DELETE /v1/tenants/customers/brokered-from/abc 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants organizations customers remove abc
is "organizations remove sends one DELETE" "$(cat "${CALLS}")" "DELETE /v1/tenants/customers/brokered-from/abc "
fresh; reply GET /v1/tenants/customers/brokered-from 200 '{"brokeredFrom":[{"name":"abc","displayName":"ABC AG","issuer":"https://id.abc.example/auth/realms/main","clientID":"broker-to-aluvian","domain":"abc.example"}]}'
gentian tenants organizations customers
has "organizations lists the sign-in alias" "${OUT}" "sign-in alias org-abc"

# A tenant's own groups, and another tenant's people signing in: one request
# each, as the director takes them.
fresh; reply PUT /v1/clusters/c1/tenants/customers/groups 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants groups customers website-editors store-editors
is "tenants groups sends the list" "$(writes | tr -d ' \n')" 'PUT/v1/clusters/c1/tenants/customers/groups{"groups":["website-editors","store-editors"]}'
fresh; reply PUT /v1/clusters/c1/tenants/customers/sign-in/team 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants sign-in customers --from team --groups website-editors,members
is "tenants sign-in sends the source and its groups" "$(writes | tr -d ' \n')" 'PUT/v1/clusters/c1/tenants/customers/sign-in/team{"groups":["website-editors","members"]}'
has "... and says it is committed" "${OUT}" "People of team sign in to customers as website-editors,members: committed"
fresh; reply DELETE /v1/clusters/c1/tenants/customers/sign-in/team 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants sign-in customers --remove team
is "tenants sign-in --remove sends one DELETE" "$(cat "${CALLS}")" "DELETE /v1/clusters/c1/tenants/customers/sign-in/team "
fresh
gentian tenants groups customers Not_A_Label
refused "a group name that is not a label is a usage error"

# Credentials: listed by tenant; set from files and stdin, never from the
# command line, every value without its trailing newline.
fresh; reply GET /v1/credentials 200 '{"credentials":[{"name":"app-customers-aluvian-website-intake","tenant":"customers","satisfied":false,"fields":[{"key":"token"}]},{"name":"repository-aluvian-team","tenant":"team","satisfied":true,"fields":[{"key":"password"}]}]}'
gentian credentials list --tenant customers
has "credentials list names the tenant's requirement and its fields" "${OUT}" "app-customers-aluvian-website-intake"
lacks "... and not another tenant's" "${OUT}" "repository-aluvian-team"
printf 'sk_test_restricted\n' > "${SANDBOX}/secret-key"
fresh; reply PUT /v1/credentials/app-customers-aluvian-website-stripe 200 '{"name":"app-customers-aluvian-website-stripe","stored":true,"validated":true}'
gentian_stdin "pk_test_x" credentials set app-customers-aluvian-website-stripe --field secret_key=@"${SANDBOX}/secret-key" --field publishable_key=-
is "credentials set sends every field, without trailing newlines" "$(writes | tr -d ' \n')" \
    'PUT/v1/credentials/app-customers-aluvian-website-stripe{"fields":{"secret_key":"sk_test_restricted","publishable_key":"pk_test_x"}}'
has "... and says it was stored and validated" "${OUT}" "Set app-customers-aluvian-website-stripe: stored and validated."
fresh
gentian credentials set app-customers-aluvian-website-intake --field token=plaintext
refused "a value on the command line is refused"
is "... and nothing is sent" "$(cat "${CALLS}")" ""

# An app waiting on a privilege: what it waits for, then one PUT that answers
# it with the person's reason.
fresh; reply GET /v1/tenants/team/apps/status 200 '{"tenant":"team","apps":[{"profile":"aluvian-commerce","name":"aluvian-commerce","pendingPrivileges":["egress/intake"]}]}'
gentian apps approve aluvian-commerce --tenant team
has "apps approve without a request lists what the app waits for" "${OUT}" "  egress/intake"
is "... and asks nothing to change" "$(writes)" ""
fresh; reply PUT /v1/tenants/team/privileges/aluvian-commerce/egress/intake 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian apps approve aluvian-commerce egress/intake --tenant team --reason "reads the website's order requests"
is "apps approve sends one PUT with the reason" "$(writes | tr -d '\n' | tr -s ' ')" \
    'PUT /v1/tenants/team/privileges/aluvian-commerce/egress/intake { "reason": "reads the website'"'"'s order requests"}'
has "... and says it is committed" "${OUT}" "Approved egress/intake for aluvian-commerce in team: committed (01234567)"
fresh
gentian apps approve aluvian-commerce egress/intake --tenant team --reason "ok"
refused "an approval without a reason of ten characters is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh
gentian apps approve aluvian-commerce hostNetwork/x --tenant team --reason "because it needs it"
refused "a request of no known kind is a usage error"

# Declining is the approver's no, relayed as an action on the app with the
# reason; nothing is committed, and the app keeps waiting.
fresh; reply POST /v1/tenants/team/apps/aluvian-commerce/actions/decline-privilege 202 '{"status":"declined","tenant":"team","profile":"aluvian-commerce","install":"aluvian-commerce","privilege":"podSecurity/run-as-root","request":{"declinedBy":"sam@example.com","declinedAt":"2026-10-21T10:00:00Z","declineReason":"no root on shared nodes; use the rootless image"}}'
gentian apps decline aluvian-commerce podSecurity/run-as-root --tenant team --reason "no root on shared nodes; use the rootless image"
is "apps decline sends one POST with the privilege and the reason" "$(writes | tr -d '\n' | tr -s ' ')" \
    'POST /v1/tenants/team/apps/aluvian-commerce/actions/decline-privilege { "privilege": "podSecurity/run-as-root", "reason": "no root on shared nodes; use the rootless image"}'
has "... and says it is recorded, not committed" "${OUT}" "Declined podSecurity/run-as-root for aluvian-commerce in team: recorded, nothing committed"
fresh
gentian apps decline aluvian-commerce podSecurity/run-as-root --tenant team --reason "no"
refused "a decline without a reason of ten characters is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh
gentian apps decline aluvian-commerce --tenant team --reason "no root on shared nodes"
refused "a decline must name the request"
fresh
gentian apps decline aluvian-commerce hostNetwork/x --tenant team --reason "because it must not"
refused "a decline of no known kind is a usage error"

# What awaits the security officer, across the cluster: the usher's list,
# read as a table with who asked and why, and the command that answers it.
fresh; reply GET /v1/clusters/c1/approvals 200 '{"approvals":[{"tenant":"team","profile":"aluvian-commerce","install":"aluvian-commerce","privilege":"podSecurity/run-as-root","approvedBy":"security-officer","reason":"the vendor image writes its cache as root","requestedBy":"tom@example.com","requestedAt":"2026-10-21T09:12:00Z","note":"needed for the pilot"},{"tenant":"other","profile":"wiki","install":"wiki","privilege":"clusterRoles/node-reader","approvedBy":"security-officer","declinedBy":"sam@example.com","declinedAt":"2026-10-21T10:00:00Z","declineReason":"node access is not for a wiki"}]}'
gentian approvals list
is "approvals list asks the usher for the cluster's list" "$(cat "${CALLS}")" "GET /v1/clusters/c1/approvals "
has "... and shows who asked" "${OUT}" "tom@example.com"
has "... and the profile's reason with the note" "${OUT}" "team/aluvian-commerce podSecurity/run-as-root: the vendor image writes its cache as root; tom@example.com adds: needed for the pilot"
has "... and a request nobody asked for yet, declined" "${OUT}" "other/wiki clusterRoles/node-reader: the profile gives no reason; declined by sam@example.com on 2026-10-21: node access is not for a wiki"
has "... with the decliner in the table" "${OUT}" "DECLINED BY"
has "... and says how to answer" "${OUT}" "kubectl gentian apps approve <app> <kind>/<name> --tenant <tenant>"
has "... either way" "${OUT}" "kubectl gentian apps decline <app> <kind>/<name> --tenant <tenant>"
fresh; reply GET /v1/clusters/c1/approvals 200 '{"approvals":[]}'
gentian approvals list
has "an empty list says so" "${OUT}" "Nothing awaits the security officer"

# What was granted, across the cluster: the director's list, as a table.
fresh; reply GET /v1/clusters/c1/privileges 200 '{"cluster":"c1","privileges":[{"tenant":"team","install":"aluvian-commerce","privilege":"podSecurity/run-as-root","approver":"sam","approvedAt":"2026-10-21T09:12:00Z","reason":"reviewed by the officer","expiresAt":"2027-01-01T00:00:00Z"}]}'
gentian approvals granted
is "approvals granted asks the director for the cluster's list" "$(cat "${CALLS}")" "GET /v1/clusters/c1/privileges "
has "... and shows the grant with its tenant" "${OUT}" "team"
has "... and who approved it" "${OUT}" "sam"
has "... and when it expires" "${OUT}" "2027-01-01"
fresh; reply GET /v1/clusters/c1/privileges 200 '{"cluster":"c1","privileges":[]}'
gentian approvals granted
has "an empty grant list says so" "${OUT}" "No tenant of this cluster has an approved privilege exception"

fresh; reply PUT "${APPROVE_ROUTE}" 202 '{"status":"updated","tenant":"globex","adminsApprove":true,"commit":"0123456789abcdef"}'
gentian tenants set globex --admins-approve-public-addresses=true
is "set turns approving on with one PUT" "$(cat "${CALLS}")" "PUT ${APPROVE_ROUTE} "
has "... and says it is committed" "${OUT}" "may now approve its public addresses: committed"

fresh; reply DELETE "${APPROVE_ROUTE}" 202 '{"status":"updated","tenant":"globex","adminsApprove":false,"commit":"0123456789abcdef"}'
gentian tenants set globex --admins-approve-public-addresses=false
is "set turns approving off with one DELETE" "$(cat "${CALLS}")" "DELETE ${APPROVE_ROUTE} "
has "... and says what stays" "${OUT}" "What is published stays until it is withdrawn"

fresh; reply PUT "${APPROVE_ROUTE}" 202 '{"status":"updated"}'; reply DELETE "${CATALOGUE_ROUTE}" 202 '{"status":"updated"}'
gentian tenants set globex --admins-add-catalogues=false --admins-approve-public-addresses=true
is "set changes both, each by its own route" "$(tr '\n' '|' < "${CALLS}")" "PUT ${APPROVE_ROUTE} |DELETE ${CATALOGUE_ROUTE} |"

fresh; reply PUT "${CATALOGUE_ROUTE}" 202 '{"status":"updated"}'
gentian tenants set globex --admins-add-catalogues=true
is "set delegates catalogues through the route that always did" "$(cat "${CALLS}")" "PUT ${CATALOGUE_ROUTE} "

fresh; reply PUT "${APPROVE_ROUTE}" 403 '{"error":"forbidden"}'
gentian tenants set globex --admins-approve-public-addresses=true
refused "a refusal by the director ends the command non-zero"
has "... with the director's own word" "${OUT}" "the director answered 403: forbidden"

for wrong in "--admins-approve-public-addresses" "--admins-approve-public-addresses=yes" "--admins-add-catalogues=on" "--everything=true" ""; do
    fresh
    # shellcheck disable=SC2086 # an empty option is no argument at all
    gentian tenants set globex ${wrong} --admins-add-catalogues=maybe
    refused "set refuses '${wrong:-nothing but a mistyped value}'"
    is "... and asks the director nothing" "$(cat "${CALLS}")" ""
done
fresh; reply PUT /v1/tenants/globex/country 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants set globex --country de
is "set --country asks the tenant's country route" "$(writes | tr -d ' \n')" 'PUT/v1/tenants/globex/country{"country":"DE"}'
has "... and says it is committed" "${OUT}" "The country of globex is DE: committed."
fresh; reply PUT /v1/tenants/globex/country 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants set globex --country ""
is "set --country \"\" clears it" "$(writes | tr -d ' \n')" 'PUT/v1/tenants/globex/country{"country":""}'
fresh
gentian tenants set globex --country DEU
refused "set refuses a country that is not two letters"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh
gentian tenants set globex
refused "set with nothing to set is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh
gentian tenants set 'globex/../acme' --admins-approve-public-addresses=true
refused "set refuses a tenant that is not a name"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

echo ""
echo "kubectl gentian exposures"
echo ""

REGISTRY='{"tenant":"acme","kinds":{"public":"Public address","publicAppCredential":"Public address that passes the caller'"'"'s credential to the app","signInAppAuthorization":"Behind sign-in: keeps the app'"'"'s own Authorization header"},"live":[
  {"install":"flows","exposureName":"web","kind":"signInAppAuthorization","owner":"u-4","reviewAt":"2027-10-09T10:00:00Z","publishedAt":"2026-10-09T10:00:00Z"},
  {"install":"sync","exposureName":"dav","kind":"publicAppCredential","owner":"u-5","reviewAt":"2027-10-09T10:00:00Z","publishedAt":"2026-10-09T10:00:00Z"},
  {"install":"shop","exposureName":"api","owner":"u-1","reviewAt":"2027-10-09T10:00:00Z","publishedAt":"2026-10-09T10:00:00Z","reason":"the store other clusters read"},
  {"install":"website","exposureName":"site","owner":"u-2","reviewAt":"2026-01-01T00:00:00Z","publishedAt":"2025-01-01T00:00:00Z","apex":true}],
 "expired":[{"install":"cloud","exposureName":"share","owner":"u-3","reviewAt":"2026-03-01T00:00:00Z","expiresAt":"2026-03-01T00:00:00Z"}],
 "reviewDue":[{"install":"website","exposureName":"site","owner":"u-2","reviewAt":"2026-01-01T00:00:00Z"}]}'

fresh
gentian exposures list
refused "list without --tenant is refused"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/tenants/acme/exposures 200 "${REGISTRY}"
gentian exposures list --tenant acme
is "list reads the tenant's registry and nothing else" "$(cat "${CALLS}")" "GET /v1/tenants/acme/exposures "
has "... an entry in force is published" "$(grep shop <<<"${OUT}")" "published"
has "... with who published it and its review date" "$(grep shop <<<"${OUT}" | tr -s ' ')" "u-1 2026-10-09 2027-10-09"
has "... one past its review date is review due" "$(grep website <<<"${OUT}")" "review due"
has "... one that ended is expired" "$(grep cloud <<<"${OUT}")" "expired"
has "... an entry with no kind is a public address" "$(grep '^shop' <<<"${OUT}" | tr -s ' ')" "shop api Public address published"
has "... one that passes the credential says so, in the director's words" "$(grep '^sync' <<<"${OUT}" | tr -s ' ')" "sync dav Public address that passes the caller's credential to the app published"
has "... and one behind sign-in is not shown as a public address" "$(grep '^flows' <<<"${OUT}" | tr -s ' ')" "flows web Behind sign-in: keeps the app's own Authorization header published"

# A director older than kinds: everything it lists is a public address.
fresh; reply GET /v1/tenants/acme/exposures 200 '{"tenant":"acme","live":[{"install":"shop","exposureName":"api","owner":"u-1","reviewAt":"2027-10-09T10:00:00Z"}],"expired":[],"reviewDue":[]}'
gentian exposures list --tenant acme
has "a director that names no kinds lists public addresses" "$(grep '^shop' <<<"${OUT}" | tr -s ' ')" "shop api Public address published"

fresh; reply GET /v1/tenants/acme/exposures 200 '{"tenant":"acme","live":[],"expired":[],"reviewDue":[]}'
gentian exposures list --tenant acme
has "an empty registry is said to be empty" "${OUT}" "has published nothing to the internet"

fresh; reply GET /v1/tenants/acme/exposures 403 '{"error":"you may not see this tenant"}'
gentian exposures list --tenant acme
refused "a read the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 403: you may not see this tenant"

WITHDRAW_ROUTE="/v1/tenants/acme/exposures/shop/api"

fresh
gentian exposures withdraw shop --tenant acme
refused "withdraw without an entry is a usage error"
gentian exposures withdraw shop api
refused "withdraw without --tenant is refused"
gentian exposures withdraw shop 'api?x=1' --tenant acme
refused "an entry that is not a name is refused"
gentian exposures withdraw 'a/b' api --tenant acme
refused "an app instance that is not a name is refused"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply DELETE "${WITHDRAW_ROUTE}" 202 '{"status":"updated","commit":"aaaabbbbccccdddd"}'
gentian exposures withdraw shop api --tenant acme
is "withdraw succeeds" "${RC}" "0"
is "... as one DELETE of the entry, with no body" "$(cat "${CALLS}")" "DELETE ${WITHDRAW_ROUTE} "
has "... and names the commit" "${OUT}" "committed (aaaabbbb)"

fresh; reply DELETE "${WITHDRAW_ROUTE}" 200 '{"status":"unchanged"}'
gentian exposures withdraw shop api --tenant acme
has "withdrawing what is not approved says so" "${OUT}" "is not approved for acme; nothing was committed"

fresh; reply DELETE "${WITHDRAW_ROUTE}" 403 '{"error":"publishing and withdrawing is the perimeter approver'"'"'s"}'
gentian exposures withdraw shop api --tenant acme
refused "a withdrawal the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 403: publishing and withdrawing is the perimeter approver's"

# A second address for a published entry, directly under the cluster's domain.
ALIAS_ROUTE="/v1/clusters/c1/tenants/acme/exposures/shop/api/alias"

fresh
gentian exposures alias shop api --tenant acme
refused "alias with neither --host nor --remove is a usage error"
gentian exposures alias shop api --tenant acme --host shop.k.example --remove
refused "alias with both is refused"
gentian exposures alias shop --tenant acme --host shop.k.example
refused "alias without an entry is a usage error"
gentian exposures alias shop api --host shop.k.example
refused "alias without --tenant is refused"
gentian exposures alias shop 'api?x=1' --tenant acme --host shop.k.example
refused "an entry that is not a name is refused"
is "... and none of them changes anything" "$(writes)" ""

fresh; reply PUT "${ALIAS_ROUTE}" 202 '{"status":"updated","commit":"1111222233334444"}'
gentian exposures alias shop api --tenant acme --host shop.k.example
is "alias succeeds" "${RC}" "0"
is "... as one PUT to the cluster, naming the host" "$(writes | tr -d ' \n')" "PUT${ALIAS_ROUTE}{\"host\":\"shop.k.example\"}"
has "... says where it answers as well, and names the commit" "${OUT}" "also answers at shop.k.example: committed (11112222)"

fresh; reply PUT "${ALIAS_ROUTE}" 200 '{"status":"unchanged"}'
gentian exposures alias shop api --tenant acme --host shop.k.example
has "an alias it already has commits nothing" "${OUT}" "already answers at shop.k.example; nothing was committed"

fresh; reply PUT "${ALIAS_ROUTE}" 409 '{"error":"the address is already in use: shop.k.example is tenant shop'"'"'s own address"}'
gentian exposures alias shop api --tenant acme --host shop.k.example
refused "an alias the director refuses ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 409: the address is already in use: shop.k.example is tenant shop's own address"

fresh; reply DELETE "${ALIAS_ROUTE}" 202 '{"status":"updated","commit":"5555666677778888"}'
gentian exposures alias shop api --tenant acme --remove
is "removing the alias is one DELETE with no body" "$(writes)" "DELETE ${ALIAS_ROUTE} "
has "... and the entry stays at its own address" "${OUT}" "has no alias any more: committed (55556666). It stays at its own address."

fresh; reply DELETE "${ALIAS_ROUTE}" 200 '{"status":"unchanged"}'
gentian exposures alias shop api --tenant acme --remove
has "removing an alias it does not have commits nothing" "${OUT}" "has no alias; nothing was committed"

fresh; reply GET /v1/tenants/acme/exposures 200 '{"tenant":"acme","live":[{"install":"shop","exposureName":"api","owner":"u-1","reviewAt":"2027-10-09T10:00:00Z","alias":"shop"}],"expired":[],"reviewDue":[]}'
gentian exposures list --tenant acme
has "the list names an entry's alias" "$(grep '^shop' <<<"${OUT}" | tr -s ' ')" "shop api Public address published - shop u-1"

# What a tenant's apps ask to have on the internet, and approving it.
RULE='Any script that runs in a page on the main address can set cookies for the whole domain. The rule: publish here only a site whose scripts your organisation itself controls -- no third-party scripts and no pages uploaded by users.'
REQUESTS='{"tenant":"acme","live":[],"expired":[],"reviewDue":[],"entries":[
  {"install":"cloud","exposureName":"shares","state":"requested","kind":"public","kindLabel":"Public address","publicAddress":true,"passesCredential":false,"rateLimit":"Each client address may make 20 requests a second.","host":"share.acme.example","paths":["/public.php/","/s/"],"denyPaths":["/s/admin/"],"authMode":"none","anyoneWithoutSignIn":true,"access":"Reachable by anyone on the internet without sign-in.","mainAddress":false},
  {"install":"flows","exposureName":"web","state":"requested","kind":"signInAppAuthorization","kindLabel":"Behind sign-in: keeps the app'"'"'s own Authorization header","publicAddress":false,"passesCredential":true,"host":"flows.acme.example","paths":["/"],"authMode":"oidc","anyoneWithoutSignIn":false,"access":"This is not a public address. People still have to sign in, and still have to be allowed to use the app, exactly as before.","mainAddress":false},
  {"install":"feeds","exposureName":"hook","state":"approved","kind":"publicAppCredential","kindLabel":"Public address that passes the caller'"'"'s credential to the app","publicAddress":true,"passesCredential":true,"rateLimit":"Each client address may make 5 requests a second.","host":"hook.acme.example","paths":["/in/"],"authMode":"app","anyoneWithoutSignIn":false,"access":"Nobody signs in at the platform'"'"'s edge: anyone on the internet can send requests to these paths. The caller'"'"'s credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it. The platform does not know or check who calls. An app password or token of a person who was removed from the tenant keeps working until the app itself revokes it.","mainAddress":false,
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
has "... an entry nobody approved is requested, with its address" "$(grep '^cloud' <<<"${OUT}" | tr -s ' ')" "cloud shares Public address requested https://share.acme.example /public.php/ /s/ none: anyone"
has "... an entry behind sign-in is listed as one, with sign-in required" "$(grep '^flows' <<<"${OUT}" | tr -s ' ')" "flows web Behind sign-in: keeps the app's own Authorization header requested https://flows.acme.example / required"
has "... an approved one says by whom and until when" "$(grep '^feeds' <<<"${OUT}" | tr -s ' ')" "Public address that passes the caller's credential to the app approved https://hook.acme.example /in/ none: the app checks the credential u-7 2027-05-01 2027-01-01"
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
PUT ${APPROVE_ROUTE} {\"reason\":\"shared calendars\",\"kind\":\"public\",\"expiresAt\":\"2027-03-31T23:59:59Z\"}"
has "... after showing the address" "${OUT}" "address:  https://share.acme.example"
has "... the paths" "${OUT}" "paths:    /public.php/  /s/"
has "... what is never published" "${OUT}" "refused:  /s/admin/"
has "... that anyone reaches it without sign-in" "${OUT}" "access:   Reachable by anyone on the internet without sign-in."
has "... its kind" "${OUT}" "kind:     Public address"
has "... the limit, in the director's words" "${OUT}" "limit:    Each client address may make 20 requests a second."
has "... and the expiry" "${OUT}" "expires:  2027-03-31T23:59:59Z"
has "... and names the commit" "${OUT}" "committed (1234abcd)"

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${APPROVE_ROUTE}" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian_typing "cloud/shares" exposures approve cloud shares --tenant acme
is "approve at a terminal succeeds once the entry is typed" "${RC}" "0"
has "... having shown what is approved first" "${OUT%%Type cloud/shares*}" "address:  https://share.acme.example"
is "... and sends an approval with no terms" "$(grep '^PUT' "${CALLS}")" "PUT ${APPROVE_ROUTE} {\"kind\":\"public\"}"
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
has "... that the caller's credential is passed to the app" "${OUT}" "The caller's credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it."
has "... that the platform does not know who calls" "${OUT}" "The platform does not know or check who calls."
has "... that a removed person's app password keeps working" "${OUT}" "keeps working until the app itself revokes it."
has "... and the stricter limit" "${OUT}" "limit:    Each client address may make 5 requests a second."
is "... and the approval names the kind that was shown" "$(grep '^PUT' "${CALLS}")" "PUT /v1/tenants/acme/exposures/feeds/hook {\"kind\":\"publicAppCredential\"}"

# The request that is no public address: behind sign-in, the app's own header.
fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "/v1/tenants/acme/exposures/flows/web" 202 '{"status":"updated","commit":"1234abcd5678"}'
gentian exposures approve flows web --tenant acme --yes
is "approving an entry behind sign-in succeeds" "${RC}" "0"
has "... having said that nothing goes on the internet" "${OUT}" "puts NOTHING on the internet. It changes this, behind sign-in:"
has "... its kind" "${OUT}" "kind:     Behind sign-in: keeps the app's own Authorization header"
has "... and the director's words for it" "${OUT}" "what:     This is not a public address. People still have to sign in"
lacks "... with no public-address wording" "${OUT}" "puts this on the internet"
is "... and the approval names the kind" "$(grep '^PUT' "${CALLS}")" "PUT /v1/tenants/acme/exposures/flows/web {\"kind\":\"signInAppAuthorization\"}"
has "... and the outcome says nothing is published" "${OUT}" "Nothing is published."

fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "/v1/tenants/acme/exposures/flows/web" 409 '{"error":"the request approves entry web of flows as \"signInAppAuthorization\", and the app'"'"'s catalogue entry declares \"public\". Nothing was changed"}'
gentian exposures approve flows web --tenant acme --yes
refused "an approval of a kind the entry no longer declares ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 409: the request approves entry web of flows"

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

# The command sends the setting the entry has; a director that refuses it all
# the same is quoted, not paraphrased.
MISMATCH="the request asks for the cluster's main address (\\\"apex\\\": true), and the profile of website does not declare entry site for the main address. The platform would publish nothing for it. Nothing was changed"
fresh; reply GET "${READ}" 200 "${REQUESTS}"
reply PUT "${SITE_ROUTE}" 422 "{\"error\":\"${MISMATCH}\"}"
gentian exposures approve website site --tenant acme --yes --acknowledge-main-address-rule
refused "an approval refused for its main-address setting ends the command non-zero"
has "... with the director's own words" "${OUT}" "the director answered 422: ${MISMATCH//\\\"/\"}"

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
echo "kubectl gentian catalogues: at an address, or as an OCI artifact"
echo ""

fresh; reply PUT /v1/clusters/c1/catalogues/acme 202 '{"status":"added","commit":"0123456789abcdef"}'
gentian catalogues add acme https://acme.example/apps
is "add with an address sends the address" "$(tr -d ' \n' < "${CALLS}")" 'PUT/v1/clusters/c1/catalogues/acme{"url":"https://acme.example/apps"}'

fresh; reply PUT /v1/clusters/c1/catalogues/acme 202 '{"status":"added","commit":"0123456789abcdef"}'
gentian catalogues add acme https://acme.example/apps
has "... and names the commit" "${OUT}" "Catalogue acme added for every tenant of this cluster, as the cluster's administrator: committed (01234567)"

# A private registry: declared for the tenant with its address, the token
# set afterwards under the name the director answers.
fresh; reply PUT /v1/tenants/demo/repositories/acme 202 '{"status":"declared","commit":"0123456789abcdef","credentialName":"repository-acme"}'
gentian repositories declare acme --tenant demo --url oci://ghcr.io/acme
is "repositories declare sends the address as an apps registry" "$(tr -d ' \n' < "${CALLS}")" \
    'PUT/v1/tenants/demo/repositories/acme{"role":"apps","type":"oci","url":"oci://ghcr.io/acme"}'
has "... names the commit" "${OUT}" "Repository acme declared for tenant demo: committed (01234567)"
has "... and where the token goes" "${OUT}" "under Credentials, as repository-acme"
fresh
gentian repositories declare acme --url oci://ghcr.io/acme
refused "... and a tenant is required"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""
fresh; reply DELETE /v1/tenants/demo/repositories/acme 202 '{"status":"removed","commit":"0123456789abcdef"}'
gentian repositories remove acme --tenant demo
is "repositories remove asks the tenant's route" "$(tr -d ' \n' < "${CALLS}")" 'DELETE/v1/tenants/demo/repositories/acme'

# A directory of the deployments repository is not a catalogue: that
# repository holds the cluster's state only.
fresh
gentian catalogues add acme --path catalogue
refused "add --path is refused"
has "... saying why" "${OUT}" "a catalogue is never kept in the deployments repository"
gentian catalogues add acme --path=catalogues/acme --tenant demo
refused "... for one tenant too"
gentian catalogues add acme
refused "neither an address nor an artifact is a usage error"
gentian catalogues remove acme --path catalogue
refused "remove takes no --path"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/catalogues 200 '{"cluster":"c1","catalogues":[{"name":"gentian","url":"https://apps.example","scope":"cluster","addedBy":"cluster"}],"tenants":[{"tenant":"demo","delegated":false,"catalogues":[{"name":"acme","url":"https://acme.example/apps","scope":"tenant","addedBy":"cluster"}]}]}'
gentian catalogues list
has "list shows an address as it is" "$(grep '^gentian' <<<"${OUT}" | tr -s ' ')" "gentian every tenant the cluster https://apps.example"
has "... for a tenant as well" "$(grep '^acme' <<<"${OUT}" | tr -s ' ')" "acme tenant demo the cluster https://acme.example/apps"

# A catalogue kept as an OCI artifact: the artifact and the repository it is
# read with, nothing else. Whose repository is the route's, never the body's.
fresh; reply PUT /v1/clusters/c1/tenants/demo/catalogues/acme 403 '{"error":"forbidden"}'
reply PUT /v1/tenants/demo/catalogues/acme 202 '{"status":"added","commit":"0123456789abcdef"}'
gentian catalogues add acme --oci ghcr.io/acme/catalogues/acme:1 --repository acme --tenant demo
is "add --oci succeeds through the tenant's route" "${RC}" "0"
has "... and sends the artifact and the repository and nothing else" "$(tr -d ' \n' < "${CALLS}")" \
    'PUT/v1/tenants/demo/catalogues/acme{"oci":{"repository":"acme","artifact":"ghcr.io/acme/catalogues/acme:1"}}'
has "... as the tenant's administrator" "${OUT}" "Catalogue acme added for tenant demo, as the tenant's administrator"

fresh; reply PUT /v1/clusters/c1/catalogues/acme 202 '{"status":"added","commit":"0123456789abcdef"}'
gentian catalogues add acme --oci=ghcr.io/acme/catalogue@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --repository=ours
is "add --oci for the cluster asks the cluster's route" "$(tr -d ' \n' < "${CALLS}")" \
    'PUT/v1/clusters/c1/catalogues/acme{"oci":{"repository":"ours","artifact":"ghcr.io/acme/catalogue@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}'

fresh
gentian catalogues add acme --oci ghcr.io/acme/catalogue:1
refused "--oci without --repository is refused"
gentian catalogues add acme --repository acme https://acme.example/apps
refused "--repository without --oci is refused"
gentian catalogues add acme https://acme.example/apps --oci ghcr.io/acme/catalogue:1 --repository acme
refused "an address and an artifact together are refused"
gentian catalogues remove acme --oci ghcr.io/acme/catalogue:1
refused "remove takes no --oci"

# A rename: one request to the cluster administrator's route, with --tenant
# as without, with the new name as its body and nothing else.
fresh; reply POST /v1/clusters/c1/catalogues/acme/actions/rename 202 '{"status":"renamed","name":"acme","newName":"acme-apps","scope":"cluster","commit":"0123456789abcdef"}'
gentian catalogues rename acme acme-apps
is "rename asks the cluster's route with the new name" "$(tr -d ' \n' < "${CALLS}")" 'POST/v1/clusters/c1/catalogues/acme/actions/rename{"name":"acme-apps"}'
has "... and names the commit" "${OUT}" "Catalogue acme renamed to acme-apps for every tenant of this cluster: committed (01234567)"
has "... and what followed" "${OUT}" "no build moves"
fresh; reply POST /v1/clusters/c1/tenants/demo/catalogues/aluvian/actions/rename 202 '{"status":"renamed","name":"aluvian","newName":"aluvian-team","scope":"tenant","tenant":"demo","commit":"0123456789abcdef"}'
gentian catalogues rename aluvian aluvian-team --tenant demo
is "rename --tenant asks the tenant's route of the cluster, and no other" "$(tr -d ' \n' < "${CALLS}")" 'POST/v1/clusters/c1/tenants/demo/catalogues/aluvian/actions/rename{"name":"aluvian-team"}'
has "... for the tenant" "${OUT}" "Catalogue aluvian renamed to aluvian-team for tenant demo: committed (01234567)"
fresh; reply POST /v1/clusters/c1/catalogues/acme/actions/rename 200 '{"status":"unchanged","name":"acme","newName":"acme","scope":"cluster"}'
gentian catalogues rename acme acme
has "the same name again is not a commit" "${OUT}" "Catalogue acme is already named acme for every tenant of this cluster; nothing was committed."
fresh; reply POST /v1/clusters/c1/catalogues/acme/actions/rename 409 '{"error":"acme-apps is a catalogue of tenant demo; remove it there or choose another name"}'
gentian catalogues rename acme acme-apps
refused "a name the director refuses is a refusal"
has "... in the director's words" "${OUT}" "acme-apps is a catalogue of tenant demo"
fresh
gentian catalogues rename acme
refused "rename needs both names"
gentian catalogues rename acme acme-apps extra
refused "... and no more"
gentian catalogues rename acme acme-apps --oci ghcr.io/acme/catalogue:1
refused "... and takes no --oci"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/tenants/demo/catalogues 200 '{"tenant":"demo","delegated":true,"catalogues":[{"name":"acme","url":"","oci":{"repository":"acme","artifact":"ghcr.io/acme/catalogue:1"},"scope":"tenant","addedBy":"tenant"}]}'
gentian catalogues list --tenant demo
has "list shows an artifact and its repository" "$(grep '^acme' <<<"${OUT}" | tr -s ' ')" \
    "acme this tenant the tenant oci: ghcr.io/acme/catalogue:1 (repository acme)"

echo ""
echo "kubectl gentian models"
echo ""

MODELS='{"cluster":"c1","settings":{"enabled":true,"gpuAcceleration":true,"instances":[{"name":"qwen","modelId":"Qwen/Qwen2.5-7B-Instruct"}],"providers":[{"name":"acme","apiBase":"https://models.example/v1","apiKeyProperty":"acme_api_key","models":[{"name":"small","model":"acme/small"}]}]},"models":[{"name":"qwen-qwen2.5-7b-instruct","kind":"instance","source":"qwen","state":"not-served","reason":"The platform does not start the vLLM instance behind this model."},{"name":"acme/small","kind":"provider","source":"acme","credential":"llm-provider-acme","apiKeyProperty":"acme_api_key","state":"declared"}]}'

fresh; reply GET /v1/clusters/c1/models 200 "${MODELS}"
gentian models list
has "list flags a model the cluster serves itself as not served" "$(grep '^qwen' <<<"${OUT}" | tr -s ' ')" "qwen-qwen2.5-7b-instruct instance qwen NOT SERVED The platform does not start"
has "... and names where a provider's token belongs" "$(grep '^acme' <<<"${OUT}" | tr -s ' ')" "acme/small provider acme declared token: credential llm-provider-acme, property acme_api_key"
is "... and only reads" "$(cat "${CALLS}")" "GET /v1/clusters/c1/models "

gentian models show
is "show prints the settings and nothing else" "$(jq -c 'keys' <<<"${OUT}")" '["enabled","gpuAcceleration","instances","providers"]'

fresh
gentian models set
refused "set without a file is a usage error"
gentian models set -f "${SANDBOX}/absent.json"
refused "... and so is a file that is not there"
echo 'providers: []' > "${SANDBOX}/models.yaml"
gentian models set -f "${SANDBOX}/models.yaml"
refused "... and one that is not JSON"
is "... and none of them asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply PUT /v1/clusters/c1/models 202 '{"status":"updated","commit":"0123456789abcdef"}'
echo '{"enabled": true, "gpuAcceleration": false, "instances": [], "providers": []}' > "${SANDBOX}/models.json"
gentian models set -f "${SANDBOX}/models.json"
is "set sends the file as the whole of the settings" "$(writes)" 'PUT /v1/clusters/c1/models {"enabled":true,"gpuAcceleration":false,"instances":[],"providers":[]}'
has "... and names the commit" "${OUT}" "committed (01234567)"

fresh; reply PUT /v1/clusters/c1/models 422 '{"error":"invalid model settings: spec.llm.providers[0].apiBase"}'
gentian models set -f "${SANDBOX}/models.json"
refused "a refusal of the director ends the command"
has "... in the director's words" "${OUT}" "the director answered 422: invalid model settings: spec.llm.providers[0].apiBase"

echo ""
echo "kubectl gentian settings"
echo ""

fresh; reply GET /v1/clusters/c1/settings 200 '{"cluster":"c1","settings":{"tenancyMode":"single","mainAddress.tenant":"","mail.serviceMode":"system"}}'
gentian settings show
is "show prints the settings and nothing else" "$(jq -c '.' <<<"${OUT}")" '{"tenancyMode":"single","mainAddress.tenant":"","mail.serviceMode":"system"}'
is "... and only reads" "$(cat "${CALLS}")" "GET /v1/clusters/c1/settings "

fresh
gentian settings set
refused "set without a setting is a usage error"
gentian settings set tenancyMode
refused "... and so is one without a value"
is "... and neither asks the director anything" "$(cat "${CALLS}")" ""

fresh; reply PATCH /v1/clusters/c1/settings 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian settings set tenancyMode=multi mainAddress.tenant=customers
is "set sends every setting named, in one request" "$(writes)" 'PATCH /v1/clusters/c1/settings {"settings":{"tenancyMode":"multi","mainAddress.tenant":"customers"}}'
has "... and names the commit" "${OUT}" "committed (01234567)"

fresh; reply PATCH /v1/clusters/c1/settings 200 '{"status":"unchanged"}'
gentian settings set tenancyMode=multi
has "a claim that already says so commits nothing" "${OUT}" "nothing was committed"

fresh; reply PATCH /v1/clusters/c1/settings 202 '{"status":"updated","commit":"0123456789abcdef","installStep":"B-01"}'
gentian settings set certificates.dnsZones=gentian.org,example.ch
is "a list setting is sent as the one value it was typed as" "$(writes)" 'PATCH /v1/clusters/c1/settings {"settings":{"certificates.dnsZones":"gentian.org,example.ch"}}'
has "... and a setting the installer reads names the step to run" "${OUT}" "./install.sh --only B-01"

fresh; reply PATCH /v1/clusters/c1/settings 400 '{"error":"unknown setting: spec.colour is not a setting"}'
gentian settings set colour=blue
refused "a refusal of the director ends the command"
has "... in the director's words" "${OUT}" "the director answered 400: unknown setting"

echo ""
echo "kubectl gentian tenants retire, an orphan"
echo ""

fresh; reply DELETE /v1/clusters/c1/tenants/ghost 202 '{"status":"orphan-removed","detail":"ghost was not in git, but the cluster still ran it: it is being deleted now."}'
gentian tenants retire ghost --yes
has "a tenant git lacks but the cluster runs is removed, and the command says so" "${OUT}" "the cluster still ran it"

echo ""
echo "kubectl gentian apps: a move that was rolled back is tried again"
echo ""

PINNED="sha256:abababababababababababababababababababababababababababababababab"
OLD="sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
apps_catalogue() {
    reply GET /v1/tenants/acme/catalogues 200 '{"catalogues":[{"name":"gentian"}]}'
    reply GET /v1/tenants/acme/catalogues/gentian/entries 200 '{"entries":[{"name":"notes","version":"2.0.0","digest":"'"${PINNED}"'"}]}'
}

fresh; apps_catalogue
reply POST /v1/tenants/acme/apps/notes 202 '{"status":"retried","commit":"0123456789abcdef","message":"retrying the move that was rolled back: the verification command failed"}'
gentian apps install notes --tenant acme
has "installing the pinned build again prints the director's retry" "${OUT}" "notes in acme: retrying the move that was rolled back: the verification command failed. Committed (01234567)"
lacks "... and not that nothing was committed" "${OUT}" "nothing was committed"

fresh; apps_catalogue
reply POST /v1/tenants/acme/apps/notes 200 '{"status":"already_installed"}'
gentian apps install notes --tenant acme
has "a current app installed again commits nothing" "${OUT}" "notes is already installed in acme at this build; nothing was committed."

# An entry the catalogue leaves to the store (me or ee) is not in the
# listing, only counted, so by name it is not installed from here: the store
# is the route. Once a tenant has it, the update report moves it like any
# other app; a new install of it is still the store's.
fresh
reply GET /v1/tenants/acme/catalogues 200 '{"catalogues":[{"name":"gentian"}]}'
reply GET /v1/tenants/acme/catalogues/gentian/entries 200 '{"entries":[],"storeOnly":1}'
gentian apps install ledger --tenant acme
refused "an app the catalogue leaves to the store is not installed by name"
has "... and the refusal says it is not listed" "${OUT}" "ledger is not listed by any catalogue acme installs from (catalogues: gentian)"
is "... and nothing was asked of the director that writes" "$(writes)" ""

fresh
reply POST /v1/clusters/c1/actions/move-outdated 202 '{"moved":[{"tenant":"acme","profile":"notes","to":"'"${PINNED}"'","status":"retried","detail":"0123456789abcdef","message":"retrying the move that did not start: the export before the move failed"}],"skipped":[]}'
gentian apps move-outdated
has "move-outdated prints a retry as one" "${OUT}" "Retrying notes in acme at ${PINNED:0:19}: retrying the move that did not start: the export before the move failed (0123456789abcdef)"

fresh
reply GET /v1/clusters/c1/updates 200 '{"cluster":"c1","policy":{"mode":"latest"},"windowOpen":true,"entries":[{"tenant":"acme","profile":"notes","pinned":"'"${PINNED}"'","running":"'"${OLD}"'","newest":"'"${PINNED}"'","outdated":false,"mode":"latest","modeFrom":"cluster","rollout":{"target":"'"${PINNED}"'","phase":"RolledBack","message":"the build did not come up within 15m0s"},"ready":true}]}'
gentian apps outdated
has "outdated says the move was rolled back" "${OUT}" "rolled back: the build did not come up within 15m0s"
has "... and how to try it again" "${OUT}" "kubectl gentian apps install notes --tenant acme retries the move to ${PINNED:0:19}"

echo ""
echo ""
echo "kubectl gentian: an installed copy says when it is out of date"
echo ""

# An installed copy, stamped as install-plugin stamps it, and the checkout it
# names: the same body, then a changed one.
CHECKOUT="${SANDBOX}/checkout"
mkdir -p "${CHECKOUT}/scripts"
cp "${REPO}/scripts/kubectl-gentian" "${CHECKOUT}/scripts/kubectl-gentian"
sed -e "s|^GENTIAN_PLUGIN_SOURCE=.*|GENTIAN_PLUGIN_SOURCE=\"${CHECKOUT}\"|" \
    -e 's|^GENTIAN_PLUGIN_COMMIT=.*|GENTIAN_PLUGIN_COMMIT="0123456789abcdef0123456789abcdef01234567"|' \
    "${REPO}/scripts/kubectl-gentian" > "${SANDBOX}/installed-kubectl-gentian"
installed() {
    OUT="$(env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" \
        STUB_CALLS="${CALLS}" STUB_REPLIES="${SANDBOX}/replies" \
        bash "${SANDBOX}/installed-kubectl-gentian" "$@" 2>&1 </dev/null)"
    RC=$?
}
TENANTS='{"tenants":[]}'

fresh; rm -f "${SANDBOX}/home/.gentian/cli-check-c1"; reply GET /v1/tenants 200 "${TENANTS}"
installed tenants list
lacks "a copy the same as its checkout's says nothing" "${OUT}" "Update it"

printf '\n# changed\n' >> "${CHECKOUT}/scripts/kubectl-gentian"
fresh; rm -f "${SANDBOX}/home/.gentian/cli-check-c1"; reply GET /v1/tenants 200 "${TENANTS}"
installed tenants list
has "a copy older than its checkout's says so" "${OUT}" "kubectl gentian is not the copy in ${CHECKOUT} any more"
has "... and gives the command that updates it" "${OUT}" "make -C ${CHECKOUT} install-plugin"

fresh; reply GET /v1/tenants 200 "${TENANTS}"
installed tenants list
lacks "... once an hour, not on every command" "${OUT}" "Update it"

fresh; rm -f "${SANDBOX}/home/.gentian/cli-check-c1"; reply GET /v1/tenants 200 "${TENANTS}"
gentian tenants list
lacks "a copy run from a checkout is never told" "${OUT}" "Update it"

installed version
has "version names where the copy came from" "${OUT}" "installed   : from ${CHECKOUT} at 0123456789abcdef"

echo ""
echo "kubectl gentian platform-roles, tenants platform-access"
echo ""

# The role holders: who could hold a role is the registrar's answer, who
# holds which the director's; add looks the login up and commits the
# subject, remove takes the entry by the login.
ADMINS='{"cluster":"c1","complete":true,"tenants":[{"tenant":"acme","realm":"acme","administrators":[{"id":"7d0f3c2e","username":"it@acme.example","email":"it@acme.example","enabled":true}]},{"tenant":"globex","realm":"globex","administrators":[]}]}'
HOLDERS='{"cluster":"c1","roles":["auditor","securityOfficer"],"roleHolders":[{"role":"securityOfficer","tenant":"acme","subject":"7d0f3c2e","username":"it@acme.example","email":"it@acme.example"}]}'
fresh; reply GET /v1/clusters/c1/tenant-administrators 200 "${ADMINS}"
gentian platform-roles tenant-administrators
has "tenant-administrators lists each tenant's administrators" "${OUT}" "it@acme.example"
has "... and a tenant with none" "${OUT}" "(no administrator)"
is "... from the registrar alone" "$(cat "${CALLS}")" "GET /v1/clusters/c1/tenant-administrators "

fresh; reply GET /v1/clusters/c1/tenant-administrators 200 "${ADMINS}"
reply PUT /v1/clusters/c1/platform-roles/holders/securityOfficer/acme/7d0f3c2e 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian platform-roles holders add securityOfficer acme it@acme.example
is "holders add commits the subject the registrar answered" "$(writes | tr -d ' \n')" 'PUT/v1/clusters/c1/platform-roles/holders/securityOfficer/acme/7d0f3c2e{"username":"it@acme.example","email":"it@acme.example"}'
has "... and says it is committed" "${OUT}" "it@acme.example of acme holds securityOfficer: committed"

fresh; reply GET /v1/clusters/c1/tenant-administrators 200 "${ADMINS}"
gentian platform-roles holders add securityOfficer acme nobody@acme.example
refused "a login that is no administrator of the tenant is refused"
is "... and nothing is committed" "$(writes)" ""
has "... and the tenant's administrators are named" "${OUT}" "it@acme.example"
fresh; reply GET /v1/clusters/c1/tenant-administrators 200 "${ADMINS}"
gentian platform-roles holders add securityOfficer nowhere it@acme.example
refused "a tenant the registrar does not list is refused"
is "... and nothing is committed" "$(writes)" ""
fresh
gentian platform-roles holders add securityOfficer Acme it@acme.example
refused "a tenant that is not a name is refused before anything is asked"
is "... and nothing is asked" "$(cat "${CALLS}")" ""
fresh
gentian platform-roles holders add acme it@acme.example
refused "add without a role is refused"
is "... and nothing is asked" "$(cat "${CALLS}")" ""

fresh; reply GET /v1/clusters/c1/platform-roles/holders 200 "${HOLDERS}"
gentian platform-roles holders
has "holders lists the holders" "${OUT}" "it@acme.example"
has "... with the role" "${OUT}" "securityOfficer"
has "... and the subject" "${OUT}" "7d0f3c2e"
fresh; reply GET /v1/clusters/c1/platform-roles/holders 200 "${HOLDERS}"
reply DELETE /v1/clusters/c1/platform-roles/holders/securityOfficer/acme/7d0f3c2e 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian platform-roles holders remove securityOfficer acme it@acme.example
is "holders remove sends one DELETE for the entry the login names" "$(writes)" "DELETE /v1/clusters/c1/platform-roles/holders/securityOfficer/acme/7d0f3c2e "
fresh; reply GET /v1/clusters/c1/platform-roles/holders 200 "${HOLDERS}"
gentian platform-roles holders remove auditor acme it@acme.example
refused "removing a role the login does not hold is refused"
is "... and nothing is committed" "$(writes)" ""

# Who holds each role through its group: the registrar's answer, read only.
fresh; reply GET /v1/clusters/c1/platform-role-members 200 '{"cluster":"c1","complete":false,"roles":[{"role":"admin","group":"gentian:platform:admin","members":[{"id":"u1","username":"ops@k.example","email":"ops@k.example","enabled":true}]},{"role":"auditor","group":"gentian:platform:auditor","members":[]},{"role":"securityOfficer","group":"gentian:platform:security","members":null,"unavailable":true}]}'
gentian platform-roles members
is "members asks the registrar alone" "$(cat "${CALLS}")" "GET /v1/clusters/c1/platform-role-members "
has "... and lists a member with the group" "${OUT}" "ops@k.example"
has "... and an empty group" "${OUT}" "(nobody)"
has "... and a group that was not read" "${OUT}" "the list is incomplete"
has "... and says where they are managed" "${OUT}" "managed in Keycloak"

# The tenant's own switch: one PUT, on or off; a name alone reads.
fresh; reply PUT /v1/tenants/acme/platform-access 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants platform-access acme on
is "tenants platform-access on sends the switch" "$(writes | tr -d ' \n')" 'PUT/v1/tenants/acme/platform-access{"adminsEnter":true}'
has "... and says where the platform's administrators enter" "${OUT}" "https://desktop.acme.k.example/?kc_idp_hint=kernel"
fresh; reply PUT /v1/tenants/acme/platform-access 202 '{"status":"updated","commit":"0123456789abcdef"}'
gentian tenants platform-access acme off
is "tenants platform-access off sends the switch" "$(writes | tr -d ' \n')" 'PUT/v1/tenants/acme/platform-access{"adminsEnter":false}'
fresh; reply GET /v1/tenants/acme/platform-access 200 '{"tenant":"acme","adminsEnter":false}'
gentian tenants platform-access acme
has "a name alone reads the switch" "${OUT}" "may not sign in to acme"
is "... and only reads" "$(cat "${CALLS}")" "GET /v1/tenants/acme/platform-access "
fresh
gentian tenants platform-access acme maybe
refused "a state that is neither on nor off is a usage error"
is "... and asks the director nothing" "$(cat "${CALLS}")" ""

if [[ "${fail}" -gt 0 ]]; then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%s%d passed%s\n' "${GREEN}" "${pass}" "${NC}"
