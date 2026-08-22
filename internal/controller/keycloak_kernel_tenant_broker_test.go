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

	// One object is left here: the kernel realm's own first-broker-login flow.
	// The broker client, the kernel-realm IdP, and the two mappers that hang off
	// it — which tenant a brokered user came from, and the groups that carry
	// their entitlements — are all tenant-default's, adopted with no drift.
	//
	// The flow cannot follow them. No XTenant covers the kernel realm, so no
	// Composition reaches it; it is one flow shared by every tenant, and this Job
	// is per-tenant only because that is where the reconcile loop lives.
	for _, gone := range []string{
		`hideOnLoginPage\":\"true`,
		`"${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients"`,
		`oidc-advanced-group-idp-mapper`,
		`hardcoded-attribute-idp-mapper`,
		`/identity-provider/instances/`,
	} {
		if strings.Contains(script, gone) {
			t.Fatalf("kernel tenant broker script still writes what the Composition owns: %s", gone)
		}
	}
	if !strings.Contains(script, kernelPortalFirstBrokerLoginFlowAlias) {
		t.Fatalf("kernel tenant broker script missing %q", kernelPortalFirstBrokerLoginFlowAlias)
	}
}

func TestKernelExternalURLIncludesAuthPath(t *testing.T) {
	got := kernelExternalURL("platform.example.test")
	want := "https://id.platform.example.test/auth"
	if got != want {
		t.Fatalf("kernelExternalURL = %q, want %q", got, want)
	}
}
