#!/usr/bin/env bash
# step: A-05-envoy-gateway
# phase: control-plane
# requires: A-01-namespaces
# provides: Envoy Gateway controller and the Gateway API CRDs in the edge namespace
# mutates: the edge namespace, Gateway API CRDs, cluster-scoped RBAC
# pins: envoy-gateway

# Only the controller. The GatewayClass and the two Gateway objects
# (authenticated and perimeter) are kernel resources the operator reconciles
# from the Cluster claim; the installer does not create what it would then have
# to keep in step with the claim.

check() {
    helm_pinned_ok envoy-gateway envoy-gateway edge &&
        kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1
}

apply() {
    banner "Envoy Gateway"
    helm_pinned envoy-gateway envoy-gateway edge
}

destroy() {
    helm uninstall envoy-gateway -n "$(ns_kernel edge)" >/dev/null 2>&1 || true
}
