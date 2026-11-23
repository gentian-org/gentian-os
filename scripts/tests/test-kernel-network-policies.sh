#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-kernel-network-policies.sh
# =============================================================================
# The kernel namespaces refuse an ingress nothing lists
# (kernel/security/network-policies, generated from
# internal/kernel/kernelnet/inventory.yaml and applied by A-01). Two things
# can go wrong that no cluster is at hand to show:
#
#   - something is deployed into a kernel namespace that the list does not
#     have: a new workload, a new port, a webhook, a chart moved to another
#     version. The next install then stalls on a timeout;
#   - the installer does not do with the file what its switch says: applies
#     policies nobody turned on, leaves them behind when the switch is off,
#     or keeps one a later list dropped;
#   - the installer does not write the API server's endpoints into the
#     egress rules, or applies them without one.
#
# The first is asked with helm and no cluster; the second with a kubectl that
# keeps its objects in a directory. What the rules admit and refuse is the Go
# test beside the inventory (go test ./internal/kernel/kernelnet/).
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"
check() {
    local what="$1"
    shift
    if "$@"; then ok "${what}"; else bad "${what}"; fi
}

echo ""
echo "Kernel namespaces: who may reach their pods"
echo ""

check "every port and webhook of the pods this repository's charts deploy there is in the inventory" \
    python3 scripts/tests/kernel_network_policies.py chart
check "everything else installed there is in the inventory, read at the version that is pinned" \
    python3 scripts/tests/kernel_network_policies.py census

# ---------------------------------------------------------------------------
# The installer's side, against a kubectl that stores NetworkPolicies as
# files named <namespace>__<name> holding the object, and answers for the
# Service kubernetes' endpoints with STUB_API_ADDRESSES and STUB_API_PORTS.
# ---------------------------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
mkdir -p "${TMP}/bin" "${TMP}/store"
cat > "${TMP}/bin/kubectl" <<'STUB'
#!/usr/bin/env python3
import os, sys, pathlib, yaml
store = pathlib.Path(os.environ["STUB_STORE"])
args = sys.argv[1:]

def matches(labels, selector):
    for term in filter(None, selector.split(",")):
        if "!=" in term:
            k, v = term.split("!=", 1)
            if labels.get(k) == v:
                return False
        else:
            k, v = term.split("=", 1)
            if labels.get(k) != v:
                return False
    return True

if args[:2] == ["apply", "-f"]:
    for doc in yaml.safe_load_all(sys.stdin.read()):
        if not doc:
            continue
        assert doc["kind"] == "NetworkPolicy" and doc["apiVersion"] == "networking.k8s.io/v1", doc["kind"]
        m = doc["metadata"]
        (store / f"{m['namespace']}__{m['name']}").write_text(yaml.safe_dump(doc))
elif args[:2] == ["get", "networkpolicy"]:
    selector = args[args.index("-l") + 1]
    for f in sorted(store.iterdir()):
        doc = yaml.safe_load(f.read_text()) or {}
        if matches(doc.get("metadata", {}).get("labels", {}), selector):
            print(f.name.replace("__", "/"))
elif args[:2] == ["get", "endpointslice"]:
    assert "kubernetes.io/service-name=kubernetes" in args and args[args.index("-n") + 1] == "default", args
    jsonpath = [a for a in args if a.startswith("jsonpath=")][0]
    if ".addresses" in jsonpath:
        print(os.environ.get("STUB_API_ADDRESSES", ""), end="")
    else:
        print(os.environ.get("STUB_API_PORTS", ""), end="")
elif args[:2] == ["delete", "networkpolicy"]:
    name, ns = args[2], args[args.index("-n") + 1]
    (store / f"{ns}__{name}").unlink(missing_ok=True)
else:
    sys.exit(f"stub kubectl: unexpected {args}")
STUB
chmod +x "${TMP}/bin/kubectl"

installer() {
    # installer <KERNEL_NETWORK_POLICIES value or ""> <function> [file]
    local switch="$1" fn="$2" file="${3:-}"
    PATH="${TMP}/bin:${PATH}" STUB_STORE="${TMP}/store" SCRIPT_DIR="${PWD}" \
        STUB_API_ADDRESSES="${STUB_API_ADDRESSES-83.166.143.133 }" STUB_API_PORTS="${STUB_API_PORTS-31099}" \
        KERNEL_NETWORK_POLICIES="${switch}" KERNEL_NETWORK_POLICIES_FILE="${file}" \
        bash -c 'set -uo pipefail
            [[ -n "${KERNEL_NETWORK_POLICIES}" ]] || unset KERNEL_NETWORK_POLICIES
            [[ -n "${KERNEL_NETWORK_POLICIES_FILE}" ]] || unset KERNEL_NETWORK_POLICIES_FILE
            error() { echo "$*" >&2; }
            source scripts/lib/namespaces.sh; "$0"' "${fn}"
}
# api_rule <namespace> -- the egress rule the stored kernel-egress policy has
# for the API server, as "<cidr> ...|<port> ...".
api_rule() {
    python3 - "${TMP}/store/${1}__kernel-egress" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
rule = doc["spec"]["egress"][-1]
print(" ".join(t["ipBlock"]["cidr"] for t in rule["to"]) + "|" + " ".join(str(p["port"]) for p in rule["ports"]))
PY
}
stored() { find "${TMP}/store" -type f | wc -l | tr -d ' '; }
want="$(grep -c '^kind: NetworkPolicy$' kernel/security/network-policies/kernel-network-policies.yaml)"

