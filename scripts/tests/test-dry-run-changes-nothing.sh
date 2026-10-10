#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-dry-run-changes-nothing.sh
# =============================================================================
# --dry-run and --validate promise to change nothing. A dry run once committed
# and pushed a cluster's definition, generated a signing key to sign it with,
# and rewrote ~/.gentian/config -- all before the first step's check() ran,
# through a function that was thought of as a check.
#
# This runs install.sh itself, in those two modes, in a sandbox:
#
#   - HOME is a temporary directory holding a deployments checkout with an
#     uncommitted edit in it (what set the accident off), cloned from a
#     repository beside it. After each run every file under HOME -- the
#     checkout's .git included -- and every ref and object of that
#     repository must be exactly what it was: same paths, same bytes, same
#     modes, same modification times.
#   - git, gpg, kubectl, helm, curl, bao and the rest are stand-ins on PATH.
#     They answer what is asked of them and record a violation for anything
#     that would change something: a commit, a push, a fetch, a key
#     generated or imported, an apply, a patch, a delete, an upgrade, a
#     request that is not a GET, a download to a file.
#   - the cluster the stand-ins describe is once an empty one and once one
#     where everything asked for is there, so the checks are walked down both
#     of their sides.
#
# This is the only way install.sh is ever executed by a test: here, against
# stand-ins, with HOME somewhere else. No cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

SB="$(mktemp -d)"
trap 'rm -rf "${SB}"' EXIT
BIN="${SB}/bin"; HOME_DIR="${SB}/home"; ORIGIN="${SB}/origin.git"
mkdir -p "${BIN}" "${HOME_DIR}/.gentian" "${SB}/tmp"
REAL_GIT="$(command -v git)"

# --- the stand-ins ----------------------------------------------------------
# Each logs its call. Anything that would change something appends to
# ${VIOLATIONS} and fails, so the run cannot go on as if it had worked.

# git: the real one for what only reads, a violation for everything else. A
# list of what is allowed, not of what is forbidden: a subcommand nobody
# thought of is refused.
cat > "${BIN}/git" <<'STUB'
#!/usr/bin/env bash
echo "git $*" >> "${CALLS}"
args=("$@"); i=0; sub=""
while (( i < ${#args[@]} )); do
    case "${args[$i]}" in
        -C|-c) i=$((i + 2)); continue ;;
        -*)    i=$((i + 1)); continue ;;
        *)     sub="${args[$i]}"; break ;;
    esac
done
rest=" ${args[*]:$((i + 1))} "
case "${sub}" in
    status|rev-parse|rev-list|ls-remote|log|merge-base|cat-file|show|diff|describe|ls-files|for-each-ref|symbolic-ref|version|"")
        exec "${REAL_GIT}" "$@" ;;
    branch)
        [[ "${rest}" == *" --show-current "* ]] && exec "${REAL_GIT}" "$@" ;;
    remote)
        [[ "${rest}" == " get-url "* || "${rest}" == "  " || "${rest}" == " -v " ]] && exec "${REAL_GIT}" "$@" ;;
    config)
        [[ "${rest}" == *" --get "* || "${rest}" == *" --get-all "* || "${rest}" == *" --list "* ]] && exec "${REAL_GIT}" "$@" ;;
esac
echo "git $*" >> "${VIOLATIONS}"
exit 1
STUB

cat > "${BIN}/gpg" <<'STUB'
#!/usr/bin/env bash
echo "gpg $*" >> "${CALLS}"
for a in "$@"; do
    case "${a}" in
        --list-keys|--list-secret-keys|--list-public-keys|--with-colons|--fingerprint|--version|--export) ;;
        --homedir|--batch|--yes|--quiet|--no-auto-check-trustdb|--pinentry-mode|--passphrase|--armor|--no-tty) ;;
        --*) echo "gpg $*" >> "${VIOLATIONS}"; exit 2 ;;
    esac
