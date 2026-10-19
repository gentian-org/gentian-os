#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-one-cluster.sh
# =============================================================================
# One run, one cluster. A machine that runs two clusters -- here "test" and
# "prod", each with its own deployments repository, install.env, credential
# cache and kubeconfig -- reached the wrong one three times in one morning:
#
#   1. a shell still exported prod's KUBECONFIG, and a run meant for test
#      went to prod;
#   2. a run with prod's settings reached the test cluster, replaced prod's
#      derivation salt with test's, and wrote it into prod's credential cache;
#   3. a shell exported prod's GENTIAN_DEPLOYMENTS_PATH, and a run with test's
#      install.env began writing clusters/test into prod's repository -- and
#      pointed ~/.gentian/config at it.
#
# Each is reproduced here with install.sh itself, in a sandbox HOME, against
# stand-ins for kubectl and the tools a run reaches for, and each must now
# stop before anything is written: no file in the other cluster's cache, no
# clusters/<id> in the other repository, no change to ~/.gentian/config. A
# run whose settings, checkout and cluster agree goes on unchanged -- also a
# first install into an empty repository, onto a cluster with nothing on it.
#
# The library half (the salt, the cache, the address comparison) is run
# directly, under set -u, the way the installer runs it. No cluster, no
# network: install.sh is only ever executed here against stand-ins.
# =============================================================================
# The library checks are strings handed to the library's shell.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

SB="$(mktemp -d)"
trap 'rm -rf "${SB}"' EXIT
BIN="${SB}/bin"; H="${SB}/home"
mkdir -p "${BIN}" "${SB}/tmp"
REAL_GIT="$(command -v git)"
g() { "${REAL_GIT}" -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

# --- the stand-ins ----------------------------------------------------------
# kubectl answers as the cluster its kubeconfig names: kube-test is the
# installed test cluster, kube-prod the installed prod cluster, kube-fresh a
# cluster nothing has been installed on. Anything that would change a
# cluster is a violation.
cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${CALLS}"
which="${KUBECONFIG##*/}"; which="${which#kube-}"
case "${which}" in test|prod|fresh) ;; *) which=test ;; esac
all=" $* "; verb=""
for a in "$@"; do [[ -z "${verb}" && "${a}" != -* ]] && verb="${a}"; done
case "${verb}" in
    config)
        if [[ "${all}" == *" current-context "* ]]; then echo "ctx-${which}"; exit 0; fi
        if [[ "${all}" == *" view "* ]]; then printf 'https://%s.api.example.test:6443' "${which}"; exit 0; fi
        exit 0 ;;
    cluster-info) exit 0 ;;
    version) echo '{"serverVersion":{"major":"1","minor":"99"}}'; exit 0 ;;
    api-resources|api-versions|explain|auth|describe|logs|top) exit 0 ;;
    port-forward) exit 1 ;;
    get)
        if [[ "${all}" == *" storageclass "* ]]; then echo "standard"; exit 0; fi
        if [[ "${all}" == *" nodes "* ]]; then echo "10.0.0.10"; exit 0; fi
        if [[ "${which}" != "fresh" && "${all}" == *" cluster.gentianos.io "* ]]; then
            printf '%s\t%s.example.test\n' "${which}-$([[ ${which} == test ]] && echo dev || echo prod)" "${which}"; exit 0
        fi
        if [[ "${which}" != "fresh" && "${all}" == *" gentian-os-master-password "* ]]; then
            printf '%s' "${which}-salt" | base64; exit 0
        fi
        echo "Error from server (NotFound)" >&2; exit 1 ;;
esac
echo "kubectl $*" >> "${VIOLATIONS}"
exit 1
STUB

# git: the real one, but a commit or a push is a violation -- nothing these
# runs do may get that far.
cat > "${BIN}/git" <<'STUB'
#!/usr/bin/env bash
echo "git $*" >> "${CALLS}"
for a in "$@"; do
    case "${a}" in
        commit|push|add|tag) echo "git $*" >> "${VIOLATIONS}"; exit 1 ;;
        # A remote with an https address is one nothing here can reach.
        fetch|ls-remote|pull) [[ "${GIT_OFFLINE:-0}" == "1" ]] && exit 128 ;;
    esac
done
exec "${REAL_GIT}" "$@"
STUB

