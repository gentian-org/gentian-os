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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kernelEdgeServiceLabels select the Envoy data-plane Service that belongs to
// the kernel's public Gateway.
//
// Ownership identifies it; where Envoy Gateway happens to be installed does
// not. That namespace was once a constant, and it stopped being true the
// moment the edge moved: every caller here then looked in
// envoy-gateway-system, found nothing, and reported a cluster with a perfectly
// good data plane as having none — the tunnel kept its stale origin and the
// kernel hostnames answered 502.
func kernelEdgeServiceLabels() client.MatchingLabels {
	return client.MatchingLabels{
		"gateway.envoyproxy.io/owning-gateway-name":      KernelPublicGatewayName,
		"gateway.envoyproxy.io/owning-gateway-namespace": servicesNamespace,
	}
}

// isKernelEdgeService reports whether an object is that Service.
func isKernelEdgeService(obj client.Object) bool {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return false
	}
	return svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-name"] == KernelPublicGatewayName &&
		svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-namespace"] == servicesNamespace
}

// findKernelEdgeService returns the Envoy data-plane Service of the kernel's
// public Gateway, wherever Envoy Gateway runs.
func findKernelEdgeService(ctx context.Context, c client.Client) (*corev1.Service, error) {
	list := &corev1.ServiceList{}
	if err := c.List(ctx, list, kernelEdgeServiceLabels()); err != nil {
		return nil, fmt.Errorf("list kernel Envoy Gateway service: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("kernel Envoy Gateway service not found")
	}
	return &list.Items[0], nil
}

// kernelGatewayTunnelOrigin returns the in-cluster HTTPS origin cloudflared should
// use for tenant and kernel hostnames in tunnel mode.
func kernelGatewayTunnelOrigin(ctx context.Context, c client.Client) (string, error) {
	svc, err := findKernelEdgeService(ctx, c)
	if err != nil {
		return "", err
	}
	port := int32(443)
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == 443 {
			port = svc.Spec.Ports[i].Port
			break
		}
	}
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, port), nil
}
