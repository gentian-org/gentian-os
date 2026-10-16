#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-namespace-teardown.sh
# =============================================================================
# A namespace is removed only once nothing is stored in it, and a purge says so
# before it reports success. A purge on MicroK8s cleared kernel-edge's
# finalizer through /finalize while the namespace still held cert-manager,
# external-dns, an Envoy proxy and their Secrets; the namespace object went,
# its contents stayed in the datastore, and they reappeared in the kernel-edge
# A-01 created next — where A-02 found cert-manager's release record and
# called a cert-manager with no running pod installed.
#
# What is held here, against a stand-in for kubectl that keeps namespaces and
# objects as files:
#
#   _delete_namespace deletes every object of every listed kind itself, strips
#   the finalizers of what will not go, and finalizes the namespace only once
#   it is empty; one that cannot be emptied is reported and never finalized.
#   It finds objects stored under a namespace that is already gone, and lists
#   kind by kind when one kind makes the combined list fail.
#   purge_verify_namespaces_gone fails, naming each survivor, while anything
#   is left, and passes once nothing is.
#   A-01 warns about objects older than their namespace, with the command that
#   removes them, and removes nothing.
#   A-02..A-05 check() are not satisfied by a release record whose Deployments
#   have no available replica, and are by one whose Deployments do.
# =============================================================================
# The scripts handed to run() are for the child shell to expand, and ok/bad
# never fail, so `cond && ok || bad` is the if-then-else it reads as:
# shellcheck disable=SC2016,SC2015
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

command -v jq >/dev/null || { echo "jq required"; exit 1; }

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
BIN="${SANDBOX}/bin"; mkdir -p "${BIN}"

# kubectl over ${STATE}:
#   ns/<name>             a namespace; its content is its creationTimestamp,
#                         ns/<name>.terminating marks it deleted
#   objs/<ns>/<ref>       an object, ref "<kind>[.<group>]%<name>" (kind in
#                         lower case, as kubectl -o name prints it); lines
#                         ts=<creationTimestamp>, and optionally fin (holds a
#                         finalizer), immortal (never goes), deleting
# The namespace controller is stalled, as when discovery of an aggregated API
# fails: deleting a namespace deletes nothing inside it. An emptied
# Terminating namespace goes once asked about, unless STUCK_NS=1, when only
# /finalize removes it. /finalize while objects remain is logged as
# FINALIZED-OVER-CONTENTS, which is the defect.
cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${STATE}/calls.log"
verb="${1:-}"; shift || true
ns=""; out=""; args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n) ns="$2"; shift ;;
        -o) out="$2"; shift ;;
        -p|-f|--type) shift ;;
        --*|-) ;;
        *) args+=("$1") ;;
    esac
    shift
