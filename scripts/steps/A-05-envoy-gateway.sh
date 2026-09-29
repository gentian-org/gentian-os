#!/usr/bin/env bash
# step: A-05-envoy-gateway
# phase: control-plane
# requires: A-01-namespaces
# provides: Envoy Gateway controller answering for the kernel's GatewayClass name, and the Gateway API CRDs, in the edge namespace
# mutates: the edge namespace, Gateway API CRDs, cluster-scoped RBAC
# pins: envoy-gateway

# The controller, and the EnvoyProxy its GatewayClass points at. The Gateway
# objects themselves (authenticated and perimeter) are kernel resources the
# operator reconciles from the Cluster claim; the installer does not create
# what it would then have to keep in step with the claim.
#
# The EnvoyProxy belongs here because it shapes the data plane a Gateway gets,
# and it has to exist before the operator creates one: a Service's address is
# honoured at creation only.

check() {
    helm_pinned_ok envoy-gateway envoy-gateway edge &&
        kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1 &&
        # The name it answers for, not merely that it is installed.
        [[ "$(kubectl get configmap envoy-gateway-config -n "$(ns_kernel edge)" -o jsonpath='{.data.envoy-gateway\.yaml}' 2>/dev/null | grep -c "${GENTIAN_GATEWAY_CONTROLLER_NAME}")" != "0" ]] &&
        kubectl get envoyproxy gentian-edge -n "$(ns_kernel edge)" >/dev/null 2>&1 &&
        [[ -n "$(kubectl get gatewayclass "${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}" -o jsonpath='{.spec.parametersRef.name}' 2>/dev/null)" ]]
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

    ENVOY_GATEWAY_NAMESPACE="$(ns_kernel edge)" apply_edge_envoyproxy
}

destroy() {
    helm uninstall envoy-gateway -n "$(ns_kernel edge)" >/dev/null 2>&1 || true
}