installer "" ns_kernel_policies_ok
check "default (switch unset) on a cluster with none of the policies: the step has work to do" test $? -ne 0
installer "true" ns_kernel_policies_ok
check "KERNEL_NETWORK_POLICIES=true on a cluster with none of the policies: the step has work to do" test $? -ne 0
installer "true" ns_kernel_policies_sync
check "KERNEL_NETWORK_POLICIES=true: every policy of the file is applied" test "$(stored)" = "${want}"
installer "true" ns_kernel_policies_ok
check "and the step then reports itself satisfied" test $? -eq 0
for ns in $(SCRIPT_DIR="${PWD}" bash -c 'source scripts/lib/namespaces.sh; ns_kernel_all'); do
    [[ -f "${TMP}/store/${ns}__kernel-ingress" ]] || bad "no kernel-ingress policy was applied in ${ns}"
done
check "nothing was applied in the mail namespaces, whose rules are not these" \
    test -z "$(find "${TMP}/store" -name 'system-mail*')"

# The API server's endpoints in the egress rules, where the file has
# placeholders.
egress_ns="$(grep -B1 '^  namespace: ' kernel/security/network-policies/kernel-network-policies.yaml \
    | awk '/name: kernel-egress$/ { getline; print $2 }')"
check "the file has egress rules to fill in" test -n "${egress_ns}"
for ns in ${egress_ns}; do
    [[ "$(api_rule "${ns}")" == "83.166.143.133/32|31099" ]] ||
        bad "${ns}: the API server rule is $(api_rule "${ns}"), not the cluster's endpoint"
done
check "no placeholder reaches the cluster" \
    test -z "$(grep -l 'API-SERVER\|api-server' "${TMP}"/store/* 2>/dev/null)"
STUB_API_ADDRESSES="192.168.0.100 192.168.0.101 fd00::1 " STUB_API_PORTS="16443 16443"
installer "true" ns_kernel_policies_ok
check "an API server that moved: the step has work to do" test $? -ne 0
installer "true" ns_kernel_policies_sync
for ns in ${egress_ns}; do
    [[ "$(api_rule "${ns}")" == "192.168.0.100/32 192.168.0.101/32 fd00::1/128|16443" ]] ||
        bad "${ns}: the API server rule is $(api_rule "${ns}"), not every endpoint once"
done
check "several endpoints are each admitted, the port once" test "$(stored)" = "${want}"
installer "true" ns_kernel_policies_ok
check "and the step is satisfied again" test $? -eq 0
STUB_API_ADDRESSES="" installer "true" ns_kernel_policies_sync 2>/dev/null
check "no endpoint of the API server: nothing is applied and the step fails" test $? -ne 0
unset STUB_API_ADDRESSES STUB_API_PORTS

# A later file that has one policy fewer: the one it dropped is removed.
sed '/^---$/,$!d' kernel/security/network-policies/kernel-network-policies.yaml \
    | awk 'BEGIN { RS = "---\n"; ORS = "" } NR > 2 { print "---\n" $0 }' > "${TMP}/fewer.yaml"
installer "true" ns_kernel_policies_ok "${TMP}/fewer.yaml"
check "another file's rules on the cluster are not this file's: the step has work to do" test $? -ne 0
installer "true" ns_kernel_policies_sync "${TMP}/fewer.yaml"
check "a policy a later file no longer has is removed" test "$(stored)" = "$((want - 1))"
installer "true" ns_kernel_policies_ok "${TMP}/fewer.yaml"
check "and the step is satisfied again" test $? -eq 0

# A policy that is not the installer's is never touched.
echo "{metadata: {labels: {app: other}}}" > "${TMP}/store/kernel-gitops__argocd-server-network-policy"
installer "false" ns_kernel_policies_ok
check "KERNEL_NETWORK_POLICIES=false with policies present: the step has work to do" test $? -ne 0
installer "false" ns_kernel_policies_sync
check "KERNEL_NETWORK_POLICIES=false removes every policy of these rules and applies none" \
    test "$(stored)" = "1"
check "and leaves a policy that is somebody else's" test -f "${TMP}/store/kernel-gitops__argocd-server-network-policy"
installer "false" ns_kernel_policies_ok
check "and the step is then satisfied" test $? -eq 0
installer "true" ns_kernel_policies_sync
check "KERNEL_NETWORK_POLICIES=true applies them again" test "$(stored)" = "$((want + 1))"
installer "" ns_kernel_policies_sync
check "and the switch unset keeps them, as true does" test "$(stored)" = "$((want + 1))"

# The operator is told the same switch, through the bootstrap chart and its own.
check "the operator is told the switch: off unless the installer passes true" \
    python3 scripts/tests/kernel_network_policies.py switch

finish
