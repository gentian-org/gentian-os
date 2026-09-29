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

func TestBuildGentianGroupsScript_WaitsForRealm(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	if !strings.Contains(script, "not available after waiting") {
		t.Fatal("expected realm wait helper in gentian groups script")
	}
	if strings.Contains(script, "groups?search=") {
		t.Fatal("expected full group list lookup without search query")
	}
	if !strings.Contains(script, "groups?max=1000") {
		t.Fatal("expected paginated group list lookup")
	}
}

// Keycloak's password reset mails the email FIELD and cannot be pointed at an
// attribute, so the field has to hold the recovery address — which means the
// email CLAIM must come from the username instead, or applications would receive
// a personal recovery address as the user's identity.
func TestBuildGentianGroupsScript_RepointsEmailClaimAtUsername(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	for _, want := range []string{
		`"name" "email"`,
		`.config["user.attribute"] = "username"`,
		"client-scopes/${EMAIL_SCOPE_ID}/protocol-mappers/models/${EMAIL_MAPPER_ID}",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("email claim step missing %q", want)
		}
	}
	// Patched, not rebuilt: a PUT replaces the mapper config wholesale, so naming
	// only the one key keeps Keycloak's own defaults exactly as it wrote them and
	// leaves nothing for the Composition to see as drift.
	if strings.Contains(script, `"config":{"user.attribute":"username"`) {
		t.Fatal("email mapper config is rebuilt rather than patched; Keycloak's own keys would be dropped")
	}
	if !strings.Contains(script, "email claim already follows the username") {
		t.Fatal("expected the already-correct case to be a no-op")
	}
}

// An admin token lives 60 seconds and the group loop takes longer, so this step
// has to run before it. Placed after the loop once, every request came back 401
// and the script reported that the email scope had no mapper named email — a
// Keycloak state that was not the case.
func TestBuildGentianGroupsScript_RepointsEmailClaimBeforeTheGroupLoop(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	claimStep := strings.Index(script, "EMAIL_SCOPE_ID=")
	groupLoop := strings.Index(script, "while read -r group; do")
	if claimStep < 0 || groupLoop < 0 {
		t.Fatalf("cannot locate both steps: claim=%d loop=%d", claimStep, groupLoop)
	}
	if claimStep > groupLoop {
		t.Fatal("the email claim step runs after the group loop, by which time the admin token has expired")
	}
}

// A request that fails must not be reported as an answer. Every status is checked
// and every failure exits non-zero, so a token that expired cannot masquerade as
// a realm whose mapper is missing.
func TestBuildGentianGroupsScript_EmailClaimFailuresAreLoud(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	for _, want := range []string{
		"ERROR: listing client scopes returned HTTP ${EMAIL_SCOPES_CODE}",
		"ERROR: listing the email scope's mappers returned HTTP ${EMAIL_MAPPERS_CODE}",
		"ERROR: repointing the email claim returned HTTP ${EMAIL_PUT_CODE}",
		"ERROR: the email client scope in realm ${REALM} has no mapper named email",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing explicit failure report: %q", want)
		}
	}
	// The fallback that caused this: a failed list became an empty list, and an
	// empty list read as "no such mapper". Scoped to the email path — the groups
	// scope above still has one, which is pre-existing and has the same hazard.
	if strings.Contains(script, `client-scopes/${EMAIL_SCOPE_ID}/protocol-mappers/models" || echo "[]"`) {
		t.Fatal("a failed email mapper listing still falls back to an empty array")
	}
}
