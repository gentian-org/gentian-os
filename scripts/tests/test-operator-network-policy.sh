#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-operator-network-policy.sh
# =============================================================================
# The operator chart puts one NetworkPolicy on the operator's pods. A policy
# that selects a pod denies every ingress it does not list, so two mistakes
# are possible and neither shows until a cluster runs it:
#
#   - a port the operator serves is missing from the policy. The pod then
#     fails its probes, or the API server cannot reach the admission webhook
#     and every Tenant write is refused;
#   - the listener's port is listed with no source, or the policy selects
#     more than the operator, and the control it exists for is not there;
#   - the membership listener's port admits more than Keycloak's pods.
#
# Rendered with helm, no cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

render() {
    helm template gentian-os charts/gentian-os -n kernel-control "$@" 2>&1
}

check() {
    # check <description> <rendered manifests> <expectation...>
    local what="$1" manifests="$2"
    shift 2
    if python3 scripts/tests/operator_network_policy.py "$@" <<<"${manifests}"; then
        ok "${what}"
    else
        bad "${what}"
    fi
}

echo ""
echo "Operator chart: the NetworkPolicy on the operator's pods"
echo ""

all="$(render --set director.enabled=true --set usher.enabled=true \
    --set membershipListener.keysSecretRef.name=keys --set webhook.enabled=true)"
check "director and usher deployed: the listener admits those two and no other pod" "${all}" director usher
check "membership listener on: its port admits the Keycloak pods of kernel-authentication alone" \
    "$(render --set membershipListener.keysSecretRef.name=keys)" defaults
check "defaults: whatever the chart deploys by default is what the listener admits" "$(render)" defaults
check "director only: the listener admits the director" \
    "$(render --set director.enabled=true --set usher.enabled=false)" director
check "usher only: the listener admits the usher" \
    "$(render --set director.enabled=false --set usher.enabled=true)" usher
check "neither deployed: the listener's port is listed nowhere, so closed" \
    "$(render --set director.enabled=false --set usher.enabled=false)"
check "a custom listener port follows into the policy" \
    "$(render --set director.enabled=true --set appLifecycle.port=18082)" director
check "networkPolicy.enabled=false: no NetworkPolicy is rendered" \
    "$(render --set director.enabled=true --set usher.enabled=true --set networkPolicy.enabled=false)" off

finish
