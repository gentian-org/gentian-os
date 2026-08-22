#!/usr/bin/env bash
# step: A-07-envoy-gateway
# phase: control-plane
# requires: A-03-namespaces
# provides: Envoy Gateway control plane (ROUTING_MODE=gateway)
# mutates: namespace envoy-gateway-system, Gateway API CRDs
# pins: envoy-gateway

_envoy_ns() { echo "${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"; }

check() {
    # `gateway` is the only supported value — anything else is rejected with a
    # hard error before the steps run (certs.sh). Reaching here with another
    # value means no validator has seen it yet, as on the --status path, and a
    # verdict about Envoy Gateway would be meaningless.
    [[ "${ROUTING_MODE:-gateway}" == "gateway" ]] || return "${CHECK_UNDEFINED}"
    kubectl get deployment -n "$(_envoy_ns)" -l control-plane=envoy-gateway \
        -o name 2>/dev/null | grep -q . || return 1

    # The Gateway API CRDs, not just the controller. apply() explicitly waits
    # for these to be Established and destroy() explicitly deletes them
    # (_delete_envoy_gateway_scaffold) — the controller Deployment stays
    # Running with them gone, it just has nothing left to reconcile, so a
    # partial/manual CRD cleanup that leaves the Deployment behind would
    # otherwise report satisfied with no Gateway API surface left at all.
    kubectl get crd gatewayclasses.gateway.networking.k8s.io >/dev/null 2>&1
}

apply() {
    install_envoy_gateway
}

destroy() {
    _delete_envoy_gateway_scaffold || true
}
