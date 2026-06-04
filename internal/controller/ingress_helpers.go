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

// cryptpadSandboxIngressSnippet allows the main CryptPad origin to embed the
// sandbox iframe (pad.<tenant> → pad-sandbox.<tenant>).
func cryptpadSandboxIngressSnippet(effectiveDomain string) string {
	parentOrigin := fmt.Sprintf("https://pad.%s", effectiveDomain)
	return frameAncestorsIngressSnippet(parentOrigin)
}

func frameAncestorsIngressSnippet(ancestorOrigin string) string {
	return fmt.Sprintf(`more_clear_headers "X-Frame-Options";
more_clear_headers "Content-Security-Policy";
more_set_headers "Content-Security-Policy: frame-ancestors 'self' %s";`, ancestorOrigin)
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
		embedding = cryptpadSandboxIngressSnippet(effectiveDomain)
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
