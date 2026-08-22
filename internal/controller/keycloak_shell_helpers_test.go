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
	// The IdP is not written here at all any more, so there is no alias to
	// preserve and no bootstrap value to fall back to. The whole
	// carry-forward-the-observed-alias mechanism existed to keep two writers from
	// contradicting each other; with one writer there is nothing to carry.
	for _, gone := range []string{
		`FBL_ALIAS`,
		`firstBrokerLoginFlowAlias`,
		`IDP_BODY`,
		`identity-provider/instances`,
		"first-broker-login-gentian",
	} {
		if strings.Contains(script, gone) {
			t.Fatalf("realm script still writes the kernel IdP: %s", gone)
		}
	}
	// The broker client stays: it is Observe-only in the Composition by design,
	// so something has to create it, and on a realm that does not exist yet that
	// something cannot be the Composition.
	if !strings.Contains(script, `${KERNEL_REALM}/clients`) {
		t.Fatal("realm script must still bootstrap the kernel-realm broker client")
	}
	// The user profile is tenant-default's now: it declares all six attributes
	// whole, where this script appended one patch to add uid and
	// gentian.inviteEmail and a second to strip `required` off the name fields.
	if strings.Contains(script, "gentian.inviteEmail") {
		t.Fatal("realm script must not write the user profile; the Composition owns it")
	}
	// The two profile-prompt required actions are tenant-default's now — it
	// composes a RequiredAction for each, and both adopted the live ones. The
	// realm script must not disable them as well.
	if strings.Contains(script, "VERIFY_PROFILE UPDATE_PROFILE") {
		t.Fatal("realm script must not disable the profile prompts; the Composition owns them")
	}
	// Nor the profile relaxation. It is the composed UserProfile's absence of
	// requiredForRoles on firstName and lastName — declared, rather than patched
	// back out of the document on every run.
	if strings.Contains(script, `.name == "firstName" or .name == "lastName"`) {
		t.Fatal("realm script must not relax the name fields; the Composition declares them optional")
	}

	path := t.TempDir() + "/realm.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("realm script must be valid POSIX sh: %v\n%s", err, out)
	}
}