done
objdir="${STATE}/objs/${ns}"
obj_json() {   # <ref-file>
    local f="$1" ref kind group name ts api
    ref="$(basename "${f}")"; name="${ref#*%}"; kind="${ref%%%*}"
    group=""; [[ "${kind}" == *.* ]] && { group="${kind#*.}"; kind="${kind%%.*}"; }
    api="v1"; [[ -n "${group}" ]] && api="${group}/v1"
    ts="$(sed -n 's/^ts=//p' "${f}")"
    jq -n --arg a "${api}" --arg k "${kind^}" --arg n "${name}" --arg t "${ts}" \
        '{apiVersion:$a, kind:$k, metadata:{name:$n, creationTimestamp:$t}}'
}
ref_file() {   # <kubectl ref kind/name> -> file
    local kind="${1%%/*}" name="${1#*/}"
    printf '%s/%s%%%s' "${objdir}" "${kind}" "${name}"
}
ns_empty() { [[ -z "$(ls -A "${STATE}/objs/$1" 2>/dev/null)" ]]; }
case "${verb}" in
    api-resources)
        printf '%s\n' configmaps secrets deployments.apps events.events.k8s.io ${EXTRA_KINDS:-} ;;
    get)
        kind="${args[0]:-}"; name="${args[1]:-}"
        case "${kind}" in
            namespace|namespaces|ns)
                f="${STATE}/ns/${name}"
                [[ -f "${f}" ]] || exit 1
                if [[ -f "${f}.terminating" ]] && ns_empty "${name}" && [[ "${STUCK_NS:-0}" != "1" ]]; then
                    rm -f "${f}" "${f}.terminating"; exit 1
                fi
                case "${out}" in
                    jsonpath=*creationTimestamp*) cat "${f}" ;;
                    json) jq -n --arg n "${name}" '{metadata:{name:$n},spec:{finalizers:["kubernetes"]}}' ;;
                esac
                exit 0 ;;
            deployment)
                printf '%s' "$(cat "${STATE}/avail/${name}" 2>/dev/null || echo 0)"; exit 0 ;;
            crd) exit 0 ;;
        esac
        [[ "${out}" == json ]] || exit 0
        # A list of kinds; one the API no longer has fails the whole call.
        [[ ",${kind}," == *,widgets.example.io,* ]] && { echo "error: the server doesn't have a resource type" >&2; exit 1; }
        items="[]"
        for k in ${kind//,/ }; do
            # Listed by resource (secrets), stored by kind (secret).
            r="${k%%.*}"; g=""; [[ "${k}" == *.* ]] && g=".${k#*.}"
            for f in "${objdir}/${r%s}${g}%"*; do
                [[ -f "${f}" ]] || continue
                items="$(jq --argjson o "$(obj_json "${f}")" '. + [$o]' <<< "${items}")"
            done
        done
        jq -n --argjson i "${items}" '{kind:"List", items:$i}' ;;
    delete)
        if [[ "${args[0]:-}" == namespace ]]; then
            [[ -f "${STATE}/ns/${args[1]}" ]] && touch "${STATE}/ns/${args[1]}.terminating"
            exit 0
        fi
        for ref in "${args[@]}"; do
            [[ "${ref%%/*}" == widgets.example.io ]] && exit 1
            f="$(ref_file "${ref}")"
            [[ -f "${f}" ]] || continue
            grep -qx immortal "${f}" && continue
            if grep -qx fin "${f}"; then echo deleting >> "${f}"; else rm -f "${f}"; fi
        done ;;
    patch)
        f="$(ref_file "${args[0]}")"
        [[ -f "${f}" ]] || exit 1
        sed -i '/^fin$/d' "${f}"
        grep -qx deleting "${f}" && ! grep -qx immortal "${f}" && rm -f "${f}"
        exit 0 ;;
    replace)
        n="$(sed -n 's#^/api/v1/namespaces/\([^/]*\)/finalize$#\1#p' <<< "${args[0]:-}")"
        ns_empty "${n}" || echo "FINALIZED-OVER-CONTENTS ${n}" >> "${STATE}/defects.log"
        rm -f "${STATE}/ns/${n}" "${STATE}/ns/${n}.terminating" ;;
    create|label)
        [[ "${args[0]}" == namespace ]] && [[ ! -f "${STATE}/ns/${args[1]}" ]] && echo "2026-10-16T05:47:17Z" > "${STATE}/ns/${args[1]}"
        exit 0 ;;
    *) exit 0 ;;
esac
STUB
chmod +x "${BIN}/kubectl"

# helm: every release deployed at whatever version the caller pins.
cat > "${BIN}/helm" <<'STUB'
#!/usr/bin/env bash
[[ "$1" == list ]] && { printf '[{"name":"%s","status":"deployed","chart":"%s-%s"}]' "${REL}" "${REL}" "${PIN}"; exit 0; }
exit 0
STUB
chmod +x "${BIN}/helm"

new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    mkdir -p "${STATE}/ns" "${STATE}/objs" "${STATE}/avail"
    : > "${STATE}/calls.log"; : > "${STATE}/defects.log"
}
# ns_add <name> [<created>]
ns_add() { echo "${2:-2026-10-16T05:47:17Z}" > "${STATE}/ns/$1"; mkdir -p "${STATE}/objs/$1"; }
# obj_add <ns> <kind[.group]> <name> <created> [fin|immortal]...
obj_add() {
    local ns="$1" kind="$2" name="$3" ts="$4"; shift 4
    mkdir -p "${STATE}/objs/${ns}"
    { echo "ts=${ts}"; for x in "$@"; do echo "${x}"; done; } > "${STATE}/objs/${ns}/${kind}%${name}"
}
left_in() { ls -A "${STATE}/objs/$1" 2>/dev/null; }

# run <script> [VAR=value ...] — the installer's libraries, as the driver loads them.
run() {
    local script="$1"; shift
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SANDBOX}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        GENTIAN_NS_DRAIN_WAIT=0 GENTIAN_NS_FINALIZE_WAIT=0 SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh"
            trap - ERR; set +e
            sleep() { :; }
            eval "${SCRIPT}"
        ' 2>&1
}

echo "Purge: a namespace goes only once it is empty"

new_state
ns_add kernel-edge
obj_add kernel-edge deployment.apps cert-manager 2026-10-11T08:00:00Z
obj_add kernel-edge secret sh.helm.release.v1.cert-manager.v1 2026-10-11T08:00:00Z
obj_add kernel-edge secret wildcard-kernel-tls 2026-10-11T08:00:00Z fin
obj_add kernel-edge configmap kube-root-ca.crt 2026-10-16T05:47:17Z
o="$(run '_delete_namespace kernel-edge; echo "rc=$?"')"
if [[ "${o}" == *"rc=0"* && -z "$(left_in kernel-edge)" && ! -f "${STATE}/ns/kernel-edge" ]]; then
    ok "a stalled namespace controller: every object deleted by the purge, then the namespace gone"
