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

// TestRealmScriptLeavesTheBrokerMappersToTheComposition guards the last write
// the broker Jobs made.
//
// Two mappers carry gentian_username across the realm boundary: the kernel
// broker client emits the claim, and the tenant realm's IdP imports it back into
// the user's uid. Three scripts used to write those two objects — this one and
// the broker-idp Job, which is now retired. tenant-default composes them.
//
// A returning writer is not loud. The mapper it POSTs is the one already there,
// so Keycloak accepts it and nothing fails; the drift only shows when the
// Composition and the script disagree about a field, which is the case that took
// three sessions to find the last time.
func TestRealmScriptLeavesTheBrokerMappersToTheComposition(t *testing.T) {
	script := buildRealmScript("corp", "Corp")

	for _, gone := range []string{
		`oidc-user-attribute-idp-mapper`,
		`oidc-usermodel-attribute-mapper`,
		`/protocol-mappers/models`,
		`/identity-provider/instances/kernel/mappers`,
	} {
		if strings.Contains(script, gone) {
			t.Fatalf("realm script still writes a mapper the Composition owns: %s", gone)
		}
	}

	// It must still preserve the flow alias rather than restate it. The
	// Composition sets the gentian first-broker-login flow; a literal here would
	// revert it on every realm re-run.
	if !strings.Contains(script, `\"firstBrokerLoginFlowAlias\":\"${FBL_ALIAS}\"`) {
		t.Fatal("realm script must carry forward the observed first-broker-login flow alias")
	}
}
