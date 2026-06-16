// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"strings"
	"time"
)

const (
	// RoutingModeIngress selects legacy ingress-nginx Ingress resources (deprecated).
	RoutingModeIngress = "ingress"
	// RoutingModeGateway selects Gateway API + Envoy Gateway for edge routing.
	RoutingModeGateway = "gateway"

	GentianGatewayClassName        = "gentian-envoy"
	GentianGatewayControllerName   = "gateway.envoyproxy.io/gentian-gatewayclass-controller"
	KernelPublicGatewayName        = "kernel-public-gateway"
	kernelCollaboraListenerName    = "https-office"
	kernelWildcardTLSSecretName    = "wildcard-tls"
	envoyGatewayInstallNamespace     = "envoy-gateway-system"
	gatewayPlatformReconcileKey      = "gateway-platform"
	conditionGatewayReady            = "GatewayReady"
	conditionTunnelIngressReady      = "TunnelIngressReady"
	operatorNamespace                = "gentian-system"
	operatorConfigMapName            = "gentian-os-config"
)

func normalizeRoutingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RoutingModeIngress, "nginx":
		return RoutingModeIngress
	case RoutingModeGateway:
		return RoutingModeGateway
	default:
		return RoutingModeGateway
	}
}

func isGatewayRoutingMode(mode string) bool {
	return normalizeRoutingMode(mode) == RoutingModeGateway
}

func tenantGatewayName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-gateway", tenantName)
}

// requeueGatewayAfter is the default requeue interval while waiting for Gateway/HTTPRoute status.
const requeueGatewayAfter = 15 * time.Second