done
# Asked about a key: this keyring has none.
exit 2
STUB

cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${CALLS}"
verb=""; jsonpath=""; out=""
for a in "$@"; do
    case "${a}" in
        jsonpath=*) jsonpath="${a}" ;;
        -ojsonpath=*) jsonpath="${a}" ;;
        name|json|yaml|wide) out="${a}" ;;
    esac
    [[ -z "${verb}" && "${a}" != -* ]] && verb="${a}"
done
all=" $* "
case "${verb}" in
    cluster-info) exit 0 ;;
    version) echo '{"serverVersion":{"major":"1","minor":"99"}}'; exit 0 ;;
    config) echo "sandbox"; exit 0 ;;
    api-resources|api-versions|explain|auth|describe|logs|top) exit 0 ;;
    port-forward) exit 1 ;;
    get)
        # What every run needs whatever the cluster holds: a default
        # StorageClass and a node address.
        if [[ "${all}" == *" storageclass "* ]]; then echo "standard"; exit 0; fi
        if [[ "${all}" == *" nodes "* ]]; then echo "10.0.0.10"; exit 0; fi
        if [[ "${CLUSTER:-empty}" == "empty" ]]; then
            echo "Error from server (NotFound)" >&2; exit 1
        fi
        case "${out}:${jsonpath}" in
            json:*) echo '{"items":[]}' ;;
            name:*) echo "thing/found" ;;
            *:jsonpath=*phase*) printf 'Ready' ;;
            *:jsonpath=*health*|*:jsonpath=*sync*) printf 'Synced Healthy' ;;
            *:jsonpath=*) printf 'True' ;;
            *) echo "found" ;;
        esac
        exit 0 ;;
esac
echo "kubectl $*" >> "${VIOLATIONS}"
exit 1
STUB

cat > "${BIN}/helm" <<'STUB'
#!/usr/bin/env bash
echo "helm $*" >> "${CALLS}"
case "${1:-}" in
    list|ls)  echo '[]'; exit 0 ;;
    status|get|history) [[ "${CLUSTER:-empty}" == "empty" ]] && exit 1; echo "{}"; exit 0 ;;
    version|template|show|lint|env) exit 0 ;;
esac
echo "helm $*" >> "${VIOLATIONS}"
exit 1
STUB

cat > "${BIN}/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >> "${CALLS}"
prev=""; code_wanted=0
for a in "$@"; do
    case "${prev}" in
        -X|--request) [[ "${a}" == "GET" || "${a}" == "HEAD" ]] || { echo "curl $*" >> "${VIOLATIONS}"; exit 1; } ;;
        # A response written anywhere but /dev/null is a file this run made.
        -o|--output)  [[ "${a}" == "/dev/null" ]] || { echo "curl $*" >> "${VIOLATIONS}"; exit 1; } ;;
        -w|--write-out) code_wanted=1 ;;
    esac
    case "${a}" in
        -d|--data|--data-*|-F|--form|-T|--upload-file|-O|--remote-name|-X?*)
            [[ "${a}" == "-XGET" || "${a}" == "-XHEAD" ]] || { echo "curl $*" >> "${VIOLATIONS}"; exit 1; } ;;
    esac
    prev="${a}"
done
if [[ "${CLUSTER:-empty}" == "empty" ]]; then
    [[ ${code_wanted} -eq 1 ]] && printf '000'
    exit 7
fi
if [[ ${code_wanted} -eq 1 ]]; then printf '200'; else printf '{"initialized":true,"sealed":false}'; fi
exit 0
STUB

cat > "${BIN}/bao" <<'STUB'
#!/usr/bin/env bash
echo "bao $*" >> "${CALLS}"
case "${1:-}:${2:-}" in
    read:*|status:*|version:*|token:lookup|kv:get|kv:list|list:*|auth:list|secrets:list|policy:read|policy:list)
        [[ "${CLUSTER:-empty}" == "empty" ]] && exit 2
        echo '{"data":{"policies":["default"]}}'; exit 0 ;;
