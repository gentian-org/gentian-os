/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"github.com/gentian-org/gentian-os/internal/layout"
	"strings"
	"time"
)

const (
	// RoutingModeGateway is the only supported edge routing stack.
	RoutingModeGateway = "gateway"

	GentianGatewayClassName      = "gentian-envoy"
	GentianGatewayControllerName = "gateway.envoyproxy.io/gentian-gatewayclass-controller"
	KernelPublicGatewayName      = "kernel-public-gateway"
	kernelWildcardTLSSecretName  = "wildcard-tls"
	gatewayPlatformReconcileKey  = "gateway-platform"
	conditionGatewayReady        = "GatewayReady"
	conditionTunnelIngressReady  = "TunnelIngressReady"
	operatorConfigMapName        = "gentian-os-config"
)

// Where the kernel's functions run. Resolved from the layout the chart passes
// in (internal/layout), which answers the v4 names when a process is started
// without it — so an operator of this release behaves identically on a cluster
// that has not been rebuilt.
var (
	operatorNamespace = layout.Namespace(layout.Control)
	argocdNamespace   = layout.Namespace(layout.GitOps)
	// identityNamespace is where Keycloak answers: the backend its route
	// points at, and where the jobs that configure it run.
	identityNamespace = layout.Namespace(layout.Authentication)
)

func normalizeRoutingMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), RoutingModeGateway) || strings.TrimSpace(mode) == "" {
		return RoutingModeGateway
	}
	return RoutingModeGateway
}

func isGatewayRoutingMode(string) bool {
	return true
}

func tenantGatewayName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-gateway", tenantName)
}

// requeueGatewayAfter is the default requeue interval while waiting for Gateway/HTTPRoute status.
const requeueGatewayAfter = 15 * time.Second
