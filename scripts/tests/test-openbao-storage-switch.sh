#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-openbao-storage-switch.sh
# =============================================================================
# Which storage backend B-01 renders for the vault (openbao_storage_for_render,
# scripts/lib/openbao.sh), and what a new claim says.
#
# Raft started on a volume of file data is a new, empty vault, and file
# storage started on a Raft volume is too. So the claim decides for a new
# cluster, and on a cluster whose vault exists the backend changes only when
# the volume says its data was moved (the annotation the move in
# docs/guides/recovery.md §5 sets). A claim written before the field said
# nothing, which is file.
#
# The library's own function under set -u, with a kubectl on PATH that
# answers from the environment. No cluster.
# =============================================================================
# Every check is a string handed to the library's shell.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home" "${SANDBOX}/bin" "${SANDBOX}/co/clusters/c1/kernel/claims"

# kubectl as the cluster would answer the three reads: does the vault's volume
# exist, what does its annotation say, and which storage stanza does the live
# openbao Application render.
cat > "${SANDBOX}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
args="$*"
case "${args}" in
    "get pvc data-openbao-0 -n "*" -o jsonpath="*)
        [[ "${FAKE_PVC:-}" == yes ]] || exit 1
        printf '%s' "${FAKE_ANNOTATION:-}" ;;
    "get pvc data-openbao-0 -n "*)
        [[ "${FAKE_PVC:-}" == yes ]] || exit 1 ;;
    "get application openbao -n "*)
        [[ -n "${FAKE_LIVE:-}" ]] || exit 1
        printf 'ui = true\nstorage "%s" {\n  path = "/openbao/data"\n}\n' "${FAKE_LIVE}" ;;
    *) exit 1 ;;
esac
EOF
chmod +x "${SANDBOX}/bin/kubectl"

claim() { # <spec lines...>
    { printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1-dev\nspec:\n  kernelDomain: k.example\n'
      printf '%s\n' "$@"; } > "${SANDBOX}/co/clusters/c1/kernel/claims/cluster.yaml"
}

# render <env assignments...> : the backend, on stdout, and what it said, on
# stderr, folded into one line each.
render() {
    env -i HOME="${SANDBOX}/home" PATH="${SANDBOX}/bin:${PATH}" SCRIPT_DIR="${REPO}" \
        GENTIAN_DEPLOYMENTS_PATH="${SANDBOX}/co" GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1 "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
                 out="$(openbao_storage_for_render 2>"${HOME}/err")"; echo "${out}"; tr "\n" " " < "${HOME}/err"' 2>&1
}

check() { # <what> <output> <first line> [stderr must contain]
    local first rest
    first="$(head -1 <<<"$2")"; rest="$(tail -n +2 <<<"$2")"
    if [[ "${first}" == "$3" && "$2" != *"unbound variable"* && ( -z "${4:-}" || "${rest}" == *"$4"* ) && ( -n "${4:-}" || -z "${rest// /}" ) ]]; then
        printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1))
    else
        printf '  %sFAIL%s  %s\n    got: %s\n' "${RED}" "${NC}" "$1" "$2"; fail=$((fail + 1))
    fi
}

echo ""
echo "The vault's storage backend: the claim, and what the volume holds"
echo ""

claim "  openbao:" "    storage: raft"
check "a new cluster (no vault volume) whose claim says raft: raft" "$(render)" raft
claim "  networkMode: tunnel"
check "a claim that says nothing: file, the XRD's default" "$(render)" file
check "  and so on a cluster whose vault exists on file storage: file, nothing said" \
    "$(render FAKE_PVC=yes FAKE_LIVE=file)" file

claim "  openbao:" "    storage: raft"
check "the claim says raft on a vault running on file storage, volume not moved: file, and why" \
    "$(render FAKE_PVC=yes FAKE_LIVE=file)" file "would start an empty vault"
check "  the same once the move annotated the volume raft: raft" \
    "$(render FAKE_PVC=yes FAKE_LIVE=file FAKE_ANNOTATION=raft)" raft
check "a vault on Raft whose claim says raft: raft" \
    "$(render FAKE_PVC=yes FAKE_LIVE=raft)" raft

claim "  openbao:" "    storage: file"
check "the claim says file on a vault running on Raft, volume not marked back: raft, and why" \
    "$(render FAKE_PVC=yes FAKE_LIVE=raft)" raft "would start an empty vault"
check "  the rollback, the volume annotated file: file" \
    "$(render FAKE_PVC=yes FAKE_LIVE=raft FAKE_ANNOTATION=file)" file

claim "  openbao:" "    storage: rafts"
check "a value that is neither: file, and said" "$(render)" file "it is raft or file"
check "OPENBAO_STORAGE from the environment is the claim's answer, as every claim setting's is" \
    "$(render OPENBAO_STORAGE=raft)" raft

# A new claim states raft; one that knows the cluster's setting states that.
fields() {
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; _claim_cluster_fields' 2>&1
}
out="$(fields)"
check "a new claim states openbao.storage: raft" \
    "$(grep -A1 '^  openbao:$' <<<"${out}" | tail -1 | tr -d ' ')" "storage:raft"
out="$(fields OPENBAO_STORAGE=file)"
check "  and file where the setting says file" \
    "$(grep -A1 '^  openbao:$' <<<"${out}" | tail -1 | tr -d ' ')" "storage:file"

echo ""
if (( fail > 0 )); then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} passed.${NC}"
