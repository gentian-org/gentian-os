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
		"TENANT_TEMPLATE_DN=\"cn=App User,cn=templates,${OU_POS}\"",
		"tenant App User template ${TENANT_TEMPLATE_DN} is the UMC default",
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
		"user ${ADMIN_USERNAME} password synced",
		"${ADMIN_PASSWORD}",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected admin user script to contain %q", want)
		}
	}
}

func TestBuildAppUserTemplateScript_PrefillsTenantMailDomain(t *testing.T) {
	t.Parallel()
	script := buildAppUserTemplateScript("ou=demo,${UDM_LDAP_BASE}", "demo", "demo.desk.gentian.org")
	for _, want := range []string{
		"openDesk User",
		"mailPrimaryAddress",
		`"mailPrimaryAddress": "<username>@${MAIL_DOMAIN}"`,
		"MAIL_DOMAIN=\"demo.desk.gentian.org\"",
		"cn=templates,${OU_POS}",
		`"name": "App User"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected App User template script to contain %q", want)
		}
	}
}

func TestBuildOUDeleteScript_FailsOnNonSuccessHTTP(t *testing.T) {
	t.Parallel()
	script := buildOUDeleteScript("ou=test,dc=example,dc=com")
	for _, want := range []string{
		`case "${HTTP}" in`,
		`200|204|404) ;;`,
		`exit 1`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("buildOUDeleteScript() missing %q", want)
		}
	}
}
