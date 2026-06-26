// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kernelGatewayTunnelOrigin returns the in-cluster HTTPS origin cloudflared should
// use for tenant and kernel hostnames in tunnel mode.
func kernelGatewayTunnelOrigin(ctx context.Context, c client.Client) (string, error) {
	list := &corev1.ServiceList{}
	if err := c.List(ctx, list,
		client.InNamespace(envoyGatewayInstallNamespace),
		client.MatchingLabels{
			"gateway.envoyproxy.io/owning-gateway-name":      KernelPublicGatewayName,
			"gateway.envoyproxy.io/owning-gateway-namespace": servicesNamespace,
		},
	); err != nil {
		return "", fmt.Errorf("list kernel Envoy Gateway service: %w", err)
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("kernel Envoy Gateway service not found")
	}
	svc := &list.Items[0]
	port := int32(443)
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == 443 {
			port = svc.Spec.Ports[i].Port
			break
		}
	}
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", svc.Name, envoyGatewayInstallNamespace, port), nil
}
