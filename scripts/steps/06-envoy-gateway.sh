#!/usr/bin/env bash
# step: 06-envoy-gateway
# requires: 02-namespaces
# provides: Envoy Gateway control plane (ROUTING_MODE=gateway)
# mutates: namespace envoy-gateway-system, Gateway API CRDs
# pins: envoy-gateway

_envoy_ns() { echo "${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"; }

check() {
    [[ "${ROUTING_MODE:-gateway}" == "gateway" ]] || return 0
    kubectl get deployment -n "$(_envoy_ns)" -l control-plane=envoy-gateway \
        -o name 2>/dev/null | grep -q .
}

apply() {
    install_envoy_gateway
}

destroy() {
    _delete_envoy_gateway_scaffold || true
}
