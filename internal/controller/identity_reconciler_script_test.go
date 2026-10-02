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
	"os"
	"os/exec"
	"strings"
	"testing"
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
	if !strings.Contains(script, "curl -sf -H \"${AUTH_HEADER}\"") {
		t.Fatalf("script must pass authorization via -H \"${AUTH_HEADER}\"")
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
