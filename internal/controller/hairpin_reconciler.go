// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	// ingressServiceName is the well-known name of the ClusterIP Service that
	// fronts the ingress controller inside the cluster.  Created by the
	// bootstrap script (install.sh) as part of the pre-flight setup.
	ingressServiceName = "ingress-controller"

	// CoreDNS ConfigMap location (standard for all K8s distributions).
	corednsConfigMapName = "coredns"
	corednsNamespace     = "kube-system"

	// Sentinel comments that delimit the operator-managed section inside the
	// CoreDNS Corefile "hosts" block.
	hairpinBegin = "# BEGIN gentian-hairpin"
	hairpinEnd   = "# END gentian-hairpin"
)

// ensureHairpinDNS makes sure CoreDNS resolves every tenant Ingress hostname
// to the ingress controller's ClusterIP so that pod→ingress "hairpin" traffic
// works regardless of the external DNS configuration.
//
// The function collects Ingress hostnames from ALL tenants, looks up the
// ingress-controller Service ClusterIP, and patches the CoreDNS Corefile to
// include a "hosts" block (or a sentineled section inside an existing one).
func (r *TenantReconciler) ensureHairpinDNS(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// ── 1. Look up the ingress controller Service ClusterIP ──────────────
	ingressSvc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      ingressServiceName,
		Namespace: ingressNamespace,
	}, ingressSvc)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ingress controller Service not found; skipping hairpin DNS",
				"service", ingressServiceName, "namespace", ingressNamespace)
			return nil
		}
		return fmt.Errorf("get ingress Service: %w", err)
	}
	clusterIP := ingressSvc.Spec.ClusterIP
	if clusterIP == "" || clusterIP == "None" {
		logger.Info("ingress controller Service has no ClusterIP; skipping hairpin DNS")
		return nil
	}

	// ── 2. Collect all Ingress hostnames across tenants ──────────────────
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	// Build set of tenant domains for filtering.
	tenantDomains := map[string]struct{}{}
	seen := map[string]struct{}{}
	for i := range tenantList.Items {
		t := &tenantList.Items[i]
		if !t.DeletionTimestamp.IsZero() {
			continue
		}
		tenantDomains[t.Spec.Domain] = struct{}{}
		for _, app := range t.Spec.Apps {
			profile := &gentianov1alpha1.AppProfile{}
			if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
				continue // deleted or not found — skip
			}
			if profile.Spec.Ingress != nil {
				host := ingressHost(app.Profile, profile.Spec.Ingress, t.Spec.Domain)
				seen[host] = struct{}{}
			}
		}
	}

	// Also scan actual Ingress resources cluster-wide for hosts that belong
	// to a tenant domain.  This picks up kernel-managed ingresses (e.g.
	// Nextcloud at files.{domain}, Keycloak at id.{domain}) that are not
	// defined via AppProfiles.
	ingressList := &networkingv1.IngressList{}
	if err := r.List(ctx, ingressList); err != nil {
		logger.Error(err, "failed to list Ingress resources for hairpin DNS")
	} else {
		for i := range ingressList.Items {
			for _, rule := range ingressList.Items[i].Spec.Rules {
				if rule.Host == "" {
					continue
				}
				if matchesTenantDomain(rule.Host, tenantDomains) {
					seen[rule.Host] = struct{}{}
				}
			}
		}
	}

	hosts := make([]string, 0, len(seen))
	for h := range seen {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	// ── 3. Build the desired sentineled hosts content ────────────────────
	var sb strings.Builder
	sb.WriteString(hairpinBegin)
	sb.WriteByte('\n')
	for _, h := range hosts {
		fmt.Fprintf(&sb, "          %s %s\n", clusterIP, h)
	}
	sb.WriteString("          ")
	sb.WriteString(hairpinEnd)
	desired := sb.String()

	// ── 4. Read the CoreDNS ConfigMap ────────────────────────────────────
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      corednsConfigMapName,
		Namespace: corednsNamespace,
	}, cm); err != nil {
		return fmt.Errorf("get CoreDNS ConfigMap: %w", err)
	}

	corefile, ok := cm.Data["Corefile"]
	if !ok {
		return fmt.Errorf("CoreDNS ConfigMap has no Corefile key")
	}

	// ── 5. Splice the sentineled block into the Corefile ─────────────────
	updated := spliceHairpinHosts(corefile, desired)
	if updated == corefile {
		return nil // nothing changed
	}

	// ── 6. Patch the ConfigMap ───────────────────────────────────────────
	patch := client.MergeFrom(cm.DeepCopy())
	cm.Data["Corefile"] = updated
	if err := r.Patch(ctx, cm, patch); err != nil {
		return fmt.Errorf("patch CoreDNS ConfigMap: %w", err)
	}

	logger.Info("updated CoreDNS hairpin DNS entries",
		"hosts", len(hosts), "ingressIP", clusterIP)
	return nil
}