# The rest answer reads and refuse writes, as in test-dry-run-changes-nothing.
cat > "${BIN}/helm" <<'STUB'
#!/usr/bin/env bash
echo "helm $*" >> "${CALLS}"
case "${1:-}" in
    list|ls) echo '[]'; exit 0 ;;
    status|get|history) exit 1 ;;
    version|template|show|lint|env) exit 0 ;;
esac
echo "helm $*" >> "${VIOLATIONS}"; exit 1
STUB
cat > "${BIN}/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >> "${CALLS}"
for a in "$@"; do case "${a}" in -w|--write-out) printf '000' ;; esac; done
exit 7
STUB
cat > "${BIN}/bao" <<'STUB'
#!/usr/bin/env bash
echo "bao $*" >> "${CALLS}"
case "${1:-}" in read|status|version|token|kv|list|auth|secrets|policy) exit 2 ;; esac
echo "bao $*" >> "${VIOLATIONS}"; exit 1
STUB
cat > "${BIN}/gpg" <<'STUB'
#!/usr/bin/env bash
echo "gpg $*" >> "${CALLS}"
exit 2
STUB
for tool in age age-keygen sudo microk8s ssh-keygen; do
    # shellcheck disable=SC2016 # written for the stand-in to expand
    printf '#!/usr/bin/env bash\necho "%s $*" >> "${VIOLATIONS}"\nexit 1\n' "${tool}" > "${BIN}/${tool}"
done
printf '#!/usr/bin/env bash\nexit 0\n' > "${BIN}/crossplane"
chmod +x "${BIN}"/*

# --- the two clusters -------------------------------------------------------
# define <origin> <checkout> <id> <claim name> <domain>
define() {
    local origin="$1" checkout="$2" id="$3" name="$4" domain="$5" d
    g init --bare -b main "${origin}"
    g clone "${origin}" "${checkout}"
    g -C "${checkout}" checkout -b main
    d="${checkout}/clusters/${id}/kernel"
    mkdir -p "${d}/claims"
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: %s\n  namespace: kernel-provisioning\nspec:\n  kernelDomain: %s\n  networkMode: tunnel\n' \
        "${name}" "${domain}" > "${d}/claims/cluster.yaml"
    printf 'kind: Repository\nmetadata:\n  name: deployments\n' > "${d}/claims/deployments-repository.yaml"
    printf 'image:\n  tag: develop-abc1234\n' > "${d}/values.yaml"
    g -C "${checkout}" add -A; g -C "${checkout}" commit -m "${id}"; g -C "${checkout}" push origin main
}

TEST_CHECKOUT="${H}/.gentian/gentian-deployments"
PROD_CHECKOUT="${H}/.gentian-prod/gentian-deployments"
TEST_CACHE="${H}/.gentian/bootstrap-credentials.env"
PROD_CACHE="${H}/.gentian-prod/bootstrap-credentials.env"
CONFIG="${H}/.gentian/config"

build() {
    rm -rf "${H}" "${SB}/test.git" "${SB}/prod.git" "${SB}/new.git"
    mkdir -p "${H}/.gentian" "${H}/.gentian-prod"
    define "${SB}/test.git" "${TEST_CHECKOUT}" test test-dev test.example.test
    define "${SB}/prod.git" "${PROD_CHECKOUT}" prod prod-prod prod.example.test
    # What each cluster's earlier runs left: a cache naming its cluster, and
    # the plugin's file pointing at test.
    printf '# cluster: test test.example.test\nMASTER_PASSWORD=test-master\nDERIVATION_SALT=test-salt\n' > "${TEST_CACHE}"
    printf '# cluster: prod prod.example.test\nMASTER_PASSWORD=prod-master\nDERIVATION_SALT=prod-salt\n' > "${PROD_CACHE}"
    chmod 600 "${TEST_CACHE}" "${PROD_CACHE}"
    printf 'GENTIAN_DEPLOYMENTS_CLUSTER_ID="test"\nGENTIAN_DEPLOYMENTS_PATH="%s"\n' "${TEST_CHECKOUT}" > "${CONFIG}"
    : > "${SB}/kube-test"; : > "${SB}/kube-prod"; : > "${SB}/kube-fresh"
}

# The gentian-os the clusters would track: a local repository with the
# branch install.env names, so that confirming the ref exists needs no network.
g init --bare -b develop "${SB}/os.git"
g clone "${SB}/os.git" "${SB}/os-work"
g -C "${SB}/os-work" checkout -b develop
echo os > "${SB}/os-work/f"; g -C "${SB}/os-work" add -A; g -C "${SB}/os-work" commit -m os
g -C "${SB}/os-work" push origin develop
OS_PIN="GENTIAN_OS_REPO=${SB}/os.git
GENTIAN_OS_BRANCH=develop"

# Each cluster's install.env, as each checkout of gentian-os holds it.
cat > "${SB}/test.env" <<EOF
${OS_PIN}
GENTIAN_DEPLOYMENTS_REPO=${SB}/test.git
GENTIAN_DEPLOYMENTS_BRANCH=main
GENTIAN_DEPLOYMENTS_CLUSTER_ID=test
GENTIAN_DEPLOYMENTS_STAGE=dev
GENTIAN_DEPLOYMENTS_AUTH=none
EOF
cat > "${SB}/prod.env" <<EOF
${OS_PIN}
GENTIAN_DEPLOYMENTS_REPO=${SB}/prod.git
GENTIAN_DEPLOYMENTS_BRANCH=main
GENTIAN_DEPLOYMENTS_CLUSTER_ID=prod
GENTIAN_DEPLOYMENTS_STAGE=prod
GENTIAN_DEPLOYMENTS_AUTH=none
EOF
# The file a second cluster's terminal sources: what its shell exports.
PROD_SHELL=(KUBECONFIG="${SB}/kube-prod" GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}"
            GENTIAN_CREDENTIAL_CACHE="${PROD_CACHE}")

# manifest <dir> — every file under it, but not a checkout's .git (a sync
# may fetch into it; what matters is what is checked out and pushed).
manifest() {
    [[ -d "$1" ]] || return 0
    (cd "$1" && find . -path '*/.git' -prune -o -type f -print | LC_ALL=C sort | while IFS= read -r f; do
        printf '%s %s\n' "${f}" "$(cksum < "${f}")"
    done)
}
origins() { for o in "${SB}"/test.git "${SB}"/prod.git "${SB}"/new.git; do [[ -d "${o}" ]] || continue; printf '%s\n' "${o##*/}"; "${REAL_GIT}" -C "${o}" for-each-ref --format='%(refname) %(objectname)'; done; }

