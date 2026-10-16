#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-operator-objects.sh
# =============================================================================
# The catalogue and the tenants sync gentianos.io kinds whose CRDs arrive with
# the operator (D-01), from ApplicationSets created before it (C-02). On a
# fresh install tenant-test and gentian-catalogue-dev failed with
# `<Kind>.gentianos.io "" not found`, spent their retries, were never tried
# again, and the install reported success.
#
# Held here, against a stand-in kubectl that keeps Applications as files:
#
#   - E-05's check is not satisfied while one of those Applications is not
#     Synced, while a tenant's Application is Synced and its Tenant is not
#     there, or while an ApplicationSet reports an error; it is once all are;
#   - E-05 asks Argo CD to sync a failed one again, and succeeds when it then
#     syncs;
#   - after the handover, a tenant that does not arrive fails the step, naming
#     the tenant and Argo CD's message;
#   - before it, a tenant other than the platform's is reported held back and
#     not asked for, and the check stays not satisfied;
#   - D-01 asks again for a failed catalogue once the operator is up.
#
# No cluster.
# =============================================================================
# The scripts handed to run() are for the child shell to expand:
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
expect() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

command -v jq >/dev/null || { echo "jq required"; exit 1; }

echo ""
echo "operator objects: the catalogue and the tenants are synced at the end, or the install says why not"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gto.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"

# ${STATE}/apps/<name>.json      an Application
# ${STATE}/appsets/<name>.json   an ApplicationSet
# ${STATE}/tenants/<name>        a Tenant
# ${STATE}/heal/<name>           a sync asked of <name> succeeds (and creates
#                                its Tenant); without it the sync fails again
# ${STATE}/handover              the handover is proven
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${STATE}/calls.log"
verb="${1:-}"; shift || true
out=""; args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|-p|--type) shift ;;
        -o) out="$2"; shift ;;
        --*) ;;
        *) args+=("$1") ;;
    esac
    shift
