// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildBrokerIdentityProviderScriptUsesInternalTokenURL(t *testing.T) {
	script := buildBrokerIdentityProviderScript()
	for _, want := range []string{
		`${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token`,
		`${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/certs`,
		`${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/auth`,
		`issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("broker IdP script missing %q", want)
		}
	}
	if strings.Contains(script, `${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token`) {
		t.Fatal("broker IdP script must not use external URL for token exchange")
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
