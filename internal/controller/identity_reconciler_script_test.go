/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestBuildAdminScript_UsesSafeAuthHeaderExpansion(t *testing.T) {
	t.Parallel()
	script := buildAdminScript("gtn-demo")

	if strings.Contains(script, "AUTH=\"-H") {
		t.Fatalf("script should not construct AUTH as embedded shell arguments")
	}
	if strings.Contains(script, "${AUTH}") {
		t.Fatalf("script should not expand ${AUTH} in curl calls")
	}
	if !strings.Contains(script, "AUTH_HEADER=\"Authorization: Bearer ${TOKEN}\"") {
		t.Fatalf("script must define AUTH_HEADER")
	}
	if !strings.Contains(script, "kc_curl -H \"${AUTH_HEADER}\"") {
		t.Fatalf("script must pass authorization via -H \"${AUTH_HEADER}\"")
	}
	// Every admin call says what failed: none goes through curl -f, which
	// hides the answer.
	// The one curl -f left is the token step, which reports its own failure.
	if strings.Count(script, "curl -sf") != 1 || !strings.Contains(script, "kc_curl() {") {
		t.Fatalf("every admin call must go through kc_curl, which reports a refused call")
	}
	// No password: the platform neither sets, resets nor prints one. Its
	// holder sets it through an activation link.
	for _, forbidden := range []string{"reset-password", "TENANT_ADMIN_PASSWORD", "INITIAL_TENANT_ADMIN"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("the tenant-admin script must not handle a password: found %q", forbidden)
		}
	}
	if !strings.Contains(script, `requiredActions\":[${ACTIONS}]`) || !strings.Contains(script, `"UPDATE_PASSWORD","CONFIGURE_TOTP"`) {
		t.Fatal("the account must be created with its activation steps as required actions")
	}
}

// In a realm people sign up to, every account must have an address, and the
// administrator's is created with its login as the address, verified: the
// realm refuses the account without one, and a confirmation sent to an
// address nothing is delivered to could never be answered. Every other
// tenant's administrator is created without an address, as before.
func TestTheAdministratorOfASignUpTenantIsCreatedWithItsAddress(t *testing.T) {
	t.Parallel()
	script := buildAdminScript("customers")
	for _, want := range []string{
		`if [ "${TENANT_SIGN_UP:-false}" = "true" ]; then`,
		`ADDRESS=",\"email\":\"${TENANT_ADMIN_USERNAME}\""`,
		`VERIFIED=true`,
		`\"emailVerified\":${VERIFIED}${ADDRESS},`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
	if strings.Count(script, "emailVerified") != 1 {
		t.Error("emailVerified is stated more than once in the request")
	}

	plain := acmeTenantFixture()
	signUp := acmeTenantFixture()
	signUp.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Enabled: true}
	for tenant, want := range map[*gentianov1alpha1.Tenant]string{plain: "false", signUp: "true"} {
		job := makeAdminJob(tenant, "acme", "admin@acme.k.example")
		got := ""
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "TENANT_SIGN_UP" {
				got = e.Value
			}
		}
		if got != want {
			t.Errorf("TENANT_SIGN_UP = %q for a tenant with sign-up %s", got, want)
		}
	}
}

// The script is shell; a quoting slip in it is a Job that fails in the
// cluster and nowhere else.
func TestBuildAdminScript_IsValidShell(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "admin-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(buildAdminScript("gtn-demo")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, err := exec.Command("sh", "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}
