#!/usr/bin/env bash
# step: A-05-envoy-gateway
# phase: control-plane
# requires: A-01-namespaces
# provides: Envoy Gateway controller answering for the kernel's GatewayClass name, and the Gateway API CRDs, in the edge namespace
# mutates: the edge namespace, Gateway API CRDs, cluster-scoped RBAC
# pins: envoy-gateway

# Only the controller. The GatewayClass and the two Gateway objects
# (authenticated and perimeter) are kernel resources the operator reconciles
# from the Cluster claim; the installer does not create what it would then have
# to keep in step with the claim.

check() {
    helm_pinned_ok envoy-gateway envoy-gateway edge &&
        kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1 &&
        # The name it answers for, not merely that it is installed.
        [[ "$(kubectl get configmap envoy-gateway-config -n "$(ns_kernel edge)" -o jsonpath='{.data.envoy-gateway\.yaml}' 2>/dev/null | grep -c "${GENTIAN_GATEWAY_CONTROLLER_NAME}")" != "0" ]]
}

apply() {
    banner "Envoy Gateway"
    # The controller answers for one GatewayClass name, and the kernel's class
    # names this one (kernel/manifests/gateway/gatewayclass.yaml). Installed
    # with the default name, the controller ignores the kernel's Gateway and
    # the Gateway waits for a controller that is running but not listening.
    local svc_type=ClusterIP
    [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]] && svc_type=LoadBalancer
    helm_pinned envoy-gateway envoy-gateway edge \
        --set "config.envoyGateway.gateway.controllerName=${GENTIAN_GATEWAY_CONTROLLER_NAME}" \
        --set deployment.replicas=1 \
        --set "kubernetesService.type=${svc_type}"
}

destroy() {
    helm uninstall envoy-gateway -n "$(ns_kernel edge)" >/dev/null 2>&1 || true
}
