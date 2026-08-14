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
        -o name 2>/dev/null | grep -q .
}

apply() {
    install_envoy_gateway
}

destroy() {
    _delete_envoy_gateway_scaffold || true
}
