#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-release-helpers.sh
# =============================================================================
# What the installer's retry wrappers and its release paths ask of kubectl,
# helm and bao, held to scripts/tests/release-helpers.transcript.txt.
#
#   - _helm_retry, _kubectl_retry and _bao_retry: what each prints, to which
#     stream, what it returns and how often it calls, for a success with a
#     warning, a failure that is not transient, a transient failure that
#     passes on the next attempt (also under set -e, as the installer runs),
#     one that never does, and bao reading its body from _BAO_RETRY_STDIN.
#   - the paths that delete objects, wait for them and strip the finalizers
#     of what is left (A-03, A-06, B-05, E-01 and the teardown library): every
#     call they make against a cluster where everything goes at once, and
#     against one where nothing ever goes.
#
# kubectl, helm and bao are stand-ins that record their arguments; sleep moves
# the shell's clock instead of waiting, a thousand times as far as it is asked,
# so that every wait ends after its first sleep whatever the real clock does. Only the sourced library and steps
# run here, never install.sh. No cluster and no network.
#
#   UPDATE=1 scripts/tests/test-release-helpers.sh   records the transcript.
# =============================================================================
# The scripts handed to run() are for the child shell to expand:
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"
TRANSCRIPT="scripts/tests/release-helpers.transcript.txt"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'

SB="$(mktemp -d)"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home" "${SB}/tmp"

# One stand-in for the three tools. A line of ${SB}/results, when there is one,
# is the next call's answer: <rc>|<stderr>. Otherwise kubectl answers for a
# cluster where nothing is left (STUCK=0) or where everything stays (STUCK=1),
# and helm and bao succeed.
cat >"${SB}/bin/tool" <<'STUB'
#!/usr/bin/env bash
tool="$(basename "$0")"
line="${tool} $*"
if [[ ! -t 0 && "${tool}" == bao ]]; then
    body="$(cat)"
    [[ -n "${body}" ]] && line+=" <<< ${body}"
fi
printf '%s\n' "${line}" >>"${SB}/calls"
if [[ -s "${SB}/results" ]]; then
    answer="$(head -n 1 "${SB}/results")"
    tail -n +2 "${SB}/results" >"${SB}/results.next"
    mv "${SB}/results.next" "${SB}/results"
    echo "out of ${tool} $*"
    [[ -n "${answer#*|}" ]] && printf '%s\n' "${answer#*|}" >&2
    exit "${answer%%|*}"
fi
[[ "${tool}" == kubectl ]] || exit 0
case "$1" in get|api-resources) ;; *) exit 0 ;; esac
[[ "$1" == api-resources ]] && { echo "configmaps"; exit 0; }
if [[ "${STUCK:-0}" != 1 ]]; then
    [[ " $* " == *" -o json "* ]] && echo '{"items":[]}'
    exit 0
fi
case " $* " in
    *" -o json "*)
        echo '{"items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"obj1","namespace":"ns1",
          "deletionTimestamp":"2026-01-01T00:00:00Z","ownerReferences":[{"kind":"ProviderRevision","name":"r"}]}}]}' ;;
    *'{.metadata.namespace} {.metadata.name}'*) echo "ns1 obj1" ;;
    *'{"|"}'*) echo "ns1|obj1" ;;
    *jsonpath*) echo "obj1.example" ;;
    *" get crd -o name "*) echo "customresourcedefinition.apiextensions.k8s.io/providerconfigs.example" ;;
    *"-o name"*) echo "thing.example/obj1" ;;
    *custom-columns*) echo "tenant1" ;;
    *--no-headers*) echo "ns1 obj1 x" ;;
    *) echo "obj1" ;;
esac
exit 0
STUB
chmod +x "${SB}/bin/tool"
for t in kubectl helm bao; do ln -s tool "${SB}/bin/${t}"; done

# run <script> [VAR=value ...] -- the installer's libraries and steps, as
# install.sh loads them, with the stand-ins first on PATH and a clock that
# sleep moves.
run() {
    local script="$1"; shift
    : >"${SB}/calls"
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="${REPO}" TMPDIR="${SB}/tmp" \
        SB="${SB}" SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh" >/dev/null 2>&1
            trap - ERR; set +e
            _clock=0
            sleep() { _clock=$((_clock + 1000 * ${1%.*})); SECONDS="${_clock}"; }
            eval "${SCRIPT}"
        ' >"${SB}/out" 2>"${SB}/err" </dev/null
    echo "rc=$?"
    sed 's/^/  out: /' "${SB}/out"
    sed 's/^/  err: /' "${SB}/err"
    sed 's/^/  call: /' "${SB}/calls"
}

