/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
