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

func TestBuildBrokerIdentityProviderScriptUsesInternalTokenURL(t *testing.T) {
	script := buildBrokerIdentityProviderScript()

	// The Job must not write the IdP. tenant-default's IdentityProvider owns it,
	// and a second writer here is what let this script and the realm script
	// disagree about which first-broker-login flow a tenant realm used.
	//
	// Checked per curl invocation rather than by searching the whole script for
	// "-X PUT": the flow this Job still builds sets its executions' requirement
	// with PUT, and that is not the object in question. The instance path is
	// matched with a trailing quote so /mappers below is not caught by it.
	for _, inv := range strings.Split(script, "curl ")[1:] {
		if !strings.Contains(inv, `/identity-provider/instances/kernel"`) {
			continue
		}
		for _, verb := range []string{"PUT", "POST", "DELETE"} {
			if strings.Contains(inv, "-X "+verb) {
				t.Fatalf("broker IdP script %ss the kernel IdP — the Composition owns it", verb)
			}
		}
	}
	if strings.Contains(script, `IDP_BODY`) {
		t.Fatal("broker IdP script must not build an IdP payload")
	}
	// The secret was only ever read to put back into that payload; the
	// Composition takes it from the broker Client's connection secret now.
	if strings.Contains(script, `client-secret`) {
		t.Fatal("broker IdP script must not read the broker client secret")
	}
	// It must still create the flow the IdP's alias refers to. The alias is a
	// reference, so Keycloak rejects an IdP naming a flow that does not exist.
	for _, want := range []string{
		firstBrokerLoginFlowAlias,
		`idp-detect-existing-broker-user`,
		`first broker login flow ${FLOW_ALIAS} ready`,
		`oidc-user-attribute-idp-mapper`,
		`claim.name":"gentian_username`,
		`user.attribute":"uid"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("broker IdP script missing %q", want)
		}
	}
	if strings.Contains(script, `${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token`) {
		t.Fatal("broker IdP script must not use external URL for token exchange")
	}
	if strings.Contains(script, `%%{http_code}`) {
		t.Fatal("broker IdP script must use %{http_code} in curl -w (raw Go string, not fmt.Sprintf)")
	}
	if !strings.Contains(script, `%{http_code}`) {
		t.Fatal("broker IdP script must read HTTP status via curl -w %{http_code}")
	}
}