# retry <wrapper> <name> <results...> -- one wrapper against the answers given.
retry() {
    local wrapper="$1" name="$2" r; shift 2
    : >"${SB}/results"
    for r in "$@"; do printf '%s\n' "${r}" >>"${SB}/results"; done
    echo "== ${wrapper}: ${name}"
    run "${wrapper} version --short" HELM_RETRY_DELAY_SECS=1 KUBECTL_RETRY_DELAY_SECS=1 BAO_RETRY_DELAY_SECS=1 \
        HELM_RETRY_ATTEMPTS=3 KUBECTL_RETRY_ATTEMPTS=3 BAO_RETRY_ATTEMPTS=3
}

# release <name> <script> -- a release path against both clusters.
release() {
    local name="$1" script="$2" stuck
    rm -f "${SB}/results"
    for stuck in 0 1; do
        echo "== ${name}, STUCK=${stuck}"
        run "${script}" STUCK="${stuck}"
    done
}

transcript() {
    local w
    for w in _helm_retry _kubectl_retry _bao_retry; do
        retry "${w}" "success with a warning" "0|Warning: something"
        retry "${w}" "not transient" "1|Error: chart not found"
        retry "${w}" "transient, then success" "1|dial tcp 10.0.0.1:443: connection refused" "0|"
        retry "${w}" "transient every time" "1|dial tcp: i/o timeout" "1|dial tcp: i/o timeout" "1|dial tcp: i/o timeout"
    done
    # The installer runs under set -e, as load.sh sets it.
    for w in _helm_retry _kubectl_retry _bao_retry; do
        printf '%s\n' "1|connection refused" "0|" >"${SB}/results"
        echo "== ${w}: transient, then success, under set -e"
        run "set -e; ${w} version --short; echo carried on" HELM_RETRY_DELAY_SECS=1 KUBECTL_RETRY_DELAY_SECS=1 BAO_RETRY_DELAY_SECS=1
    done
    printf '%s\n' "1|connection refused" "0|" >"${SB}/results"
    echo "== _bao_retry: body from _BAO_RETRY_STDIN, read again on the retry"
    run '_BAO_RETRY_STDIN="path \"x\" {}" _bao_retry policy write p -' BAO_RETRY_DELAY_SECS=1

    release "A-03 external secrets" 'source scripts/steps/A-03-external-secrets.sh; _a03_release_external_secrets'
    release "A-06 applications" 'source scripts/steps/A-06-argocd.sh; _a06_release_applications argocd'
    release "B-05 managed resources" 'source scripts/steps/B-05-crossplane-providers.sh; _b05_sweep_managed'
    release "B-05 provider configs" 'source scripts/steps/B-05-crossplane-providers.sh; _b05_release_provider_configs'
    release "B-05 terminating CRDs" 'source scripts/steps/B-05-crossplane-providers.sh; _b05_release_terminating_provider_crds'
    release "E-01 tenants" 'source scripts/steps/E-01-tenants.sh; destroy'
    release "teardown: namespace contents" 'GENTIAN_NS_DRAIN_WAIT=10; _ns_purge_contents ns1'
    release "teardown: PVCs" '_drain_pvcs ns1'
    release "teardown: CRDs matching" '_delete_crds_matching "example$"'
    release "teardown: Crossplane finalizers" '_purge_strip_crossplane_finalizers'
}

echo ""
echo "release helpers: what the retry wrappers and the release paths ask of the tools"
echo ""
got="$(transcript)"
if [[ "${UPDATE:-}" == 1 ]]; then
    printf '%s\n' "${got}" >"${TRANSCRIPT}"
    echo "recorded ${TRANSCRIPT}"
    exit 0
fi
if [[ "${got}" == "$(cat "${TRANSCRIPT}")" ]]; then
    printf '  %sok%s    the transcript is %s\n' "${GREEN}" "${NC}" "${TRANSCRIPT}"
    exit 0
fi
printf '  %sFAIL%s  the transcript differs from %s (UPDATE=1 records it)\n' "${RED}" "${NC}" "${TRANSCRIPT}"
diff <(printf '%s\n' "${got}") "${TRANSCRIPT}"
exit 1