# install <stdin> <env words...> -- <args...> : install.sh in the sandbox.
install() {
    local input="$1"; shift
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    : > "${SB}/calls.log"; : > "${SB}/violations.log"
    printf '%b' "${input}" | env -i HOME="${H}" PATH="${BIN}:${PATH}" TMPDIR="${SB}/tmp" TERM=dumb \
        CALLS="${SB}/calls.log" VIOLATIONS="${SB}/violations.log" REAL_GIT="${REAL_GIT}" \
        GENTIAN_VALIDATE_TIMEOUT=2 ${envs[@]+"${envs[@]}"} \
        bash "${REPO}/install.sh" "$@" 2>&1
    echo "exit=$?"
}

# stops <what> <stdin> <env...> -- <args...> : the run stops (exit 1) and
# leaves HOME's files, both repositories and the cluster as they were.
LAST=""
stops() {
    local what="$1" input="$2"; shift 2
    local before_home before_origins problems=""
    build
    before_home="$(manifest "${H}")"; before_origins="$(origins)"
    LAST="$(install "${input}" "$@")"
    [[ "${LAST}" == *"exit=1" ]] || problems+=$'\n'"    wanted exit 1: $(tail -1 <<< "${LAST}")"
    [[ -s "${SB}/violations.log" ]] && problems+=$'\n'"    it ran:"$'\n'"$(sed 's/^/      /' "${SB}/violations.log" | head)"
    [[ "$(manifest "${H}")" == "${before_home}" ]] ||
        problems+=$'\n'"    HOME changed:"$'\n'"$(diff <(printf '%s\n' "${before_home}") <(manifest "${H}") | sed 's/^/      /' | head)"
    [[ "$(origins)" == "${before_origins}" ]] || problems+=$'\n'"    a repository was pushed to"
    [[ ! -e "${PROD_CHECKOUT}/clusters/test" ]] || problems+=$'\n'"    clusters/test was written into prod's checkout"
    [[ ! -e "${TEST_CHECKOUT}/clusters/prod" ]] || problems+=$'\n'"    clusters/prod was written into test's checkout"
    if grep -q '^bao \|^helm ' "${SB}/calls.log"; then problems+=$'\n'"    it went on to the cluster's vault or charts"; fi
    [[ "${LAST}" != *"Credentials collected"* && "${LAST}" != *"Cached bootstrap credentials"* ]] ||
        problems+=$'\n'"    it collected or cached credentials"
    grep -qE 'unbound variable|: command not found' <<< "${LAST}" && problems+=$'\n'"    the run broke on the way"
    if [[ -z "${problems}" ]]; then ok "${what}"; else bad "${what}" "${problems}"$'\n'"$(tail -30 <<< "${LAST}" | sed 's/^/    | /')"; fi
}
says() { if [[ "${LAST}" == *"$2"* ]]; then ok "$1"; else bad "$1" "    wanted: $2"$'\n'"$(tail -30 <<< "${LAST}" | sed 's/^/    | /')"; fi; }
quiet() { if [[ "${LAST}" != *"$2"* ]]; then ok "$1"; else bad "$1" "    did not want: $2"; fi; }

