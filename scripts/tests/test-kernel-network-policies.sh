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
#     or keeps one a later list dropped.
#
# The first is asked with helm and no cluster; the second with a kubectl that
# keeps its objects in a directory. What the rules admit and refuse is the Go
# test beside the inventory (go test ./internal/kernel/kernelnet/).
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

fail=0
ok()   { printf '  \033[0;32mok\033[0m    %s\n' "$1"; }
bad()  { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
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
# files named <namespace>__<name> holding their labels.
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
        (store / f"{m['namespace']}__{m['name']}").write_text(yaml.safe_dump(m.get("labels", {})))
elif args[:2] == ["get", "networkpolicy"]:
    selector = args[args.index("-l") + 1]
    for f in sorted(store.iterdir()):
        if matches(yaml.safe_load(f.read_text()) or {}, selector):
            print(f.name.replace("__", "/"))
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
        KERNEL_NETWORK_POLICIES="${switch}" KERNEL_NETWORK_POLICIES_FILE="${file}" \
        bash -c 'set -uo pipefail
            [[ -n "${KERNEL_NETWORK_POLICIES}" ]] || unset KERNEL_NETWORK_POLICIES
            [[ -n "${KERNEL_NETWORK_POLICIES_FILE}" ]] || unset KERNEL_NETWORK_POLICIES_FILE
            source scripts/lib/namespaces.sh; "$0"' "${fn}"
}
stored() { find "${TMP}/store" -type f | wc -l | tr -d ' '; }
want="$(grep -c '^kind: NetworkPolicy$' kernel/security/network-policies/kernel-network-policies.yaml)"

installer "" ns_kernel_policies_ok
check "default (switch unset) on a cluster with none of the policies: nothing to do" test $? -eq 0
installer "" ns_kernel_policies_sync
check "default (switch unset): nothing is applied" test "$(stored)" = "0"
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
echo "{app: other}" > "${TMP}/store/kernel-gitops__argocd-server-network-policy"
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
check "and the switch unset removes them, as false does" test "$(stored)" = "1"

# The operator is told the same switch, through the bootstrap chart and its own.
check "the operator is told the switch: off unless the installer passes true" \
    python3 scripts/tests/kernel_network_policies.py switch

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${fail} check(s) failed"
    exit 1
fi
echo "all checks passed"