esac
echo "bao $*" >> "${VIOLATIONS}"
exit 1
STUB

# Present, because the preflight looks for them; a violation when run,
# because nothing a read-only run does should need them.
for tool in age age-keygen sudo microk8s ssh-keygen; do
    # shellcheck disable=SC2016 # written for the stand-in to expand
    printf '#!/usr/bin/env bash\necho "%s $*" >> "${CALLS}"\necho "%s $*" >> "${VIOLATIONS}"\nexit 1\n' "${tool}" "${tool}" > "${BIN}/${tool}"
done
# shellcheck disable=SC2016 # written for the stand-in to expand
printf '#!/usr/bin/env bash\necho "crossplane $*" >> "${CALLS}"\nexit 0\n' > "${BIN}/crossplane"
chmod +x "${BIN}"/*

# --- the fixture ------------------------------------------------------------
g() { "${REAL_GIT}" -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
CHECKOUT="${HOME_DIR}/.gentian/gentian-deployments"
CLUSTER_DIR="${CHECKOUT}/clusters/sandbox"

# The gentian-os the cluster would track: a local repository with the branch
# install.env names, so that confirming the ref exists needs no network.
g init --bare -b develop "${SB}/os.git"
g clone "${SB}/os.git" "${SB}/os-work"
g -C "${SB}/os-work" checkout -b develop
echo os > "${SB}/os-work/f"; g -C "${SB}/os-work" add -A; g -C "${SB}/os-work" commit -m os
g -C "${SB}/os-work" push origin develop

build_fixture() {
    rm -rf "${ORIGIN}" "${HOME_DIR}"
    mkdir -p "${HOME_DIR}/.gentian"
    g init --bare -b main "${ORIGIN}"
    g clone "${ORIGIN}" "${CHECKOUT}"
    g -C "${CHECKOUT}" checkout -b main
    mkdir -p "${CLUSTER_DIR}/kernel/claims" "${CLUSTER_DIR}/kernel/signing" "${CLUSTER_DIR}/tenants/platform" "${CLUSTER_DIR}/catalogue"
    # A claim with no spec.catalogue: an install appends the default section,
    # which is the edit the dry run made and then committed.
    cat > "${CLUSTER_DIR}/kernel/claims/cluster.yaml" <<'EOF'
apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: sandbox-dev
  namespace: kernel-provisioning
spec:
  kernelDomain: sandbox.example.test
  platformRoles:
    admin: gentian:platform:admin
  networkMode: tunnel
EOF
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Suze\nmetadata:\n  name: sandbox-dev-suze\nspec: {}\n' > "${CLUSTER_DIR}/kernel/claims/suze.yaml"
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Repository\nmetadata:\n  name: deployments\nspec:\n  type: git\n' > "${CLUSTER_DIR}/kernel/claims/deployments-repository.yaml"
    printf 'image:\n  tag: develop-abc1234\n' > "${CLUSTER_DIR}/kernel/values.yaml"
    printf 'GENTIAN_SIGNING_KEY_DIRECTOR=1111111111111111\nGENTIAN_SIGNING_KEY_BREAK_GLASS=2222222222222222\n' > "${CLUSTER_DIR}/kernel/signing/keys.env"
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: platform\n' > "${CLUSTER_DIR}/tenants/platform/tenant.yaml"
    printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n' > "${CLUSTER_DIR}/catalogue/kustomization.yaml"
    g -C "${CHECKOUT}" add -A; g -C "${CHECKOUT}" commit -m "the cluster"
    g -C "${CHECKOUT}" push origin main
    # The uncommitted edit: what an install commits and pushes.
    printf '  nodeIp: 10.0.0.10\n' >> "${CLUSTER_DIR}/kernel/claims/cluster.yaml"
    echo "an operator's note" > "${CLUSTER_DIR}/kernel/NOTES"
    case "${1:-bare}" in
        furnished)
            # A host that has installed before: a config, an init file with a
            # token, a keyring directory, a credential cache.
            printf '# written by an earlier install\nGENTIAN_DEPLOYMENTS_CLUSTER_ID="another"\n' > "${HOME_DIR}/.gentian/config"
            printf '{"root_token":"s.sandbox-root","unseal_keys_b64":["x"]}' > "${HOME_DIR}/.gentian/openbao-init.json"
            mkdir -p "${HOME_DIR}/.gentian/gnupg"; chmod 700 "${HOME_DIR}/.gentian/gnupg"
            echo "keybox" > "${HOME_DIR}/.gentian/gnupg/pubring.kbx"
            printf 'MASTER_PASSWORD=cached\n' > "${HOME_DIR}/.gentian/bootstrap-credentials.env"
            chmod 600 "${HOME_DIR}/.gentian/bootstrap-credentials.env" ;;
        first)
            # Before the first install: the checkout holds no definition of
            # this cluster at all, and nothing is uncommitted.
            rm -rf "${CHECKOUT}/clusters"
            g -C "${CHECKOUT}" add -A; g -C "${CHECKOUT}" commit -m "no cluster yet"
            g -C "${CHECKOUT}" push origin main ;;
    esac
}

cat > "${SB}/install.env" <<EOF
GENTIAN_DEPLOYMENTS_REPO=${ORIGIN}
GENTIAN_DEPLOYMENTS_BRANCH=main
GENTIAN_DEPLOYMENTS_CLUSTER_ID=sandbox
GENTIAN_DEPLOYMENTS_STAGE=dev
GENTIAN_DEPLOYMENTS_AUTH=none
GENTIAN_OS_REPO=${SB}/os.git
GENTIAN_OS_BRANCH=develop
EOF

# manifest <dir> -- every path under it with its type, mode, size,
# modification time and content hash. Two equal manifests are the same tree.
manifest() {
    python3 - "$1" <<'PY'
import hashlib, os, stat, sys
root = sys.argv[1]
for base, dirs, files in sorted(os.walk(root)):
    dirs.sort()
    for name in sorted(dirs + files):
        p = os.path.join(base, name)
        st = os.lstat(p)
        if stat.S_ISLNK(st.st_mode):
            what = "link:" + os.readlink(p)
        elif stat.S_ISREG(st.st_mode):
            with open(p, "rb") as f:
                what = "file:%d:%s" % (st.st_size, hashlib.sha256(f.read()).hexdigest())
        else:
            what = "dir"
        # A directory's own modification time moves when an entry is added or
        # removed, which the path list already shows; a file's is the write.
        mtime = st.st_mtime_ns if stat.S_ISREG(st.st_mode) else 0
        print(os.path.relpath(p, root), oct(st.st_mode), mtime, what)
PY
}
# The installer's own checkout: untracked and ignored files it would leave.
repo_state() {
    "${REAL_GIT}" -C "${REPO}" --no-optional-locks status --porcelain --ignored 2>/dev/null
    [[ -f "${REPO}/.install-state.env" ]] && cksum "${REPO}/.install-state.env" 2>/dev/null
    return 0
}

# install <cluster: empty|full> <args...> -- install.sh in the sandbox.
install() {
    local cluster="$1"; shift
    : > "${SB}/calls.log"; : > "${SB}/violations.log"
    env -i HOME="${HOME_DIR}" PATH="${BIN}:${PATH}" TMPDIR="${SB}/tmp" TERM=dumb \
        CALLS="${SB}/calls.log" VIOLATIONS="${SB}/violations.log" REAL_GIT="${REAL_GIT}" CLUSTER="${cluster}" \
        INSTALL_CONFIG_FILE="${SB}/install.env" GENTIAN_NONINTERACTIVE=1 KUBECONFIG="${SB}/no-kubeconfig" \
        GENTIAN_VALIDATE_TIMEOUT=2 ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
        bash "${REPO}/install.sh" "$@" </dev/null 2>&1
    echo "exit=$?"
}

# Further environment for the next runs, as NAME=value words.
EXTRA_ENV=()

# scenario <what> <fixture: bare|furnished|first> <cluster> <want exit: N or any> <args...>
scenario() {
    local what="$1" fixture="$2" cluster="$3" want="$4"; shift 4
    local before_home before_origin before_repo out problems=""
    build_fixture "${fixture}"
    before_home="$(manifest "${HOME_DIR}")"
    before_origin="$(manifest "${ORIGIN}")"
    before_repo="$(repo_state)"
    out="$(install "${cluster}" "$@")"
    LAST_OUT="${out}"

    [[ -s "${SB}/violations.log" ]] && problems+=$'\n'"    it ran:"$'\n'"$(sed 's/^/      /' "${SB}/violations.log" | head -20)"
    if [[ "$(manifest "${HOME_DIR}")" != "${before_home}" ]]; then
        problems+=$'\n'"    HOME changed:"$'\n'"$(diff <(printf '%s\n' "${before_home}") <(manifest "${HOME_DIR}") | sed 's/^/      /' | head -20)"
    fi
    [[ "$(manifest "${ORIGIN}")" == "${before_origin}" ]] || problems+=$'\n'"    the deployments repository's remote changed"
    [[ "$(repo_state)" == "${before_repo}" ]] || problems+=$'\n'"    the installer's own checkout changed:"$'\n'"$(repo_state | sed 's/^/      /' | head)"
    local left; left="$(find "${SB}/tmp" -mindepth 1 -maxdepth 1 | tr '\n' ' ')"
    [[ -z "${left}" ]] || problems+=$'\n'"    left in TMPDIR: ${left}"
    if [[ "${want}" != "any" && "${out}" != *"exit=${want}" ]]; then
        problems+=$'\n'"    wanted exit ${want}: $(tail -1 <<< "${out}")"
    fi
    # The shell's own two complaints, in the shell's wording ("name: command
    # not found"): the preflight says "Optional command not found: qrencode"
    # on a host without it, and that is not a fault.
    local broke; broke="$(grep -E 'unbound variable|: command not found' <<< "${out}" | head -5 || true)"
    [[ -z "${broke}" ]] || problems+=$'\n'"    the run broke on the way:"$'\n'"${broke}"

    if [[ -z "${problems}" ]]; then
        ok "${what}"
    else
        bad "${what}" "${problems}"$'\n'"$(tail -25 <<< "${out}" | sed 's/^/    | /')"
    fi
    rm -rf "${SB}/tmp"; mkdir -p "${SB}/tmp"
}
says() {   # says <what> <text>
    if [[ "${LAST_OUT}" == *"$2"* ]]; then ok "$1"; else bad "$1" "$(tail -30 <<< "${LAST_OUT}" | sed 's/^/    | /')"; fi
}
steps_checked() {
    grep -c '^     check: ' <<< "${LAST_OUT}" || true
}

echo ""
echo "--dry-run and --validate change nothing"
echo ""

# --- the stand-ins catch what they are for ----------------------------------
# A harness that reports nothing proves nothing until it is shown to report.
build_fixture bare
: > "${SB}/calls.log"; : > "${SB}/violations.log"
export CALLS="${SB}/calls.log" VIOLATIONS="${SB}/violations.log" REAL_GIT CLUSTER=full
before="$(manifest "${HOME_DIR}")"
"${BIN}/git" -C "${CHECKOUT}" commit -q --allow-empty -m x >/dev/null 2>&1
"${BIN}/git" -C "${CHECKOUT}" push -q origin HEAD:main >/dev/null 2>&1
"${BIN}/git" -C "${CHECKOUT}" fetch -q origin >/dev/null 2>&1
"${BIN}/gpg" --homedir x --quick-generate-key "a <a@b>" ed25519 sign never >/dev/null 2>&1
"${BIN}/kubectl" apply -f - </dev/null >/dev/null 2>&1
"${BIN}/kubectl" delete ns x >/dev/null 2>&1
"${BIN}/helm" upgrade --install x y >/dev/null 2>&1
"${BIN}/curl" -s -X POST https://example.test/ >/dev/null 2>&1
"${BIN}/curl" -fsSL https://example.test/a -o "${SB}/tmp/a" >/dev/null 2>&1
"${BIN}/bao" kv put -mount=secret a b=c >/dev/null 2>&1
if [[ "$(wc -l < "${SB}/violations.log" | tr -d ' ')" == "10" && "$(manifest "${HOME_DIR}")" == "${before}" ]]; then
    ok "the stand-ins record a commit, a push, a fetch, a key, an apply, a delete, an upgrade, a POST, a download and a vault write -- and carry none out"
else
    bad "the stand-ins record every mutating call" "$(cat "${SB}/violations.log")"
fi
: > "${SB}/violations.log"
"${BIN}/git" -C "${CHECKOUT}" status --porcelain >/dev/null 2>&1
"${BIN}/kubectl" get ns >/dev/null 2>&1
"${BIN}/curl" -sk https://example.test/v1/sys/health >/dev/null 2>&1
if [[ ! -s "${SB}/violations.log" ]]; then ok "and let a status, a get and a GET through"; else bad "the stand-ins refuse a read" "$(cat "${SB}/violations.log")"; fi
echo x >> "${CLUSTER_DIR}/kernel/values.yaml"
if [[ "$(manifest "${HOME_DIR}")" != "${before}" ]]; then ok "one byte appended under HOME is seen"; else bad "one byte appended under HOME is seen"; fi
unset CALLS VIOLATIONS CLUSTER

# --- the runs ---------------------------------------------------------------
scenario "--dry-run on a host with only the checkout, against an empty cluster" bare empty 0 --dry-run
says "  it reached the end of the plan" "Dry run complete"
says "  it says what an install would commit and push" "Would commit these changes in clusters/sandbox"
says "  it names the uncommitted edit" "kernel/claims/cluster.yaml"
says "  it says the claim would be given its catalogue section" "Would edit claims/cluster.yaml"
says "  it says ~/.gentian/config would be written" "Would save the deployments repository's address"
says "  it says the default profile would be placed only at the digest the catalogue's index lists" "place operations-console only if"
says "  it says an install would stop: the break-glass key keys.env records is not on this host" "Recorded:  2222222222222222"
says "    and that the preview goes on" "Would stop here for that reason; this preview goes on"
n="$(steps_checked)"
if (( n >= 25 )); then ok "  it ran the check() of ${n} steps"; else bad "  it ran the check() of only ${n} steps" "${LAST_OUT}"; fi
if [[ ! -e "${HOME_DIR}/.gentian/gnupg" && ! -e "${HOME_DIR}/.gentian/config" && ! -e "${HOME_DIR}/.local" ]]; then
    ok "  no keyring, no config and no ~/.local were made"
else
    bad "  no keyring, no config and no ~/.local were made"
fi
if ! grep -q '^gpg ' "${SB}/calls.log"; then ok "  gpg was not started at all"; else bad "  gpg was not started at all" "$(grep '^gpg ' "${SB}/calls.log")"; fi

scenario "--dry-run on a host that has installed before, against a cluster that has everything" furnished full 0 --dry-run
says "  it reached the end of the plan" "Dry run complete"
if grep -q '^kubectl get ' "${SB}/calls.log" && grep -q '^curl ' "${SB}/calls.log"; then
    ok "  it asked the cluster and OpenBao ($(grep -c '^kubectl get ' "${SB}/calls.log") gets)"
else
    bad "  it asked the cluster and OpenBao"
fi

scenario "--dry-run --only A-06 (one step)" furnished full 0 --dry-run --only A-06
# --validate reports the configuration and then runs the pre-flight. It used
# to stop after the report, whatever the report said, and to count the master
# password -- which no first install has yet -- as an error.
scenario "--validate" bare empty 0 --validate
says "  it validated the configuration" "Result:"
says "  it names the claim where the claim is" "clusters/sandbox/kernel/claims/cluster.yaml"
says "  a master password nobody has typed yet is pending, not missing" "[PENDING]  MASTER_PASSWORD"
says "  it went on to the pre-flight" "All pre-flight checks passed"
says "  and reached its end" "Validation complete"
scenario "--validate on a host that has installed before" furnished full 0 --validate
says "  it went on to the pre-flight" "All pre-flight checks passed"
EXTRA_ENV=(KERNEL_DOMAIN=sandbox.example.test)
scenario "--validate before the first install, with no definition of the cluster" first empty 0 --validate
says "  it says an install writes the definition" "no complete deployment definition yet"
says "  it went on to the pre-flight" "All pre-flight checks passed"
EXTRA_ENV=()
scenario "--validate of an unattended install with no domain to give" first empty 1 --validate
says "  it says what is missing" "[MISSING]  KERNEL_DOMAIN"
says "  and that the configuration is not ready" "config is NOT ready"

scenario "--uninstall --dry-run" furnished full 0 --uninstall --dry-run
says "  it reached the end of the teardown preview" "Teardown complete"
scenario "--purge --cluster-infra --dry-run, with no confirmation to give" furnished full 0 --purge --cluster-infra --dry-run
says "  it names what the purge would remove" "Would remove"
scenario "--purge --dry-run against an empty cluster" bare empty 0 --purge --dry-run

# A checkout that is behind its remote: reported, not fetched.
build_fixture bare
g clone "${ORIGIN}" "${SB}/other"; echo more > "${SB}/other/more"; g -C "${SB}/other" add -A
g -C "${SB}/other" commit -m "somebody else's"; g -C "${SB}/other" push origin HEAD:main
"${REAL_GIT}" -C "${CHECKOUT}" checkout -q -- . 2>/dev/null; rm -f "${CLUSTER_DIR}/kernel/NOTES"
before="$(manifest "${HOME_DIR}")"; before_origin="$(manifest "${ORIGIN}")"
out="$(install empty --dry-run)"
if [[ "${out}" == *"behind origin/main"* && ! -s "${SB}/violations.log" && "$(manifest "${HOME_DIR}")" == "${before}" \
      && "$(manifest "${ORIGIN}")" == "${before_origin}" ]]; then
    ok "a checkout behind its remote is reported as behind, and nothing is fetched into it"
else
    bad "a checkout behind its remote is reported as behind, and nothing is fetched into it" "$(cat "${SB}/violations.log"; tail -15 <<< "${out}")"
fi
rm -rf "${SB}/other" "${SB}/tmp"; mkdir -p "${SB}/tmp"

# --- the commands a dry run has no preview of -------------------------------
for combo in "--dry-run --activate-admin" "--dry-run --verify-only" "--dry-run --export-recovery-kit" \
             "--validate --activate-admin" "--validate --verify-only"; do
    # shellcheck disable=SC2086 # two flags, deliberately split
    scenario "${combo} is refused" furnished full 1 ${combo}
    says "  and says why" "has no preview"
    if [[ ! -s "${SB}/calls.log" ]] || ! grep -qE '^(kubectl|curl|bao|helm|gpg) ' "${SB}/calls.log"; then
        ok "  before anything was asked of the cluster"
    else
        bad "  before anything was asked of the cluster" "$(head -5 "${SB}/calls.log")"
    fi
done

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
