// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"strings"
)

const nginxConfigurationSnippetAnnotation = "nginx.ingress.kubernetes.io/configuration-snippet"

// substituteIngressAnnotationPlaceholders expands AppProfile ingress annotation
// templates. ${TENANT_DOMAIN} is the tenant effective domain; ${KERNEL_DOMAIN} is
// the cluster platform domain (portal, Keycloak, …).
func substituteIngressAnnotationPlaceholders(s, effectiveDomain, kernelDomain string) string {
	s = strings.ReplaceAll(s, "${TENANT_DOMAIN}", effectiveDomain)
	s = strings.ReplaceAll(s, "${KERNEL_DOMAIN}", kernelDomain)
	return s
}

// cryptpadSandboxSubDomain is the additional CryptPad ingress hostname prefix for
// httpSafeOrigin (client-side crypto isolation). That origin is framed by the
// main CryptPad host (pad.<tenant>), not by the kernel portal.
const cryptpadSandboxSubDomain = "pad-sandbox"

// portalEmbeddingIngressSnippet returns NGINX directives that allow the shared
// kernel portal (portal.<kernelDomain>) to embed the app in an iframe.
func portalEmbeddingIngressSnippet(kernelDomain string) string {
	portalOrigin := fmt.Sprintf("https://portal.%s", kernelDomain)
	return frameAncestorsIngressSnippet(portalOrigin)
}

// cryptpadSandboxIngressSnippet allows the main CryptPad origin and the shared
// kernel portal to embed the sandbox iframe. CSP frame-ancestors checks the full
// ancestor chain: portal → pad.<tenant> → pad-sandbox.<tenant> when the portal
// opens CryptPad in an embedded window; pad alone is sufficient in a top-level tab.
func cryptpadSandboxIngressSnippet(effectiveDomain, kernelDomain string) string {
	origins := fmt.Sprintf("https://pad.%s", effectiveDomain)
	if kernelDomain != "" {
		origins += fmt.Sprintf(" https://portal.%s", kernelDomain)
	}
	return frameAncestorsIngressSnippet(origins)
}

func frameAncestorsIngressSnippet(ancestorOrigin string) string {
	// Append a second CSP policy instead of replacing the app's header. CryptPad
	// (and similar apps) rely on upstream script-src/connect-src rules — e.g. the
	// sandbox origin must keep script-src without 'unsafe-eval' so client-side
	// isolation self-tests pass. Clearing Content-Security-Policy and setting only
	// frame-ancestors breaks those apps with "eval should not be permitted".
	// Use native add_header (not headers-more more_add_headers): microk8s and
	// several ingress-nginx builds only expose more_set_headers/more_clear_headers.
	return fmt.Sprintf(`more_clear_headers "X-Frame-Options";
add_header Content-Security-Policy "frame-ancestors 'self' %s" always;`, ancestorOrigin)
}

// stripLegacyPortalEmbeddingSnippet removes per-profile frame-ancestors / X-Frame-Options
// lines so the operator can inject the canonical kernel-portal CSP.
func stripLegacyPortalEmbeddingSnippet(snippet string) string {
	var kept []string
	for _, line := range strings.Split(snippet, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, `more_clear_headers "X-Frame-Options"`) ||
			strings.Contains(trimmed, `more_clear_headers "Content-Security-Policy"`) ||
			strings.Contains(trimmed, `more_set_headers "Content-Security-Policy`) ||
			strings.Contains(trimmed, `more_add_headers "Content-Security-Policy`) ||
			strings.Contains(trimmed, `add_header Content-Security-Policy`) ||
			strings.Contains(trimmed, "frame-ancestors") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// ensurePortalEmbeddingAnnotations merges iframe CSP into ingress annotations.
// Most app ingresses allow the shared kernel portal (portal.<kernelDomain>).
// CryptPad's pad-sandbox additional ingress instead allows the main app origin
// (pad.<tenantDomain>) because the sandbox is embedded inside CryptPad, not the portal.
func ensurePortalEmbeddingAnnotations(annotations map[string]string, kernelDomain, effectiveDomain, ingressSubDomain string) {
	var embedding string
	switch {
	case ingressSubDomain == cryptpadSandboxSubDomain && effectiveDomain != "":
		embedding = cryptpadSandboxIngressSnippet(effectiveDomain, kernelDomain)
	case kernelDomain != "":
		embedding = portalEmbeddingIngressSnippet(kernelDomain)
	default:
		return
	}
	if existing, ok := annotations[nginxConfigurationSnippetAnnotation]; ok && strings.TrimSpace(existing) != "" {
		if rest := stripLegacyPortalEmbeddingSnippet(existing); rest != "" {
			annotations[nginxConfigurationSnippetAnnotation] = embedding + "\n" + rest
			return
		}
	}
	annotations[nginxConfigurationSnippetAnnotation] = embedding
}
