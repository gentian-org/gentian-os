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
)

func TestBuildRealmScript_UsesKeycloakJSONIDExtractor(t *testing.T) {
	t.Parallel()
	script := buildRealmScript("demo", "Demo")
	if !strings.Contains(script, "keycloak_json_id_by_attr") {
		t.Fatal("expected keycloak_json_id_by_attr helper in realm script")
	}
	// No broker section at all: the bridges between a tenant realm and the
	// kernel realm, in either direction, are the Compositions' now
	// (hub_projection.go), so the script neither reads KERNEL_REALM nor
	// writes a client in that realm.
	for _, gone := range []string{"SSO Identity Brokering", "KERNEL_REALM", "KERNEL_EXTERNAL_URL", "broker-${REALM_NAME}"} {
		if strings.Contains(script, gone) {
			t.Fatalf("realm script still carries the kernel broker: %s", gone)
		}
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