echo ""
echo "One run, one cluster: settings, checkout and cluster must agree"
echo ""

# --- 1. a shell exporting the other cluster's KUBECONFIG --------------------
stops "1. test's install.env, a shell exporting prod's KUBECONFIG: stops" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-prod" --
says "   the export is named at the start, by name" "This shell exports settings that decide which cluster this run is for:"
says "   it names the context in use and says KUBECONFIG is exported" "kube context:       ctx-prod   (KUBECONFIG is exported in this shell)"
says "   it names the API server" "API server:         https://prod.api.example.test:6443"
says "   it names the cluster the definition describes" "the definition:     test-dev, kernel domain test.example.test"
says "   it names the cluster that answered" "the cluster says:   prod-prod, kernel domain prod.example.test"
says "   it names the settings it loaded" "settings loaded:    ${SB}/test.env"
says "   it says what to do" "unset KUBECONFIG"
quiet "   it never prints a value it was given" "${SB}/kube-prod"

# --- 2. prod's settings, kubectl at the test cluster ------------------------
stops "2. prod's install.env and shell, kubectl at the test cluster: stops before the salt or the cache" "" \
    INSTALL_CONFIG_FILE="${SB}/prod.env" GENTIAN_NONINTERACTIVE=1 \
    GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" GENTIAN_CREDENTIAL_CACHE="${PROD_CACHE}" KUBECONFIG="${SB}/kube-test" --
says "   the definition is prod's" "the definition:     prod-prod, kernel domain prod.example.test"
says "   the cluster is test" "the cluster says:   test-dev, kernel domain test.example.test"
quiet "   the salt is not mentioned, let alone replaced" "using the cluster's"
if [[ "$(cat "${PROD_CACHE}")" == *"DERIVATION_SALT=prod-salt"* ]]; then ok "   prod's cache still holds prod's salt"; else bad "   prod's cache still holds prod's salt"; fi
if grep -q 'gentian-os-master-password' "${SB}/calls.log"; then bad "   the test cluster's salt was not even read"; else ok "   the test cluster's salt was not even read"; fi

stops "2b. prod's install.env in a clean shell, kubectl at the test cluster: stops at the checkout" "" \
    INSTALL_CONFIG_FILE="${SB}/prod.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-test" --
says "   the default checkout is test's repository, not the one prod's install.env names" "settings name:    ${SB}/prod.git"
if [[ "$(cat "${PROD_CACHE}")" == *"DERIVATION_SALT=prod-salt"* ]]; then ok "   prod's cache still holds prod's salt"; else bad "   prod's cache still holds prod's salt"; fi

# --- 3. a shell exporting the other cluster's deployments path -------------
stops "3. test's install.env, a shell exporting prod's GENTIAN_DEPLOYMENTS_PATH: stops" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" KERNEL_DOMAIN=test.example.test --
says "   it says the checkout is another repository" "The deployments checkout is a different repository from the one this run's settings name."
says "   and names the path and where it came from" "checkout:         ${PROD_CHECKOUT}   (GENTIAN_DEPLOYMENTS_PATH, exported in this shell)"
says "   and says what to do" "unset GENTIAN_DEPLOYMENTS_PATH KERNEL_DOMAIN"
quiet "   it does not offer to write a definition" "Writing one first"
if grep -q "^git -C ${PROD_CHECKOUT} fetch" "${SB}/calls.log"; then bad "   prod's checkout was fetched into"; else ok "   prod's checkout was not even fetched into"; fi

stops "3b. the same with prod's repository exported too: no scaffold into a repository that defines others" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" \
    GENTIAN_DEPLOYMENTS_REPO="${SB}/prod.git" KERNEL_DOMAIN=test.example.test --
