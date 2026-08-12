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
