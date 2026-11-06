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

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home" "${SANDBOX}/bin" "${SANDBOX}/co/clusters/c1/kernel/claims"

# kubectl as the cluster would answer the three reads: does the instance's
# volume exist (FAKE_PVC names the one that does), what does its annotation
# say, and which storage stanza does its Application (FAKE_APP, openbao by
# default) render: FAKE_LIVE, "none" for an Application that names no
# configuration, empty for no Application. What does not exist is answered
# NotFound, as the API server does. FAKE_FAIL makes one read fail otherwise,
# as an API server that cannot be reached does: pvc (the volume),
# annotation (its annotation) or application.
cat > "${SANDBOX}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
args="$*"
unreachable() { echo 'Unable to connect to the server: dial tcp 10.0.0.1:6443: i/o timeout' >&2; exit 1; }
notfound() { echo "Error from server (NotFound): $1 \"$2\" not found" >&2; exit 1; }
case "${args}" in
    "get pvc ${FAKE_PVC:-none} -n "*" -o jsonpath="*)
        [[ "${FAKE_FAIL:-}" == annotation ]] && unreachable
        printf '%s' "${FAKE_ANNOTATION:-}" ;;
    "get pvc "*)
        [[ "${FAKE_FAIL:-}" == pvc ]] && unreachable
        [[ "${args}" == "get pvc ${FAKE_PVC:-none} -n "* ]] || notfound persistentvolumeclaims "$3"
        echo "persistentvolumeclaim/$3" ;;
    "get application "*)
        [[ "${FAKE_FAIL:-}" == application ]] && unreachable
        [[ "${args}" == "get application ${FAKE_APP:-openbao} -n "* && -n "${FAKE_LIVE:-}" ]] ||
            notfound applications.argoproj.io "$3"
        [[ "${FAKE_LIVE}" == none ]] && exit 0
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
                 rc=0; out="$(openbao_storage_for_render "${INSTANCE:-openbao}" 2>"${HOME}/err")" || rc=$?
                 echo "${out}"; tr "\n" " " < "${HOME}/err"; if (( rc != 0 )); then echo "exit ${rc}"; fi' 2>&1
}

check() { # <what> <output> <first line> [stderr must contain]
    local first rest
    first="$(head -1 <<<"$2")"; rest="$(tail -n +2 <<<"$2")"
    if [[ "${first}" == "$3" && "$2" != *"unbound variable"* && ( -z "${4:-}" || "${rest}" == *"$4"* ) && ( -n "${4:-}" || -z "${rest// /}" ) ]]; then
        ok "$1"
    else
        bad "$1" "    got: $2"
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
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" file

claim "  openbao:" "    storage: raft"
check "the claim says raft on a vault running on file storage, volume not moved: file, and why" \
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" file "would start it empty"
check "  the same once the move annotated the volume raft: raft" \
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=file FAKE_ANNOTATION=raft)" raft
check "a vault on Raft whose claim says raft: raft" \
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=raft)" raft

claim "  openbao:" "    storage: file"
check "the claim says file on a vault running on Raft, volume not marked back: raft, and why" \
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=raft)" raft "would start it empty"
check "  the rollback, the volume annotated file: file" \
    "$(render FAKE_PVC=data-openbao-0 FAKE_LIVE=raft FAKE_ANNOTATION=file)" file

check "a vault volume whose Application is gone: the claim answers" \
    "$(render OPENBAO_STORAGE=raft FAKE_PVC=data-openbao-0)" raft

# The seal, decided by its own volume. Its Application named no
# configuration before the seal's storage followed the claim: file.
claim "  openbao:" "    storage: raft"
check "the seal on a new cluster whose claim says raft: raft" "$(render INSTANCE=openbao-transit)" raft
check "the seal whose Application names no configuration, volume not moved: file, and why" \
    "$(render INSTANCE=openbao-transit FAKE_APP=openbao-transit FAKE_PVC=data-openbao-transit-0 FAKE_LIVE=none)" \
    file "data-openbao-transit-0"
check "  the same once its volume is annotated raft: raft" \
    "$(render INSTANCE=openbao-transit FAKE_APP=openbao-transit FAKE_PVC=data-openbao-transit-0 FAKE_LIVE=none FAKE_ANNOTATION=raft)" raft
check "the seal on Raft whose claim says raft: raft" \
    "$(render INSTANCE=openbao-transit FAKE_APP=openbao-transit FAKE_PVC=data-openbao-transit-0 FAKE_LIVE=raft)" raft
check "the vault's volume says nothing about the seal's" \
    "$(render INSTANCE=openbao-transit FAKE_APP=openbao FAKE_PVC=data-openbao-0 FAKE_LIVE=file FAKE_ANNOTATION=raft)" raft

# Absent is what the API server says is NotFound, nothing else. A read that
# fails otherwise stops the render: taking it for "no volume" would start the
# claim's backend on a volume of the other's data, an empty vault.
claim "  openbao:" "    storage: raft"
check "the volume cannot be read (API server unreachable): nothing rendered, the step fails, and why" \
    "$(render FAKE_FAIL=pvc FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" "" "could not be read"
check "  and the failure is the exit status" \
    "$(render FAKE_FAIL=pvc FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" "" "exit 1"
check "the volume exists, its Application cannot be read: nothing rendered, and why" \
    "$(render FAKE_FAIL=application FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" "" "Application openbao"
check "the volume's annotation cannot be read: nothing rendered, and why" \
    "$(render FAKE_FAIL=annotation FAKE_PVC=data-openbao-0 FAKE_LIVE=file)" "" "i/o timeout"
check "the seal's volume cannot be read: the same for the seal" \
    "$(render INSTANCE=openbao-transit FAKE_FAIL=pvc FAKE_APP=openbao-transit FAKE_PVC=data-openbao-transit-0 FAKE_LIVE=file)" \
    "" "data-openbao-transit-0"

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

finish