says "   it names what the repository defines and what this run is" "defines:      prod"
says "   it refuses, and names the opt-in" "export GENTIAN_ADD_CLUSTER=test"

stops "3c. the same, at a terminal, answering nothing: the question defaults to No" "\n" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" \
    GENTIAN_DEPLOYMENTS_REPO="${SB}/prod.git" KERNEL_DOMAIN=test.example.test --
says "   the question names the cluster, the repository and the path" "Write a new cluster 'test' into ${SB}/prod.git at ${PROD_CHECKOUT}? [y/N]"
says "   and stops" "Not writing clusters/test into"

stops "3d. GENTIAN_ADD_CLUSTER naming another cluster does not open the door" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" \
    GENTIAN_DEPLOYMENTS_REPO="${SB}/prod.git" KERNEL_DOMAIN=test.example.test GENTIAN_ADD_CLUSTER=other --
says "   it still refuses" "Not writing clusters/test into"

# --- --force is not a way past -------------------------------------------
stops "--force does not get past it" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-prod" -- --force
says "   for the same reason" "the cluster says:   prod-prod"

# --- read-only modes report it and exit non-zero ---------------------------
for mode in --dry-run --validate --status; do
    stops "${mode}, kubectl at the other cluster: an error, exit 1, nothing written" "" \
        INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-prod" -- "${mode}"
    says "   it says why" "the cluster says:   prod-prod"
done
stops "--dry-run, a scaffold into a repository that defines others: an error, exit 1" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" \
    GENTIAN_DEPLOYMENTS_REPO="${SB}/prod.git" KERNEL_DOMAIN=test.example.test -- --dry-run
stops "--uninstall, kubectl at the other cluster: stops before the teardown" "" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-prod" -- --uninstall
says "   for the same reason" "the cluster says:   prod-prod"
quiet "   no teardown began" "Uninstalling"

# --- matching runs ----------------------------------------------------------
# passes <what> <env...> -- <args...> : exit 0 and nothing changed (read-only).
passes() {
    local what="$1"; shift
    local before_home before_origins
    before_home="$(manifest "${H}")"; before_origins="$(origins)"
    LAST="$(install "" "$@")"
    if [[ "${LAST}" == *"exit=0" && ! -s "${SB}/violations.log" && "$(manifest "${H}")" == "${before_home}" \
          && "$(origins)" == "${before_origins}" && "${LAST}" != *"[ERROR]"* ]]; then
        ok "${what}"
    else
        bad "${what}" "$(cat "${SB}/violations.log"; tail -30 <<< "${LAST}" | sed 's/^/    | /')"
    fi
}
build
passes "test's settings, test's checkout, the test cluster: --dry-run goes through" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-test" -- --dry-run
says "   it confirmed the cluster" "Cluster identity: ctx-test is test-dev (test.example.test), the cluster clusters/test describes."
says "   and says ~/.gentian/config would be written" "Would save the deployments repository's address"
says "   KUBECONFIG exported is named, as a warning only" "KUBECONFIG"
quiet "   nothing about another repository" "different repository"
passes "prod's settings and shell, the prod cluster: --dry-run goes through" \
    INSTALL_CONFIG_FILE="${SB}/prod.env" GENTIAN_NONINTERACTIVE=1 "${PROD_SHELL[@]}" -- --dry-run
says "   it confirmed the cluster" "ctx-prod is prod-prod (prod.example.test)"
# An https origin carrying a push token, and install.env naming the same
# repository without it and without .git.
"${REAL_GIT}" -C "${TEST_CHECKOUT}" remote set-url origin "https://x-access-token:s3cret@Git.Example.test/org/test.git"
printf '%s\nGENTIAN_DEPLOYMENTS_REPO=https://git.example.test/org/test\nGENTIAN_DEPLOYMENTS_BRANCH=main\nGENTIAN_DEPLOYMENTS_CLUSTER_ID=test\nGENTIAN_DEPLOYMENTS_STAGE=dev\nGENTIAN_DEPLOYMENTS_AUTH=none\n' "${OS_PIN}" > "${SB}/https.env"
passes "an https origin with a token in it is the repository install.env names without one" \
    INSTALL_CONFIG_FILE="${SB}/https.env" GENTIAN_NONINTERACTIVE=1 GIT_OFFLINE=1 KUBECONFIG="${SB}/kube-test" -- --validate