done
kind="${args[0]:-}"; name="${args[1]:-}"
case "${verb}:${kind}" in
    get:applications)
        jq -s '{items: .}' "${STATE}"/apps/*.json 2>/dev/null || echo '{"items":[]}' ;;
    get:application)
        f="${STATE}/apps/${name}.json"; [[ -f "${f}" ]] || exit 1
        case "${out}" in
            json) cat "${f}" ;;
            *sync.status*) jq -j '.status.sync.status' "${f}" ;;
        esac ;;
    get:applicationset)
        f="${STATE}/appsets/${name}.json"; [[ -f "${f}" ]] || exit 1; cat "${f}" ;;
    get:tenants.gentianos.io|get:tenant)
        [[ -f "${STATE}/tenants/${name}" ]] || exit 1 ;;
    get:configmap)
        [[ -f "${STATE}/handover" ]] && printf 'true' ;;
    patch:application)
        f="${STATE}/apps/${name}.json"
        if [[ -f "${STATE}/heal/${name}" ]]; then
            jq '.status.sync.status = "Synced" | .status.operationState = {phase: "Succeeded", message: "ok"}' "${f}" > "${f}.new" && mv "${f}.new" "${f}"
            case "${name}" in tenant-*) touch "${STATE}/tenants/${name#tenant-}" ;; esac
        fi ;;
esac
exit 0
STUB
printf '#!/bin/sh\nexec /bin/sleep 0.05\n' > "${SB}/bin/sleep"
chmod +x "${SB}/bin/kubectl" "${SB}/bin/sleep"

# app <name> <owner appset> <sync> <phase> <message>
app() {
    jq -n --arg n "$1" --arg o "$2" --arg s "$3" --arg p "$4" --arg m "$5" \
        '{metadata: {name: $n, ownerReferences: [{kind: "ApplicationSet", name: $o}]},
          status: {sync: {status: $s}, operationState: {phase: $p, message: $m}}}' \
        > "${STATE}/apps/$1.json"
}
appset() { echo '{"status":{"conditions":[]}}' > "${STATE}/appsets/$1.json"; }
CRD_GONE='one or more synchronization tasks are not valid: Tenant.gentianos.io "" not found (retried 10 times).'
PROFILE_GONE='one or more synchronization tasks are not valid: ComponentProfile.gentianos.io "" not found (retried 10 times).'

# fresh — a cluster where both ApplicationSets generate, the platform tenant
# is there, and nothing else is decided yet.
fresh() {
    STATE="$(mktemp -d "${SB}/state.XXXXXX")"
    mkdir -p "${STATE}/apps" "${STATE}/appsets" "${STATE}/tenants" "${STATE}/heal"
    : > "${STATE}/calls.log"
    appset gentian-catalogue; appset gentian-tenants
    app tenant-platform gentian-tenants Synced Succeeded ok; touch "${STATE}/tenants/platform"
}

# run <step> <shell> — the step loaded as the driver loads it, then <shell>.
run() {
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" STATE="${STATE}" STEP="$1" \
        GENTIAN_OPERATOR_OBJECTS_WAIT_SECS=1 \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
source "scripts/steps/${STEP}.sh"
'"$2" 2>&1
}
asked() { grep -q "kubectl patch application $1 " "${STATE}/calls.log"; }

# 1. Everything synced and there.
fresh
app gentian-catalogue-dev gentian-catalogue Synced Succeeded ok
app tenant-test gentian-tenants Synced Succeeded ok; touch "${STATE}/tenants/test"
out="$(run E-05-operator-objects 'check; echo "rc=$?"' | tail -1)"
expect "everything synced and every Tenant there: satisfied" '[[ "${out}" == "rc=0" ]]' "${out}"

# 2. The catalogue failed before its CRD existed.
fresh
app gentian-catalogue-dev gentian-catalogue OutOfSync Failed "${PROFILE_GONE}"
out="$(run E-05-operator-objects 'check; echo "rc=$?"' | tail -1)"
expect "a failed catalogue: not satisfied" '[[ "${out}" == "rc=1" ]]' "${out}"
touch "${STATE}/heal/gentian-catalogue-dev"
out="$(run E-05-operator-objects 'apply; echo "rc=$?"')"
expect "a failed catalogue is asked again, and the step succeeds once it syncs" \
    '[[ "${out}" == *"rc=0"* ]] && asked gentian-catalogue-dev' "${out}"

# 3. A tenant Synced on paper with no Tenant: not satisfied.
fresh; touch "${STATE}/handover"
app tenant-test gentian-tenants Synced Succeeded ok
out="$(run E-05-operator-objects 'check; echo "rc=$?"' | tail -1)"
expect "a tenant's Application Synced but no Tenant: not satisfied" '[[ "${out}" == "rc=1" ]]' "${out}"

# 4. After the handover, a tenant that stays failed fails the step loudly.
fresh; touch "${STATE}/handover"
app tenant-test gentian-tenants OutOfSync Failed "${CRD_GONE}"
out="$(run E-05-operator-objects 'apply; echo "rc=$?"')"
expect "a declared tenant that does not arrive fails the step" '[[ "${out}" == *"rc=1"* ]]' "${out}"
expect "  naming the tenant and Argo CD's message" \
    '[[ "${out}" == *"tenant-test"* && "${out}" == *"Tenant.gentianos.io \"\" not found"* && "${out}" == *"Tenant/test does not exist"* ]]' "${out}"
expect "  having asked Argo CD to sync it again" 'asked tenant-test' "$(cat "${STATE}/calls.log")"

# 5. After the handover, a failed tenant is asked again and arrives.
fresh; touch "${STATE}/handover"
app tenant-test gentian-tenants OutOfSync Failed "${CRD_GONE}"; touch "${STATE}/heal/tenant-test"
out="$(run E-05-operator-objects 'apply; echo "rc=$?"; check; echo "check=$?"')"
expect "a failed tenant is asked again, arrives, and the step is satisfied" \
    '[[ "${out}" == *"rc=0"* && "${out}" == *"check=0"* && -f "${STATE}/tenants/test" ]]' "${out}"

# 6. Before the handover: held back, not asked, not satisfied.
fresh
app tenant-test gentian-tenants OutOfSync Failed "admission webhook denied the request"
out="$(run E-05-operator-objects 'apply; echo "rc=$?"; check; echo "check=$?"')"
expect "before the handover a tenant is reported held back, and the check stays not satisfied" \
    '[[ "${out}" == *"held back"* && "${out}" == *"Tenant/test"* && "${out}" == *"rc=0"* && "${out}" == *"check=1"* ]]' "${out}"
expect "  and it is not asked for" '! asked tenant-test' "$(cat "${STATE}/calls.log")"

# 7. An ApplicationSet that cannot generate: not satisfied, and named on failure.
fresh; touch "${STATE}/handover"
echo '{"status":{"conditions":[{"type":"ErrorOccurred","status":"True","message":"repository not found"}]}}' > "${STATE}/appsets/gentian-tenants.json"
out="$(run E-05-operator-objects 'check; echo "check=$?"; apply; echo "rc=$?"')"
expect "an ApplicationSet in error: not satisfied, and the step fails naming it" \
    '[[ "${out}" == *"check=1"* && "${out}" == *"rc=1"* && "${out}" == *"ApplicationSet gentian-tenants: repository not found"* ]]' "${out}"

# 8. D-01 asks again for the catalogue once the operator is up.
fresh
app gentian-catalogue-dev gentian-catalogue OutOfSync Failed "${PROFILE_GONE}"
run D-01-operator 'resync_operator_kind_apps "$(ns_kernel gitops)"' >/dev/null
expect "D-01's resync asks for the failed catalogue and leaves a synced tenant alone" \
    'asked gentian-catalogue-dev && ! asked tenant-platform' "$(cat "${STATE}/calls.log")"
expect "D-01 calls that resync once the operator is running" \
    'grep -q "^    resync_operator_kind_apps \"\${ns}\"" scripts/steps/D-01-operator.sh'

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} checks passed.${NC}"
