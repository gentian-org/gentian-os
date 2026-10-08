#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-store-network-policies.sh
# =============================================================================
# Each shared store of the system tier -- PostgreSQL, MariaDB, Redis, MinIO --
# carries one NetworkPolicy saying who may connect to it. A policy that
# selects a pod denies every ingress it does not list, so the mistakes that
# matter show on a cluster and nowhere else:
#
#   - a client is not admitted. Its Job hangs until its deadline -- a role or
#     bucket that is never made, an export that never finishes -- or the
#     engine's own operator cannot reach it and it never reports healthy;
#   - the selector misses the server's pods, and the policy restricts nothing
#     while everything reads as protected;
#   - a rule is wider than its client, and what it was written to keep out
#     still gets in.
#
# Rendered with helm, no cluster: the policies from each engine's chart with
# the parameters the data-plane ApplicationSet passes, the engines from the
# packages the Cluster composition installs, the clients from
# scripts/tests/store-clients.yaml -- which a Go test holds to the code that
# builds them (internal/controller/store_clients_test.go).
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

check() {
    # check <description> <assertion...>
    local what="$1"
    shift
    if python3 scripts/tests/store_network_policies.py "$@"; then
        printf '  \033[0;32mok\033[0m    %s\n' "${what}"
    else
        printf '  \033[0;31mFAIL\033[0m  %s\n' "${what}"
        fail=$((fail + 1))
    fi
}

check_llm() {
    # check_llm <description> <assertion>
    local what="$1"
    shift
    if python3 scripts/tests/llm_network_policies.py "$@"; then
        printf '  \033[0;32mok\033[0m    %s\n' "${what}"
    else
        printf '  \033[0;31mFAIL\033[0m  %s\n' "${what}"
        fail=$((fail + 1))
    fi
}

fail=0
echo ""
echo "System tier: the NetworkPolicy on each shared store"
echo ""

for store in postgresql mariadb cache s3; do
    check "${store}: selects the server's pods and no other; every port they declare is listed or recorded as closed" \
        shape "${store}"
    check "${store}: every client of the table is admitted, everything it says to refuse is refused" \
        clients "${store}"
done
check "the namespaces, the operator's labels, the tenant label and the CloudNativePG pin are what the policies assume" wiring
check "storeNetworkPolicies=false: no store carries a policy; on, each carries one" off

# The servers that are not shared stores: the kernel's own PostgreSQL and the
# mail servers. Same assertions, from the `servers` section of the same file;
# a mail server also has ports that face the internet, which are open to any
# source and recorded as such.
check_server() {
    local what="$1"
    shift
    if python3 scripts/tests/server_network_policies.py "$@"; then
        printf '  \033[0;32mok\033[0m    %s\n' "${what}"
    else
        printf '  \033[0;31mFAIL\033[0m  %s\n' "${what}"
        fail=$((fail + 1))
    fi
}

echo ""
echo "The kernel's own PostgreSQL and the mail servers"
echo ""
for server in kernel-postgres dovecot postfix; do
    check_server "${server}: selects the server's pods and no other; every port they serve is listed or recorded as closed" \
        shape "${server}"
    check_server "${server}: every client of the table is admitted, everything it says to refuse is refused" \
        clients "${server}"
done
check_server "namespaces, labels, hosts, ports and sync waves are what the policies assume; the installer's check runs where it is admitted" wiring
check_server "storeNetworkPolicies=false: kernel-postgres admits everything, the mail servers carry no policy; on, each carries one" off
check_server "a cluster that relays mail has no mail policy; without a certificate or an MX no rule is left for the port" modes

# The model gateway's namespace is no store and is held to the same: one
# policy per pod set that serves there, the clients from the same table.
echo ""
echo "System tier: the NetworkPolicies of the model gateway's namespace"
echo ""
check_llm "each policy selects its server's pods and no other; every port they declare is listed or recorded as closed" shape
check_llm "every client of the table is admitted to the server it names, everything it says to refuse is refused" clients
check_llm "the namespace, the parameters, the gateway's port and the edge's label are what the policies assume" wiring
check_llm "storeNetworkPolicies=false: no policy; with GPUs no mock and no policy for one; no models, no ApplicationSet" off

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${fail} check(s) failed"
    exit 1
fi
echo "all checks passed"