quiet "   and the token is never printed" "s3cret"
build
"${REAL_GIT}" -C "${TEST_CHECKOUT}" remote set-url origin "https://x-access-token:s3cret@git.example.test/org/other.git"
LAST="$(install "" INSTALL_CONFIG_FILE="${SB}/https.env" GENTIAN_NONINTERACTIVE=1 GIT_OFFLINE=1 KUBECONFIG="${SB}/kube-test" -- --validate)"
says "a different https origin stops the run, named without its token" "its origin:       https://git.example.test/org/other.git"
quiet "   the token is never printed" "s3cret"
build
passes "GENTIAN_ADD_CLUSTER=test lets a preview of a new cluster in prod's repository through" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_PATH="${PROD_CHECKOUT}" \
    GENTIAN_DEPLOYMENTS_REPO="${SB}/prod.git" KERNEL_DOMAIN=test.example.test GENTIAN_ADD_CLUSTER=test KUBECONFIG="${SB}/kube-fresh" -- --validate
says "   and says so" "GENTIAN_ADD_CLUSTER=test says to add it"
passes "GENTIAN_ACCEPT_CLUSTER naming the cluster that answered lets a preview through" \
    INSTALL_CONFIG_FILE="${SB}/test.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-prod" GENTIAN_ACCEPT_CLUSTER=prod-prod -- --validate
says "   and says so" "GENTIAN_ACCEPT_CLUSTER=prod-prod says they are the same cluster"

# A first install: a repository with nothing in it yet, a cluster with
# nothing on it.
build
g init --bare -b main "${SB}/new.git"
rm -rf "${TEST_CHECKOUT}"; g clone "${SB}/new.git" "${TEST_CHECKOUT}"; g -C "${TEST_CHECKOUT}" checkout -b main
printf '%s\nGENTIAN_DEPLOYMENTS_REPO=%s\nGENTIAN_DEPLOYMENTS_BRANCH=main\nGENTIAN_DEPLOYMENTS_CLUSTER_ID=test\nGENTIAN_DEPLOYMENTS_STAGE=dev\nGENTIAN_DEPLOYMENTS_AUTH=none\n' \
    "${OS_PIN}" "${SB}/new.git" > "${SB}/new.env"
passes "a first install, an empty repository, a cluster with nothing on it: --validate goes through" \
    INSTALL_CONFIG_FILE="${SB}/new.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/kube-fresh" KERNEL_DOMAIN=test.example.test -- --validate
says "   it says an install writes the definition" "An install writes it first"
quiet "   it does not ask to add a cluster" "defines other clusters"

# --- the library: addresses -------------------------------------------------
run() {
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${H}" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" REAL_GIT="${REAL_GIT}" \
        CALLS="${SB}/calls.log" VIOLATIONS="${SB}/violations.log" ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}
is() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2"$'\n'"    want: $3"; fi; }

echo ""
echo "  the comparison of addresses"
k='for u in "https://u:secret@GitHub.com/org/repo.git" "git@github.com:org/repo" "ssh://git@github.com:22/org/repo.git/" "https://github.com/org/repo"; do _gentian_repo_key "$u"; echo; done'
is "  https with a token, scp, ssh with a port, plain: one repository" "$(run -- "${k}" | sort -u)" "github.com/org/repo"
is "  a local path and its file:// form are one" "$(run -- '_gentian_repo_key /srv/git/r.git/; echo; _gentian_repo_key file:///srv/git/r; echo' | sort -u)" "/srv/git/r"
is "  another organisation is another repository" \
    "$(run -- '[[ "$(_gentian_repo_key https://github.com/a/deployments)" == "$(_gentian_repo_key https://github.com/b/deployments)" ]] && echo same || echo different')" "different"
is "  a token in an address is not printed" "$(run -- '_gentian_url_redact https://user:secret@example.test/o/r.git')" "https://example.test/o/r.git"

# --- the library: the salt and the cache -----------------------------------
# One collection's salt and cache handling, as collect_bootstrap_credentials
# runs it, after A and B have run for the given kubeconfig.
salt='load_operator_config >/dev/null; resolve_deployments_path
      gentian_check_deployments_checkout >/dev/null 2>&1 && gentian_check_one_cluster scaffold >/dev/null 2>&1 || { echo "guard=stopped"; exit; }
      _load_credential_cache >/dev/null 2>&1 || { echo "load=refused"; exit; }
      _settle_derivation_salt >/dev/null 2>&1 || { echo "salt=refused"; exit; }
      _save_credential_cache >/dev/null 2>&1; echo "salt=${DERIVATION_SALT}"'
T=(INSTALL_CONFIG_FILE="${SB}/test.env" KERNEL_DOMAIN=test.example.test)

echo ""
echo "  the derivation salt and the credential cache"
build
before="$(cat "${TEST_CACHE}")"
is "  matching: the cluster's salt, the same as the cache's" "$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" -- "${salt}")" "salt=test-salt"
is "    and the cache is rewritten with the same content" "$(cat "${TEST_CACHE}")" "${before}"

