#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-mail-egress.sh
# =============================================================================
# A cluster that runs its own mail and names mail.egressHost sends from a
# floating IP attached to one node, whose reverse DNS is that name. Both are
# set at the provider, so the install cannot make them true; what it can do is
# refuse to go on while they are not, before anything is applied. Otherwise
# Postfix is pinned to a node that does not exist and the install waits for the
# tenant's mail until it times out.
#
# Held here, against a stand-in kubectl and a stand-in dig:
#
#   - check_mail_egress says nothing where the cluster relays its mail, and
#     warns without stopping where it runs its own and names no egress host;
#   - it stops when no node reports an external address, and when the nodes
#     report more than one;
#   - it stops when the address resolves back to another name, or to none;
#   - it passes when one node carries the address and the address resolves
#     back to the name, whatever the case and with the trailing dot dig prints;
#   - step 0 asks for the name on a cluster that runs its own mail, refuses an
#     address typed there, takes a blank, and does not ask a cluster that
#     relays.
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
echo "mail egress: the install stops before it applies anything it cannot finish"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gme.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"

# kubectl answers the one question asked of it: the nodes' external addresses,
# one per line, from FAKE_NODE_ADDRS. dig answers the reverse lookup from
# FAKE_PTR.
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    "get nodes"*) printf '%s' "${FAKE_NODE_ADDRS:-}" | tr ' ' '\n'; [[ -n "${FAKE_NODE_ADDRS:-}" ]] && echo ;;
esac
exit 0
STUB
cat >"${SB}/bin/dig" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    *"-x "*) [[ -n "${FAKE_PTR:-}" ]] && printf '%s\n' "${FAKE_PTR}" ;;
esac
exit 0
STUB
chmod +x "${SB}/bin/kubectl" "${SB}/bin/dig"

# run <snippet> [VAR=value ...] — the snippet, with the installer's functions
# loaded and the stand-ins first on PATH. Prints what it printed, then rc=<n>.
run() {
    local snippet="$1"; shift
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
'"${snippet}"'
echo "rc=$?"' 2>&1
}

out="$(run check_mail_egress MAIL_SERVICE_MODE=external MAIL_EGRESS_HOST=out.example.org)"
check "a cluster that relays its mail is not asked anything" '[[ "${out}" == "rc=0" ]]' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system)"
check "its own mail and no egress host: a warning, and the install goes on" \
    'grep -q "rc=0" <<<"${out}" && grep -q "shared address" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org)"
check "an egress host and no node with an external address: stopped" \
    'grep -q "rc=1" <<<"${out}" && grep -q "no node reports an external address" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org "FAKE_NODE_ADDRS=192.0.2.10 192.0.2.11")"
check "two nodes with different external addresses: stopped, and both are named" \
    'grep -q "rc=1" <<<"${out}" && grep -q "192.0.2.10 192.0.2.11" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org FAKE_NODE_ADDRS=192.0.2.10)"
check "the address resolves back to nothing: stopped" \
    'grep -q "rc=1" <<<"${out}" && grep -q "resolves back to nothing" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org FAKE_NODE_ADDRS=192.0.2.10 FAKE_PTR=ip-10.provider.example.)"
check "the address resolves back to the provider's name: stopped, and the name is said" \
    'grep -q "rc=1" <<<"${out}" && grep -q "resolves back to ip-10.provider.example" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org FAKE_NODE_ADDRS=192.0.2.10 FAKE_PTR=Out.Example.org.)"
check "one node, and the address resolves back to the name: passed" \
    'grep -q "rc=0" <<<"${out}" && grep -q "mail leaves from 192.0.2.10" <<<"${out}"' "${out}"

out="$(run check_mail_egress MAIL_SERVICE_MODE=system MAIL_EGRESS_HOST=out.example.org "FAKE_NODE_ADDRS=192.0.2.10 192.0.2.10" FAKE_PTR=out.example.org.)"
check "the same address reported twice is one address" 'grep -q "rc=0" <<<"${out}"' "${out}"

# Step 0. NETWORK_MODE=static-ip is what makes the mail mode a question at all.
ask='prompt_mail_mode; echo "host=${MAIL_EGRESS_HOST:-<unset>}"'
out="$(printf 'out.example.org\n' | run "${ask}" NETWORK_MODE=static-ip MAIL_SERVICE_MODE=system KERNEL_DOMAIN=example.org)"
check "its own mail: step 0 asks for the egress host and keeps the answer" \
    'grep -q "host=out.example.org" <<<"${out}" && grep -q "rc=0" <<<"${out}"' "${out}"

out="$(printf '192.0.2.10\nout.example.org\n' | run "${ask}" NETWORK_MODE=static-ip MAIL_SERVICE_MODE=system KERNEL_DOMAIN=example.org)"
check "an address typed for the name is refused and the question asked again" \
    'grep -q "That is an address" <<<"${out}" && grep -q "host=out.example.org" <<<"${out}"' "${out}"

out="$(printf '\n' | run "${ask}" NETWORK_MODE=static-ip MAIL_SERVICE_MODE=system KERNEL_DOMAIN=example.org)"
check "a blank answer is taken, and what it means is said" \
    'grep -q "host=<unset>" <<<"${out}" && grep -q "shared address" <<<"${out}" && grep -q "rc=0" <<<"${out}"' "${out}"

out="$(printf 'smtp.example.net\n' | run "${ask}" NETWORK_MODE=static-ip MAIL_SERVICE_MODE=external KERNEL_DOMAIN=example.org)"
check "a cluster that relays is not asked for an egress host" \
    'grep -q "host=<unset>" <<<"${out}" && ! grep -q "egressHost" <<<"${out}"' "${out}"

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} checks passed.${NC}"
