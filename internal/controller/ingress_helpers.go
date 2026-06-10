// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"strings"
)

const nginxConfigurationSnippetAnnotation = "nginx.ingress.kubernetes.io/configuration-snippet"

// keycloakProxyIngressName is the shared id.<kernel> ingress (Nubus extensions proxy).
func keycloakProxyIngressName() string {
	return envOrDefault("KEYCLOAK_PROXY_INGRESS_NAME", "nubus-dev-keycloak-extensions-proxy")
}

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

// cryptpadMainSubDomain is the main CryptPad ingress hostname prefix. CryptPad
// ships a full Content-Security-Policy (script-src without 'unsafe-eval'); the
// operator must append frame-ancestors, not replace the upstream header.
const cryptpadMainSubDomain = "pad"

// keycloakOIDCAncestorOrigins builds space-separated https origins for the
// Keycloak proxy ingress frame-ancestors policy: kernel portal plus, per tenant
// effective domain, a tenant wildcard and explicit OIDC app ingress hosts
// discovered from installed AppProfiles (see collectOIDCIngressSubdomainsByTenant).
func keycloakOIDCAncestorOrigins(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
) string {
	if kernelDomain == "" {
		return ""
	}
	seen := make(map[string]struct{})
	var origins []string
	add := func(origin string) {
		if origin == "" {
			return
		}
		if _, ok := seen[origin]; ok {
			return
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	add(fmt.Sprintf("https://portal.%s", kernelDomain))
	// Explicit kernel IdP + ICS origins (visible in curl checks; also covers browsers
	// that treat 'self' / *.kernel wildcards differently in nested iframe chains).
	add(fmt.Sprintf("https://id.%s", kernelDomain))
	add(fmt.Sprintf("https://ics.%s", kernelDomain))
	// Kernel-zone apps (Nextcloud Files, …) run on *.<kernelDomain> and may embed
	// id.<kernel> in a nested iframe during OIDC — not under tenant zones.
	add(fmt.Sprintf("https://*.%s", kernelDomain))
	for i, effective := range tenantEffectiveDomains {
		if effective == "" {
			continue
		}
		add(fmt.Sprintf("https://*.%s", effective))
		var tenantName string
		if i < len(tenantNames) {
			tenantName = tenantNames[i]
		}
		for _, sub := range tenantOIDCSubdomains[tenantName] {
			add(fmt.Sprintf("https://%s.%s", sub, effective))
		}
	}
	return strings.Join(origins, " ")
}

// keycloakOIDCIngressServerSnippet strips upstream framing headers at the server
// block (microk8s ingress sometimes misses proxy_hide_header in location only).
const nginxServerSnippetAnnotation = "nginx.ingress.kubernetes.io/server-snippet"

func keycloakOIDCIngressServerSnippet() string {
	return `proxy_hide_header X-Frame-Options;
proxy_hide_header Content-Security-Policy;`
}

// keycloakOIDCEmbeddingIngressSnippet returns NGINX directives for the shared
// Keycloak ingress so portal-embedded apps can frame OIDC login pages.
func keycloakOIDCEmbeddingIngressSnippet(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
) string {
	// Repeat hide directives in the location block; browsers enforce X-Frame-Options
	// before CSP frame-ancestors, so SAMEORIGIN from Keycloak blocks broker endpoints
	// even when frame-ancestors lists chat.<tenant>.
	return keycloakOIDCIngressServerSnippet() + "\n" +
		frameAncestorsIngressSnippetReplace(keycloakOIDCAncestorOrigins(
			kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames))
}

// portalEmbeddingIngressSnippet returns NGINX directives that allow the shared
// kernel portal (portal.<kernelDomain>) to embed the app in an iframe.
func portalEmbeddingIngressSnippet(kernelDomain string) string {
	portalOrigin := fmt.Sprintf("https://portal.%s", kernelDomain)
	return frameAncestorsIngressSnippetReplace(portalOrigin)
}

func portalEmbeddingIngressSnippetAppend(kernelDomain string) string {
	portalOrigin := fmt.Sprintf("https://portal.%s", kernelDomain)
	return frameAncestorsIngressSnippetAppend(portalOrigin)
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
	return frameAncestorsIngressSnippetAppend(origins)
}

// frameAncestorsIngressSnippetReplace clears upstream X-Frame-Options and
// Content-Security-Policy, then sets a single frame-ancestors policy. Use for
// standard AppProfile apps (Element, Jitsi, OpenProject, …) whose nginx only
// emits frame-ancestors 'self' — appending a second CSP header leaves both
// policies active and browsers still block portal embedding.
func frameAncestorsIngressSnippetReplace(ancestorOrigins string) string {
	// proxy_hide_header works on stock ingress-nginx (including microk8s builds
	// that lack the headers-more module). more_clear_headers is not available there.
	return fmt.Sprintf(`proxy_hide_header X-Frame-Options;
proxy_hide_header Content-Security-Policy;
add_header Content-Security-Policy "frame-ancestors 'self' %s" always;`, ancestorOrigins)
}

// frameAncestorsIngressSnippetAppend adds a second CSP header without clearing
// the upstream policy. Required for CryptPad, which relies on upstream
// script-src/connect-src (sandbox must not gain 'unsafe-eval').
func frameAncestorsIngressSnippetAppend(ancestorOrigins string) string {
	return fmt.Sprintf(`proxy_hide_header X-Frame-Options;
add_header Content-Security-Policy "frame-ancestors 'self' %s" always;`, ancestorOrigins)
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
			strings.Contains(trimmed, `proxy_hide_header X-Frame-Options`) ||
			strings.Contains(trimmed, `proxy_hide_header Content-Security-Policy`) ||
			strings.Contains(trimmed, `more_set_headers "Content-Security-Policy`) ||
			strings.Contains(trimmed, `more_add_headers "Content-Security-Policy`) ||
			strings.Contains(trimmed, `add_header Content-Security-Policy`) ||
			strings.Contains(trimmed, "frame-ancestors") ||
			strings.Contains(trimmed, "nginx.ingress.kubernetes.io/") {
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
	case ingressSubDomain == cryptpadMainSubDomain && kernelDomain != "":
		embedding = portalEmbeddingIngressSnippetAppend(kernelDomain)
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
