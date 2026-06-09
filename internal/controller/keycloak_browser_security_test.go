// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildRealmBrowserSecurityHeadersScript(t *testing.T) {
	script := buildRealmBrowserSecurityHeadersScript("demo")
	if !strings.Contains(script, `admin/realms/demo`) {
		t.Fatalf("expected demo realm PUT, got:\n%s", script)
	}
	if !strings.Contains(script, `"xFrameOptions":""`) {
		t.Fatal("expected empty xFrameOptions in browserSecurityHeaders payload")
	}
}

func TestKeycloakOIDCEmbeddingIngressSnippetStripsXFrameOptions(t *testing.T) {
	snippet := keycloakOIDCEmbeddingIngressSnippet("desk.gentian.org", []string{"demo.desk.gentian.org"})
	if !strings.Contains(snippet, `proxy_hide_header X-Frame-Options`) {
		t.Fatalf("expected X-Frame-Options hide in IdP snippet, got:\n%s", snippet)
	}
	if strings.Count(snippet, `proxy_hide_header X-Frame-Options`) < 2 {
		t.Fatal("expected server+location X-Frame-Options hide for microk8s ingress")
	}
}
