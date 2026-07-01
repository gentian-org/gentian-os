// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"strings"
)

const nginxConfigurationSnippetAnnotation = "nginx.ingress.kubernetes.io/configuration-snippet"

// cryptpadSandboxSubDomain is the additional CryptPad ingress hostname prefix for
// httpSafeOrigin (client-side crypto isolation). That origin is framed by the
// main CryptPad host (pad.<tenant>), not by the kernel portal.
const cryptpadSandboxSubDomain = "pad-sandbox"

// cryptpadMainSubDomain is the main CryptPad ingress hostname prefix. CryptPad
// ships a full Content-Security-Policy (script-src without 'unsafe-eval'); the
// operator must append frame-ancestors, not replace the upstream header.
const cryptpadMainSubDomain = "pad"

// collaboraSubDomain is the per-tenant Collabora ingress prefix (Nextcloud Office).
const collaboraSubDomain = "collabora"

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

func portalEmbeddingIngressSnippetAppend(kernelDomain string) string {
	portalOrigin := fmt.Sprintf("https://portal.%s", kernelDomain)
	return frameAncestorsIngressSnippetAppend(portalOrigin)
}

// cryptpadSandboxFrameAncestorOrigins lists https origins allowed to embed
// pad-sandbox.<padDomain>. Shared by kernel HTTPRoutes, tenant AppProfile
// ingress snippets, and Gateway API response filters (DRY embedding policy).
//
// Nextcloud Files (files.<kernelDomain>) may iframe the sandbox directly via
// openincryptpad, not only via pad.<padDomain>. With CryptPad's upstream CSP
// still present, the appended frame-ancestors policy must allow every direct
// parent — browsers enforce all CSP headers.
func cryptpadSandboxFrameAncestorOrigins(kernelDomain, padDomain string) string {
	var origins []string
	add := func(origin string) {
		if origin != "" {
			origins = append(origins, origin)
		}
	}
	add(padOrigin(padDomain))
	add(kernelPortalOrigin(kernelDomain))
	add(kernelFilesOrigin(kernelDomain))
	return strings.Join(origins, " ")
}

// cryptpadKernelMainFrameAncestorOrigins lists embedders for the shared kernel
// CryptPad service at pad.<kernelDomain> (diagrams.net from Nextcloud Files).
func cryptpadKernelMainFrameAncestorOrigins(kernelDomain string) string {
	return strings.Join([]string{
		kernelFilesOrigin(kernelDomain),
		kernelPortalOrigin(kernelDomain),
	}, " ")
}

func kernelPortalOrigin(kernelDomain string) string {
	if kernelDomain == "" {
		return ""
	}
	return fmt.Sprintf("https://portal.%s", kernelDomain)
}

func kernelFilesOrigin(kernelDomain string) string {
	if kernelDomain == "" {
		return ""
	}
	return fmt.Sprintf("https://files.%s", kernelDomain)
}

func padOrigin(padDomain string) string {
	if padDomain == "" {
		return ""
	}
	return fmt.Sprintf("https://pad.%s", padDomain)
}

// cryptpadSandboxIngressSnippet allows pad, portal, and kernel Nextcloud Files
// to embed the CryptPad sandbox iframe (append mode — preserve upstream script-src).
func cryptpadSandboxIngressSnippet(effectiveDomain, kernelDomain string) string {
	return frameAncestorsIngressSnippetAppend(cryptpadSandboxFrameAncestorOrigins(kernelDomain, effectiveDomain))
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

// frameAncestorsIngressSnippetAppend adds a second CSP header without clearing
// the upstream policy. Required for CryptPad, which relies on upstream
// script-src/connect-src (sandbox must not gain 'unsafe-eval').
func frameAncestorsIngressSnippetAppend(ancestorOrigins string) string {
	return fmt.Sprintf(`proxy_hide_header X-Frame-Options;
add_header X-Frame-Options "" always;
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
