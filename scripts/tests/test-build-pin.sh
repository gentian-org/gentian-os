#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-build-pin.sh
# =============================================================================
# The installer pins the platform's image to the build of the commit it is run
# from, and a run from a newer checkout is how a cluster takes a newer build.
# B-01 writes the pin and never asked whether it was still this checkout's: a
# run from a newer checkout skipped the step, `--only B-01` with it, and the
# cluster kept the old binary under new manifests.
#
# Held here, against a stand-in kubectl:
#
#   - B-01's check is not satisfied while the Application names another build,
#     and is once it names this one;
#   - a run that resolved no build, and a cluster with no Application yet,
#     have nothing to compare and are not held up by it;
#   - D-01's check is not satisfied while a Deployment still runs the build
#     before, though the Application is Synced and Healthy and names the new.
#
# No cluster.
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
echo "build pin: a run from a newer checkout is not satisfied with the build before"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gbp.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"

# Everything the two checks ask is answered as in place, except the two
# things under test: the tag the Application names (PINNED, or no Application
# at all when it is "none") and the image the Deployments run (RUNNING).
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
all="$*"
case "${all}" in
    *"get application gentian-os"*)
        [[ "${PINNED:-}" == "none" ]] && exit 1
        case "${all}" in
            *"image.tag"*)             printf '%s' "${PINNED:-}" ;;
            *"kernelNetworkPolicies"*) printf 'false' ;;
            *"sync.status"*)           printf 'Synced Healthy' ;;
        esac ;;
    *"get application"*)   [[ "${all}" == *"sync.status"* ]] && printf 'Synced Healthy' ;;
    *"get configmap gentian-kernel-services"*) printf 'false' ;;
    # The vault's TLS, settled: signed by its CA, which is handed out.
    *"get certificate openbao-tls"*)
        case "${all}" in
            *"issuerRef"*) printf 'openbao-ca' ;;
            *"Ready"*)     printf 'True' ;;
        esac ;;
    *"get secret"*"{.data."*) printf 'Q0E=' ;;
    *"get deployment"*)
        case "${all}" in
            *"containers[0].image"*) printf 'ghcr.io/gentian-org/gentian-os:%s' "${RUNNING:-}" ;;
            *"readyReplicas"*)       printf '1' ;;
        esac ;;
esac
exit 0
STUB
chmod +x "${SB}/bin/kubectl"

# verdict <step> [VAR=value ...] — that step's check(), as the driver loads it.
verdict() {
    local step="$1"; shift
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" STEP="${step}" "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
source "scripts/steps/${STEP}.sh"
check; echo "rc=$?"' 2>&1 | tail -1
}

NEW=develop-bbbbbbb; OLD=develop-aaaaaaa
out="$(verdict B-01-bootstrap-apps GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=${NEW})"
check "B-01: the Application names this checkout's build: satisfied" '[[ "${out}" == "rc=0" ]]' "${out}"
out="$(verdict B-01-bootstrap-apps GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=${OLD})"
check "B-01: it names the build before: not satisfied, so the run moves the pin" '[[ "${out}" == "rc=1" ]]' "${out}"
out="$(verdict B-01-bootstrap-apps PINNED=${OLD})"
check "B-01: a run that resolved no build has nothing to compare" '[[ "${out}" == "rc=0" ]]' "${out}"
out="$(verdict B-01-bootstrap-apps GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=none)"
check "B-01: no Application yet (the operator is D-01's): not held up by the build" '[[ "${out}" == "rc=0" ]]' "${out}"

out="$(verdict D-01-operator GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=${NEW} RUNNING=${NEW})"
check "D-01: pinned and running this checkout's build: satisfied" '[[ "${out}" == "rc=0" ]]' "${out}"
out="$(verdict D-01-operator GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=${NEW} RUNNING=${OLD})"
check "D-01: pinned, Synced and Healthy, and still running the build before: not satisfied" '[[ "${out}" == "rc=1" ]]' "${out}"
out="$(verdict D-01-operator GENTIAN_OS_IMAGE_TAG=${NEW} PINNED=${OLD} RUNNING=${OLD})"
check "D-01: the pin itself is the build before: not satisfied" '[[ "${out}" == "rc=1" ]]' "${out}"

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} checks passed.${NC}"
