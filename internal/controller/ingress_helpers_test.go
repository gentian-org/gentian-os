/*
Copyright 2026 The Gentian Authors.

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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsPortalRedirectIngress(t *testing.T) {
	portalRedirect := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-demo-portal-redirect",
			Labels: map[string]string{
				managedByLabel:               managedByValue,
				tenantLabel:                  "demo",
				portalRedirectComponentLabel: portalRedirectComponentValue,
			},
		},
	}
	if !isPortalRedirectIngress(portalRedirect) {
		t.Fatal("expected portal redirect ingress to be recognized")
	}
	app := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ingress-demo-jitsi",
			Labels: map[string]string{
				managedByLabel: managedByValue,
				tenantLabel:    "demo",
			},
		},
	}
	if isPortalRedirectIngress(app) {
		t.Fatal("expected app ingress not to be treated as portal redirect")
	}
}

func TestPortalEmbeddingIngressSnippetUsesKernelPortal(t *testing.T) {
	snippet := portalEmbeddingIngressSnippet("desk.gentian.org")
	if !strings.Contains(snippet, "https://portal.desk.gentian.org") {
		t.Fatalf("expected kernel portal origin in snippet, got:\n%s", snippet)
	}
	if strings.Contains(snippet, "portal.demo.desk.gentian.org") {
		t.Fatal("must not use tenant-scoped portal hostname")
	}
	if !strings.Contains(snippet, `proxy_hide_header Content-Security-Policy`) {
		t.Fatal("standard apps must replace upstream CSP (Element double-header bug)")
	}
}

func TestEnsurePortalEmbeddingAnnotationsReplacesLegacyTenantPortalCSP(t *testing.T) {
	annotations := map[string]string{
		nginxConfigurationSnippetAnnotation: `more_clear_headers "X-Frame-Options";
more_clear_headers "Content-Security-Policy";
more_set_headers "Content-Security-Policy: frame-ancestors 'self' https://portal.demo.desk.gentian.org";`,
	}
	ensurePortalEmbeddingAnnotations(annotations, "desk.gentian.org", "demo.desk.gentian.org", "meet")
	got := annotations[nginxConfigurationSnippetAnnotation]
	if !strings.Contains(got, "https://portal.desk.gentian.org") {
		t.Fatalf("expected kernel portal in merged snippet, got:\n%s", got)
	}
	if strings.Contains(got, "portal.demo.desk.gentian.org") {
		t.Fatal("legacy tenant portal origin must be removed")
	}
}

func TestEnsurePortalEmbeddingAnnotationsPreservesCustomSnippet(t *testing.T) {
	annotations := map[string]string{
		nginxConfigurationSnippetAnnotation: `proxy_set_header Accept-Encoding "";
sub_filter_once on;`,
	}
	ensurePortalEmbeddingAnnotations(annotations, "desk.gentian.org", "demo.desk.gentian.org", "chat")
	got := annotations[nginxConfigurationSnippetAnnotation]
	if !strings.Contains(got, "sub_filter_once on;") {
		t.Fatalf("expected custom snippet preserved, got:\n%s", got)
	}
	if !strings.Contains(got, "https://portal.desk.gentian.org") {
		t.Fatalf("expected kernel portal in merged snippet, got:\n%s", got)
	}
}

func TestEnsurePortalEmbeddingAnnotationsReplacesUpstreamCSPForElement(t *testing.T) {
	annotations := map[string]string{
		nginxConfigurationSnippetAnnotation: `nginx.ingress.kubernetes.io/proxy-body-size: 100M`,
	}
	ensurePortalEmbeddingAnnotations(annotations, "desk.gentian.org", "demo.desk.gentian.org", "chat")
	got := annotations[nginxConfigurationSnippetAnnotation]
	if !strings.Contains(got, `proxy_hide_header Content-Security-Policy`) {
		t.Fatalf("Element/chat must hide upstream frame-ancestors 'self', got:\n%s", got)
	}
	if !strings.Contains(got, "https://portal.desk.gentian.org") {
		t.Fatalf("expected kernel portal in snippet, got:\n%s", got)
	}
}

func TestKeycloakOIDCEmbeddingIngressSnippet(t *testing.T) {
	oidcSubs := map[string][]string{
		"demo": {"chat", "matrix", "meet", "wiki"},
	}
	snippet := keycloakOIDCEmbeddingIngressSnippet(
		"desk.gentian.org",
		[]string{"demo.desk.gentian.org"},
		oidcSubs,
		[]string{"demo"},
	)
	if !strings.Contains(snippet, "https://portal.desk.gentian.org") {
		t.Fatalf("expected kernel portal origin, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://id.desk.gentian.org") {
		t.Fatalf("expected explicit id origin for nested IdP iframes, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://*.desk.gentian.org") {
		t.Fatalf("expected kernel-zone wildcard for nested OIDC SSO, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://*.demo.desk.gentian.org") {
		t.Fatalf("expected tenant wildcard origin, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://chat.demo.desk.gentian.org") {
		t.Fatalf("expected explicit chat origin for Element SSO, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://matrix.demo.desk.gentian.org") {
		t.Fatalf("expected explicit matrix origin for Synapse OIDC, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, "https://wiki.demo.desk.gentian.org") {
		t.Fatalf("expected explicit wiki origin for XWiki OIDC, got:\n%s", snippet)
	}
	if !strings.Contains(snippet, `proxy_hide_header Content-Security-Policy`) {
		t.Fatal("Keycloak IdP ingress must replace upstream CSP (not append)")
	}
}
