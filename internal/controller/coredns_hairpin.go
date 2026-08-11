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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	coreDNSNamespace     = "kube-system"
	coreDNSConfigMapName = "coredns"
	coreDNSDeployment    = "coredns"
	hairpinBeginMarker   = "# BEGIN gentian-hairpin"
	hairpinEndMarker     = "# END gentian-hairpin"
)

// kernelHTTPSHairpinHosts returns kernel hostnames that tenant and kernel pods
// resolve via CoreDNS hosts overrides to reach the edge proxy in-cluster.
func kernelHTTPSHairpinHosts(kernelDomain string) map[string]struct{} {
	hosts := []string{
		kernelDomain,
		"portal." + kernelDomain,
		"id." + kernelDomain,
		"argocd." + kernelDomain,
	}
	out := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		out[h] = struct{}{}
	}
	return out
}

func kernelMailHairpinHost(kernelDomain string) string {
	return "mail." + kernelDomain
}

// patchHairpinCorefile updates the gentian-hairpin block so kernel HTTPS hosts
// and optional tenant app hostnames resolve to edgeIP. The mail.<kernelDomain>
// entry is preserved (Dovecot).
func patchHairpinCorefile(corefile, edgeIP, kernelDomain string, tenantHosts map[string]struct{}) (string, bool) {
	if edgeIP == "" || kernelDomain == "" {
		return corefile, false
	}

	httpsHosts := kernelHTTPSHairpinHosts(kernelDomain)
	for host := range tenantHosts {
		httpsHosts[host] = struct{}{}
	}
	mailHost := kernelMailHairpinHost(kernelDomain)

	beginIdx := strings.Index(corefile, hairpinBeginMarker)
	endIdx := strings.Index(corefile, hairpinEndMarker)
	if beginIdx == -1 || endIdx == -1 || endIdx < beginIdx {
		block := buildHairpinBlock(edgeIP, kernelDomain, "")
		if idx := strings.Index(corefile, "hosts {"); idx != -1 {
			insertAt := idx + len("hosts {")
			patched := corefile[:insertAt] + "\n" + block + corefile[insertAt:]
			return patched, true
		}
		return corefile, false
	}

	block := corefile[beginIdx : endIdx+len(hairpinEndMarker)]
	lines := strings.Split(block, "\n")
	present := make(map[string]bool)
	changed := false
	var outLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == hairpinBeginMarker || trimmed == hairpinEndMarker || trimmed == "" {
			outLines = append(outLines, line)
			continue
		}

		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			outLines = append(outLines, line)
			continue
		}

		ip, host := parts[0], parts[1]
		if host == mailHost {
			outLines = append(outLines, line)
			present[host] = true
			continue
		}

		if _, ok := httpsHosts[host]; ok {
			present[host] = true
			if ip != edgeIP {
				changed = true
			}
			outLines = append(outLines, fmt.Sprintf("          %s %s", edgeIP, host))
			continue
		}

		outLines = append(outLines, line)
	}

	for host := range httpsHosts {
		if present[host] {
			continue
		}
		outLines = insertBeforeMarker(outLines, hairpinEndMarker, fmt.Sprintf("          %s %s", edgeIP, host))
		changed = true
	}

	if !changed {
		return corefile, false
	}

	patchedBlock := strings.Join(outLines, "\n")
	return corefile[:beginIdx] + patchedBlock + corefile[endIdx+len(hairpinEndMarker):], true
}

func buildHairpinBlock(edgeIP, kernelDomain string, existingMailLine string) string {
	var b strings.Builder
	b.WriteString("          ")
	b.WriteString(hairpinBeginMarker)
	b.WriteByte('\n')
	for _, host := range sortedHairpinHosts(kernelDomain) {
		fmt.Fprintf(&b, "          %s %s\n", edgeIP, host)
	}
	if existingMailLine != "" {
		b.WriteString(existingMailLine)
		if !strings.HasSuffix(existingMailLine, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("          ")
	b.WriteString(hairpinEndMarker)
	return b.String()
}

func sortedHairpinHosts(kernelDomain string) []string {
	return []string{
		kernelDomain,
		"argocd." + kernelDomain,
		"id." + kernelDomain,
		"portal." + kernelDomain,
	}
}

func insertBeforeMarker(lines []string, marker, newLine string) []string {
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			out := append([]string{}, lines[:i]...)
			out = append(out, newLine)
			out = append(out, lines[i:]...)
			return out
		}
	}
	return append(lines, newLine)
}

