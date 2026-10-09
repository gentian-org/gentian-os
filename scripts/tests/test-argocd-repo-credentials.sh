#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-argocd-repo-credentials.sh
# =============================================================================
# Argo CD reads two repositories before their Repository claims can hand it a
# credential from the vault: gentian-os, and the deployments repository, whose
# own claim is a file inside it. The installer registered a bootstrap
# credential for the first and not for the second, so on a private deployments
# repository the claim that would have supplied the credential could never be
# read: C-02 waited fifteen minutes for gentian-claims and failed.
#
# What is held here, against a stand-in for kubectl that keeps Secrets in a
# directory:
#
#   A-06  registers a bridge for each repository that authenticates and none
#         for one that does not; its check() asks for a credential Argo CD can
#         use, from the bridge or from the claim; a rotated token replaces the
#         bridge's; and a credential alone does not reinstall Argo CD.
#   C-05  removes a bridge once the claim's own Secret carries a login, and
#         not a moment before.
#   The token is in the Secret and nowhere else: not in the output, and not in
#   any command line.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
BIN="${SANDBOX}/bin"; mkdir -p "${BIN}"

# kubectl, as far as these steps use it. Secrets are files under
# ${STATE}/secrets, written by `apply -f -` and by the tests themselves;
# Repository claims are files under ${STATE}/claims. Everything about Argo
# CD's own installation answers as ${ARGOCD_INSTALLED} says. Every call is
# logged with its arguments, which is how the tests see that no token was
# ever one of them.
cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${STATE}/calls.log"
verb="${1:-}"; shift || true
jsonpath=""; args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|-l) shift ;;
        -o) [[ "$2" == jsonpath=* ]] && jsonpath="${2#jsonpath=}"; shift ;;
        -f|--ignore-not-found|--wait=*|--timeout=*|--server-side|--force-conflicts) ;;
        *) args+=("$1") ;;
    esac
    shift
done
kind="${args[0]:-}"; name="${args[1]:-}"
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }
case "${verb}:${kind}" in
    apply:*)
        doc="$(cat)"
        n="$(jq -r '.metadata.name' <<< "${doc}")"
        printf '%s' "${doc}" > "${STATE}/secrets/${n}.json"
        echo "secret/${n} configured" ;;
    get:secret)
        f="${STATE}/secrets/${name}.json"
        [[ -f "${f}" ]] || exit 1
        case "${jsonpath}" in
            '{.data.password}{.data.bearerToken}')
                p="$(jq -r '.stringData.password // empty' "${f}")"; t="$(jq -r '.stringData.bearerToken // empty' "${f}")"
                [[ -n "${p}" ]] && b64 "${p}"; [[ -n "${t}" ]] && b64 "${t}" ;;
            '{.data.url}') b64 "$(jq -r '.stringData.url // empty' "${f}")" ;;
        esac
        exit 0 ;;
    delete:secret)
        rm -f "${STATE}/secrets/${name}.json" ;;
    get:repository.gentianos.io)
        [[ -f "${STATE}/claims/${name}" ]] || exit 1 ;;
    get:crd|get:deployment|get:configmap|get:clusterrolebinding)
        [[ "${ARGOCD_INSTALLED:-0}" == "1" ]] || exit 1
        case "${jsonpath}" in
            *server*insecure*|*diff*server*side*) printf 'true' ;;
            *subjects*) printf '%s' "${GITOPS_NS}" ;;
            *) echo "deployment.apps/argocd-image-updater" ;;
        esac ;;
    *)
        # Anything that would install or restart Argo CD.
        echo "UNEXPECTED kubectl ${verb} ${args[*]}" >> "${STATE}/unexpected.log" ;;
esac
STUB
chmod +x "${BIN}/kubectl"

TOKEN="s3cr3t-token-value-ZZZ"
URL="https://git.example.domain/acme/deployments"

new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    mkdir -p "${STATE}/secrets" "${STATE}/claims"
    : > "${STATE}/calls.log"
}

# run <step file> <shell> [VAR=value ...] -- the step's verbs, in a fresh shell
# with the installer's libraries loaded, the way the driver loads a step.
run() {
    local step="$1" script="$2"; shift 2
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SANDBOX}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        GITOPS_NS="kernel-gitops" STEP="${step}" SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh"
            trap - ERR; set +e
            sleep() { :; }
            source "${SCRIPT_DIR}/scripts/steps/${STEP}.sh"
            eval "${SCRIPT}"
        ' 2>&1
}
secret() { [[ -f "${STATE}/secrets/$1.json" ]]; }
field()  { jq -r "$2 // empty" "${STATE}/secrets/$1.json" 2>/dev/null; }
# The Secret a Repository claim composes for Argo CD, as External Secrets
# writes it: with a login, or (a claim that declares none) without.
claim_secret() {   # claim_secret <name> [password]
    touch "${STATE}/claims/$1"
    jq -n --arg n "repo-$1" --arg p "${2:-}" \
        '{metadata: {name: $n}, stringData: ({url: "x", type: "git"} + if $p == "" then {} else {username: "u", password: $p} end)}' \
        > "${STATE}/secrets/repo-$1.json"
}

