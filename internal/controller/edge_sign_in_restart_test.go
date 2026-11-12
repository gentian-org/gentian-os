/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

// Every policy in front of a session carries the restart: the authenticated
// Gateway's, and the administration console's on the perimeter Gateway.
func TestSessionPoliciesRestartAFailedSignIn(t *testing.T) {
	for name, spec := range map[string]map[string]interface{}{
		"the authenticated Gateway":  authenticatedGatewayBackendTrafficPolicySpec("12h", ""),
		"the administration console": keycloakAdminBackendTrafficPolicySpec(""),
	} {
		overrides, ok := spec["responseOverride"].([]interface{})
		if !ok || len(overrides) != 1 {
			t.Fatalf("%s: responseOverride = %#v, want the sign-in restart", name, spec["responseOverride"])
		}
	}
}

// Only the OIDC filter's own refusals are rewritten, and their status is kept.
func TestSignInRestartRewritesOnlyTheFiltersRefusals(t *testing.T) {
	rule := edgeSignInRestart()[0].(map[string]interface{})
	if rule["source"] != "Local" {
		t.Errorf("source = %v, want Local: a backend's 401 is the backend's", rule["source"])
	}
	match := rule["match"].(map[string]interface{})
	codes := match["statusCodes"].([]interface{})
	if len(codes) != 1 || codes[0].(map[string]interface{})["value"] != int64(401) {
		t.Errorf("statusCodes = %v, want 401 alone", codes)
	}
	header := match["responseHeaders"].([]interface{})[0].(map[string]interface{})
	value := header["value"].(map[string]interface{})
	if header["name"] != "content-type" || value["value"] != "text/plain" {
		t.Errorf("responseHeaders = %v, want content-type text/plain: the bouncer's HTML refusals keep their page", header)
	}
	response := rule["response"].(map[string]interface{})
	if _, set := response["statusCode"]; set {
		t.Error("the status is overridden; a refused fetch must still read 401")
	}
	if _, has := rule["redirect"]; has {
		t.Error("redirect is set: Envoy follows it itself, and the browser never starts a new sign-in")
	}
}

// The page restarts once, on the callback, to this origin only.
func TestSignInRestartPageCannotLoopOrLeave(t *testing.T) {
	page := edgeSignInRestartPage
	if strings.Contains(page, "%") {
		t.Error("the page contains a percent sign, which Envoy reads as a command operator")
	}
	for _, want := range []string{
		`location.pathname.indexOf("/oauth2/callback") !== 0`, // only the callback restarts
		`sessionStorage`,                 // once per tab ...
		`now - last > 120000`,            // ... in two minutes
		`url.origin === location.origin`, // never another origin
		`href="/"`,                       // a link when it does not restart
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}
