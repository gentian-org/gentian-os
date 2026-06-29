// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildKernelTenantBrokerScript(t *testing.T) {
	script := buildKernelTenantBrokerScript()
	for _, want := range []string{
		kernelPortalBrokerClientID,
		kernelPortalFirstBrokerLoginFlowAlias,
		`_resolve_external_oidc_base`,
		`"${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}"`,
		`tokenUrl\":\"${KEYCLOAK_URL}/realms/${TENANT_REALM}/protocol/openid-connect/token`,
		`oidc-advanced-group-idp-mapper`,
		`hideOnLoginPage\":\"true`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("kernel tenant broker script missing %q", want)
		}
	}
	if strings.Contains(script, `${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}/protocol/openid-connect/token`) {
		t.Fatal("kernel tenant broker script must not use external URL for token exchange")
	}
}

func TestKernelExternalURLIncludesAuthPath(t *testing.T) {
	got := kernelExternalURL("desk.gentian.org")
	want := "https://id.desk.gentian.org/auth"
	if got != want {
		t.Fatalf("kernelExternalURL = %q, want %q", got, want)
	}
}