BRIDGE="argocd-repo-creds-bootstrap-deployments"
OS_BRIDGE="argocd-repo-creds-bootstrap-gentian-os"

echo ""
echo "Argo CD's credential for a repository it reads before the vault can serve one"
echo ""

# --- A-06: what is registered ------------------------------------------------
new_state
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_AUTH=none GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}")"
if [[ "${out}" == *"rc=0"* ]] && ! grep -q "kubectl apply" "${STATE}/calls.log" && [[ -z "$(ls "${STATE}/secrets")" ]]; then
    ok "a deployments repository that does not authenticate: nothing is registered"
else
    bad "a deployments repository that does not authenticate: nothing is registered" "${out}"
fi

new_state
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}")"
if [[ "${out}" == *"rc=0"* ]] && secret "${BRIDGE}" \
    && [[ "$(field "${BRIDGE}" '.metadata.labels["argocd.argoproj.io/secret-type"]')" == "repo-creds" ]] \
    && [[ "$(field "${BRIDGE}" .metadata.namespace)" == "kernel-gitops" ]] \
    && [[ "$(field "${BRIDGE}" .stringData.url)" == "${URL}" ]] \
    && [[ "$(field "${BRIDGE}" .stringData.username)" == "x-access-token" ]] \
    && [[ "$(field "${BRIDGE}" .stringData.password)" == "${TOKEN}" ]]; then
    ok "a private deployments repository (the default, basic): a repo-creds Secret for its address, in the gitops namespace"
else
    bad "a private deployments repository (the default, basic): a repo-creds Secret for its address, in the gitops namespace" "${out}"
fi
if ! secret "${OS_BRIDGE}"; then ok "the public gentian-os beside it gets none"; else bad "the public gentian-os beside it gets none"; fi
if [[ "${out}" != *"${TOKEN}"* ]] && ! grep -qF "${TOKEN}" "${STATE}/calls.log"; then
    ok "the token is neither printed nor an argument of any command"
else
    bad "the token is neither printed nor an argument of any command" "${out}"
fi

new_state
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_AUTH=bearer GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}" \
    GENTIAN_DEPLOYMENTS_GIT_USERNAME=someone)"
if [[ "$(field "${BRIDGE}" .stringData.bearerToken)" == "${TOKEN}" && -z "$(field "${BRIDGE}" .stringData.password)" \
      && -z "$(field "${BRIDGE}" .stringData.username)" ]]; then
    ok "bearer: the token as bearerToken, and no username"
else
    bad "bearer: the token as bearerToken, and no username" "${out}"
fi

new_state
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}" GENTIAN_DEPLOYMENTS_GIT_USERNAME=someone \
    GENTIAN_OS_REPO="https://git.example.domain/acme/os" GENTIAN_OS_AUTH=basic GENTIAN_OS_GIT_TOKEN="os-token")"
if [[ "$(field "${BRIDGE}" .stringData.username)" == "someone" && "$(field "${OS_BRIDGE}" .stringData.password)" == "os-token" ]]; then
    ok "a username that was given is used; a private gentian-os still gets its own bridge"
else
    bad "a username that was given is used; a private gentian-os still gets its own bridge" "${out}"
fi

new_state
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' GENTIAN_DEPLOYMENTS_REPO="${URL}")"
if [[ "${out}" == *"rc=0"* && "${out}" == *"no token was collected"* ]] && ! secret "${BRIDGE}"; then
    ok "private and no token collected: said so, nothing registered, not a failure"
else
    bad "private and no token collected: said so, nothing registered, not a failure" "${out}"
fi

new_state
claim_secret deployments "${TOKEN}"
out="$(run A-06-argocd '_a06_register_repo_credentials; echo "rc=$?"' \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}")"
if [[ "${out}" == *"rc=0"* ]] && ! secret "${BRIDGE}"; then
    ok "the claim already supplies the login: no bridge is written beside it"
else
    bad "the claim already supplies the login: no bridge is written beside it" "${out}"
fi

