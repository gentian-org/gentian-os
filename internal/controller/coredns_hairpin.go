// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
)

const (
	coreDNSNamespace     = "kube-system"
	coreDNSConfigMapName = "coredns"
	coreDNSDeployment    = "coredns"
	hairpinBeginMarker    = "# BEGIN gentian-hairpin"
	hairpinEndMarker      = "# END gentian-hairpin"
	ingressServiceName    = "ingress-controller"
)

// kernelHTTPSHairpinHosts returns kernel hostnames that tenant and kernel pods
// resolve via CoreDNS hosts overrides to reach the edge proxy in-cluster.
func kernelHTTPSHairpinHosts(kernelDomain string) map[string]struct{} {
	hosts := []string{
		kernelDomain,
		"portal." + kernelDomain,
		"id." + kernelDomain,
		"files." + kernelDomain,
		"ics." + kernelDomain,
		"office." + kernelDomain,
		"openproject." + kernelDomain,
		"webmail." + kernelDomain,
		"pad." + kernelDomain,
		"pad-sandbox." + kernelDomain,
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
// resolve to edgeIP. The mail.<kernelDomain> entry is preserved (Dovecot).
func patchHairpinCorefile(corefile, edgeIP, kernelDomain string) (string, bool) {
	if edgeIP == "" || kernelDomain == "" {
		return corefile, false
	}

	httpsHosts := kernelHTTPSHairpinHosts(kernelDomain)
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
		"files." + kernelDomain,
		"ics." + kernelDomain,
		"id." + kernelDomain,
		"office." + kernelDomain,
		"openproject." + kernelDomain,
		"pad-sandbox." + kernelDomain,
		"pad." + kernelDomain,
		"portal." + kernelDomain,
		"webmail." + kernelDomain,
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

func kernelEdgeClusterIP(ctx context.Context, c client.Client, routingMode string) (string, error) {
	if isGatewayRoutingMode(routingMode) {
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

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: ingressServiceName, Namespace: ingressNamespace,
	}, svc); err != nil {
		return "", fmt.Errorf("get ingress controller service: %w", err)
	}
	if svc.Spec.ClusterIP == "" {
		return "", fmt.Errorf("ingress controller service has no ClusterIP")
	}
	return svc.Spec.ClusterIP, nil
}

func ensureCoreDNSHairpin(ctx context.Context, c client.Client, kernelDomain, routingMode string) error {
	edgeIP, err := kernelEdgeClusterIP(ctx, c, routingMode)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: coreDNSNamespace, Name: coreDNSConfigMapName,
	}, cm); err != nil {
		return fmt.Errorf("get CoreDNS ConfigMap: %w", err)
	}

	corefile := cm.Data["Corefile"]
	patched, changed := patchHairpinCorefile(corefile, edgeIP, kernelDomain)
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