else
    bad "kernel-edge not emptied and removed" "${o}"$'\n'"left: $(left_in kernel-edge)"
fi
if [[ ! -s "${STATE}/defects.log" ]]; then
    ok "never finalized over its contents"
else
    bad "finalized while objects remained" "$(cat "${STATE}/defects.log")"
fi
grep -q 'kubectl patch secret/wildcard-kernel-tls' "${STATE}/calls.log" &&
    ok "the object held by a finalizer had it stripped" ||
    bad "finalizer of secret/wildcard-kernel-tls not stripped" "$(cat "${STATE}/calls.log")"
o="$(run 'purge_verify_namespaces_gone; echo "rc=$?"' )"
[[ "${o}" == *"rc=0"* ]] && ok "verification passes once nothing is left" || bad "verification failed on a clean cluster" "${o}"

new_state
ns_add kernel-edge
obj_add kernel-edge deployment.apps cert-manager 2026-10-11T08:00:00Z
obj_add kernel-edge secret undying 2026-10-11T08:00:00Z fin immortal
o="$(run '_delete_namespace kernel-edge; echo "rc=$?"; _GENTIAN_REMOVED_NAMESPACES=" kernel-edge"; purge_verify_namespaces_gone; echo "verify=$?"' STUCK_NS=1)"
if [[ "${o}" == *"rc=1"* && ! -s "${STATE}/defects.log" && -f "${STATE}/ns/kernel-edge" ]]; then
    ok "a namespace that will not empty is reported and left Terminating, never finalized"
else
    bad "a namespace with a survivor was finalized or reported as deleted" "${o}"$'\n'"$(cat "${STATE}/defects.log")"
fi
if [[ "${o}" == *"verify=1"* && "${o}" == *"secret/undying"* && "${o}" == *"namespace kernel-edge still exists"* ]]; then
    ok "verification fails and names the survivor"
else
    bad "verification did not report the survivor" "${o}"
fi

new_state
ns_add kernel-edge
obj_add kernel-edge deployment.apps cert-manager 2026-10-11T08:00:00Z
o="$(run '_delete_namespace kernel-edge; echo "rc=$?"' STUCK_NS=1)"
if [[ "${o}" == *"rc=0"* && ! -f "${STATE}/ns/kernel-edge" && ! -s "${STATE}/defects.log" ]] &&
    grep -q 'finalize' "${STATE}/calls.log"; then
    ok "an emptied namespace stuck Terminating is finalized, after its contents are gone"
else
    bad "emptied namespace not finalized" "${o}"
fi

echo "Purge: objects stored under a namespace that is gone"

new_state
obj_add kernel-edge deployment.apps cert-manager 2026-10-11T08:00:00Z
obj_add kernel-edge secret letsencrypt-account 2026-10-11T08:00:00Z
o="$(run '_GENTIAN_REMOVED_NAMESPACES=" kernel-edge"; purge_verify_namespaces_gone; echo "rc=$?"')"
if [[ "${o}" == *"rc=1"* && "${o}" == *"deployment.apps/cert-manager"* && "${o}" == *"secret/letsencrypt-account"* &&
      "${o}" == *"kubectl delete -n kernel-edge"* && "${o}" != *"No removed namespace"* ]]; then
    ok "verification fails on objects whose namespace no longer exists, naming them"
else
    bad "ghost objects under a removed namespace were not reported" "${o}"
fi
o="$(run '_delete_namespace kernel-edge; echo "rc=$?"; purge_verify_namespaces_gone; echo "verify=$?"')"
if [[ "${o}" == *"rc=0"* && "${o}" == *"verify=0"* && -z "$(left_in kernel-edge)" ]]; then
    ok "deleting a namespace that is already gone deletes what was stored under it"
else
    bad "ghost objects survived _delete_namespace" "${o}"
fi

new_state
ns_add kernel-edge
obj_add kernel-edge secret s1 2026-10-11T08:00:00Z
o="$(run '_delete_namespace kernel-edge; echo "rc=$?"' EXTRA_KINDS=widgets.example.io)"
if [[ "${o}" == *"rc=0"* && -z "$(left_in kernel-edge)" ]]; then
    ok "a kind whose API is gone fails the combined list, and the kinds are listed one by one"
else
    bad "contents not found when one kind's API is gone" "${o}"
fi

echo "A-01: objects older than their namespace"

