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
	"fmt"
	"strings"
)

const nginxConfigurationSnippetAnnotation = "nginx.ingress.kubernetes.io/configuration-snippet"

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
	// Explicit kernel IdP origin (visible in curl checks; also covers browsers
	// that treat 'self' / *.kernel wildcards differently in nested iframe chains).
	add(fmt.Sprintf("https://id.%s", kernelDomain))
	// Kernel-zone apps run on *.<kernelDomain> and may embed id.<kernel> in a
	// nested iframe during OIDC — not under tenant zones.
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
// block (legacy nginx ingress sometimes missed proxy_hide_header in location only).
const (
	nginxProxyBodySizeAnnotation        = "nginx.ingress.kubernetes.io/proxy-body-size"
	nginxProxyBufferSizeAnnotation      = "nginx.ingress.kubernetes.io/proxy-buffer-size"
	nginxProxyBuffersNumberAnnotation   = "nginx.ingress.kubernetes.io/proxy-buffers-number"
	nginxProxyBusyBuffersSizeAnnotation = "nginx.ingress.kubernetes.io/proxy-busy-buffers-size"
)

// keycloakProxyIngressBufferAnnotations prevents nginx 502 "upstream sent too big
// header" on broker /endpoint redirects (Keycloak sets large session cookies).
var keycloakProxyIngressBufferAnnotations = map[string]string{
	nginxProxyBodySizeAnnotation:        "128k",
	nginxProxyBufferSizeAnnotation:      "64k",
	nginxProxyBuffersNumberAnnotation:   "4",
	nginxProxyBusyBuffersSizeAnnotation: "128k",
}

func ensureKeycloakProxyIngressBuffers(annotations map[string]string) {
	for k, v := range keycloakProxyIngressBufferAnnotations {
		annotations[k] = v
	}
}

func keycloakProxyIngressBuffersApplied(annotations map[string]string) bool {
	for k, v := range keycloakProxyIngressBufferAnnotations {
		if annotations[k] != v {
			return false
		}
	}
	return true
}

func keycloakOIDCIngressServerSnippet() string {
	// proxy_hide_header alone is not always enough on microk8s ingress-nginx; an
	// empty X-Frame-Options override prevents Keycloak SAMEORIGIN from blocking
	// broker /endpoint callbacks in portal iframes (see keycloak_browser_security.go).
	return `proxy_hide_header X-Frame-Options;
add_header X-Frame-Options "" always;
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

// frameAncestorsIngressSnippetReplace clears upstream X-Frame-Options and
// Content-Security-Policy, then sets a single frame-ancestors policy. Use for
// standard AppProfile apps (Element, Jitsi, OpenProject, …) whose nginx only
// emits frame-ancestors 'self' — appending a second CSP header leaves both
// policies active and browsers still block portal embedding.
func frameAncestorsIngressSnippetReplace(ancestorOrigins string) string {
	// proxy_hide_header works on stock ingress-nginx (including microk8s builds
	// that lack the headers-more module). more_clear_headers is not available there.
	return fmt.Sprintf(`proxy_hide_header X-Frame-Options;
add_header X-Frame-Options "" always;
proxy_hide_header Content-Security-Policy;
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

// ensurePortalEmbeddingAnnotations merges iframe CSP into ingress annotations so
// the shared kernel portal (portal.<kernelDomain>) can embed the app.
func ensurePortalEmbeddingAnnotations(annotations map[string]string, kernelDomain, _, _ string) {
	if kernelDomain == "" {
		return
	}
	embedding := portalEmbeddingIngressSnippet(kernelDomain)
	if existing, ok := annotations[nginxConfigurationSnippetAnnotation]; ok && strings.TrimSpace(existing) != "" {
		if rest := stripLegacyPortalEmbeddingSnippet(existing); rest != "" {
			annotations[nginxConfigurationSnippetAnnotation] = embedding + "\n" + rest
			return
		}
	}
	annotations[nginxConfigurationSnippetAnnotation] = embedding
}