func kernelEdgeClusterIP(ctx context.Context, c client.Client, _ string) (string, error) {
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
	ip := list.Items[0].Spec.ClusterIP
	if ip == "" {
		return "", fmt.Errorf("kernel Envoy Gateway service has no ClusterIP")
	}
	return ip, nil
}

func ensureCoreDNSHairpin(ctx context.Context, c client.Client, kernelDomain, tenancyMode, routingMode string) error {
	edgeIP, err := kernelEdgeClusterIP(ctx, c, routingMode)
	if err != nil {
		return err
	}

	tenantHosts, err := collectTenantAppHairpinHosts(ctx, c, kernelDomain, tenancyMode)
	if err != nil {
		return err
	}

	cmName, err := resolveCoreDNSConfigMapName(ctx, c)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: coreDNSNamespace, Name: cmName,
	}, cm); err != nil {
		return fmt.Errorf("get CoreDNS ConfigMap %q: %w", cmName, err)
	}

	corefile := cm.Data["Corefile"]
	patched, changed := patchHairpinCorefile(corefile, edgeIP, kernelDomain, tenantHosts)
	if !changed {
		return nil
	}

	patch := client.MergeFrom(cm.DeepCopy())
	cm.Data["Corefile"] = patched
	if err := c.Patch(ctx, cm, patch); err != nil {
		return fmt.Errorf("patch CoreDNS ConfigMap: %w", err)
	}
	return restartCoreDNSDeployment(ctx, c)
}

// resolveCoreDNSConfigMapName finds the ConfigMap backing CoreDNS by reading
// the volume the CoreDNS Deployment actually mounts, falling back to the
// upstream "coredns" name.
//
// The name is not portable. kubeadm and most distributions use "coredns", but
// managed providers prefix it per cluster — Infomaniak serves this cluster's
// CoreDNS from "pck-whmxvl2-addon-coredns". Hardcoding the upstream name made
// every hairpin reconcile fail with `ConfigMap "coredns" not found`, and
// because that error propagates up through the tenant stage pipeline it
// short-circuited the reconcile before Finalize: the tenant's NamespaceReady
// and CrossplaneReady conditions froze at their initial "waiting" values while
// the namespace was Active and the XTenant already Ready, and the phase stayed
// Provisioning forever. The visible symptom was a tenant that never converged,
// with nothing pointing at DNS.
//
// Reading the Deployment is the reliable discovery path: whatever CoreDNS is
// actually serving, it is mounted there. A cluster whose CoreDNS Deployment is
// itself named differently still falls back to the constant, which fails with
// the same clear error as before rather than silently doing nothing.
func resolveCoreDNSConfigMapName(ctx context.Context, c client.Client) (string, error) {
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: coreDNSNamespace, Name: coreDNSDeployment,
	}, dep); err != nil {
		if errors.IsNotFound(err) {
			return coreDNSConfigMapName, nil
		}
		return "", fmt.Errorf("get CoreDNS Deployment for ConfigMap discovery: %w", err)
	}

	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name != "" {
			return v.ConfigMap.Name, nil
		}
	}
	return coreDNSConfigMapName, nil
}

func collectTenantAppHairpinHosts(ctx context.Context, c client.Client, kernelDomain, tenancyMode string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if kernelDomain == "" {
		return out, nil
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return nil, fmt.Errorf("list tenants for CoreDNS hairpin: %w", err)
	}
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		effectiveDomain := tenant.EffectiveDomain(kernelDomain, tenancyMode)
		if effectiveDomain == "" {
			continue
		}
		intents, err := collectTenantIngressIntents(ctx, c, tenant)
		if err != nil {
			return nil, err
		}
		for _, intent := range intents {
			if intent.ingress == nil {
				continue
			}
			out[ingressHost(intent.appProfile, intent.ingress, effectiveDomain)] = struct{}{}
		}
	}
	return out, nil
}

func restartCoreDNSDeployment(ctx context.Context, c client.Client) error {
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: coreDNSNamespace, Name: coreDNSDeployment,
	}, dep); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get CoreDNS Deployment: %w", err)
	}

	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().UTC().Format(time.RFC3339)
	if err := c.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("restart CoreDNS Deployment: %w", err)
	}
	return nil
}
