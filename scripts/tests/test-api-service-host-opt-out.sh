#!/usr/bin/env bash
# shellcheck disable=SC1091,SC2015,SC2329 # sourced lib; ok/bad report, never fail the line; kubectl stand-in called by the sourced functions
# Every namespace gentian-os creates carries Gardener's opt-out from pointing
# pods at the API server's public name (os #1374); the platform's own
# namespaces, which the installer only labels, do not.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LABEL="apiserver-proxy.networking.gardener.cloud/inject"
fail=0
ok()  { echo "  ok   $*"; }
bad() { echo "  FAIL $*"; fail=1; }

# shellcheck source=../lib/namespaces.sh
source "${ROOT}/scripts/lib/namespaces.sh"

for ns in $(ns_kernel_all); do
    [[ "$(_ns_own_labels "${ns}")" == "${LABEL}=disable" ]] && ok "${ns} opts out" || bad "${ns} lacks the opt-out"
done
[[ -z "$(_ns_own_labels kube-system)" ]] && ok "kube-system is left alone" || bad "kube-system gets the opt-out"

# ns_ensure and ns_labelled_ok against a stand-in kubectl.
labels_applied=""
kubectl() {
    case "$1 $2" in
        "get namespace") [[ "$*" == *jsonpath* ]] || return 0
            case "$*" in
                *tier*)     echo kernel ;;
                *function*) _ns_table | awk -v n="$3" '$1 == n { print $NF }' ;;
                *inject*)   echo "${STANDIN_INJECT:-}" ;;
            esac ;;
        "label --overwrite") labels_applied="$*" ;;
    esac
}
ns="$(ns_kernel secrets)"
ns_ensure "${ns}"
[[ "${labels_applied}" == *"${LABEL}=disable"* ]] && ok "ns_ensure applies the opt-out" || bad "ns_ensure: ${labels_applied}"
STANDIN_INJECT="" ; ns_labelled_ok "${ns}" && bad "a namespace without the opt-out counts as labeled" || ok "missing opt-out re-runs A-01"
STANDIN_INJECT=disable ; ns_labelled_ok "${ns}" && ok "with the opt-out, labeled" || bad "with the opt-out, not labeled"

for f in crossplane/compositions/cluster-default.yaml crossplane/compositions/tenant-default.yaml \
         kernel/bootstrap/chart/templates/external-dns.yaml; do
    grep -q "${LABEL}: disable" "${ROOT}/${f}" && ok "${f}" || bad "${f} lacks the opt-out"
done

exit "${fail}"
