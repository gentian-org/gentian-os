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

func TestBuildKernelTenantBrokerScript(t *testing.T) {
	script := buildKernelTenantBrokerScript()

	// The broker client and the kernel-realm IdP are tenant-default's now, and
	// both adopted the live objects with no drift. What is left here is the
	// kernel realm's own first-broker-login flow — one object shared by every
	// tenant, so it cannot be composed per tenant — and the IdP mappers that
	// hang off the IdP.
	for _, gone := range []string{
		`tokenUrl\":\"${KEYCLOAK_URL}/realms/${TENANT_REALM}/protocol/openid-connect/token`,
		`hideOnLoginPage\":\"true`,
		`"${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients"`,
	} {
		if strings.Contains(script, gone) {
			t.Fatalf("kernel tenant broker script still writes what the Composition owns: %s", gone)
		}
	}
	for _, want := range []string{
		kernelPortalBrokerClientID,
		kernelPortalFirstBrokerLoginFlowAlias,
		`_resolve_external_oidc_base`,
		// Read, not written: the mappers below need the IdP to exist.
		`"${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}"`,
		`oidc-advanced-group-idp-mapper`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("kernel tenant broker script missing %q", want)
		}
	}
}

func TestKernelExternalURLIncludesAuthPath(t *testing.T) {
	got := kernelExternalURL("platform.example.test")
	want := "https://id.platform.example.test/auth"
	if got != want {
		t.Fatalf("kernelExternalURL = %q, want %q", got, want)
	}
}