# --- A-06: check() -----------------------------------------------------------
verdict() {   # verdict <want rc> <what> [VAR=value ...]
    local want="$1" what="$2" out; shift 2
    out="$(run A-06-argocd 'check; echo "rc=$?"' ARGOCD_INSTALLED=1 GENTIAN_DEPLOYMENTS_REPO="${URL}" "$@")"
    if [[ "${out}" == *"rc=${want}"* && "${out}" != *"${TOKEN}"* ]]; then ok "${what}"; else bad "${what}" "${out}"; fi
}
new_state
verdict 0 "check: public deployments repository, nothing registered: satisfied" GENTIAN_DEPLOYMENTS_AUTH=none
verdict 1 "check: private, a token in hand, no credential on the cluster: not satisfied" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}"
verdict 0 "check: private, no token in hand (--status, --dry-run): not this run's to report"
run A-06-argocd '_a06_register_repo_credentials' GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}" >/dev/null
verdict 0 "check: the bridge holds this run's token: satisfied" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}"
verdict 1 "check: the bridge holds another token (rotated): not satisfied" GENTIAN_DEPLOYMENTS_GIT_TOKEN="rotated-token"
out="$(run A-06-argocd 'check; echo "rc=$?"' ARGOCD_INSTALLED=1 GENTIAN_DEPLOYMENTS_REPO="${URL}/moved" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}")"
if [[ "${out}" == *"rc=1"* ]]; then ok "check: the bridge is for another address (repository moved): not satisfied"; else bad "check: the bridge is for another address" "${out}"; fi
run A-06-argocd '_a06_register_repo_credentials' GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="rotated-token" >/dev/null
if [[ "$(field "${BRIDGE}" .stringData.password)" == "rotated-token" ]]; then
    ok "apply after a rotation: the bridge holds the new token, and there is still one bridge"
else
    bad "apply after a rotation: the bridge holds the new token"
fi

new_state
claim_secret deployments "from-the-vault"
verdict 0 "check: no bridge, the claim's Secret carries a login: satisfied" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}"
new_state
claim_secret deployments ""
verdict 1 "check: no bridge, the claim's Secret carries no login: not satisfied" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}"

# --- A-06: a missing credential does not reinstall Argo CD -------------------
new_state
out="$(run A-06-argocd 'banner() { echo "BANNER $*"; }; apply; echo "rc=$?"' ARGOCD_INSTALLED=1 \
    GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}")"
if [[ "${out}" == *"rc=0"* && "${out}" != *"BANNER"* ]] && secret "${BRIDGE}" && [[ ! -e "${STATE}/unexpected.log" ]]; then
    ok "apply on an installed Argo CD: the bridge is registered and nothing is reinstalled or restarted"
else
    bad "apply on an installed Argo CD: the bridge is registered and nothing is reinstalled or restarted" "${out}$(cat "${STATE}/unexpected.log" 2>/dev/null)"
fi

# --- C-05: the handoff -------------------------------------------------------
handoff() {
    run C-05-repository-handoff 'apply; echo "rc=$?"; check; echo "check=$?"' \
        GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_REPOSITORY_HANDOFF_TIMEOUT=0
}

new_state
out="$(handoff)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"check=0"* ]]; then ok "handoff: no bridge, nothing to do, satisfied"; else bad "handoff: no bridge" "${out}"; fi

new_state
run A-06-argocd '_a06_register_repo_credentials' GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}" >/dev/null
out="$(handoff)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"check=1"* && "${out}" == *"keeping the bootstrap bridge"* ]] && secret "${BRIDGE}"; then
    ok "handoff: no Repository claim yet: the bridge stays, and the step is not satisfied"
else
    bad "handoff: no Repository claim yet: the bridge stays" "${out}"
fi

claim_secret deployments ""
out="$(handoff)"
if [[ "${out}" == *"check=1"* ]] && secret "${BRIDGE}"; then
    ok "handoff: the claim's Secret has no login yet: the bridge stays"
else
    bad "handoff: the claim's Secret has no login yet: the bridge stays" "${out}"
fi

claim_secret deployments "from-the-vault"
out="$(handoff)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"check=0"* ]] && ! secret "${BRIDGE}" && secret repo-deployments; then
    ok "handoff: the claim's Secret carries a login: the bridge is removed, the claim's Secret is not"
else
    bad "handoff: the claim's Secret carries a login: the bridge is removed" "${out}"
fi
if [[ "${out}" != *"${TOKEN}"* && "${out}" != *"from-the-vault"* ]] && ! grep -qF -e "${TOKEN}" -e "from-the-vault" "${STATE}/calls.log"; then
    ok "handoff: no credential is printed or passed as an argument"
else
    bad "handoff: no credential is printed or passed as an argument" "${out}"
fi

# --- A-06: teardown ----------------------------------------------------------
new_state
run A-06-argocd '_a06_register_repo_credentials' GENTIAN_DEPLOYMENTS_REPO="${URL}" GENTIAN_DEPLOYMENTS_GIT_TOKEN="${TOKEN}" \
    GENTIAN_OS_REPO="https://git.example.domain/acme/os" GENTIAN_OS_AUTH=basic GENTIAN_OS_GIT_TOKEN="os-token" >/dev/null
run A-06-argocd '_a06_release_applications() { :; }; helm() { :; }; curl() { :; }; destroy' >/dev/null
if ! secret "${BRIDGE}" && ! secret "${OS_BRIDGE}"; then
    ok "teardown removes both bridges"
else
    bad "teardown removes both bridges"
fi

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