new_state
ns_add kernel-edge "2026-10-16T05:47:17Z"
obj_add kernel-edge deployment.apps cert-manager 2026-10-11T08:00:00Z
obj_add kernel-edge secret sh.helm.release.v1.cert-manager.v1 2026-10-11T08:00:00Z
obj_add kernel-edge configmap kube-root-ca.crt 2026-10-16T05:47:17Z
o="$(run 'source "${SCRIPT_DIR}/scripts/steps/A-01-namespaces.sh"; apply; echo "rc=$?"')"
if [[ "${o}" == *"kernel-edge (created 2026-10-16T05:47:17Z) holds 2 object(s) older than itself"* &&
      "${o}" == *"deployment.apps/cert-manager"* && "${o}" == *"secret/sh.helm.release.v1.cert-manager.v1"* &&
      "${o}" != *"configmap/kube-root-ca.crt"* ]]; then
    ok "A-01 names the stale objects and not the namespace's own"
else
    bad "A-01 did not warn about the stale objects" "${o}"
fi
cmd="$(grep -o 'kubectl delete -n kernel-edge --wait=false .*' <<< "${o}")"
[[ "${cmd}" == *" deployment.apps/cert-manager"* && "${cmd}" == *" secret/sh.helm.release.v1.cert-manager.v1"* &&
   "${cmd}" != *kube-root-ca* ]] &&
    ok "with the command that removes them" || bad "no removal command" "${o}"
[[ "$(find "${STATE}/objs/kernel-edge" -type f | wc -l)" == 3 ]] && ! grep -q '^kubectl delete' "${STATE}/calls.log" &&
    ok "and removes nothing itself" || bad "A-01 deleted something" "$(cat "${STATE}/calls.log")"
[[ "${o}" == *"rc=0"* ]] && ok "the warning does not fail the step" || bad "A-01 apply failed" "${o}"

new_state
ns_add kernel-edge "2026-10-16T05:47:17Z"
obj_add kernel-edge configmap kube-root-ca.crt 2026-10-16T05:47:18Z
o="$(run 'ns_warn_stale_objects kernel-edge; echo "rc=$?"')"
[[ "${o}" == *"rc=0"* && "${o}" != *"older than itself"* ]] &&
    ok "a namespace holding only newer objects draws no warning" || bad "false ghost warning" "${o}"

echo "A-02..A-05: a release record is not a running release"

# check <step> <release> <component> <deployment>...
check_step() {
    local step="$1" rel="$2" comp="$3"; shift 3
    local d o
    new_state
    for d in "$@"; do echo 0 > "${STATE}/avail/${d}"; done
    o="$(run 'export PIN="$(gentian_pin '"${comp}"' chart)"; edge_filter_order_ok() { :; }; envoy_gateway_crds_current() { :; }
              source "${SCRIPT_DIR}/scripts/steps/'"${step}"'.sh"; check; echo "rc=$?"' REL="${rel}")"
    [[ "${o}" == *"rc=1"* ]] && ok "${step}: not satisfied while its Deployments have no available replica" ||
        bad "${step} satisfied with dead Deployments" "${o}"
    for d in "$@"; do echo 1 > "${STATE}/avail/${d}"; done
    echo 0 > "${STATE}/avail/${1}"
    o="$(run 'export PIN="$(gentian_pin '"${comp}"' chart)"; edge_filter_order_ok() { :; }; envoy_gateway_crds_current() { :; }
              source "${SCRIPT_DIR}/scripts/steps/'"${step}"'.sh"; check; echo "rc=$?"' REL="${rel}")"
    [[ "${o}" == *"rc=1"* ]] && ok "${step}: not satisfied while ${1} alone is unavailable" ||
        bad "${step} satisfied with ${1} unavailable" "${o}"
    echo 1 > "${STATE}/avail/${1}"
    [[ "${step}" == A-05-envoy-gateway ]] && return 0   # the rest of its check asks the live gateway
    o="$(run 'export PIN="$(gentian_pin '"${comp}"' chart)"
              source "${SCRIPT_DIR}/scripts/steps/'"${step}"'.sh"; check; echo "rc=$?"' REL="${rel}")"
    [[ "${o}" == *"rc=0"* ]] && ok "${step}: satisfied once every Deployment is available" ||
        bad "${step} not satisfied with everything available" "${o}"
}
check_step A-02-cert-manager cert-manager cert-manager cert-manager cert-manager-cainjector cert-manager-webhook
check_step A-03-external-secrets external-secrets external-secrets external-secrets external-secrets-cert-controller external-secrets-webhook
check_step A-04-crossplane crossplane crossplane crossplane crossplane-rbac-manager
check_step A-05-envoy-gateway envoy-gateway envoy-gateway envoy-gateway

echo ""
echo "${pass} passed, ${fail} failed"
[[ "${fail}" -eq 0 ]]
