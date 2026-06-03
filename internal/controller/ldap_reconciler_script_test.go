/*
Copyright 2026 The Gentian Authors.

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

func TestBuildAdminPolicyScript_IdempotentRedeploy(t *testing.T) {
	t.Parallel()
	script := buildAdminPolicyScript("ou=demo,${UDM_LDAP_BASE}", "demo")
	for _, want := range []string{
		"udm_patch_ok",
		"uid=${ADMIN_USERNAME},${USERS_OU_POS}",
		"grep -qF \"${ADMINS_GRP_DN}\"",
		"grep -qF \"${ADMIN_DN}\"",
		"admin policy provisioning complete for ${ADMIN_USERNAME}",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected admin policy script to contain %q", want)
		}
	}
	if strings.Contains(script, "NEEDS_PATCH") {
		t.Fatal("admin policy script should not use combined NEEDS_PATCH PATCH")
	}
}

func TestBuildAdminUserScript_IdempotentRedeploy(t *testing.T) {
	t.Parallel()
	script := buildAdminUserScript("ou=demo,${UDM_LDAP_BASE}", "demo", "admin-demo@gentian.org")
	for _, want := range []string{
		"udm_patch_ok",
		"UDM user ${ADMIN_USERNAME} already exists",
		"Never reset the password when the user already existed",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected admin user script to contain %q", want)
		}
	}
}
