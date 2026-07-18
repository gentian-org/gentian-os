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

func TestBuildRealmScript_UsesKeycloakJSONIDExtractor(t *testing.T) {
	t.Parallel()
	script := buildRealmScript("demo", "Demo")
	if !strings.Contains(script, "keycloak_json_id_by_attr") {
		t.Fatal("expected keycloak_json_id_by_attr helper in realm script")
	}
	if !strings.Contains(script, "SSO Identity Brokering") {
		t.Fatal("expected kernel SSO brokering block in realm script")
	}
	if !strings.Contains(script, `\"firstBrokerLoginFlowAlias\":\"first broker login\"`) {
		t.Fatal("realm script must use built-in first broker login flow for initial IdP registration")
	}
	if strings.Contains(script, firstBrokerLoginFlowAlias) {
		t.Fatal("realm script must register kernel IdP with built-in first broker login flow only")
	}
	if !strings.Contains(script, "gentian.inviteEmail") {
		t.Fatal("expected gentian.inviteEmail user profile block in realm script")
	}
	if !strings.Contains(script, "VERIFY_PROFILE UPDATE_PROFILE") {
		t.Fatal("expected profile prompt required actions to be disabled in realm script")
	}

	path := t.TempDir() + "/realm.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("realm script must be valid POSIX sh: %v\n%s", err, out)
	}
}
