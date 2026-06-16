// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ensureKernelGatewayTunnelIngress programs kernel apex and wildcard hostnames on
// the Cloudflare tunnel to reach kernel-public-gateway. Requires matchSNItoHost so
// cloudflared presents the public hostname to Envoy, not the in-cluster service name.
func ensureKernelGatewayTunnelIngress(ctx context.Context, c client.Client, cf *CloudflareDNSClient, kernelDomain string) error {
	if cf == nil || kernelDomain == "" {
		return nil
	}
	origin, err := kernelGatewayTunnelOrigin(ctx, c)
	if err != nil {
		return fmt.Errorf("resolve kernel gateway tunnel origin: %w", err)
	}
	logger := ctrl.LoggerFrom(ctx)
	for _, host := range []string{"*." + kernelDomain, kernelDomain} {
		if err := cf.ensureTunnelIngress(ctx, host, origin); err != nil {
			logger.Error(err, "ensure Cloudflare kernel tunnel ingress", "host", host, "origin", origin)
			return err
		}
	}
	return nil
}
