#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-llm-models.sh
# =============================================================================
# The models the gateway offers are the ones the Cluster claim declares under
# spec.llm -- its instances and its providers' models -- and no step, Job or
# controller registers them: the gateway's chart writes its configuration file
# from the claim, which the LLM ApplicationSet hands it as a values file.
#
# The mistakes this is for show on a cluster and nowhere else: a claim whose
# models never reach the gateway, a provider's token written into a ConfigMap,
# a gateway that keeps the list it started with.
#
# Rendered with helm, no cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

check() {
    # check <description> <assertion>
    local what="$1"
    shift
    if python3 scripts/tests/llm_models.py "$@"; then
        ok "${what}"
    else
        bad "${what}"
    fi
}

echo ""
echo "The model gateway: its models are the Cluster claim's"
echo ""
check "the ApplicationSet hands the chart the claim the claims Application applies, and no parameter over it" claim
check "instances (with GPUs) and providers' models are offered under their names; none without a claim; a changed list replaces the pods" models
check "a provider's token is a variable set from the mounted Secret, never in the file; no provider, no ExternalSecret" tokens
check "the gateway starts with the file it mounts and takes models from nowhere else" gateway
check "every model is in the access groups a tenant's team names; a fallback is held to the caller's models" groups

finish
