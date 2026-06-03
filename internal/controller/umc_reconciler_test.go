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
)

func TestUmcChartPortalDomain(t *testing.T) {
	tests := []struct {
		effective, kernel string
		wantPortal        string
		wantDomain        string
	}{
		{"demo.desk.gentian.org", "desk.gentian.org", "demo", "desk.gentian.org"},
		{"acme.desk.gentian.org", "desk.gentian.org", "acme", "desk.gentian.org"},
		{"portal.customer.example", "desk.gentian.org", "portal", "customer.example"},
		{"", "desk.gentian.org", "", "desk.gentian.org"},
	}
	for _, tt := range tests {
		portal, domain := umcChartPortalDomain(tt.effective, tt.kernel)
		if portal != tt.wantPortal || domain != tt.wantDomain {
			t.Errorf("umcChartPortalDomain(%q, %q) = (%q, %q), want (%q, %q)",
				tt.effective, tt.kernel, portal, domain, tt.wantPortal, tt.wantDomain)
		}
	}
}

func TestBuildUMCGatewayHelmValues_GlobalDomain(t *testing.T) {
	vals := buildUMCGatewayHelmValues("demo.desk.gentian.org", "desk.gentian.org")
	global, ok := vals["global"].(map[string]interface{})
	if !ok {
		t.Fatal("expected global map in gateway values")
	}
	if global["domain"] != "desk.gentian.org" {
		t.Fatalf("global.domain = %v, want desk.gentian.org", global["domain"])
	}
	subs, ok := global["subDomains"].(map[string]interface{})
	if !ok {
		t.Fatal("expected global.subDomains map")
	}
	if subs["portal"] != "demo" {
		t.Fatalf("global.subDomains.portal = %v, want demo", subs["portal"])
	}
	if global["configMapUcr"] != umcUCRConfigMapName {
		t.Fatalf("global.configMapUcr = %v, want %s", global["configMapUcr"], umcUCRConfigMapName)
	}
	ingress, ok := vals["ingress"].(map[string]interface{})
	if !ok {
		t.Fatal("expected ingress map in gateway values")
	}
	if ingress["enableLoginPath"] != true {
		t.Fatalf("ingress.enableLoginPath = %v, want true", ingress["enableLoginPath"])
	}
}

func TestNubusTenantLoginURL(t *testing.T) {
	got := nubusTenantLoginURL("demo.desk.gentian.org")
	want := "https://demo.desk.gentian.org/univention/login/?location=%2Funivention%2Fmanagement%2F"
	if got != want {
		t.Fatalf("nubusTenantLoginURL = %q, want %q", got, want)
	}
}

func TestBuildUMCHelmValues_IngressPathsExcludeManagement(t *testing.T) {
	vals := buildUMCHelmValues("demo.desk.gentian.org", "desk.gentian.org")
	ingress, ok := vals["ingress"].(map[string]interface{})
	if !ok {
		t.Fatal("expected ingress map in umc-server values")
	}
	paths, ok := ingress["paths"].([]interface{})
	if !ok || len(paths) != 1 {
		t.Fatalf("expected single ingress path, got %#v", ingress["paths"])
	}
	path, ok := paths[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected path map")
	}
	pathStr, _ := path["path"].(string)
	if strings.Contains(pathStr, "management") {
		t.Fatalf("umc-server ingress path must not include management (served by umc-gateway): %s", pathStr)
	}
	podAnn, ok := vals["podAnnotations"].(map[string]interface{})
	if !ok || podAnn[reloaderAutoAnnotation] != "true" {
		t.Fatalf("expected reloader pod annotation on umc-server, got %#v", vals["podAnnotations"])
	}
}

func TestUmcOIDCUCRLines(t *testing.T) {
	lines := umcOIDCUCRLines(
		"https://id.desk.gentian.org/realms/demo",
		"http://keycloak/realms/demo",
		"https://demo.desk.gentian.org/univention/oidc/",
	)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"umc/oidc/default-op: nubus",
		"umc/oidc/nubus/openid-configuration:",
		"umc/oidc/nubus/client-id: https://demo.desk.gentian.org/univention/oidc/",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in OIDC UCR lines:\n%s", want, joined)
		}
	}
}

func TestUmcApacheUCRLines(t *testing.T) {
	lines := umcApacheUCRLines()
	if !strings.Contains(strings.Join(lines, "\n"), "apache2/loglevel: info") {
		t.Fatalf("expected apache2/loglevel in gateway UCR lines: %v", lines)
	}
}