build
printf '# cluster: test test.example.test\nMASTER_PASSWORD=test-master\nDERIVATION_SALT=stale\n' > "${TEST_CACHE}"
is "  a confirmed cluster, a stale salt in its own cache: the cluster's is used (as before)" \
    "$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" -- "${salt}")" "salt=test-salt"
if grep -qx 'DERIVATION_SALT=test-salt' "${TEST_CACHE}"; then ok "    and its own cache is corrected"; else bad "    and its own cache is corrected" "$(cat "${TEST_CACHE}")"; fi

build
printf 'MASTER_PASSWORD=someone\nDERIVATION_SALT=prod-salt\n' > "${TEST_CACHE}"
before="$(cat "${TEST_CACHE}")"
is "  a confirmed cluster, a cache naming no cluster whose salt is wrong: the cluster's is used" \
    "$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" -- "${salt}")" "salt=test-salt"
is "    but that cache, which may be another cluster's, is left as it was" "$(cat "${TEST_CACHE}")" "${before}"

build
before="$(cat "${PROD_CACHE}")"
is "  test's settings with prod's cache path: the cache is not read" \
    "$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" GENTIAN_CREDENTIAL_CACHE="${PROD_CACHE}" -- "${salt}")" "load=refused"
is "    nor written" "$(cat "${PROD_CACHE}")" "${before}"
out="$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" GENTIAN_CREDENTIAL_CACHE="${PROD_CACHE}" -- \
    'load_operator_config >/dev/null; gentian_report_identity_exports >/dev/null; gentian_check_deployments_checkout; gentian_check_one_cluster scaffold >/dev/null; _load_credential_cache')"
if [[ "${out}" == *"it names:   prod prod.example.test"* && "${out}" == *"unset"*"GENTIAN_CREDENTIAL_CACHE"* ]]; then
    ok "    and it says whose it is and what to unset"
else
    bad "    and it says whose it is and what to unset" "${out}"
fi

build
is "  prod's salt in the shell, the test cluster installed but not yet answering who it is: stops" \
    "$(run "${T[@]}" KUBECONFIG="${SB}/kube-test" DERIVATION_SALT=prod-salt -- \
        'load_operator_config >/dev/null; _GENTIAN_CHECKOUT_CHECKED=x; _GENTIAN_ONE_CLUSTER_KEY=x; _GENTIAN_CLUSTER_IDENTITY=unasked
         _settle_derivation_salt >/dev/null 2>&1 || { echo refused; exit; }; echo "salt=${DERIVATION_SALT}"')" "refused"

build
rm -f "${TEST_CACHE}"
out="$(run "${T[@]}" KUBECONFIG="${SB}/kube-fresh" -- "${salt}")"
if [[ "${out}" =~ ^salt=[0-9a-f]{32}$ ]] && head -1 "${TEST_CACHE}" | grep -qx '# cluster: test test.example.test'; then
    ok "  a first install: a new salt, cached under this cluster's name"
else
    bad "  a first install: a new salt, cached under this cluster's name" "${out}"
fi

build
rm -f "${TEST_CACHE}"
run "${T[@]}" KUBECONFIG="${SB}/kube-test" DERIVATION_SALT=test-salt -- '_save_credential_cache' >/dev/null
if [[ ! -e "${TEST_CACHE}" ]]; then ok "  nothing is cached by a run that has not compared settings, checkout and cluster"; else bad "  nothing is cached by a run that has not compared settings, checkout and cluster"; fi
before="$(manifest "${H}")"
run "${T[@]}" GENTIAN_DEPLOYMENTS_REPO=https://git.example.test/d -- 'save_kubectl_gentian_config' >/dev/null
is "  nor is ~/.gentian/config written" "$(manifest "${H}")" "${before}"

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
