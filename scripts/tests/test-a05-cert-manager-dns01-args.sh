#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-a05-cert-manager-dns01-args.sh
# =============================================================================
# A-05 runs cert-manager with --dns01-recursive-nameservers-only, because the
# CoreDNS hairpin hides the kernel zone's SOA and NS from in-cluster lookups and
# every DNS-01 challenge otherwise waits on "not yet propagated" (#180).
#
# What these exercise is the part that decides whether a release needs
# changing: the flags the claim asks for, how they merge into a release's
# existing extraArgs, and whether a release already carries them. `helm` is a
# stub on PATH; the default is read from the real XRD, so a test cannot pass
# against a default the claim does not have.
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT
cat > "${STUB_DIR}/helm" <<'STUB'
#!/usr/bin/env bash
# status → whether a release exists; get values → HELM_STUB_VALUES.
case "$1" in
    status) [[ "${HELM_STUB_RELEASE:-1}" == "1" ]] ;;
    get)    printf '%s\n' "${HELM_STUB_VALUES:-"{}"}" ;;
    *)      exit 1 ;;
esac
STUB
chmod +x "${STUB_DIR}/helm"
PATH="${STUB_DIR}:${PATH}"

# The real XRD default, not a copy of it.
xrd_default() {
    python3 - "$1" <<'PY'
import sys, yaml
node = yaml.safe_load(open("crossplane/xrds/cluster.yaml"))
node = node["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
for part in sys.argv[1].split("."):
    node = node["properties"][part]
print(node.get("default", ""))
PY
}

# The functions under test, lifted out of certs.sh rather than sourcing the
# whole library and everything it pulls in.
for fn in cert_manager_dns01_args _cert_manager_extra_args_json \
          _cert_manager_release_extra_args cert_manager_dns01_converged; do
    eval "$(sed -n "/^${fn}() {/,/^}/p" scripts/lib/certs.sh)"
done

check() {
    local name="$1" got="$2" want="$3"
    if [[ "${got}" == "${want}" ]]; then
        printf '  %s✓%s %s\n' "${GREEN}" "${NC}" "${name}"
        pass=$(( pass + 1 ))
    else
        printf '  %s✗%s %s\n      want: %s\n      got:  %s\n' "${RED}" "${NC}" "${name}" "${want}" "${got}"
        fail=$(( fail + 1 ))
    fi
}

converged() {
    if cert_manager_dns01_converged; then echo yes; else echo no; fi
}

echo ""
echo "A-05 cert-manager DNS-01 flags"
echo ""

OURS='["--dns01-recursive-nameservers-only","--dns01-recursive-nameservers=1.1.1.1:53,8.8.8.8:53"]'

unset DNS01_RECURSIVE_NAMESERVERS
check "claim silent: public resolvers from the XRD default" \
    "$(cert_manager_dns01_args | tr '\n' ' ')" \
    "--dns01-recursive-nameservers-only --dns01-recursive-nameservers=1.1.1.1:53,8.8.8.8:53 "
check "fresh install: exactly the two flags" \
    "$(_cert_manager_extra_args_json '[]')" "${OURS}"
check "operator flags kept, stale resolvers replaced" \
    "$(_cert_manager_extra_args_json '["--v=4","--dns01-recursive-nameservers=9.9.9.9:53"]')" \
    '["--v=4","--dns01-recursive-nameservers-only","--dns01-recursive-nameservers=1.1.1.1:53,8.8.8.8:53"]'

# The release ifk-w4h runs: installed before the flags existed.
check "release without the flags is not converged" \
    "$(HELM_STUB_VALUES='{"crds":{"enabled":true}}' converged)" no
check "release with the flags is converged" \
    "$(HELM_STUB_VALUES="{\"extraArgs\":${OURS}}" converged)" yes
check "release with other resolvers is not converged" \
    "$(HELM_STUB_VALUES='{"extraArgs":["--dns01-recursive-nameservers-only","--dns01-recursive-nameservers=9.9.9.9:53"]}' converged)" no
check "no Helm release: not this check's to fail" \
    "$(HELM_STUB_RELEASE=0 converged)" yes

export DNS01_RECURSIVE_NAMESERVERS=cluster
check "cluster: no flags" "$(cert_manager_dns01_args)" ""
check "cluster: flags removed, others kept" \
    "$(_cert_manager_extra_args_json "[\"--v=4\",${OURS:1}")" '["--v=4"]'
check "cluster: release still carrying the flags is not converged" \
    "$(HELM_STUB_VALUES="{\"extraArgs\":${OURS}}" converged)" no
check "cluster: release without them is converged" \
    "$(HELM_STUB_VALUES='{}' converged)" yes

export DNS01_RECURSIVE_NAMESERVERS=10.0.0.53:53
check "claim override reaches the flag" \
    "$(cert_manager_dns01_args | tail -1)" "--dns01-recursive-nameservers=10.0.0.53:53"

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d checks passed.%s\n' "${GREEN}" "${pass}" "${NC}"
