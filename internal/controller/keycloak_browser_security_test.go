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
	snippet := keycloakOIDCEmbeddingIngressSnippet("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	if !strings.Contains(snippet, `proxy_hide_header X-Frame-Options`) {
		t.Fatalf("expected X-Frame-Options hide in IdP snippet, got:\n%s", snippet)
	}
	if strings.Count(snippet, `proxy_hide_header X-Frame-Options`) < 2 {
		t.Fatal("expected server+location X-Frame-Options hide for microk8s ingress")
	}
	if !strings.Contains(snippet, `add_header X-Frame-Options "" always`) {
		t.Fatal("expected empty X-Frame-Options override in IdP snippet")
	}
}
