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

func TestBuildBrokerIdentityProviderScriptUsesInternalTokenURL(t *testing.T) {
	script := buildBrokerIdentityProviderScript()
	if strings.Contains(script, `\"firstBrokerLoginFlowAlias\":`+`"`+firstBrokerLoginFlowAlias) {
		t.Fatal("broker IdP script must not use Go-quoted alias inside shell JSON")
	}
	if !strings.Contains(script, `\"firstBrokerLoginFlowAlias\":\"`+firstBrokerLoginFlowAlias+`\"`) {
		t.Fatal("broker IdP script must embed firstBrokerLoginFlowAlias with shell-safe JSON escaping")
	}
	for _, want := range []string{
		firstBrokerLoginFlowAlias,
		`idp-detect-existing-broker-user`,
		`first broker login flow ${FLOW_ALIAS} ready`,
		`oidc-user-attribute-idp-mapper`,
		`claim.name":"gentian_username`,
		`user.attribute":"uid"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("broker IdP script missing %q", want)
		}
	}
	if strings.Contains(script, `${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token`) {
		t.Fatal("broker IdP script must not use external URL for token exchange")
	}
	if strings.Contains(script, `%%{http_code}`) {
		t.Fatal("broker IdP script must use %{http_code} in curl -w (raw Go string, not fmt.Sprintf)")
	}
	if !strings.Contains(script, `%{http_code}`) {
		t.Fatal("broker IdP script must read HTTP status via curl -w %{http_code}")
	}
}

func TestKeycloakProxyIngressBufferAnnotations(t *testing.T) {
	ann := map[string]string{}
	ensureKeycloakProxyIngressBuffers(ann)
	if !keycloakProxyIngressBuffersApplied(ann) {
		t.Fatal("expected buffer annotations to be applied")
	}
	if ann[nginxProxyBufferSizeAnnotation] != "64k" {
		t.Fatalf("proxy-buffer-size = %q, want 64k", ann[nginxProxyBufferSizeAnnotation])
	}
}
