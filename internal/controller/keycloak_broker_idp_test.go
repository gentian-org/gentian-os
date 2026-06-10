// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildBrokerIdentityProviderScriptUsesInternalTokenURL(t *testing.T) {
	script := buildBrokerIdentityProviderScript()
	for _, want := range []string{
		firstBrokerLoginFlowAlias,
		`${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token`,
		`user-attribute-ldap-mapper`,
		`ensure_ldap_uid_attribute_mapper "${KERNEL_REALM}" "ldap-provider"`,
		`oidc-user-attribute-idp-mapper`,
		`claim.name":"opendesk_username`,
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