// spliceHairpinHosts inserts or replaces the sentineled hairpin section in a
// CoreDNS Corefile.  Three cases:
//
//  1. Sentinels already present → replace the content between them.
//  2. A "hosts {" block exists without sentinels → inject before "fallthrough".
//  3. No "hosts" block at all → inject a complete block before "kubernetes".
func spliceHairpinHosts(corefile, desired string) string {
	// Case 1: sentinels exist — replace in-place
	beginIdx := strings.Index(corefile, hairpinBegin)
	endIdx := strings.Index(corefile, hairpinEnd)
	if beginIdx >= 0 && endIdx > beginIdx {
		return corefile[:beginIdx] + desired + corefile[endIdx+len(hairpinEnd):]
	}

	// Case 2: hosts block exists without sentinels — inject before fallthrough
	hostsIdx := strings.Index(corefile, "hosts {")
	if hostsIdx < 0 {
		hostsIdx = strings.Index(corefile, "hosts{")
	}
	if hostsIdx >= 0 {
		// Find the closing brace for this block
		blockStart := hostsIdx + strings.Index(corefile[hostsIdx:], "{")
		closingBrace := findMatchingBrace(corefile, blockStart)
		if closingBrace < 0 {
			return corefile
		}

		// Look for "fallthrough" inside the hosts block
		blockContent := corefile[blockStart+1 : closingBrace]
		ftIdx := strings.Index(blockContent, "fallthrough")
		if ftIdx >= 0 {
			// Insert before fallthrough
			insertPos := blockStart + 1 + ftIdx
			return corefile[:insertPos] + desired + "\n          " + corefile[insertPos:]
		}
		// No fallthrough — insert before closing brace, with fallthrough
		return corefile[:closingBrace] + "  " + desired + "\n          fallthrough\n        " + corefile[closingBrace:]
	}

	// Case 3: no hosts block — inject a complete one before the "kubernetes" directive
	kubeIdx := strings.Index(corefile, "kubernetes ")
	if kubeIdx < 0 {
		return corefile // cannot find insertion point
	}
	// Detect indentation of the kubernetes line
	lineStart := strings.LastIndex(corefile[:kubeIdx], "\n")
	indent := ""
	if lineStart >= 0 {
		indent = corefile[lineStart+1 : kubeIdx]
	}
	hostsBlock := fmt.Sprintf("%shosts {\n%s  %s\n%s  fallthrough\n%s}\n",
		indent, indent, desired, indent, indent)
	return corefile[:kubeIdx] + hostsBlock + corefile[kubeIdx:]
}

// findMatchingBrace returns the index of the closing '}' that matches the '{'
// at position openIdx, accounting for nested braces.
func findMatchingBrace(s string, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchesTenantDomain returns true if host is a subdomain of any tenant domain
// (e.g. "files.desk.gentian.org" matches "desk.gentian.org").
func matchesTenantDomain(host string, tenantDomains map[string]struct{}) bool {
	for domain := range tenantDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
