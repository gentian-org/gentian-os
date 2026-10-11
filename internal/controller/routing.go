/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/layout"
)

const (
	// RoutingModeGateway is the only supported edge routing stack.
	RoutingModeGateway = "gateway"

	GentianGatewayClassName      = "gentian-envoy"
	GentianGatewayControllerName = "gateway.envoyproxy.io/gentian-gatewayclass-controller"
	// The two edges (networking.md §1): one Envoy fleet, two policy domains,
	// both Gateways in the edge namespace under mergeGateways. The
	// authenticated Gateway serves every surface behind a session; the
	// perimeter Gateway serves surfaces on their own hostname with no
	// session -- for the kernel, exactly two: the identity provider's realm
	// endpoints on id.<kernel>, and the ACME challenge on :80.
	AuthenticatedGatewayName    = "authenticated"
	PerimeterGatewayName        = "perimeter"
	kernelWildcardTLSSecretName = "wildcard-tls"
	gatewayPlatformReconcileKey = "gateway-platform"
	conditionGatewayReady       = "GatewayReady"
	conditionTunnelIngressReady = "TunnelIngressReady"
	operatorConfigMapName       = "gentian-os-config"
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
	// observabilityNamespace is where the cluster view runs.
	observabilityNamespace = layout.Namespace(layout.Observability)
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
