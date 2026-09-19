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
// a personal recovery address as the user's identity. Enabling reset without this
// is the failure this guards.
func TestBuildGentianGroupsScript_RepointsEmailClaimAtUsername(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	for _, want := range []string{
		`keycloak_json_id_by_attr "${SCOPE_LIST}" "name" "email"`,
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
	// Doing nothing when it already points at the username keeps the Job quiet on
	// every later run instead of rewriting the same value.
	if !strings.Contains(script, "email claim already follows the username") {
		t.Fatal("expected the already-correct case to be a no-op")
	}
}
