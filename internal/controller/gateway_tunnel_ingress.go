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
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ensureKernelGatewayTunnelIngress programs explicit kernel and tenant apex hostnames
// on the Cloudflare tunnel to reach kernel-public-gateway. Wildcard tunnel hostname
// rules such as *.platform.example.com are unreliable for multi-label kernel domains;
// tenant app wildcards (*.demo.platform.example.com) are handled per-tenant separately.
func ensureKernelGatewayTunnelIngress(
	ctx context.Context,
	c client.Client,
	edge *Edge,
	kernelDomain, tenancyMode string,
) error {
	if edge == nil || kernelDomain == "" {
		return nil
	}
	origin, err := kernelGatewayTunnelOrigin(ctx, c)
	if err != nil {
		return fmt.Errorf("resolve kernel gateway tunnel origin: %w", err)
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for kernel tunnel ingress: %w", err)
	}
	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, c, tenantList.Items)
	if err != nil {
		return fmt.Errorf("collect OIDC ingress subdomains: %w", err)
	}

	hosts := map[string]struct{}{
		kernelDomain:                   {},
		kernelPortalHost(kernelDomain): {},
		"corp." + kernelDomain:         {},
	}
	for _, spec := range kernelHTTPRouteSpecs(kernelDomain, effectiveDomains, oidcSubs, tenantNames,
		clusterLLMEnabled(ctx, c)) {
		if spec.host != "" {
			hosts[spec.host] = struct{}{}
		}
	}
	for _, d := range effectiveDomains {
		hosts[d] = struct{}{}
	}

	logger := ctrl.LoggerFrom(ctx)
	sorted := make([]string, 0, len(hosts))
	for host := range hosts {
		sorted = append(sorted, host)
	}
	sort.Strings(sorted)
	for _, host := range sorted {
		// Route and record together. An ingress rule for a hostname nothing
		// resolves is unreachable, so EnsureHostname does both rather than
		// leaving it to each caller to remember the second half — which is
		// exactly what this path forgot: it programmed routes for every kernel
		// hostname and wrote no DNS at all.
		//
		// It stayed invisible because every cluster until now ran on a zone
		// whose kernel records predated the install, made by hand. A new zone
		// has none, so id.<domain> and portal.<domain> never resolved, D-03
		// waited out its full 15 minutes, and the warning it prints blames the
		// Cloudflare credential — which is the one thing that was working.
		if err := edge.EnsureHostname(ctx, host, origin); err != nil {
			logger.Error(err, "ensure kernel edge hostname", "host", host, "origin", origin)
			return err
		}
	}
	// Direct, not through Edge: this removes a stale ROUTE without touching
	// DNS. The wildcard record is still wanted; only the wildcard tunnel rule
	// is being retired in favour of the explicit per-host rules above.
	if edge.Ingress != nil {
		if err := edge.Ingress.DeleteRoute(ctx, "*."+kernelDomain); err != nil {
			logger.Error(err, "delete kernel wildcard tunnel route", "host", "*."+kernelDomain)
			return err
		}
	}
	return nil
}
